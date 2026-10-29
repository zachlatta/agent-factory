package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIdeasAddDedupeDismiss(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "Make tests faster", By: "scout", Tier: "low"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if idea.Status != "waiting" || idea.Title != "Make tests faster" || idea.Tier != "low" {
		t.Errorf("idea = %+v", idea)
	}
	if _, err := f.Act("idea", Body{Request: "Make tests faster"}); err == nil {
		t.Error("duplicate waiting idea accepted")
	}
	if _, err := f.Act("idea", Body{Request: "  "}); err == nil {
		t.Error("empty idea accepted")
	}
	if _, err := f.Act("archive", Body{ID: idea.ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.ideas.List(); got[0].Status != "dismissed" {
		t.Errorf("after dismiss: %+v", got)
	}
	_, err = f.Act("idea", Body{Request: "make TESTS faster"})
	if err == nil || !strings.Contains(err.Error(), idea.ID) || !strings.Contains(err.Error(), "dismissed") {
		t.Errorf("re-adding a dismissed idea: %v", err)
	}
}

func TestIdeasStartedTitleBlockedFor14Days(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "Cache the board"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if _, err := f.Act("approve", Body{ID: idea.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = f.Act("idea", Body{Request: "cache the board"})
	if err == nil || !strings.Contains(err.Error(), idea.ID) || !strings.Contains(err.Error(), "started") {
		t.Errorf("re-adding a started idea: %v", err)
	}
	f.Now = func() time.Time { return t0.Add(15 * 24 * time.Hour) }
	if _, err := f.Act("idea", Body{Request: "Cache the board"}); err != nil {
		t.Errorf("after 14 days: %v", err)
	}
}

func TestTickStartsIdeasWithinCaps(t *testing.T) {
	f, p := newTestFactory(t)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":2,"daily":3}}}`)
	for _, r := range []string{"one", "two", "three", "four"} {
		if _, err := f.Act("idea", Body{Request: r}); err != nil {
			t.Fatal(err)
		}
	}
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("one idea per tick, got %d", n)
	}
	// the started idea is now an agent in flight
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "plan", labelOrigin: "idea"})})
	f.Tick()
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "plan", labelOrigin: "idea"}),
		card("new-agent-2", map[string]string{labelStage: "building", labelOrigin: "idea"})})
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 2 {
		t.Fatalf("WIP cap 2: started %d", n)
	}
	// both finish; the daily cap (3) still allows one more, then stops
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "landed", labelOrigin: "idea"}),
		card("new-agent-2", map[string]string{labelStage: "landed", labelOrigin: "idea"})})
	f.Tick()
	f.SetAgents(nil)
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 3 {
		t.Fatalf("daily cap 3: started %d", n)
	}
	// Zach can start one by hand past the caps
	var waiting string
	for _, i := range f.ideas.List() {
		if i.Status == "waiting" {
			waiting = i.ID
		}
	}
	if _, err := f.Act("approve", Body{ID: waiting}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 4 {
		t.Fatalf("manual start: %d", n)
	}
}

func TestDailyCapResetsNextDay(t *testing.T) {
	f, p := newTestFactory(t)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":5,"daily":1}}}`)
	f.Act("idea", Body{Request: "a"})
	f.Act("idea", Body{Request: "b"})
	f.Tick()
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("started %d, want 1", n)
	}
	f.Now = func() time.Time { return t0.Add(24 * time.Hour) }
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 2 {
		t.Fatalf("next day started %d, want 2", n)
	}
}

