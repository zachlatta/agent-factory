package factory

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// hookPaseo calls onCall before each paseo call, to change the world mid-pass.
type hookPaseo struct {
	Paseo
	onCall func(args []string)
}

func (h hookPaseo) Run(ctx context.Context, args ...string) (string, error) {
	h.onCall(args)
	return h.Paseo.Run(ctx, args...)
}

// three code reviews asking for changes: each sends its card back to building
func threeReviews(t *testing.T) (*Factory, *fakePaseo) {
	t.Helper()
	var agents []Agent
	for _, n := range []string{"1", "2", "3"} {
		agents = append(agents, card("c"+n, map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1",
			labelCodeRounds: "1"}), reviewer("r"+n, "c"+n, "code"))
	}
	f, p := newTestFactory(t, agents...)
	// the files hold the agents as they are now; this fake does not write labels back, so only the
	// pass's own record keeps a reread reviewer from being applied twice
	writeAgents(t, f, agents...)
	for _, n := range []string{"1", "2", "3"} {
		storeLog(t, f, p, "r"+n, "[Assistant] - fix c"+n+"\nVERDICT: changes")
	}
	if err := f.reload(); err != nil {
		t.Fatal(err)
	}
	return f, p
}

func sendOrder(p *fakePaseo) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		switch c[0] {
		case "send":
			out = append(out, "send "+c[len(c)-2])
		case "archive":
			out = append(out, "archive "+c[1])
		}
	}
	return out
}

// A reviewer the watcher marks hot mid-pass is handled before the rest of the pass, once: the main
// loop skips it when it gets there. Reviewer archives wait until every move is sent.
func TestHotReviewerMidPassIsAppliedOnce(t *testing.T) {
	f, p := threeReviews(t)
	marked := false
	f.p = hookPaseo{p, func(args []string) {
		if args[0] == "send" && !marked {
			marked = true
			f.markHot([]string{"r3"})
		}
	}}
	f.Tick()
	for _, n := range []string{"1", "2", "3"} {
		if got := len(p.sent("c" + n)); got != 1 {
			t.Errorf("c%s got %d sends, want 1", n, got)
		}
		applied := 0
		for _, v := range p.verbs("r" + n) {
			if v == "update" {
				applied++
			}
		}
		if applied != 1 {
			t.Errorf("r%s labelled %d times, want once", n, applied)
		}
	}
	want := []string{"send c1", "send c3", "send c2", "archive r1", "archive r3", "archive r2"}
	if got := sendOrder(p); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Hot agents marked before a pass go first; a hot pass handles only them.
func TestHotPassHandlesOnlyHotAgents(t *testing.T) {
	f, p := threeReviews(t)
	f.markHot([]string{"r2"})
	f.hotPass()
	if got := sendOrder(p); !slices.Equal(got, []string{"send c2", "archive r2"}) {
		t.Errorf("hot pass = %v", got)
	}
	// as Paseo does, r2's file now says its verdict was applied: a later pass leaves it alone
	r2 := f.agent("r2")
	r2.Labels = map[string]string{}
	for k, v := range f.agent("r2").Labels {
		r2.Labels[k] = v
	}
	r2.Labels[labelPrefix+labelApplied] = "1"
	writeAgents(t, f, r2)
	f.markHot([]string{"r3"})
	f.Tick()
	if got := sendOrder(p)[2:]; !slices.Equal(got[:2], []string{"send c3", "send c1"}) {
		t.Errorf("tick after hot r3 = %v", got)
	}
	if n := len(p.sent("c2")); n != 1 {
		t.Errorf("c2 got %d sends", n)
	}
}

// An archive asked for outside a pass (a direct ApplyVerdict) still runs at once.
func TestArchiveOutsidePassRunsAtOnce(t *testing.T) {
	f, p := threeReviews(t)
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if !p.called("archive", "r1") {
		t.Error("reviewer not archived")
	}
}

// A changed file marks a factory agent hot only when what the rules read changed.
func TestNoteAgentFilesComparesWhatRulesRead(t *testing.T) {
	run := card("c1", map[string]string{labelStage: "building"})
	run.Status = "running"
	other := Agent{ID: "x1", Status: "running", CreatedAt: t0, LastActivityAt: t0, Labels: map[string]string{}}
	f, _ := newTestFactory(t)
	writeAgents(t, f, run, other)
	f.reload()
	note := func(a Agent) []string {
		t.Helper()
		writeAgents(t, f, a)
		f.noteAgentFiles([]string{agentFile(f.cfg.Home, a.ID)})
		f.mu.Lock()
		defer f.mu.Unlock()
		var ids []string
		for id := range f.hot {
			ids = append(ids, id)
		}
		clear(f.hot)
		return ids
	}
	busy := run
	busy.LastActivityAt = t0.Add(time.Minute)
	if ids := note(busy); len(ids) != 0 {
		t.Errorf("activity only marked %v hot", ids)
	}
	idle := run
	idle.Status = "idle"
	if ids := note(idle); !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("stopped agent: hot = %v", ids)
	}
	moved := run
	moved.Labels = map[string]string{labelPrefix + labelStage: "review"}
	if ids := note(moved); !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("label change: hot = %v", ids)
	}
	stopped := other
	stopped.Status = "idle"
	if ids := note(stopped); len(ids) != 0 {
		t.Errorf("an agent that is not the factory's marked hot: %v", ids)
	}
	fresh := card("c9", map[string]string{labelStage: "plan"})
	if ids := note(fresh); !slices.Equal(ids, []string{"c9"}) {
		t.Errorf("new factory agent: hot = %v", ids)
	}
}

