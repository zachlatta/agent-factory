package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// withRepos makes ~/dev/<repo>/.git checkouts so Repos() lists them.
func withRepos(t *testing.T, f *Factory, repos ...string) {
	t.Helper()
	for _, r := range repos {
		if err := os.MkdirAll(filepath.Join(f.cfg.Home, "dev", r, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(f.cfg.Home, "dev", "example/ops", "README.md"), []byte("# ops\n\nHosts and runbooks.\n"), 0o644)
}

// picker is a stopped repo picker that answered after its last move.
func picker(id string, labels map[string]string) Agent {
	l := map[string]string{labelStage: "plan", labelSource: "board", labelRouting: "1", labelSince: ago(10 * time.Minute)}
	for k, v := range labels {
		l[k] = v
	}
	a := card(id, l)
	a.Cwd, a.Repo = "/srv/dev", ""
	return a
}

func runArgs(p *fakePaseo) [][]string {
	var out [][]string
	for _, c := range p.calls {
		if c[0] == "run" {
			out = append(out, c)
		}
	}
	return out
}

func argAfter(c []string, flag string) string {
	if i := slices.Index(c, flag); i >= 0 && i+1 < len(c) {
		return c[i+1]
	}
	return ""
}

func TestAutoNewCardStartsPicker(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk", "example/ops")
	res, err := f.Act("new", Body{Request: "Rotate the backup key on the backup host", Repo: "auto", Thread: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	runs := runArgs(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	r := runs[0]
	if slices.Contains(r, "--new-workspace") || argAfter(r, "--cwd") != filepath.Join(f.cfg.Home, "dev") {
		t.Errorf("picker should run in ~/dev without a worktree: %v", r)
	}
	if !slices.Contains(r, labelArg(labelRouting, "1")) || !slices.Contains(r, labelArg(labelStage, "plan")) {
		t.Errorf("picker labels missing: %v", r)
	}
	prompt := r[len(r)-1]
	for _, want := range []string{"Rotate the backup key on the backup host", "example/ops: ops: Hosts and runbooks.", "REPO: unsure"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	rt, ok := f.routes.get(res.(map[string]string)["id"])
	if !ok || rt.Request != "Rotate the backup key on the backup host" || rt.Thread != "t1" {
		t.Errorf("saved route = %+v, %v", rt, ok)
	}
}

func TestHandPickedRepoSkipsPicker(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "x", Repo: "example/ops"}); err != nil {
		t.Fatal(err)
	}
	r := runArgs(p)[0]
	if !slices.Contains(r, "--new-workspace") || slices.Contains(r, labelArg(labelRouting, "1")) {
		t.Errorf("hand-picked repo should start the Fellow directly: %v", r)
	}
}

func TestPickerRoutesToNamedRepo(t *testing.T) {
	a := picker("pk", nil)
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk", "example/ops")
	f.routes.put("pk", Route{Request: "Rotate the key\nsecond line", Title: "Rotate the key", Thread: "t1"})
	p.logs["pk"] = "[User] Candidates... REPO: <owner/name from the list>\nIt is a host change.\nREPO: example/ops"
	f.Tick()
	runs := runArgs(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	r := runs[0]
	if argAfter(r, "--cwd") != filepath.Join(f.cfg.Home, "dev", "example/ops") || !slices.Contains(r, "--new-workspace") {
		t.Errorf("Fellow not started in the ops worktree: %v", r)
	}
	if argAfter(r, "--title") != "Rotate the key" || !strings.Contains(r[len(r)-1], "Rotate the key\nsecond line") {
		t.Errorf("request or title not carried over: %v", r)
	}
	if !slices.Contains(r, gmailThreadLabel+"=t1") {
		t.Errorf("thread not carried over: %v", r)
	}
	if !p.archived["pk"] {
		t.Error("picker not archived")
	}
	if l := p.labels("pk"); l[labelRouted] != "new-agent-1" || l[labelNote] != "started in example/ops" {
		t.Errorf("picker labels = %v", l)
	}
	if _, ok := f.routes.get("pk"); ok {
		t.Error("route not deleted")
	}
}

func TestPickerNotStartedTwice(t *testing.T) {
	a := picker("pk", nil)
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	f.mu.Lock()
	f.routing["pk"] = true // a click is starting it right now
	f.mu.Unlock()
	p.logs["pk"] = "[User] x\nREPO: example/desk"
	f.Tick()
	if err := f.pickRepo(a, "example/desk"); err == nil {
		t.Error("second start allowed while the first is in progress")
	}
	if n := len(runArgs(p)); n != 0 {
		t.Errorf("runs = %d, want 0", n)
	}
	// after the start, the stale snapshot still shows the picker in Plan with routed set
	f.mu.Lock()
	delete(f.routing, "pk")
	f.mu.Unlock()
	f.SetAgents([]Agent{picker("pk", map[string]string{labelRouted: "card9", labelNote: "started in example/desk"})})
	f.Tick()
	if n := len(runArgs(p)); n != 0 {
		t.Errorf("routed picker started again: %d runs", n)
	}
	if !p.archived["pk"] {
		t.Error("routed picker not archived on retry")
	}
	if err := f.pickRepo(f.agent("pk"), "example/desk"); err == nil {
		t.Error("approve allowed on a picker that already started its card")
	}
}

func TestPickerAsksZachWhenUnsure(t *testing.T) {
	for name, reply := range map[string]string{
		"unsure":  "Could be either.\nWhich app?\nCANDIDATES: zachlatta/personal-data-warehouse, example/ops, nope/nope\nREPO: unsure",
		"unknown": "REPO: hackclub/nonexistent",
		"no line": "I looked around.",
	} {
		t.Run(name, func(t *testing.T) {
			f, p := newTestFactory(t, picker("pk", nil))
			withRepos(t, f, "example/desk", "example/ops", "zachlatta/personal-data-warehouse")
			f.routes.put("pk", Route{Request: "q", Title: "q"})
			p.logs["pk"] = "[User] prompt\n" + reply
			f.Tick()
			if n := len(runArgs(p)); n != 0 {
				t.Errorf("runs = %d, want 0", n)
			}
			l := p.labels("pk")
			if l[labelStage] != "zach" || l[labelAwaiting] != "repo" || l[labelNote] != "pick a repo" {
				t.Errorf("labels = %v", l)
			}
			if name == "unsure" && l[labelCandidates] != "zachlatta/personal-data-warehouse example/ops" {
				t.Errorf("candidates = %q", l[labelCandidates])
			}
			if _, ok := f.routes.get("pk"); !ok {
				t.Error("route dropped")
			}
		})
	}
}

func TestRepoAskOffersCandidates(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo",
		labelCandidates: "example/ops,example/desk"})
	f, _ := newTestFactory(t, a)
	c, _ := f.card("pk")
	if c.Ask == nil || c.Ask.Title != "Which repo should this start in?" {
		t.Fatalf("ask = %+v", c.Ask)
	}
	var got []string
	for _, x := range c.Ask.Actions {
		got = append(got, x.ID+":"+x.Repo)
	}
	want := []string{"approve:example/ops", "approve:example/desk", "pickrepo:", "reply:", "close:"}
	if !slices.Equal(got, want) {
		t.Errorf("actions = %v, want %v", got, want)
	}
	if !c.Routing || c.Repo != "auto" {
		t.Errorf("card routing=%v repo=%q", c.Routing, c.Repo)
	}
}

func TestApprovePickerStartsInRepo(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo"})
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk", "example/ops")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	for _, bad := range []string{"", "hackclub/nope"} {
		if _, err := f.Act("approve", Body{ID: "pk", Repo: bad}); err == nil {
			t.Errorf("approve with repo %q accepted", bad)
		}
	}
	if len(p.sent("pk")) != 0 {
		t.Errorf("approve treated as a plan or landing approval: %v", p.sent("pk"))
	}
	if _, err := f.Act("approve", Body{ID: "pk", Repo: "example/ops"}); err != nil {
		t.Fatal(err)
	}
	runs := runArgs(p)
	if len(runs) != 1 || argAfter(runs[0], "--cwd") != filepath.Join(f.cfg.Home, "dev", "example/ops") {
		t.Errorf("runs = %v", runs)
	}
	if !p.archived["pk"] {
		t.Error("picker not archived")
	}
}

func TestPickerRunFailureKeepsRequest(t *testing.T) {
	a := picker("pk", nil)
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	p.logs["pk"] = "[User] x\nREPO: example/desk"
	f.p = runFails{p}
	f.Tick()
	l := p.labels("pk")
	if l[labelStage] != "zach" || l[labelNote] != "routing failed" || l[labelRouted] != "" || l[labelAwaiting] != "repo" {
		t.Errorf("labels = %v", l)
	}
	if _, ok := f.routes.get("pk"); !ok {
		t.Fatal("request lost after a failed start")
	}
	f.p = p
	f.SetAgents([]Agent{picker("pk", l)})
	if _, err := f.Act("approve", Body{ID: "pk", Repo: "example/desk"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// the failed run, then the retry
	if len(p.runsWithLabel(labelArg(labelSource, "board"))) != 2 || !p.archived["pk"] {
		t.Errorf("retry did not start the card: %v", p.calls)
	}
}

// runFails fails every `paseo run`.
type runFails struct{ *fakePaseo }

func (r runFails) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "run" {
		r.calls = append(r.calls, args)
		return "", errors.New("paseo run: daemon unreachable")
	}
	return r.fakePaseo.Run(ctx, args...)
}

func TestInterruptedRoutingGoesToZach(t *testing.T) {
	f, p := newTestFactory(t, picker("pk", map[string]string{labelRouted: "starting"}))
	withRepos(t, f, "example/desk")
	f.Tick()
	if l := p.labels("pk"); l[labelStage] != "zach" || l[labelNote] != "routing interrupted" {
		t.Errorf("labels = %v", l)
	}
	if n := len(runArgs(p)); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

func TestReplyToPickerRoutesAgain(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo"})
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk", "zachlatta/personal-data-warehouse")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	if _, err := f.Act("send", Body{ID: "pk", Message: "It's the Android app"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("pk")
	if l[labelStage] != "plan" || l[labelAwaiting] != "" || l[labelNote] != "picking a repo" || l[labelAutoReview] != "" {
		t.Errorf("labels after reply = %v", l)
	}
	if s := p.sent("pk"); len(s) != 1 || !strings.Contains(s[0], "REPO: line") {
		t.Errorf("sent = %v", s)
	}
	// the picker answers; the next Tick routes it instead of starting a plan review
	b := picker("pk", l)
	b.LastActivityAt = t0
	b.Labels[labelPrefix+labelSince] = ago(5 * time.Minute)
	f.SetAgents([]Agent{b})
	p.logs["pk"] = "[User] Message from Zach\nREPO: zachlatta/personal-data-warehouse"
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "pk")); len(runs) != 0 {
		t.Errorf("plan review started for a picker: %v", runs)
	}
	runs := runArgs(p)
	if len(runs) != 1 || argAfter(runs[0], "--cwd") != filepath.Join(f.cfg.Home, "dev", "zachlatta/personal-data-warehouse") {
		t.Errorf("runs = %v", runs)
	}
}

func TestRerunStuckPickerRoutes(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	p.logs["pk"] = "[User] x\nREPO: example/desk"
	if _, err := f.Act("rerun", Body{ID: "pk"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "pk")); len(runs) != 0 {
		t.Errorf("rerun started a plan review: %v", runs)
	}
	if !p.archived["pk"] {
		t.Error("rerun did not route the picker")
	}
}

func TestDropPickerForgetsRequest(t *testing.T) {
	f, _ := newTestFactory(t, picker("pk", nil))
	f.routes.put("pk", Route{Request: "q"})
	if _, err := f.Act("close", Body{ID: "pk"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.routes.get("pk"); ok {
		t.Error("route kept after drop")
	}
}

func TestPickerSkipsRecordBase(t *testing.T) {
	a := picker("pk", nil)
	a.Status = "running"
	f, _ := newTestFactory(t, a)
	called := false
	f.GitOut = func(context.Context, string, ...string) (string, error) { called = true; return "", errors.New("x") }
	f.Tick()
	if called {
		t.Error("recordBase ran git in ~/dev for a picker")
	}
}

func TestParseRepo(t *testing.T) {
	repos := []string{"example/desk", "example/ops"}
	for in, want := range map[string]string{
		"REPO: example/ops":                "example/ops",
		"**REPO:** `example/desk`":         "example/desk",
		"REPO: Example/Desk.":              "example/desk",
		"REPO: example/desk\nREPO: unsure": "",
		"REPO: <owner/name from the list>": "",
		"no line":                          "",
	} {
		if got, _ := ParseRepo(in, repos); got != want {
			t.Errorf("ParseRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAutoScoutStartsPicker(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk", "example/ops")
	res, err := f.Act("scout", Body{Repo: "auto", Request: "Find ways to make backups faster"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelRole, "scout"))); n != 0 {
		t.Errorf("scout started before a repo was picked: %d", n)
	}
	runs := runArgs(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	r := runs[0]
	if argAfter(r, "--cwd") != filepath.Join(f.cfg.Home, "dev") || !slices.Contains(r, labelArg(labelRouting, "1")) {
		t.Errorf("picker not started in ~/dev: %v", r)
	}
	if !strings.HasPrefix(argAfter(r, "--title"), "Scout: Find ways to make backups faster") {
		t.Errorf("picker title = %q", argAfter(r, "--title"))
	}
	prompt := r[len(r)-1]
	for _, want := range []string{"Find ways to make backups faster", "Suggest ideas", "REPO: unsure"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	rt, ok := f.routes.get(res.(map[string]string)["id"])
	if !ok || !rt.Scout || rt.Request != "Find ways to make backups faster" || rt.Agent != "" || rt.Tier != "" {
		t.Errorf("saved route = %+v, %v", rt, ok)
	}
}

func TestAutoScoutNeedsGuidance(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk")
	_, err := f.Act("scout", Body{Repo: "auto", Request: "  "})
	if err == nil || !strings.Contains(err.Error(), "say what ideas you want") {
		t.Errorf("err = %v", err)
	}
	if n := len(runArgs(p)); n != 0 {
		t.Errorf("runs = %d, want 0", n)
	}
}

func TestScoutPickerStartsScoutInNamedRepo(t *testing.T) {
	f, p := newTestFactory(t, picker("pk", nil))
	withRepos(t, f, "example/desk", "example/ops")
	f.routes.put("pk", Route{Request: "Find ways to make backups faster", Title: "Scout: Find ways", Scout: true})
	p.logs["pk"] = "[User] prompt\nBackups live in ops.\nREPO: example/ops"
	f.Tick()
	runs := runArgs(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	r := runs[0]
	if !slices.Contains(r, labelArg(labelRole, "scout")) || argAfter(r, "--cwd") != filepath.Join(f.cfg.Home, "dev", "example/ops") {
		t.Errorf("scout not started in ops: %v", r)
	}
	if slices.Contains(r, "--new-workspace") || slices.Contains(r, labelArg(labelStage, "plan")) {
		t.Errorf("a card was started instead of a scout: %v", r)
	}
	if !strings.Contains(r[len(r)-1], "Guidance:\nFind ways to make backups faster") {
		t.Error("scout lost the guidance")
	}
	if !p.archived["pk"] {
		t.Error("picker not archived")
	}
	if _, ok := f.routes.get("pk"); ok {
		t.Error("route not deleted")
	}
}

func TestApproveScoutPickerStartsScout(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo"})
	f, p := newTestFactory(t, a)
	withRepos(t, f, "example/desk", "example/ops")
	f.routes.put("pk", Route{Request: "speed", Title: "Scout: speed", Scout: true})
	if _, err := f.Act("approve", Body{ID: "pk", Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelRole, "scout"))
	if len(runs) != 1 || argAfter(runs[0], "--cwd") != filepath.Join(f.cfg.Home, "dev", "example/desk") {
		t.Errorf("scout runs = %v", runs)
	}
	if !p.archived["pk"] {
		t.Error("picker not archived")
	}
}
