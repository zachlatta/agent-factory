package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A landing is judged on the landing agent's own final reply. A provider that shows idle while the
// agent's turn ended on a command (bin/land running, its result not answered yet), or while its
// landing review is still deciding, has not finished landing.

// landingAgent is a card sent its land prompt ten minutes ago that has acted since.
func landingAgent(id string, extra map[string]string) Agent {
	l := map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute)}
	for k, v := range extra {
		l[k] = v
	}
	return card(id, l)
}

// nudged checks that a landing that never reported went to the stopped-step recovery: the card stays in
// Landing, is not marked stuck or sent to Zach, and the agent is asked once to land or say what blocks it.
func nudged(t *testing.T, name string, p *fakePaseo) {
	t.Helper()
	l := p.labels("c1")
	if l[labelStage] == "zach" || l[labelStuck] != "" || l[labelNote] != "" {
		t.Errorf("%s: labels = %v", name, l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], nudgeMsg("landing")) {
		t.Errorf("%s: sent = %v", name, msgs)
	}
}

// rebuilding checks that a failed landing went back to Building with a code review queued.
func rebuilding(t *testing.T, name string, p *fakePaseo) {
	t.Helper()
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != "code" ||
		!strings.Contains(l[labelNote], "landing not verified") {
		t.Errorf("%s: labels = %v", name, l)
	}
}

// mainCheck makes the on-main check answer err (nil: HEAD is on origin/main) and counts its calls.
func mainCheck(f *Factory, err error) *int {
	n := 0
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "merge-base" {
			n++
			return err
		}
		return nil
	}
	return &n
}

func TestLandingTurnEndingOnACommandIsNotAResult(t *testing.T) {
	c := landingAgent("c1", nil)
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	checks := mainCheck(f, errors.New("exit status 1")) // not pushed yet
	storeLog(t, f, p, "c1", "[User] Land it now.\nRunning bin/land.\n[Tool] Bash")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "" || l[labelNote] != "" {
		t.Fatalf("landing judged mid-command: %v", l)
	}
	if *checks != 0 {
		t.Errorf("on-main check ran mid-command")
	}
	// a command that never comes back goes through the stopped-step recovery, as any stalled landing does
	f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
	f.Tick()
	nudged(t, "stalled", p)
}

// After a handoff, the replaced agent's earlier report (a Ready summary with a REMAINING line) is not
// the result of the landing its replacement is still running.
func TestLandingIgnoresAPredecessorsReport(t *testing.T) {
	old := card("c0", map[string]string{labelStage: "zach", labelReplacedBy: "c1"})
	cur := landingAgent("c1", map[string]string{labelReplaces: "c0"})
	cur.Cwd = t.TempDir()
	f, p := newTestFactory(t, old, cur)
	mainCheck(f, nil)
	storeLog(t, f, p, "c0", "DECISION: land this change?\nREMAINING: none")
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "" || l[labelLandedVerified] != "" {
		t.Fatalf("landing finished on the predecessor's report: %v", l)
	}
}

// While its landing review is still deciding, an agent that wrote a progress line and went idle is
// waiting on bin/land: no landing verdict, and not stuck.
func TestLandingWaitsForItsLandingReview(t *testing.T) {
	c := landingAgent("c1", nil)
	c.Cwd = t.TempDir()
	lr := reviewer("r9", "c1", "land")
	lr.Status = "running"
	f, p := newTestFactory(t, c, lr)
	checks := mainCheck(f, errors.New("exit status 1"))
	storeLog(t, f, p, "c1", "[User] Land it now.\nWaiting on bin/land and its landing review.")
	if err := f.landReviews.put(LandReview{By: "c1", Review: "r9", Status: "running", Card: true,
		StartedAt: t0.Add(-time.Minute)}, t0); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 10 * time.Minute} {
		f.Now = func() time.Time { return t0.Add(at) }
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != "" || l[labelNote] != "" {
			t.Fatalf("at +%v: landing judged during its landing review: %v", at, l)
		}
	}
	if *checks != 0 {
		t.Errorf("on-main check ran during the landing review")
	}
}