func TestScoutGuidance(t *testing.T) {
	for _, guidance := range []string{"", "Find ways to speed up tests.\nPrioritize flaky cases and report 50% gains."} {
		t.Run(guidance, func(t *testing.T) {
			f, p := newTestFactory(t)
			raw, err := json.Marshal(map[string]string{
				"repo": "example/desk", "request": guidance, "focus": "legacy/path",
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/scout", strings.NewReader(string(raw)))
			req.Header.Set("X-Factory", "1")
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("scout: %d %s", rec.Code, rec.Body)
			}
			runs := p.runsWithLabel(labelArg(labelRole, "scout"))
			if len(runs) != 1 {
				t.Fatalf("scout runs = %v", runs)
			}
			prompt := runs[0][len(runs[0])-1]
			if guidance != "" && !strings.Contains(prompt, guidance) {
				t.Error("scout prompt lost guidance")
			}
			if guidance == "" && strings.Contains(prompt, "Guidance:") {
				t.Error("empty guidance added a prompt section")
			}
			if strings.Contains(strings.Join(runs[0], " "), "legacy/path") || strings.Contains(prompt, "Only look at") {
				t.Error("legacy focus restricted the scout")
			}
			if strings.Contains(prompt, "%!") || !strings.Contains(prompt, "localhost:7478/api/idea") ||
				!strings.Contains(prompt, "fetch -q origin") || !strings.Contains(prompt, "localhost:7478/api/ideas") {
				t.Errorf("bad scout prompt:\n%s", prompt)
			}
		})
	}
}

func TestIdeaKeepsSummaryAndDoneWhen(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "In foo.go change bar", Summary: "Board loads twice as fast",
		DoneWhen: "page loads in under 1 s", Why: "it is slow"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if idea.Summary != "Board loads twice as fast" || idea.DoneWhen != "page loads in under 1 s" {
		t.Errorf("idea = %+v", idea)
	}
}

func TestIdeaTitleCutAtWord(t *testing.T) {
	f, _ := newTestFactory(t)
	long := "skill_search: merge partial and body hits from a full candidate pool, then apply the limit"
	i, err := f.Act("idea", Body{Request: "x", Title: long})
	if err != nil {
		t.Fatal(err)
	}
	got := i.(Idea).Title
	if got != "skill_search: merge partial and body hits from a full candidate…" {
		t.Errorf("title = %q", got)
	}
	if got := truncWords("short title", 70); got != "short title" {
		t.Errorf("short = %q", got)
	}
	if got := truncWords("abcdefghij", 5); got != "abcd…" {
		t.Errorf("one long word = %q", got)
	}
}

func TestScoutAsksForSummaryAndDoneWhen(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("scout", Body{Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelRole, "scout"))
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{`"summary":`, `"doneWhen":`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("scout prompt lacks %s", want)
		}
	}
}

func TestStartIdeaWithZachsChanges(t *testing.T) {
	f, p := newTestFactory(t)
	i, _ := f.addIdea(Body{Request: "Cache the board in memory", Title: "cache", Why: "slow"})
	if _, err := f.Act("approve", Body{ID: i.ID, Message: "  Use a 5s TTL, not forever.  "}); err != nil {
		t.Fatal(err)
	}
	r := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(r) != 1 {
		t.Fatalf("runs = %v", r)
	}
	prompt := r[0][len(r[0])-1]
	brief := strings.Index(prompt, "Cache the board in memory")
	note := strings.Index(prompt, "Zach's changes before starting (his own words; where they differ from the brief above, his win):\nUse a 5s TTL, not forever.")
	if brief < 0 || note < brief {
		t.Errorf("prompt lacks the marked note after the brief:\n%s", prompt)
	}
	got := f.ideas.List()[0]
	if got.Note != "Use a 5s TTL, not forever." {
		t.Errorf("idea note = %q", got.Note)
	}
	f.SetAgents([]Agent{card(got.Card, map[string]string{labelStage: "plan", labelOrigin: "idea"})})
	if c, _ := f.card(got.Card); c.Idea == nil || c.Idea.Note != got.Note {
		t.Errorf("started card idea = %+v", c.Idea)
	}

	// no text, or only whitespace: today's request, no note
	for n, msg := range []string{"", " \n\t "} {
		j, _ := f.addIdea(Body{Request: "Plain idea " + string(rune('a'+n)), Title: "plain " + string(rune('a'+n))})
		if _, err := f.Act("approve", Body{ID: j.ID, Message: msg}); err != nil {
			t.Fatal(err)
		}
		r = p.runsWithLabel(labelArg(labelOrigin, "idea"))
		if prompt := r[len(r)-1][len(r[len(r)-1])-1]; strings.Contains(prompt, "Zach's changes before starting") {
			t.Errorf("message %q added a note:\n%s", msg, prompt)
		}
		for _, k := range f.ideas.List() {
			if k.ID == j.ID && k.Note != "" {
				t.Errorf("message %q stored note %q", msg, k.Note)
			}
		}
	}
}

