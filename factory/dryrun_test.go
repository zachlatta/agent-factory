package factory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

// gitLog records git calls; Peers runs git from several goroutines at once, so it locks.
type gitLog struct {
	mu    sync.Mutex
	calls [][]string
}

func (g *gitLog) add(args []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, args)
}

// all is a copy of the calls so far.
func (g *gitLog) all() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// newDryRunFactory is newTestFactory with DryRun on: the fake paseo sits behind the dry-run wrapper,
// and git calls reach the returned log only when the wrapper lets them through.
func newDryRunFactory(t *testing.T, agents ...Agent) (*Factory, *fakePaseo, *gitLog) {
	t.Helper()
	p := &fakePaseo{archived: map[string]bool{}, status: map[string]string{},
		busy: map[string]int{}, providers: map[string]string{}, fixed: map[string]bool{},
		cwd: map[string]string{}, unknown: map[string]bool{}}
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: t.TempDir(), DefaultRepo: "example/desk", DryRun: true}, p)
	f.Now = func() time.Time { return t0 }
	f.Sleep = func(time.Duration) {}
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }
	gitCalls := &gitLog{}
	f.Git, f.GitOut = f.dryRunGit(
		func(_ context.Context, _ string, args ...string) error { gitCalls.add(args); return nil },
		func(_ context.Context, _ string, args ...string) (string, error) {
			gitCalls.add(args)
			return "", errors.New("no git in tests")
		})
	f.SetAgents(agents)
	return f, p, gitCalls
}

// writes are the paseo subcommands that change something.
func paseoWrites(p *fakePaseo) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out [][]string
	for _, c := range p.calls {
		if !paseoRead(c) {
			out = append(out, c)
		}
	}
	return out
}