// A final reply after a failed landing is still a failed landing: the card fixes its branch.
func TestFinalReplyAfterFailedLandingReturnsToBuild(t *testing.T) {
	c := landingAgent("c1", nil)
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	mainCheck(f, errors.New("exit status 1"))
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash\nbin/land failed: the rebase conflicts.\nREMAINING: the rebase")
	f.Tick()
	rebuilding(t, "failed", p)
}

func TestFailedLandingReviewerDoesNotHideTheFinalReport(t *testing.T) {
	for _, status := range []string{"error", "stalled"} {
		t.Run(status, func(t *testing.T) {
			c := landingAgent("c1", nil)
			c.Cwd = t.TempDir()
			r := reviewer("r9", "c1", "land")
			r.Status = status
			if status == "stalled" {
				r.Status = "running"
				r.LastActivityAt = t0.Add(-time.Hour)
			}
			f, p := newTestFactory(t, c, r)
			mainCheck(f, errors.New("not published"))
			storeLog(t, f, p, c.ID, "[User] Land it now.\nThe landing review failed.\nREMAINING: agent: retry the review")
			if err := f.landReviews.put(LandReview{By: c.ID, Review: r.ID, Status: "running", Card: true}, t0); err != nil {
				t.Fatal(err)
			}
			if err := f.tickAgent(c, t0); err != nil {
				t.Fatal(err)
			}
			rebuilding(t, "failed reviewer", p)
		})
	}
}

// While the agent's own bin/land process runs, a progress line is not the landing's result, however
// long its scan, tests and reviews take; once that process exits, the reply is judged as usual.
func TestLandingWaitsWhileItsBinLandRuns(t *testing.T) {
	cmd, marker := startLandRun(t)
	c := landingAgent("c1", map[string]string{labelLandRun: marker})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	checks := mainCheck(f, errors.New("exit status 1")) // not pushed yet
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash\nbin/land is running its tests; waiting on it.")
	// the reused approval for this HEAD is already done: no landing review is pending
	if err := f.landReviews.put(LandReview{By: "c1", Status: "done", Verdict: "approve", Card: true}, t0); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{0, 10 * time.Minute} {
		f.Now = func() time.Time { return t0.Add(at) }
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != "" || l[labelNote] != "" {
			t.Fatalf("at +%v: landing judged while bin/land runs: %v", at, l)
		}
	}
	if *checks != 0 {
		t.Errorf("on-main check ran while bin/land runs")
	}
	cmd.Process.Kill()
	cmd.Wait()
	f.Tick()
	nudged(t, "after bin/land exited with no report", p)
}

// After a bin/land run, a progress line with no REMAINING line is not a final report either: it waits
// like a turn that ended on a command, and a landing that never reports gets the stopped-step recovery.
func TestLandingProgressLineWithoutARunWaitsThenStops(t *testing.T) {
	c := landingAgent("c1", map[string]string{labelLandRun: "1.Thu_Jan_1_00_00_00_1970"}) // stale marker
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	mainCheck(f, errors.New("exit status 1"))
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash\nStarted bin/land; waiting on it.")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "" || l[labelNote] != "" {
		t.Fatalf("progress line judged as the landing result: %v", l)
	}
	f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
	f.Tick()
	nudged(t, "progress line", p)
}

// Without a bin/land run on record, a reply with no REMAINING line is the landing's final report, as
// it always was: a failure goes back to Building.
func TestFailureReplyWithoutARunReturnsToBuild(t *testing.T) {
	c := landingAgent("c1", nil)
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	mainCheck(f, errors.New("exit status 1"))
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash\nLanding failed: conflict.")
	f.Tick()
	rebuilding(t, "no run", p)
}

// A landed reply with no REMAINING line still gets the one completion-line nudge.
func TestLandedReplyWithoutRemainingIsAskedOnce(t *testing.T) {
	c := landingAgent("c1", nil)
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	mainCheck(f, nil)
	storeLog(t, f, p, "c1", "[User] Land it now.\n[Tool] Bash\nLanded.")
	f.Tick()
	if l := p.labels("c1"); l[labelRemainingAsk] != "1" || l[labelNote] != "asking what remains" {
		t.Errorf("labels = %v", l)
	}
}
