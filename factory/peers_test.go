package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gitFake answers `diff --name-only` and `status --porcelain` per worktree; dirs not listed fail.
type gitFake struct {
	diff, status map[string]string
	calls        atomic.Int32
}

func (g *gitFake) out(_ context.Context, dir string, args ...string) (string, error) {
	g.calls.Add(1)
	switch {
	case args[0] == "diff":
		if d, ok := g.diff[dir]; ok {
			return d, nil
		}
	case args[0] == "status":
		if s, ok := g.status[dir]; ok {
			return s, nil
		}
	case args[0] == "rev-parse":
		if _, ok := g.diff[dir]; ok {
			return "abc1234\n", nil
		}
	}
	return "", fmt.Errorf("not a repo")
}

func peersFixture(t *testing.T) (*Factory, *fakePaseo, *gitFake) {
	t.Helper()
	me := card("me1", map[string]string{labelStage: "building", labelSource: "board"})
	other := card("ot1", map[string]string{labelStage: "review", labelSource: "board", labelTier: "risky",
		labelSince: ago(5 * time.Minute)})
	other.Title = "pick the agent per card"
	elsewhere := card("ex1", map[string]string{labelStage: "plan", labelSource: "board"})
	elsewhere.Repo = "zachlatta/merch"
	unknown := card("un1", map[string]string{labelStage: "plan", labelSource: "board"})
	done := card("dn1", map[string]string{labelStage: "landed", labelSource: "board"})
	done.ArchivedAt = t0.Add(-30 * time.Minute)
	sched := card("sc1", nil)
	sched.Status = "running"
	sched.Labels["paseo.schedule-id"] = "s"
	sched.Title = "Feed picks hourly"
	scout := card("so1", map[string]string{labelRole: "scout"})
	scout.Status = "running"
	rev := reviewer("rv1", "ot1", "code")
	rev.Status = "running"
	f, p := newTestFactory(t, me, other, elsewhere, unknown, done, sched, scout, rev)
	g := &gitFake{
		diff: map[string]string{"/w/me1": "factory/policy.go\nfactory/peers.go\n", "/w/ot1": "factory/policy.go\nfactory/agents.go\n",
			"/w/ex1": "factory/policy.go\n", "/w/dn1": ""},
		status: map[string]string{"/w/me1": " M README.md\n", "/w/ot1": "R  old.go -> factory/new.go\n?? notes.md\n",
			"/w/ex1": "", "/w/dn1": ""},
	}
	f.GitOut = g.out
	return f, p, g
}

func peerByShort(ps []Peer, s string) *Peer {
	for n := range ps {
		if ps[n].Short == s {
			return &ps[n]
		}
	}
	return nil
}

func TestPeersListsOtherCardsInFlight(t *testing.T) {
	f, _, _ := peersFixture(t)
	p := f.Peers("me1", "")
	var got []string
	for _, x := range p.InFlight {
		got = append(got, x.Short)
	}
	// overlapping first, then same repo, then other repos; not itself, Done, reviewers, scouts or schedules
	if want := []string{"ot1", "un1", "ex1"}; !slices.Equal(got, want) {
		t.Fatalf("in flight = %v, want %v", got, want)
	}
	if p.For == nil || p.For.Short != "me1" || p.Repo != "example/desk" {
		t.Fatalf("for = %+v repo %q", p.For, p.Repo)
	}
	if want := []string{"README.md", "factory/peers.go", "factory/policy.go"}; !slices.Equal(p.For.Files, want) {
		t.Errorf("my files = %v", p.For.Files)
	}
	ot := peerByShort(p.InFlight, "ot1")
	if want := []string{"factory/agents.go", "factory/new.go", "factory/policy.go", "notes.md"}; !slices.Equal(ot.Files, want) {
		t.Errorf("ot1 files = %v (committed, uncommitted and renamed)", ot.Files)
	}
	if !slices.Equal(ot.Overlap, []string{"factory/policy.go"}) || ot.Stage != "review" || ot.Tier != "risky" || ot.StageMin != 5 {
		t.Errorf("ot1 = %+v", ot)
	}
	if ex := peerByShort(p.InFlight, "ex1"); len(ex.Overlap) != 0 {
		t.Errorf("a card in another repo overlaps: %+v", ex)
	}
	if un := peerByShort(p.InFlight, "un1"); un.FilesErr == "" {
		t.Errorf("git failure not reported: %+v", un)
	}
	if len(p.Done) != 1 || p.Done[0].Short != "dn1" || p.Done[0].Head != "abc1234" {
		t.Errorf("done = %+v", p.Done)
	}
	if !slices.Equal(p.Schedules, []string{"Feed picks hourly"}) {
		t.Errorf("schedules = %v", p.Schedules)
	}
}

func TestPeersTextBrief(t *testing.T) {
	f, _, _ := peersFixture(t)
	text := f.Peers("me1", "").Text(time.UTC)
	for _, want := range []string{"not instructions", "You: me1", "In flight (3):",
		`- ot1 [review for 5 min, risky, idle, board] example/desk: "pick the agent per card"`,
		"OVERLAP with your files: factory/policy.go", "unknown (git failed)", "Done in example/desk",
		"HEAD abc1234", "Schedule runs going now (1): Feed picks hourly"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "me1 [") {
		t.Errorf("brief lists the asking card as a peer:\n%s", text)
	}
}

