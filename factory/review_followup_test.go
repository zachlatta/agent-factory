package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A review generation covers only the request it was started for, and only the committed work it
// saw: a follow-up sent while it runs retires it, and uncommitted work at approval time is not
// approved.

// dirtyIs makes f.GitOut answer `status --porcelain` with out (or fail with err when err is set);
// other git reads go to the GitOut it replaces.
func dirtyIs(f *Factory, out string, err error) {
	prev := f.GitOut
	f.GitOut = func(ctx context.Context, dir string, args ...string) (string, error) {
		if strings.Join(args, " ") == "status --porcelain" {
			return out, err
		}
		return prev(ctx, dir, args...)
	}
}

// requested is a card Zach asked for: its approved work stops Ready for him before it lands.
func requested(extra map[string]string) map[string]string {
	l := reviewLabels(extra)
	l[labelOrigin] = ""
	return l
}

func noReady(t *testing.T, name string, p *fakePaseo) {
	t.Helper()
	l := p.labels("c1")
	if l[labelReady] != "" || l[labelStage] == "zach" || l[labelCodeOK] != "" {
		t.Errorf("%s: approved or Ready: %v", name, l)
	}
	for _, m := range p.sent("c1") {
		if strings.Contains(m, "Ready") {
			t.Errorf("%s: Ready prompt went out: %q", name, m)
		}
	}
}

func merged(a Agent, labels map[string]string) Agent {
	l := map[string]string{}
	for k, v := range a.Labels {
		l[k] = v
	}
	for k, v := range labels {
		l[labelPrefix+k] = v
	}
	a.Labels = l
	return a
}

// Act and Tick are not serialized: a Tick whose list was read before the follow-up still sees the old
// reviewer unapplied. Its approval of the same, clean HEAD must not pass the new request.
func TestLateVerdictFromStaleListAfterFollowUpIsRetired(t *testing.T) {
	old := reviewer("r1", "c1", gateCheck)
	f, p := newTestFactory(t, freshCard(false, requested(nil)), old)
	headIs(f, reviewedSHA, nil)
	dirtyIs(f, "", nil)
	storeLog(t, f, p, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	after := p.labels("c1")
	// the list Tick holds predates every label the follow-up wrote
	if err := f.ApplyVerdict(old); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "superseded" {
		t.Errorf("old reviewer = %v", l)
	}
	if l := p.labels("c1"); l[labelStage] != after[labelStage] || l[labelRoundsDesign] != "" {
		t.Errorf("stale verdict moved the card: %v", l)
	}
	noReady(t, "stale list", p)
	noLandPrompt(t, "stale list", p)
}

func TestDirtyWorktreeAtApprovalGoesBackToReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		public bool
	}{
		{"requested", requested(nil), false},
		{"idea", reviewLabels(nil), false},
		{"public", reviewLabels(nil), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t, freshCard(tc.public, tc.labels), reviewer("r1", "c1", gateCheck))
			ranges := fakeScan(f, nil, nil)
			headIs(f, reviewedSHA, nil)
			dirtyIs(f, " M factory/policy.go", nil)
			storeLog(t, f, p, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
			if err := f.ApplyVerdict(f.agent("r1")); err != nil {
				t.Fatal(err)
			}
			backToReview(t, tc.name, p.labels("c1"))
			noReady(t, tc.name, p)
			noLandPrompt(t, tc.name, p)
			if len(*ranges) != 0 {
				t.Errorf("scanned unreviewed work: %v", *ranges)
			}
		})
	}
}

func TestDirtyWorktreeAtZachApprovalDoesNotLand(t *testing.T) {
	f, p := newTestFactory(t, freshCard(false, requested(map[string]string{labelStage: "zach", labelReady: "1",
		labelCodeOK: reviewedSHA, labelVerdict: "code approve", labelRoundsDesign: "1"})))
	headIs(f, reviewedSHA, nil)
	dirtyIs(f, "?? new.go", nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	backToReview(t, "approve", p.labels("c1"))
	noLandPrompt(t, "approve", p)
}

func TestUnreadableWorktreeAtApprovalFailsClosed(t *testing.T) {
	f, p := newTestFactory(t, freshCard(false, requested(nil)), reviewer("r1", "c1", gateCheck))
	headIs(f, reviewedSHA, nil)
	dirtyIs(f, "", errors.New("index locked"))
	storeLog(t, f, p, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], unverifiedReviewNote) || l[labelStuckFrom] != "review" ||
		l[labelReady] != "" || l[labelCodeOK] != "" {
		t.Errorf("labels = %v", l)
	}
	noLandPrompt(t, "unreadable", p)
}