func writeGitOrigin(t *testing.T, dir, url string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = " + url + "\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoDirResolvesOriginRemote(t *testing.T) {
	f, p := newTestFactory(t)
	// Default checkout path exists (even as a symlink): returned unchanged without resolving symlinks.
	realDir := filepath.Join(t.TempDir(), "real-checkout")
	writeGitOrigin(t, realDir, "https://github.com/example/desk.git")
	defLink := filepath.Join(f.cfg.Home, "dev", "example/desk")
	if err := os.MkdirAll(filepath.Dir(defLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, defLink); err != nil {
		t.Fatal(err)
	}
	if got := f.repoDir("example/desk"); got != defLink {
		t.Fatalf("default path with .git = %q, want %q unchanged", got, defLink)
	}

	// Checkout under a different folder name whose origin remote matches owner/repo.
	widgetsDir := filepath.Join(f.cfg.Home, "dev", "acme/old-folder")
	writeGitOrigin(t, widgetsDir, "https://github.com/acme/widgets.git")
	otherCopy := filepath.Join(f.cfg.Home, "dev", "other/old-copy")
	writeGitOrigin(t, otherCopy, "git@github.com:acme/widgets.git")
	if got := f.repoDir("acme/widgets"); got != widgetsDir {
		t.Fatalf("repoDir(acme/widgets) = %q, want %q", got, widgetsDir)
	}

	// Checkout under a different owner folder when no same-owner folder matches.
	toolsDir := filepath.Join(f.cfg.Home, "dev", "acme/tools")
	writeGitOrigin(t, toolsDir, "https://github.com/example/tools.git")
	if got := f.repoDir("example/tools"); got != toolsDir {
		t.Fatalf("repoDir(example/tools) = %q, want %q", got, toolsDir)
	}

	// Non-origin remotes must not match (a fork's upstream remote is not a checkout of upstream).
	forkCfg := filepath.Join(toolsDir, ".git", "config")
	if err := os.WriteFile(forkCfg, []byte("[remote \"origin\"]\n\turl = https://github.com/example/tools.git\n[remote \"upstream\"]\n\turl = https://github.com/acme/upstream-only.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantFallback := filepath.Join(f.cfg.Home, "dev", "acme/upstream-only")
	if got := f.repoDir("acme/upstream-only"); got != wantFallback {
		t.Fatalf("non-origin remote matched: got %q, want fallback %q", got, wantFallback)
	}

	// Starting an idea and a scout in the remote-named repo uses the resolved checkout path.
	idea, err := f.addIdea(Body{Request: "Add widget index", Repo: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("approve", Body{ID: idea.ID}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(runs) != 1 || argAfter(runs[0], "--cwd") != widgetsDir {
		t.Fatalf("idea run cwd = %q, want %q", argAfter(runs[0], "--cwd"), widgetsDir)
	}
	if _, err := f.Act("scout", Body{Repo: "acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	scouts := p.runsWithLabel(labelArg(labelRole, "scout"))
	if len(scouts) != 1 || argAfter(scouts[0], "--cwd") != widgetsDir {
		t.Fatalf("scout run cwd = %q, want %q", argAfter(scouts[0], "--cwd"), widgetsDir)
	}

	// Repo picker also accepts a known origin remote name.
	pk := picker("pk-remote", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo"})
	f.SetAgents([]Agent{pk})
	f.routes.put("pk-remote", Route{Request: "Fix widget cache", Title: "Fix widget cache"})
	if _, err := f.Act("approve", Body{ID: "pk-remote", Repo: "acme/widgets"}); err != nil {
		t.Fatalf("pickRepo with origin remote name: %v", err)
	}
}

type failCwdPaseo struct {
	*fakePaseo
	badCwd string
	err    error
	runs   int
}

func (fp *failCwdPaseo) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "run" && quietRunLabels(args)[labelPrefix+labelRole] != quietAnchorRole {
		fp.runs++
		if argAfter(args, "--cwd") == fp.badCwd {
			return "", fp.err
		}
	}
	return fp.fakePaseo.Run(ctx, args...)
}

func TestTickIdeasRecordsStartErrorAndMovesOn(t *testing.T) {
	f, baseP := newTestFactory(t)
	badCwd := filepath.Join(f.cfg.Home, "dev", "acme/missing")
	longErr := "paseo run --provider: Error: Create worktree requires a git repository " + strings.Repeat("x", 350)
	fp := &failCwdPaseo{fakePaseo: baseP, badCwd: badCwd, err: errors.New(longErr)}
	f.p = fp

	bad := addIdeaAt(t, f, t0.Add(-2*time.Hour), Body{Request: "fix missing repo", Title: "Broken idea", Repo: "acme/missing"})
	good := addIdeaAt(t, f, t0.Add(-time.Hour), Body{Request: "fix desk", Title: "Healthy idea", Repo: "example/desk"})

	// Tick 1: tries bad first, records truncated error + ErrorAt, and stops without trying good in the same tick.
	f.Tick()
	if fp.runs != 1 {
		t.Fatalf("tick 1 run attempts = %d, want 1 (at most one start attempt per tick)", fp.runs)
	}
	ideas := f.ideas.List()
	var gotBad, gotGood Idea
	for _, i := range ideas {
		switch i.ID {
		case bad.ID:
			gotBad = i
		case good.ID:
			gotGood = i
		}
	}
	if gotBad.Status != "waiting" || gotBad.Error == "" || len([]rune(gotBad.Error)) > 300 || !gotBad.ErrorAt.Equal(t0) {
		t.Fatalf("bad idea after tick 1 = %+v", gotBad)
	}
	if gotGood.Status != "waiting" {
		t.Fatalf("good idea should still be waiting after tick 1, got %+v", gotGood)
	}

	// Tick 2 (15s later): skips bad during its 1-hour backoff and starts good.
	now := t0.Add(15 * time.Second)
	f.Now = func() time.Time { return now }
	f.Tick()
	if n := len(baseP.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("tick 2 started %d ideas, want 1", n)
	}
	for _, i := range f.ideas.List() {
		if i.ID == good.ID && i.Status != "started" {
			t.Fatalf("good idea did not start on tick 2: %+v", i)
		}
	}

	// Tick 3 (30s later): bad is still within 1-hour backoff, so no new run is attempted.
	now = t0.Add(30 * time.Second)
	runsBefore := fp.runs
	f.Tick()
	if fp.runs != runsBefore {
		t.Fatalf("tick 3 retried failed idea before 1h backoff: runs = %d, want %d", fp.runs, runsBefore)
	}

	// After 1 hour, once the checkout exists, tickIdeas retries bad and clears Error and ErrorAt.
	fp.badCwd = ""
	now = t0.Add(61 * time.Minute)
	f.Tick()
	for _, i := range f.ideas.List() {
		if i.ID == bad.ID {
			if i.Status != "started" || i.Error != "" || !i.ErrorAt.IsZero() {
				t.Fatalf("bad idea after 1h retry = %+v", i)
			}
		}
	}
}

func TestFailedIdeaSurfacesOnBoardAndClearsOnManualRetry(t *testing.T) {
	f, p := newTestFactory(t)
	first := addIdeaAt(t, f, t0.Add(-2*time.Hour), Body{Request: "broken start", Title: "Broken start", Summary: "Fix sync"})
	second := addIdeaAt(t, f, t0.Add(-time.Hour), Body{Request: "healthy start", Title: "Healthy start", Summary: "Speed up query"})
	f.Refresh()

	p.runErr = errors.New("paseo run --provider: Error: Create worktree requires a git repository")
	req := httptest.NewRequest("POST", "/api/approve", strings.NewReader(`{"id":"`+first.ID+`"}`))
	req.Header.Set("X-Factory", "1")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("approve failed idea: code = %d, want 400", rec.Code)
	}

	// handleAction reloaded the board so GET /api/board immediately carries the error, queue, flag and Stuck entry.
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var board struct {
		Cards    []Card   `json:"cards"`
		Sections Sections `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	var c1, c2 Card
	for _, c := range board.Cards {
		switch c.ID {
		case first.ID:
			c1 = c
		case second.ID:
			c2 = c
		}
	}
	if c1.Idea == nil || c1.Idea.Error == "" || c1.Idea.ErrorAt == "" || c1.Idea.Queue != 0 || !slices.Contains(c1.Flags, "start failed") {
		t.Fatalf("failed idea card = %+v, idea = %+v", c1, c1.Idea)
	}
	if c2.Idea == nil || c2.Idea.Queue != 1 {
		t.Fatalf("second idea queue = %+v, want 1", c2.Idea)
	}
	if len(board.Sections.Stuck) != 1 || board.Sections.Stuck[0].ID != first.ID ||
		!strings.HasPrefix(board.Sections.Stuck[0].State, "could not start: ") {
		t.Fatalf("sections.stuck = %+v", board.Sections.Stuck)
	}

	// Verify board.html renders the error on the card and in the idea panel.
	page := boardPage(t)
	node, err := exec.LookPath("node")
	if err == nil {
		cardFn := regexp.MustCompile(`(?s)function cardHtml\(c\)\{.*?\n\}`).FindString(page)
		ideaStart := strings.Index(page, "// ideaHtml:")
		ideaEnd := strings.Index(page, "async function act(")
		if cardFn == "" || ideaStart < 0 || ideaEnd < ideaStart {
			t.Fatal("missing cardHtml or ideaHtml in board.html")
		}
		c1JSON, _ := json.Marshal(c1)
		script := `
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const ago = () => '2 min ago', clock = () => '16:00', md = s => s, repoTag = () => 'desk', agentLabel = s => s;
const latestReviews = () => [], cardKids = () => [], kidSummary = () => '', askKind = () => '', askQuestion = () => '', askTodo = () => '', evidenceHtml = () => '', questionsHtml = () => '', choicesHtml = () => '';
const $ = () => ({checked:false});
const B = {ideaCaps:{}, tiers:{}};
let sel = null;
` + cardFn + "\n" + page[ideaStart:ideaEnd] + `
const c = ` + string(c1JSON) + `;
console.log(JSON.stringify({card: cardHtml(c), panel: ideaHtml(c, true)}));
`
		out, err := exec.Command(node, "-e", script).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var rendered struct{ Card, Panel string }
		if err := json.Unmarshal(out, &rendered); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rendered.Card, `<div class="err">`) || !strings.Contains(rendered.Card, "Create worktree requires a git repository") {
			t.Errorf("cardHtml missing error:\n%s", rendered.Card)
		}
		if !strings.Contains(rendered.Panel, "Could not start:") || !strings.Contains(rendered.Panel, "moved on to other waiting ideas") {
			t.Errorf("ideaHtml missing error explanation:\n%s", rendered.Panel)
		}
	}

	// Manual retry once fixed clears the error and starts the card.
	p.runErr = nil
	if _, err := f.Act("approve", Body{ID: first.ID}); err != nil {
		t.Fatalf("retry approve: %v", err)
	}
	for _, i := range f.ideas.List() {
		if i.ID == first.ID && (i.Status != "started" || i.Error != "" || !i.ErrorAt.IsZero()) {
			t.Fatalf("idea after manual retry = %+v", i)
		}
	}
}