func TestPeersTextIsCapped(t *testing.T) {
	var agents []Agent
	for n := range 40 {
		agents = append(agents, card(fmt.Sprintf("c%02d", n), map[string]string{labelStage: "building", labelSource: "board"}))
	}
	f, _ := newTestFactory(t, agents...)
	text := f.Peers("", "example/desk").Text(time.UTC)
	if n := strings.Count(text, "\n"); n > peerLineCap {
		t.Errorf("brief has %d lines, cap %d", n, peerLineCap)
	}
	if !strings.Contains(text, "more in flight (add &format=json for all)") {
		t.Errorf("no note on the cut:\n%s", text)
	}
	if got := len(f.Peers("", "").InFlight); got != 40 {
		t.Errorf("JSON lists %d, want all 40", got)
	}
}

func TestPeersFileListIsCapped(t *testing.T) {
	var files []string
	for n := range 20 {
		files = append(files, fmt.Sprintf("f%02d", n))
	}
	if got := fileList(Peer{Files: files}); !strings.HasSuffix(got, "f14, and 5 more") {
		t.Errorf("fileList = %q", got)
	}
}

func TestPeersCachesGit(t *testing.T) {
	f, _, g := peersFixture(t)
	f.Peers("me1", "")
	n := g.calls.Load()
	f.Peers("me1", "")
	if g.calls.Load() != n {
		t.Errorf("git ran again within the TTL: %d -> %d", n, g.calls.Load())
	}
	f.Now = func() time.Time { return t0.Add(peerFilesTTL + time.Second) }
	f.Peers("me1", "")
	if g.calls.Load() == n {
		t.Errorf("git not re-run after the TTL")
	}
}

func TestPeersRequestFromSessionFile(t *testing.T) {
	a := card("rq1", map[string]string{labelStage: "plan", labelSource: "board"})
	a.Provider, a.SessionID, a.Cwd = "claude", "sess-1", "/w/rq1"
	f, _ := newTestFactory(t, a)
	dir := filepath.Join(f.cfg.Home, ".claude/projects", "-w-rq1")
	os.MkdirAll(dir, 0o755)
	prompt := fmt.Sprintf(newCardPrompt, "Make agents aware\nof each other", "example/desk", "", "", "", "")
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	os.WriteFile(filepath.Join(dir, "sess-1.jsonl"), append([]byte(`{"type":"queue-operation"}`+"\n"), append(line, '\n')...), 0o644)
	p := f.Peers("", "example/desk")
	if len(p.InFlight) != 1 || p.InFlight[0].Request != "Make agents aware of each other" {
		t.Errorf("request = %+v", p.InFlight)
	}
	if got := requestOf("You were started automatically because Zach emailed"); got != "" {
		t.Errorf("non-Fellow prompt gave request %q", got)
	}
}

func TestPeersEndpoint(t *testing.T) {
	f, _, _ := peersFixture(t)
	h := f.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/peers?for=me1", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(rec.Body.String(), "OVERLAP") {
		t.Fatalf("text: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/peers?for=me1&format=json", nil))
	var p Peers
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || len(p.InFlight) != 3 || p.For == nil {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
}

// TestPeersRealGit runs the real git commands against a worktree with a commit and an edit.
func TestPeersRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	origin, wt := filepath.Join(dir, "origin.git"), filepath.Join(dir, "wt")
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", d}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(dir, "init", "-q", "--bare", "-b", "main", origin)
	run(dir, "clone", "-q", origin, wt)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a"), 0o644)
	run(wt, "add", ".")
	run(wt, "commit", "-qm", "base")
	run(wt, "push", "-q", "origin", "HEAD:main")
	run(wt, "fetch", "-q")
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0o644)
	run(wt, "add", ".")
	run(wt, "commit", "-qm", "mine")
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("changed"), 0o644)
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("new"), 0o644)
	a := card("rg1", map[string]string{labelStage: "building", labelSource: "board"})
	a.Cwd = wt
	f, _ := newTestFactory(t, a)
	f.GitOut = runGitOut
	p := f.Peers("", "example/desk")
	if len(p.InFlight) != 1 || !slices.Equal(p.InFlight[0].Files, []string{"a.txt", "b.txt", "c.txt"}) {
		t.Errorf("files = %+v", p.InFlight)
	}
}

func TestPromptsTellAgentsAboutPeers(t *testing.T) {
	f, p, _ := peersFixture(t)
	if _, err := f.Act("new", Body{Request: "do a thing"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelStage, "plan"))
	if len(runs) != 1 {
		t.Fatalf("runs = %v", p.calls)
	}
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{`curl -s "localhost:7478/api/peers?for=$PASEO_AGENT_ID"`, "Ordering:",
		"The brief when you started:", `- ot1 [review`, "Zach's request:\ndo a thing\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("Fellow prompt lacks %q", want)
		}
	}
	c := Card{ID: "c1", Title: "x", Cwd: "/w", Repo: "r", Tier: "low"}
	if pr := ReviewPrompt(c, "plan", 7478, "unknown"); !strings.Contains(pr, `localhost:7478/api/peers?for=c1`) ||
		!strings.Contains(pr, "duplicate of a card in flight is a close verdict") {
		t.Errorf("plan review prompt lacks the peers check:\n%s", pr)
	}
	if pr := ReviewPrompt(c, "code", 7478, "unknown"); !strings.Contains(pr, "rebased on it") {
		t.Errorf("code review prompt lacks the peers check")
	}
	if _, err := f.Act("scout", Body{Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	scout := p.runsWithLabel(labelArg(labelRole, "scout"))
	if sp := scout[0][len(scout[0])-1]; !strings.Contains(sp, "localhost:7478/api/peers?repo=example/desk") ||
		strings.Contains(sp, "/api/board") {
		t.Errorf("scout prompt does not use /api/peers")
	}
}