// After a restart the factory has no memory of the follow-up: the retired reviewer's labels, read
// back from its file, keep its verdict from being applied.
func TestRetiredReviewerStaysRetiredAfterRestart(t *testing.T) {
	old := reviewer("r1", "c1", gateCheck)
	f, p := newTestFactory(t, freshCard(false, requested(nil)), old)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	restarted, p2 := newTestFactory(t, merged(freshCard(false, requested(nil)), p.labels("c1")), merged(old, p.labels("r1")))
	headIs(restarted, reviewedSHA, nil)
	dirtyIs(restarted, "", nil)
	storeLog(t, restarted, p2, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	restarted.Tick()
	if l := p2.labels("r1"); l[labelVerdict] != "" {
		t.Errorf("retired verdict applied after restart: %v", l)
	}
	noReady(t, "restart", p2)
	noLandPrompt(t, "restart", p2)
}

// failUpdate fails `agent update` of one agent, like a label write Paseo rejects.
type failUpdate struct {
	*fakePaseo
	id string
}

func (p failUpdate) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 2 && args[0] == "agent" && args[1] == "update" && args[2] == p.id {
		return "", errors.New("paseo agent update: write failed")
	}
	return p.fakePaseo.Run(ctx, args...)
}

// A follow-up whose reviewer could not be marked retired is not sent, and the old verdict still
// does not pass the card: the retirement is not lost with the label write.
func TestFollowUpFailsClosedWhenTheReviewerCannotBeRetired(t *testing.T) {
	old := reviewer("r1", "c1", gateCheck)
	f, p := newTestFactory(t, freshCard(false, requested(nil)), old)
	f.p = failUpdate{p, "r1"}
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err == nil {
		t.Fatal("send succeeded with the old reviewer still live")
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("follow-up sent before its old review was retired: %v", p.sent("c1"))
	}
	f.p = p
	headIs(f, reviewedSHA, nil)
	dirtyIs(f, "", nil)
	storeLog(t, f, p, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(old); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("r1"); l[labelVerdict] != "superseded" {
		t.Errorf("old reviewer = %v", l)
	}
	noReady(t, "failed retire", p)
	noLandPrompt(t, "failed retire", p)
}

// queuedForCode is a card that stopped with a code review queued, as Tick lists it before a follow-up.
func queuedForCode() Agent {
	return card("c1", map[string]string{labelStage: "building", labelAutoReview: gateCheck, labelSource: "board",
		labelRoundsRequest: "1", labelSince: ago(10 * time.Minute)})
}

// Run side by side, a follow-up and a review start never put a reviewer on the card after the
// follow-up was sent: a reviewer started before it is retired when its verdict comes (ApplyVerdict).
func TestFollowUpAndReviewStartRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		f, p := newTestFactory(t, queuedForCode())
		f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
		c, _ := f.card("c1")
		done := make(chan error, 2)
		go func() { _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); done <- err }()
		go func() { _, err := f.StartReview(c, gateCheck); done <- err }()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		sent := false
		for _, call := range p.calls {
			switch {
			case call[0] == "send" && call[len(call)-2] == "c1":
				sent = true
			case call[0] == "run" && sent:
				t.Fatalf("round %d: reviewer started after the follow-up was sent: %v", i, p.calls)
			}
		}
	}
}

// The no-verdict retry runs inside ApplyVerdict, which holds the card's gate: it still starts its one
// automatic re-run.
func TestNoVerdictRetryStillRuns(t *testing.T) {
	f, p := newTestFactory(t, freshCard(false, requested(nil)), reviewer("r1", "c1", gateCheck))
	f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
	storeLog(t, f, p, "r1", "no verdict line here")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 1 || p.labels("c1")[labelReviewerRetries] != "1" {
		t.Errorf("no automatic re-run: %v", p.calls)
	}
}

// Zach's new request after a spent code gate gets its own bounded review: a clear "changes" verdict
// is sent back as rework, not straight to "did not settle". Plan state is untouched.

// Recovery that runs on a list read before a follow-up does not start reviewers for the old request:
// its replacement would look newer than the follow-up and pass it. The follow-up's own labels queue
// the next review.