func TestGraceDue(t *testing.T) {
	young := reviewer("r1", "c1", "code")
	young.CreatedAt = t0.Add(-40 * time.Second)
	moved := card("c1", map[string]string{labelStage: "review", labelSince: ago(10 * time.Second)})
	moved.LastActivityAt = t0.Add(-5 * time.Second)
	due, ids := graceDue([]Agent{young, moved}, t0)
	if want := t0.Add(21 * time.Second); !due.Equal(want) || !slices.Equal(ids, []string{"r1"}) {
		t.Errorf("due %v %v, want %v [r1]", due, ids, want)
	}
	young.CreatedAt = t0.Add(-5 * time.Minute) // past its grace: the rules act now, no wake needed
	due, ids = graceDue([]Agent{young, moved}, t0)
	if want := t0.Add(36 * time.Second); !due.Equal(want) || !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("due %v %v, want %v [c1]", due, ids, want)
	}
	if due, _ := graceDue([]Agent{card("c2", map[string]string{labelStage: "plan"})}, t0); !due.IsZero() {
		t.Errorf("nothing waiting, due %v", due)
	}
}

// startRun runs f's loop in the background and returns stop, which cancels the loop and waits for
// Run to return. stop also runs as a cleanup registered after the factory's TempDirs, so no pass
// writes while those are removed, even when the test fails early.
func startRun(t *testing.T, f *Factory) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

// The loop wakes when a reviewer's first minute ends, not at the next poll.
func TestRunWakesAtGraceEnd(t *testing.T) {
	f, p := threeReviews(t)
	start := time.Now()
	f.Now = func() time.Time { return t0.Add(time.Since(start)) }
	r := f.agent("r1")
	r.CreatedAt = t0.Add(-time.Minute + 300*time.Millisecond)
	writeAgents(t, f, r)
	f.p = hookPaseo{p, func([]string) {}}
	f.cfg.Poll = time.Hour
	startRun(t, f)
	waitFor(t, "r1's verdict sent", func() bool { return p.countCalls("send", "") >= 3 })
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("verdict after %v", took)
	}
}

func TestHealthReportsPaseoCalls(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", nil))
	f.paseo("permit", "ls", "--json")
	f.paseo("permit", "ls", "--json")
	f.paseo("inspect", "c1", "--json")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	c := h.PaseoCalls
	// the fake Paseo is neither the CLI nor the helper, and the factory runs no helper
	if c.Calls != 3 || c.ByVerb["permit ls"].Calls != 2 || c.ByVerb["inspect"].Calls != 1 || c.Window != "10m0s" ||
		c.CLICalls != 0 || c.HelperCalls != 0 || c.Helper != nil {
		t.Errorf("paseoCalls = %+v", c)
	}
	f.Now = func() time.Time { return t0.Add(callWindow + time.Second) }
	if c := f.calls.report(f.Now()); c.Calls != 0 {
		t.Errorf("calls older than the window kept: %+v", c)
	}
}
