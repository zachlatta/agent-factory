package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// landSetup is a factory whose home holds dev/o/r, a clean git checkout, and a live agent "ag1" in it.
func landSetup(t *testing.T) (*Factory, *fakePaseo, string) {
	t.Helper()
	f, p := newTestFactory(t)
	f.Git, f.GitOut = runGit, runGitOut
	r := newRepos(t)
	dir := filepath.Join(f.cfg.Home, "dev", "o", "r")
	os.MkdirAll(filepath.Dir(dir), 0o755)
	gitRun(t, filepath.Dir(dir), "clone", "-q", filepath.Join(filepath.Dir(r.main), "up.git"), "r")
	commitFile(t, dir, "work/a/README.md", "the work\n")
	p.cwd["ag1"] = dir
	return f, p, dir
}

func TestLandReviewRefusesBadRequests(t *testing.T) {
	f, p, dir := landSetup(t)
	outside := t.TempDir()
	gitRun(t, outside, "init", "-q")
	link := filepath.Join(f.cfg.Home, "dev", "o", "sneaky")
	os.Symlink(outside, link)
	notGit := filepath.Join(f.cfg.Home, "dev", "o", "plain")
	os.MkdirAll(notGit, 0o755)
	p.unknown["ghost"] = true
	p.archived["old"], p.cwd["old"] = true, dir
	p.cwd["out"], p.cwd["sym"], p.cwd["plain"] = outside, link, notGit
	p.cwd["dirty"] = dir
	for _, tc := range []struct{ by, want string }{
		{"", "PASEO_AGENT_ID"},
		{"ghost", "unknown agent"},
		{"old", "archived"},
		{"out", "outside"},
		{"sym", "outside"},
		{"plain", "not a git checkout"},
	} {
		if _, err := f.Act("land-review", Body{By: tc.by}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("by %q: err = %v, want %q", tc.by, err, tc.want)
		}
	}
	os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o644)
	if _, err := f.Act("land-review", Body{By: "dirty"}); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Errorf("dirty: err = %v", err)
	}
	if p.runs != 0 {
		t.Errorf("a reviewer started: %v", p.calls)
	}
}