func TestDryRunPaseoPassesOnlyReads(t *testing.T) {
	f, p, _ := newDryRunFactory(t)
	for _, args := range [][]string{{"inspect", "c1", "--json"}, {"ls", "--json"},
		{"permit", "ls", "--json"}, {"provider", "ls", "--json"}} {
		if _, err := f.paseo(args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(p.calls) != 4 {
		t.Errorf("reads reached paseo %d times, want 4: %v", len(p.calls), p.calls)
	}
	long := strings.Repeat("x", 200)
	writes := [][]string{{"agent", "update", "c1", "--label", "factory.stage=done"}, {"send", "--no-wait", "c1", long},
		{"run", "--provider", "claude", "--background", "--quiet", "multi\nline"}, {"archive", "c1"}, {"stop", "c1"},
		{"provider", "diagnostic", "claude"}, {"permit", "allow", "c1"}, {"brand-new", "c1"}, {"logs", "c1", "--tail", "5"}, {}}
	for _, args := range writes {
		if _, err := f.paseo(args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(p.calls) != 4 {
		t.Errorf("writes reached paseo: %v", p.calls[4:])
	}
	log := strings.Join(f.log, "\n")
	for _, want := range []string{"dry run: would paseo agent update c1 --label factory.stage=done",
		"dry run: would paseo send --no-wait c1 <200 chars>", "dry run: would paseo archive c1", "dry run: would paseo stop c1",
		"dry run: would paseo provider diagnostic claude", "dry run: would paseo brand-new c1", "<10 chars>"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, long) || strings.Contains(log, "multi\nline") {
		t.Errorf("log shows a message body:\n%s", log)
	}
	if out, _ := f.paseo("run", "--provider", "claude"); lastLine(out) == "" {
		t.Errorf("dry-run paseo run gave no placeholder id: %q", out)
	}
}

func TestDryRunGitPassesOnlyReads(t *testing.T) {
	f, _, calls := newDryRunFactory(t)
	ctx := context.Background()
	f.GitOut(ctx, "/w", "rev-parse", "HEAD")
	f.GitOut(ctx, "/w", "remote", "get-url", "origin")
	if err := f.Git(ctx, "/w", "fetch", "-q", "origin", "main"); err != nil {
		t.Errorf("dry-run fetch: %v", err)
	}
	f.Git(ctx, "/w", "remote", "set-url", "origin", "x")
	f.GitOut(ctx, "/w", "push", "origin", "HEAD:main")
	if want := [][]string{{"rev-parse", "HEAD"}, {"remote", "get-url", "origin"}}; !slices.EqualFunc(calls.all(), want, slices.Equal) {
		t.Errorf("git calls %v, want %v", calls.all(), want)
	}
	if log := strings.Join(f.log, "\n"); !strings.Contains(log, "dry run: would git -C /w fetch -q origin main") ||
		!strings.Contains(log, "would git -C /w push") {
		t.Errorf("log:\n%s", log)
	}
}

func TestNewDryRunWrapsRealGit(t *testing.T) {
	f := New(Config{Home: t.TempDir(), StateDir: t.TempDir(), DryRun: true}, &fakePaseo{})
	missing := filepath.Join(t.TempDir(), "nope")
	if err := f.Git(context.Background(), missing, "fetch", "origin"); err != nil {
		t.Errorf("dry-run fetch ran git: %v", err)
	}
	if _, err := f.GitOut(context.Background(), missing, "rev-parse", "HEAD"); err == nil {
		t.Error("dry-run rev-parse did not run git")
	}
}

// dryPost posts a board action. An action reloads the board from agent files, which tests have
// none of, so agents are put back first.
func dryPost(t *testing.T, f *Factory, agents []Agent, action, body string) int {
	t.Helper()
	f.SetAgents(agents)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/"+action, strings.NewReader(body))
	r.Header.Set("X-Factory", "1")
	f.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Errorf("POST %s %s: %d %s", action, body, rec.Code, rec.Body)
	}
	return rec.Code
}

func TestDryRunActionsChangeNothing(t *testing.T) {
	agents := []Agent{card("c1", map[string]string{labelStage: "plan"}), card("c2", map[string]string{labelStage: "building"})}
	f, p, gitCalls := newDryRunFactory(t, agents...)
	idea, err := f.addIdea(Body{Repo: "example/desk", Title: "An idea", Summary: "s", Request: "r"})
	if err != nil {
		t.Fatal(err)
	}
	img := base64.StdEncoding.EncodeToString([]byte("png"))
	dryPost(t, f, agents, "stage", `{"id":"c1","stage":"building"}`)
	dryPost(t, f, agents, "send", `{"id":"c2","message":"hi","attachments":[{"name":"a.png","type":"image/png","dataBase64":"`+img+`"}]}`)
	dryPost(t, f, agents, "new", `{"request":"do a thing","repo":"example/desk"}`)
	dryPost(t, f, agents, "close", `{"id":"c2"}`)
	dryPost(t, f, agents, "approve", `{"id":"`+idea.ID+`"}`)
	if w := paseoWrites(p); len(w) != 0 {
		t.Errorf("dry-run actions wrote to paseo: %v", w)
	}
	for _, c := range gitCalls.all() {
		if c[0] == "fetch" {
			t.Errorf("dry-run ran git %v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Home, ".paseo")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote under Home/.paseo: %v", err)
	}
	if log := strings.Join(f.log, "\n"); !strings.Contains(log, "would paseo send --no-wait") ||
		!strings.Contains(log, "would paseo archive c2") {
		t.Errorf("log:\n%s", log)
	}
}

func TestDryRunApproveLandingFetchesNothing(t *testing.T) {
	f, p, gitCalls := newDryRunFactory(t, publicCard("c1", map[string]string{labelStage: "zach",
		labelVerdict: "code approve", labelCodeRounds: "1", labelNote: "review passed: approve to land",
		labelReviewed: reviewedSHA}))
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) { return nil, nil }
	// rev-parse and status are reads, so the dry run passes them through: HEAD is still the reviewed
	// commit, with nothing uncommitted
	f.Git, f.GitOut = f.dryRunGit(
		func(_ context.Context, _ string, args ...string) error { gitCalls.add(args); return nil },
		func(_ context.Context, _ string, args ...string) (string, error) {
			gitCalls.add(args)
			switch strings.Join(args, " ") {
			case "rev-parse HEAD":
				return reviewedSHA + "\n", nil
			case "status --porcelain":
				return "", nil
			}
			return "", errors.New("no git in tests")
		})
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range gitCalls.all() {
		if c[0] == "fetch" {
			t.Errorf("dry-run approve ran git %v", c)
		}
	}
	if w := paseoWrites(p); len(w) != 0 {
		t.Errorf("dry-run approve wrote to paseo: %v", w)
	}
}

func TestDryRunRunMakesOnlyReads(t *testing.T) {
	f, p, _ := newDryRunFactory(t, card("c1", map[string]string{labelStage: "plan-review"}))
	f.cfg.Poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	f.Run(ctx)
	if w := paseoWrites(p); len(w) != 0 {
		t.Errorf("dry-run Run wrote to paseo: %v", w)
	}
	if strings.Contains(strings.Join(f.log, "\n"), "would paseo") {
		t.Errorf("dry-run Run tried writes (Tick ran?):\n%s", strings.Join(f.log, "\n"))
	}
}

func TestDryRunStateCopiesAndLeavesOriginal(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "ideas.json"), []byte(`[{"id":"idea-1"}]`), 0o644)
	os.WriteFile(filepath.Join(src, "routes.json"), []byte(`{}`), 0o644)
	dst, err := DryRunState(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dst) })
	if dst == src {
		t.Fatal("dry-run state is the real state directory")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ideas.json")); string(b) != `[{"id":"idea-1"}]` {
		t.Errorf("copied ideas.json: %q", b)
	}
	if missing, err := DryRunState(filepath.Join(src, "none")); err != nil {
		t.Errorf("missing state dir: %v", err)
	} else {
		os.RemoveAll(missing)
	}
}