func TestFollowUpDuringReviewRetiresTheOldReviewer(t *testing.T) {
	old := reviewer("r1", "c1", gateCheck)
	old.Status = "running"
	other := reviewer("r2", "c2", gateCheck)
	other.Status = "running"
	land := reviewer("r3", "c1", "land")
	land.Status = "running"
	f, p := newTestFactory(t, freshCard(false, requested(nil)), card("c2", reviewLabels(nil)), old, other, land)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "superseded" || !p.called("archive", "r1") {
		t.Errorf("old reviewer not retired: %v", l)
	}
	if p.labels("r2")[labelApplied] != "" || p.called("archive", "r2") {
		t.Error("another card's reviewer was retired")
	}
	if p.labels("r3")[labelApplied] != "" || p.called("archive", "r3") {
		t.Error("a landing review was retired by a message")
	}
	c := p.labels("c1")
	if c[labelStage] != "building" || c[labelAutoReview] != gateCheck {
		t.Errorf("follow-up not queued for a fresh review: %v", c)
	}

	// The old reviewer's late approval of the same HEAD arrives; the builder has committed its fix.
	storeLog(t, f, p, "r1", "TIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	storeLog(t, f, p, "c1", "Fixed the empty case.")
	late := merged(old, p.labels("r1"))
	late.Status = "idle"
	answered := merged(freshCard(false, requested(nil)), c)
	answered.LastActivityAt = t0.Add(5 * time.Minute)
	f.Now = func() time.Time { return t0.Add(10 * time.Minute) }
	f.SetAgents([]Agent{answered, late})
	f.GitOut = gitStub(cleanDiff, map[string]string{"status --porcelain": "", "rev-parse HEAD": movedSHA + "\n"})
	f.Tick()
	noReady(t, "late verdict", p)
	noLandPrompt(t, "late verdict", p)
	if len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 1 {
		t.Fatalf("no fresh code reviewer: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != gateCheck || l[labelReviewed] != movedSHA {
		t.Errorf("fresh review not on the new commit: %v", l)
	}
}

// A review start decided from a list read before a follow-up (Tick and Act run side by side) does not
// review the card before the builder has answered it, and does not touch the follow-up's queue.
func TestStaleReviewStartAfterFollowUpWaitsForTheBuilder(t *testing.T) {
	stale := queuedForCode()
	f, p := newTestFactory(t, stale)
	f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	queued := p.labels("c1")
	c, _ := f.card("c1")
	if _, err := f.StartReview(c, gateCheck); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Fatalf("reviewed before the builder answered the follow-up: %v", runs)
	}
	if l := p.labels("c1"); l[labelStage] != queued[labelStage] || l[labelAutoReview] != gateCheck || l[labelSince] != queued[labelSince] {
		t.Errorf("follow-up's queue changed: %v, was %v", l, queued)
	}

	// the builder answers: the queued review starts as usual
	storeLog(t, f, p, "c1", "Fixed the empty case.")
	answered := merged(stale, p.labels("c1"))
	answered.LastActivityAt = t0.Add(5 * time.Minute)
	f.Now = func() time.Time { return t0.Add(10 * time.Minute) }
	f.SetAgents([]Agent{answered})
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 1 {
		t.Fatalf("no review after the builder answered: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != gateCheck {
		t.Errorf("labels = %v", l)
	}
}

// A panel whose reviewers all finished before a follow-up is not closed on it later: the stuck check
// that closes a finished panel holds the card's gate and sees the round predates the follow-up.

// The last panel reviewer's verdict, from a list read before a follow-up, does not close the panel.

// Zach's new request after a spent code gate gets its own bounded review: a clear "changes" verdict
// is sent back as rework, not straight to "did not settle". Plan state is untouched.

// Automatic rework inside a generation keeps its cap: the third changes verdict after a follow-up
// goes to the arbiter, not to another rework round.

// Recovery that runs on a list read before a follow-up does not start reviewers for the old request:
// its replacement would look newer than the follow-up and pass it. The follow-up's own labels queue
// the next review.

// Recovery of a review that started after the follow-up still replaces its blocked reviewer.
func TestRecoveryAfterFollowUpStillReplacesABlockedReviewer(t *testing.T) {
	gone := reviewer("r1", "c1", gateCheck)
	gone.CreatedAt = t0.Add(2 * time.Minute)
	gone.Status = "error" // blocked without a verdict
	f, p := newTestFactory(t, freshCard(false, requested(nil)))
	f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	// the builder answered, and its new review started after the follow-up
	c := freshCard(false, requested(map[string]string{labelSince: t0.Add(2 * time.Minute).Format(time.RFC3339)}))
	c.LastActivityAt = t0.Add(time.Minute)
	f.SetAgents([]Agent{c, gone})
	if err := f.checkStuck(c, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 1 || p.labels("c1")[labelReviewerRetries] != "1" {
		t.Errorf("blocked reviewer not replaced: %v", p.labels("c1"))
	}
}

// A panel whose reviewers all finished before a follow-up is not closed on it later: the stuck check
// that closes a finished panel holds the card's gate and sees the round predates the follow-up.

// The last panel reviewer's verdict, from a list read before a follow-up, does not close the panel.

// Zach's new request after a spent code gate gets its own bounded review: a clear "changes" verdict
// is sent back as rework, not straight to "did not settle". Plan state is untouched.

// Automatic rework inside a generation keeps its cap: the third changes verdict after a follow-up
// goes to the arbiter, not to another rework round.

// Recovery that runs on a list read before a follow-up does not start reviewers for the old request:
// its replacement would look newer than the follow-up and pass it. The follow-up's own labels queue
// the next review.