func TestLandReviewStartsOneReviewer(t *testing.T) {
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	res, err := f.Act("land-review", Body{By: "ag1", Why: "weekly self-improve: fix sync"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(LandReview)
	if r.Status != "running" || r.Head != head || r.Cwd != dir || r.Review != "new-agent-1" || r.Visibility != "unknown" {
		t.Errorf("review = %+v", r)
	}
	runs := p.runsWithLabel(labelArg(labelReviewOf, "ag1"))
	if len(runs) != 1 || !contains(runs[0], labelArg(labelReviewKind, "land")) || !contains(runs[0], dir) {
		t.Fatalf("runs = %v", runs)
	}
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{"LANDING REVIEW", head, "weekly self-improve", "Repo visibility: unknown", "paste the command"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	// asking again while it runs returns the same review
	res, err = f.Act("land-review", Body{By: "ag1"})
	if err != nil || res.(LandReview).Review != "new-agent-1" || p.runs != 1 {
		t.Errorf("second ask: %+v %v runs=%d", res, err, p.runs)
	}
}

func TestLandVerdictIsStoredNotSent(t *testing.T) {
	f, p, _ := landSetup(t)
	if _, err := f.Act("land-review", Body{By: "ag1"}); err != nil {
		t.Fatal(err)
	}
	ag := card("ag1", nil)
	ag.Labels["paseo.schedule-id"] = "s1"
	rev := reviewer("new-agent-1", "ag1", "land")
	f.SetAgents([]Agent{ag, rev})
	storeLog(t, f, p, "new-agent-1", "[Assistant] Tests pass (go test ./... ok).\nTIER: low\nVERDICT: nits")
	f.Tick()
	r, _ := f.landReviews.Get("ag1")
	if r.Status != "done" || r.Verdict != "nits" || !strings.Contains(r.Findings, "Tests pass") {
		t.Errorf("stored = %+v", r)
	}
	if l := p.labels("ag1"); l[labelLandReview] != "nits@"+short(r.Head) || l[labelStage] != "" {
		t.Errorf("requester labels = %v", l)
	}
	if len(p.sent("ag1")) != 0 || !p.called("archive", "new-agent-1") {
		t.Errorf("calls = %v", p.calls)
	}
	// the server answers with it
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/land-review/ag1", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"verdict": "nits"`) && !strings.Contains(rec.Body.String(), `"verdict":"nits"`) {
		t.Errorf("GET: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/land-review/nobody", nil))
	if rec.Code != 404 {
		t.Errorf("GET unknown: %d", rec.Code)
	}
}

func TestLandReviewPostNeedsHeader(t *testing.T) {
	f, p, _ := landSetup(t)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/land-review", strings.NewReader(`{"by":"ag1"}`)))
	if rec.Code != 403 || p.runs != 0 {
		t.Errorf("no header: %d runs=%d", rec.Code, p.runs)
	}
}

func TestVisibility(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/repos/zachlatta/file-share":
			fmt.Fprint(w, `{"private": false}`)
		case "/repos/zachlatta/private-notes":
			w.WriteHeader(404)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	f, _ := newTestFactory(t)
	f.GitHubAPI = srv.URL
	for remote, want := range map[string]string{
		"https://github.com/zachlatta/file-share.git":    "public",
		"git@github.com:zachlatta/file-share.git":        "public",
		"https://github.com/zachlatta/private-notes":     "private",
		"https://github.com/zachlatta/broken":            "unknown",
		"/tmp/some/bare.git":                             "unknown",
		"https://gitlab.com/zachlatta/private-notes.git": "unknown",
	} {
		f.GitOut = gitStub(map[string]string{"remote get-url origin": remote + "\n"})
		if got := f.visibility("/x"); got != want {
			t.Errorf("%s: %s, want %s", remote, got, want)
		}
	}
	before := hits.Load()
	f.GitOut = gitStub(map[string]string{"remote get-url origin": "https://github.com/zachlatta/file-share\n"})
	if f.visibility("/x") != "public" || hits.Load() != before {
		t.Error("cached answer not used")
	}
	f.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	f.visibility("/x")
	if hits.Load() != before+1 {
		t.Error("cache did not expire after an hour")
	}
}

func TestReviewPromptsCarryVisibilityAndTests(t *testing.T) {
	c := Card{ID: "c1", Title: "t", Cwd: "/w"}
	for _, kind := range []string{"code", "land"} {
		pub := ReviewPrompt(c, kind, 7478, "public")
		if !strings.Contains(pub, "Repo visibility: PUBLIC") || !strings.Contains(pub, "*.ts.net") ||
			!strings.Contains(pub, "paste the command") {
			t.Errorf("%s prompt:\n%s", kind, pub)
		}
		if !strings.Contains(ReviewPrompt(c, kind, 7478, "unknown"), "Treat it as public") {
			t.Errorf("%s: unknown visibility not treated as public", kind)
		}
	}
}

// landServer fakes the factory API for the client: POST returns a running review, GETs return
// running then done with verdict.
func landServer(t *testing.T, head, verdict string, postStatus int) *httptest.Server {
	var gets atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			if r.Header.Get("X-Factory") != "1" {
				w.WriteHeader(403)
				return
			}
			var b Body
			json.NewDecoder(r.Body).Decode(&b)
			if postStatus != 200 || b.By != "ag1" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"worktree not clean"}`)
				return
			}
			json.NewEncoder(w).Encode(LandReview{By: "ag1", Review: "rev1", Head: head, Status: "running"})
		case r.URL.Path == "/api/land-review/ag1":
			rv := LandReview{By: "ag1", Review: "rev1", Head: head, Status: "running"}
			if gets.Add(1) >= 2 {
				rv.Status, rv.Verdict, rv.Findings = "done", verdict, "- finding one"
			}
			json.NewEncoder(w).Encode(rv)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestLandClient(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, headNow string
		post, want             int
		timeout                time.Duration
	}{
		{name: "approve", verdict: "approve", headNow: "abc", post: 200, want: LandOK},
		{name: "nits", verdict: "nits", headNow: "abc", post: 200, want: LandOK},
		{name: "head moved", verdict: "approve", headNow: "def", post: 200, want: LandError},
		{name: "changes", verdict: "changes", headNow: "abc", post: 200, want: LandChanges},
		{name: "escalate", verdict: "escalate", headNow: "abc", post: 200, want: LandZach},
		{name: "close", verdict: "close", headNow: "abc", post: 200, want: LandZach},
		{name: "refused", verdict: "approve", headNow: "abc", post: 400, want: LandError},
		{name: "timeout", verdict: "approve", headNow: "abc", post: 200, want: LandError, timeout: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := landServer(t, "abc", tc.verdict, tc.post)
			defer srv.Close()
			var out bytes.Buffer
			timeout := tc.timeout
			if timeout == 0 {
				timeout = time.Minute
			}
			code := LandClient{Base: srv.URL, By: "ag1", Out: &out, Poll: time.Second, Timeout: timeout,
				Sleep: func(time.Duration) {}, Head: func() (string, error) { return tc.headNow + "\n", nil }}.Run()
			if code != tc.want {
				t.Errorf("exit %d, want %d\n%s", code, tc.want, out.String())
			}
		})
	}
	var out bytes.Buffer
	if (LandClient{Out: &out}).Run() != LandError || !strings.Contains(out.String(), "PASEO_AGENT_ID") {
		t.Error("no agent id: want an error")
	}
}

func TestStartReviewNeedsCleanTree(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "?? scratch.txt\n M x.go\n", "rev-parse HEAD": "h1\n"})
	storeLog(t, f, p, "c1", "Built the requested change.\nTIER: low")
	f.Tick()
	if p.runs != 0 {
		t.Fatalf("review started on a dirty tree: %v", p.calls)
	}
	l := p.labels("c1")
	if l[labelAutoReview] != "code" || l[labelDirtyAsks] != "1" || l[labelStage] != "building" {
		t.Errorf("labels = %v", l)
	}
	if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "scratch.txt") || !strings.Contains(s[0], "do NOT land") {
		t.Errorf("sent = %v", s)
	}

	// after two asks, Zach decides
	c.Labels[labelPrefix+labelDirtyAsks] = "2"
	f.SetAgents([]Agent{c})
	storeLog(t, f, p, "c1", "Built the requested change.\nTIER: low")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != "worktree not clean for review" {
		t.Errorf("labels = %v", l)
	}

	// clean: the review starts and records the reviewed commit
	c.Labels[labelPrefix+labelDirtyAsks] = "1"
	f, p = newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": "h1\n"})
	storeLog(t, f, p, "c1", "Built the requested change.\nTIER: low")
	f.Tick()
	if p.runs != 1 {
		t.Fatalf("no review: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelReviewed] != "h1" || l[labelStage] != "review" || l[labelDirtyAsks] != "" {
		t.Errorf("labels = %v", l)
	}
}

const diffFlags = "diff --no-ext-diff --no-textconv --no-color --full-index --src-prefix=a/ --dst-prefix=b/ -U0 "

// landedGit answers git for a card whose reviewed commit h1 landed, rebased, as n2.
var landedGit = map[string]string{
	"reflog show --format=%H%x09%gs HEAD":  "n2\trebase (finish): x\nn2\trebase (pick): w\nm\trebase (start): checkout origin/main\nh1\tcommit: w\n",
	"merge-base origin/main h1":            "m\n",
	"rev-list --reverse --no-merges m..h1": "h1\n",
	"rev-list --no-merges m..origin/main":  "n2\no1\n",
	diffFlags + "h1^ h1":                   "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -4 +4 @@\n-old\n+new\n",
	diffFlags + "o1^ o1":                   "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -2 +2,2 @@\n-ctx\n+ctx1\n+ctx2\n",
	diffFlags + "n2^ n2":                   "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -5 +5 @@\n-old\n+new\n",
}

// landedExtraGit: a commit was made after review and landed with it.
var landedExtraGit = withAnswers(landedGit, "reflog show --format=%H%x09%gs HEAD",
	"n3\trebase (finish): x\nn3\trebase (pick): more\nn2\tcommit: more\nh1\tcommit: w\n")

func TestLandingChecksTheReviewedCommit(t *testing.T) {
	landed := landedGit
	for _, tc := range []struct {
		name, stage string
		git         map[string]string
	}{
		{"as reviewed", "landed", landed},
		{"commit after review", "building", landedExtraGit},
		{"reviewed commit diff changed", "building", withAnswers(landed, diffFlags+"n2^ n2",
			"diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -5 +5 @@\n-old\n+edited\n")},
		{"reviewed commit missing", "building", withAnswers(landed, "rev-list --no-merges m..origin/main", "o1\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelReviewed: "h1"})
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = gitStub(tc.git)
			storeLog(t, f, p, "c1", "Landed.\nREMAINING: none")
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != tc.stage {
				t.Errorf("labels = %v", l)
			}
			if tc.stage == "building" && (l[labelNote] != "rebuilding: landed diff differs" || l[labelAutoReview] != "code" ||
				len(p.sent("c1")) != 1 || !strings.Contains(p.sent("c1")[0], "already on origin/main")) {
				t.Errorf("not sent back to building: %v %v", l, p.sent("c1"))
			}
		})
	}
}

func TestDirectCardOutsideItsWorkItemGoesToZach(t *testing.T) {
	for _, tc := range []struct {
		name, stage, note string
		git               map[string]string
	}{
		{"work item only", "landed", "done in one pass", directOK},
		{"changed bin/", "zach", "direct card changed files outside its work item without review: bin/land",
			withAnswers(directOK, "show --name-only --format= aaa", "work/x/README.md\nbin/land\n")},
		{"main checkout", "zach", "direct card scope not verified: not a linked worktree (its reflog is shared)",
			withAnswers(directOK, "rev-parse --path-format=absolute --git-dir --git-common-dir", "/r/.git\n/r/.git\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inboxCard("c1", map[string]string{labelStage: "plan", labelSource: "email", labelSince: ago(10 * time.Minute)})
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = gitStub(tc.git)
			storeLog(t, f, p, "c1", "Answered Zach.\nTRACK: direct")
			f.Tick()
			if l := p.labels("c1"); l[labelStage] != tc.stage || l[labelNote] != tc.note {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestLandReviewerRunsOnTheReviewerModel(t *testing.T) {
	f, p, _ := landSetup(t)
	ag := card("ag1", map[string]string{labelAgent: "codex"}) // the requester runs on another agent
	f.SetAgents([]Agent{ag})
	if _, err := f.Act("land-review", Body{By: "ag1"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelReviewKind, "land"))
	if len(runs) != 1 || !contains(runs[0], ReviewerModel) {
		t.Fatalf("land reviewer run = %v", runs)
	}
}

func TestMergedReviewPromptKeepsPeersRulingsAndVisibility(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Tier: "low", Decided: true}
	for _, kind := range []string{"code", "land"} {
		pr := ReviewPrompt(c, kind, 7478, "public")
		for _, want := range []string{"localhost:7478/api/peers?for=c1", "already ruled", "Repo visibility: PUBLIC"} {
			if !strings.Contains(pr, want) {
				t.Errorf("%s prompt lacks %q", kind, want)
			}
		}
	}
}