func TestDryRunStateIsRemovedWhenTheRunStops(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "ideas.json"), []byte(`[]`), 0o644)
	dst, err := DryRunState(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveDryRunState(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("copy still there: %v", err)
	}
	// only a dry run's own copy: never the real state directory or anything else
	for _, dir := range []string{src, filepath.Dir(dst), filepath.Join(src, "agent-factory-dry-run-1")} {
		os.MkdirAll(dir, 0o755)
		if RemoveDryRunState(dir) == nil {
			t.Errorf("removed %s", dir)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "ideas.json")); err != nil {
		t.Errorf("real state: %v", err)
	}
}

func TestAgentsAreToldToRemoveOnlyInsideTheirWorktree(t *testing.T) {
	for name, s := range map[string]string{"shellRule": shellRule, "uiReviewRule": uiReviewRule} {
		if strings.Contains(s, "(/tmp)") {
			t.Errorf("%s still sends scratch copies to /tmp", name)
		}
	}
	if !strings.Contains(shellRule, "rm outside your worktree") || !strings.Contains(uiReviewRule, ".factory-scratch/") ||
		!strings.Contains(uiReviewRule, "deletes its copied state when it stops") {
		t.Errorf("rules:\n%s\n%s", shellRule, uiReviewRule)
	}
}

func TestDryRunGitFakeIsSafeConcurrently(t *testing.T) {
	var agents []Agent
	for n := range 8 {
		agents = append(agents, card(fmt.Sprintf("c%d", n), map[string]string{labelStage: "building"}))
	}
	f, _, calls := newDryRunFactory(t, agents...)
	if p := f.Peers("", "example/desk"); len(p.InFlight) != 8 {
		t.Fatalf("in flight %d, want 8", len(p.InFlight))
	}
	if got := len(calls.all()); got != 16 {
		t.Errorf("Peers recorded %d git calls, want 16 (diff and status per card)", got)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.GitOut(context.Background(), "/w", "status", "--porcelain")
		}()
	}
	wg.Wait()
	if got := len(calls.all()); got != 36 {
		t.Errorf("recorded %d git calls, want 36", got)
	}
}
