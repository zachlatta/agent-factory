package factory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseVerdictAndTier(t *testing.T) {
	text := "[Assistant] looks fine\nTIER: **risky**\nVERDICT: approve\n"
	if got := ParseVerdict(text); got != "approve" {
		t.Errorf("verdict = %q", got)
	}
	if got := ParseTier(text); got != "risky" {
		t.Errorf("tier = %q", got)
	}
	if got := ParseTrack("[User] prompt\nTRACK: direct\nAnything else\nPlan...\nTIER: low"); got != "" {
		t.Errorf("track mid-text = %q, want empty", got)
	}
	if got := ParseTrack("Done and landed.\nTRACK: **direct**\n"); got != "direct" {
		t.Errorf("track = %q", got)
	}
	if got := ParseVerdict("no verdict line"); got != "" {
		t.Errorf("missing verdict = %q, want empty", got)
	}
	// the last line wins when a reviewer quotes the format earlier
	if got := ParseVerdict("VERDICT: <approve|changes>\nVERDICT: changes"); got != "changes" {
		t.Errorf("last verdict = %q", got)
	}
}

func TestMaxTier(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"low", "risky"}, "risky"},
		{[]string{"architecture", "low"}, "architecture"},
		{[]string{"", "bogus"}, ""},
		{[]string{"", "low"}, "low"},
	}
	for _, c := range cases {
		if got := MaxTier(c.in...); got != c.want {
			t.Errorf("MaxTier(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPlanApprovedLowTierStartsBuild(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "[Assistant] plan is sound\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[Assistant] Plan ... TIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelTier] != "low" || l[labelPlanRounds] != "1" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Plan approved") {
		t.Errorf("sent = %v", msgs)
	}
	if p.labels("r1")[labelApplied] != "1" || !p.called("archive", "r1") {
		t.Errorf("reviewer not marked applied and archived: %v", p.calls)
	}
}

func TestBuilderTierCanRaiseReviewerTier(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "plan: replace the storage layer\nTIER: architecture")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelAwaiting] != "plan" || l[labelTier] != "architecture" {
		t.Errorf("architecture plan should wait for Zach: %v", l)
	}
	for _, m := range p.sent("c1") {
		if strings.Contains(m, "Build it now") {
			t.Errorf("builder must not be told to build before Zach approves: %v", m)
		}
	}
}

func TestZachApprovesArchitecturePlan(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelTier: "architecture"}))
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelZachPlan] != "approved" || l[labelAutoReview] != "code" || l[labelAwaiting] != "" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach allowed this build") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestCodeApprovedAutoLandsByTier(t *testing.T) {
	// work the factory started lands after the gates when nothing is on the always-ask list
	for _, tier := range []string{"low", "risky"} {
		f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: tier, labelPlanRounds: "1"}),
			reviewer("r1", "c1", "code"))
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: "+tier+"\nVERDICT: nits")
		f.ApplyVerdict(f.agent("r1"))
		l := p.labels("c1")
		if l[labelStage] != "landing" {
			t.Errorf("%s: stage = %q, want landing", tier, l[labelStage])
		}
		if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
			t.Errorf("%s: sent = %v", tier, msgs)
		}
	}
}

func TestAllowedBuildStillComesToZachFinished(t *testing.T) {
	// allowing a build's side effects is not approving the finished work he asked for
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelZachPlan: "approved", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "zach" || !isReadyNote(l[labelNote]) || l[labelAskWhy] != "you asked for it" {
		t.Errorf("labels = %v", l)
	}
}

func TestCodeReviewFindsArchitectureOnLowCard(t *testing.T) {
	// raised to architecture at code review: the same preview stop before landing
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "it swapped the database\nTIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelTier] != "architecture" || l[labelNote] != readyNote(1) {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("must ask for the preview, not land: %v", msgs)
	}
}

func TestUntieredCardGoesToZachAfterReview(t *testing.T) {
	// a session sent to review by hand: no plan review, no tier; even one the factory started stops
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review"}), reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || !isReadyNote(l[labelNote]) || l[labelAskWhy] != "it had no plan review" || l[labelTier] != "low" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("must not land: %v", msgs)
	}
}

func TestHandReviewedCardWaitsForZachAfterRework(t *testing.T) {
	// a hand-sent card whose first code review asked for changes now has a tier label, but no plan review
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: "low", labelCodeRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "zach" || !isReadyNote(l[labelNote]) {
		t.Errorf("labels = %v", l)
	}
	for _, m := range p.sent("c1") {
		if strings.Contains(m, "Land it now") {
			t.Errorf("must not land: %v", m)
		}
	}
}

func TestChangesGoBackUntilReworkCap(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "[Assistant] - test is missing\nVERDICT: changes")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelCodeRounds] != "2" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "test is missing") {
		t.Errorf("findings not sent: %v", msgs)
	}

	f, p = newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "2"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "VERDICT: changes")
	f.ApplyVerdict(f.agent("r1"))
	// at the cap an arbiter agent rules, not Zach
	if l := p.labels("c1"); l[labelStage] != "review" || l[labelNote] != arbiterNote || l[labelArbitrated] != "code" {
		t.Errorf("at cap: labels = %v", l)
	}
	if len(p.runsWithLabel(labelArg(labelArbiter, "1"))) != 1 {
		t.Errorf("no arbiter started: %v", p.calls)
	}
}

func TestMissingVerdictRerunsOnceBeforeAskingZach(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review", labelTier: "low"})
	r1 := reviewer("r1", "c1", "code")
	f, p := newTestFactory(t, c, r1)
	storeLog(t, f, p, "r1", "I ran out of time")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "review" || l[labelNoVerdict] != "1" || l[labelCodeRounds] != "" {
		t.Fatalf("first no-verdict should re-run in review without adding a round: labels = %v", l)
	}
	if p.labels("r1")[labelApplied] != "1" || !p.called("archive", "r1") {
		t.Errorf("first reviewer not marked applied and archived: %v", p.calls)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("first no-verdict started %d reviews, want 1", len(runs))
	}
	if tm, ok := f.timings.get("c1"); ok && len(tm.Rounds) != 0 {
		t.Errorf("first no-verdict recorded timing rounds: %+v", tm.Rounds)
	}

	// Second no-verdict in the same round moves the card to Needs Zach and clears no-verdict.
	cAfterFirst := withLabels(c, l)
	r2 := reviewer("r2", "c1", "code")
	f.SetAgents([]Agent{cAfterFirst, r2})
	storeLog(t, f, p, "r2", "killed again")
	if err := f.ApplyVerdict(f.agent("r2")); err != nil {
		t.Fatal(err)
	}
	l = p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != noVerdictNote || l[labelStuckFrom] != "review" ||
		l[labelVerdict] != "code none" || l[labelCodeRounds] != "1" || l[labelNoVerdict] != "" {
		t.Errorf("second no-verdict labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("no brief without a question: %v", msgs)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Errorf("second no-verdict started another review: %d runs", len(runs))
	}
	if tm, _ := f.timings.get("c1"); len(tm.Rounds) != 1 || tm.Rounds[0].Verdict != "" || tm.Rounds[0].N != 1 {
		t.Errorf("second no-verdict timing rounds = %+v", tm.Rounds)
	}

	// Manual Re-run from Needs Zach starts a fresh review.
	f.SetAgents([]Agent{withLabels(cAfterFirst, l)})
	if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 2 {
		t.Errorf("rerun started %d total reviews, want 2", len(runs))
	}
}

func TestCardBeforeArchivedClosedReviewerStartsOnlyOneReviewer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("r1", "c1", "plan")
	r.Status = "closed"
	r.ArchivedAt = t0.Add(-time.Minute)
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "Review interrupted before a verdict.")
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("started %d reviewers in one Tick, want 1: %v", len(runs), p.calls)
	}
	l := p.labels("c1")
	if l[labelStage] != "plan-review" || l[labelNoVerdict] != "1" {
		t.Errorf("labels = %v", l)
	}
}

func TestCheckStuckRerunsMissingReviewerOnce(t *testing.T) {
	since := ago(10 * time.Minute)
	for _, tc := range []struct {
		name  string
		stage string
		extra []Agent
	}{
		{"archived while running", "plan-review", []Agent{func() Agent {
			r := reviewer("r1", "c1", "plan")
			r.Status = "running"
			r.ArchivedAt = t0.Add(-time.Minute)
			return r
		}()}},
		{"gone from agent list", "review", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: tc.stage, labelSince: since})
			c.LastActivityAt = t0.Add(-time.Hour)
			f, p := newTestFactory(t, append([]Agent{c}, tc.extra...)...)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != tc.stage || l[labelNoVerdict] != "1" {
				t.Fatalf("first stuck check should re-run in %s: labels = %v", tc.stage, l)
			}
			if len(tc.extra) > 0 && p.labels("r1")[labelApplied] != "1" {
				t.Errorf("archived running reviewer not marked applied: %v", p.labels("r1"))
			}
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
				t.Fatalf("runs = %d, want 1", len(runs))
			}

			// Second missing reviewer in the same round goes to Needs Zach.
			c2 := withLabels(c, l)
			c2.Labels[labelPrefix+labelSince] = since
			f.SetAgents([]Agent{c2})
			f.Tick()
			l = p.labels("c1")
			if l[labelStage] != "zach" || l[labelNote] != "stuck" || l[labelStuckFrom] != tc.stage || l[labelNoVerdict] != "" {
				t.Errorf("second stuck check labels = %v", l)
			}
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
				t.Errorf("second stuck check started another review: %d runs", len(runs))
			}
		})
	}
}

func TestFailedAutoRerunGoesToZachOnNextTick(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("r1", "c1", "plan")
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "Review interrupted before a verdict.")
	p.runErr = errors.New("paseo run failed")
	if err := f.ApplyVerdict(f.agent("r1")); err == nil {
		t.Fatal("expected StartReview error when paseo run fails")
	}
	l := p.labels("c1")
	if l[labelNoVerdict] != "1" {
		t.Fatalf("no-verdict not recorded before StartReview: %v", l)
	}
	p.runErr = nil
	rApplied := withLabels(r, p.labels("r1"))
	f.SetAgents([]Agent{withLabels(c, l), rApplied})
	f.Tick()
	l = p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != "stuck" || l[labelStuckFrom] != "plan-review" || l[labelNoVerdict] != "" {
		t.Errorf("after failed auto-rerun, next Tick should send card to Zach: labels = %v", l)
	}
	// Only the single failed run call from ApplyVerdict was attempted; checkStuck did not retry again.
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Errorf("runs = %d, want 1", len(runs))
	}
}

func TestVerdictAndDirtyCommitClearNoVerdictFlag(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelNoVerdict: "1"})
	r := reviewer("r1", "c1", "plan")
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: changes\n- fix x")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if v, ok := l[labelNoVerdict]; !ok || v != "" || l[labelStage] != "plan" {
		t.Errorf("valid verdict did not clear no-verdict: %v", l)
	}

	// Same-pass dirty worktree: ApplyVerdict sets no-verdict=1, then StartReview sees a dirty tree and calls
	// askToCommit in the same pass (while the cached agent snapshot still has no-verdict unset). askToCommit
	// must still clear no-verdict so the next code review round gets its own retry allowance.
	cDirty := card("c2", map[string]string{labelStage: "review", labelSource: "board"})
	cDirty.Cwd = "/w/c2"
	rDirty := reviewer("r2", "c2", "code")
	f2, p2 := newTestFactory(t, cDirty, rDirty)
	storeLog(t, f2, p2, "r2", "Review interrupted before a verdict.")
	f2.GitOut = gitStub(map[string]string{
		"status --porcelain": " M a.go\n",
		"rev-parse HEAD":     "abc123\n",
	})
	if err := f2.ApplyVerdict(f2.agent("r2")); err != nil {
		t.Fatal(err)
	}
	l2 := p2.labels("c2")
	if v, ok := l2[labelNoVerdict]; !ok || v != "" || l2[labelStage] != "building" {
		t.Errorf("same-pass askToCommit did not clear no-verdict: %v", l2)
	}
}

func TestApproveLandsOtherwise(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach"}))
	f.Act("approve", Body{ID: "c1"})
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelNote] != "approved by Zach" {
		t.Errorf("labels = %v", l)
	}
}

func TestTickAppliesFinishedReviewersOnce(t *testing.T) {
	r := reviewer("r1", "c1", "code")
	done := reviewer("r2", "c1", "code")
	done.Labels[labelPrefix+labelApplied] = "1"
	young := reviewer("r3", "c1", "code")
	young.CreatedAt = t0.Add(-10 * time.Second)
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low"}), r, done, young)
	storeLog(t, f, p, "r1", "VERDICT: approve")
	f.Tick()
	// r2's verdict is not read again (the idle reap archives it); r3 is too young to apply
	if !p.called("archive", "r1") || p.called("logs", "r2") || p.called("archive", "r3") {
		t.Errorf("calls = %v", p.calls)
	}
}

func TestTickStartsPlanReviewWhenPlannerStops(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	f.Tick()
	runs := p.runsWithLabel(labelArg(labelReviewOf, "c1"))
	if len(runs) != 1 || !contains(runs[0], labelArg(labelReviewKind, "plan")) {
		t.Fatalf("plan review not started: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != "plan-review" {
		t.Errorf("labels = %v", l)
	}
}

func TestTickFinishesDirectCardWithoutReview(t *testing.T) {
	for _, tc := range []struct {
		name, stage, note string
		mergeBase         error
	}{
		{name: "on main", stage: "landed", note: "done in one pass"},
		{name: "not on main", stage: "zach", note: "landing not verified", mergeBase: errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			storeLog(t, f, p, "c1", "Emailed Zach the answer and landed the work item.\nTRACK: direct")
			f.GitOut = gitStub(directOK)
			f.Git = func(_ context.Context, dir string, args ...string) error {
				if args[0] == "merge-base" {
					return tc.mergeBase
				}
				return nil
			}
			f.Tick()
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
				t.Fatalf("review started for a direct card: %v", runs)
			}
			if l := p.labels("c1"); l[labelStage] != tc.stage || l[labelNote] != tc.note {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestTickWaitsForAgentToActAfterMove(t *testing.T) {
	// moved 10 s ago, no activity since: the agent has not picked up the message yet
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: t0.Add(-10 * time.Second).Format(time.RFC3339)})
	c.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c)
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 0 {
		t.Errorf("review started too early: %v", p.calls)
	}
}

func TestTickFinishesLanding(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		gone        bool  // cwd does not exist
		mergeBase   error // result of the ancestor check
		gitCalls    int
	}{
		{name: "on main", stage: "landed", gitCalls: 2},
		{name: "not on main", stage: "zach", mergeBase: errors.New("exit status 1"), gitCalls: 2},
		{name: "cwd gone", stage: "zach", gone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "landing", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
			c.Cwd = t.TempDir()
			if tc.gone {
				c.Cwd += "/gone"
			}
			f, p := newTestFactory(t, c)
			var git [][]string
			f.Git = func(_ context.Context, dir string, args ...string) error {
				git = append(git, append([]string{dir}, args...))
				if args[0] == "merge-base" {
					return tc.mergeBase
				}
				return nil
			}
			storeLog(t, f, p, "c1", "Landed.\nREMAINING: none")
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != tc.stage {
				t.Errorf("labels = %v", l)
			}
			if tc.stage == "zach" && l[labelNote] != "landing not verified" {
				t.Errorf("note = %q", l[labelNote])
			}
			if p.called("archive", "c1") {
				t.Error("card archived")
			}
			if len(git) != tc.gitCalls {
				t.Fatalf("git calls = %v", git)
			}
			if tc.gitCalls > 0 && (git[0][0] != c.Cwd || git[0][1] != "fetch" || strings.Join(git[1][1:], " ") != "merge-base --is-ancestor HEAD origin/main") {
				t.Errorf("git calls = %v", git)
			}
		})
	}
}

func TestTickArchivesFinishedScout(t *testing.T) {
	s := card("s1", map[string]string{labelRole: "scout"})
	f, p := newTestFactory(t, s)
	f.Tick()
	if !p.called("archive", "s1") {
		t.Errorf("scout not archived: %v", p.calls)
	}
}

func TestReviewPromptCarriesRules(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	for _, kind := range []string{"plan", "code"} {
		pr := ReviewPrompt(c, kind, 7478, "unknown")
		for _, want := range []string{"DIFFERENT agent", "TIER:", "VERDICT:", "Read-only", wholeChangeRule,
			"Scope: nothing outside the area of the ask", "- Whole change", "A partial change is a blocking finding"} {
			if !strings.Contains(pr, want) {
				t.Errorf("%s prompt lacks %q", kind, want)
			}
		}
		if strings.Contains(pr, "unasked-for") || strings.Contains(pr, "no unrelated files") {
			t.Errorf("%s prompt keeps the old scope rule", kind)
		}
	}
}

// Changes are whole: callers, tests, docs and README, no old path. Refactoring inside the area of the
// ask does not raise the tier; outside it, it is architecture Zach did not ask for.
func TestWholeChangeRules(t *testing.T) {
	for _, want := range []string{"one way", "caller, test, doc and README", "no old path", "blocking finding"} {
		if !strings.Contains(wholeChangeRule, want) {
			t.Errorf("wholeChangeRule lacks %q", want)
		}
	}
	for _, want := range []string{"Refactoring inside the area of the ask is part of the change and does not raise its tier",
		"a refactor or cleanup outside the area of the ask"} {
		if !strings.Contains(strings.Join(strings.Fields(tierRules), " "), want) {
			t.Errorf("tierRules lacks %q", want)
		}
	}
	if strings.Contains(tierRules, "any refactor") {
		t.Error("tierRules still makes any refactor architecture")
	}
	if !strings.Contains(TierHelp["architecture"], "inside the area of the ask") {
		t.Errorf("TierHelp architecture = %q", TierHelp["architecture"])
	}
	if !strings.Contains(ideaRule(7478, "x", ""), "outside the area of the ask") {
		t.Error("ideaRule still invites follow-ups of the card's own work")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestTickSkipsCardArchivedSinceSnapshot(t *testing.T) {
	since := t0.Add(-10 * time.Minute).Format(time.RFC3339)
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"plan", map[string]string{labelStage: "plan", labelSource: "board", labelSince: since}},
		{"auto-review", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: since}},
		{"landing", map[string]string{labelStage: "landing", labelSince: since}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t, card("c1", tc.labels))
			f.Git = func(context.Context, string, ...string) error { return nil }
			p.archived["c1"] = true // archived outside the board; the snapshot still says live
			f.Tick()
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
				t.Errorf("review started: %v", runs)
			}
			if l := p.labels("c1"); len(l) != 0 {
				t.Errorf("labels changed: %v", l)
			}
		})
	}
}

func TestTickSkipsMoveWhenInspectFails(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
	p.inspectErr = errors.New("daemon down")
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Errorf("review started: %v", runs)
	}
}

func TestVerdictForArchivedTargetIsNotSent(t *testing.T) {
	for _, snapshot := range []bool{true, false} {
		c := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"})
		if snapshot {
			c.ArchivedAt = t0.Add(-time.Minute)
		}
		f, p := newTestFactory(t, c, reviewer("r1", "c1", "plan"))
		p.archived["c1"] = true
		storeLog(t, f, p, "r1", "[Assistant] fix it\nTIER: risky\nVERDICT: changes")
		f.Tick()
		if msgs := p.sent("c1"); len(msgs) != 0 {
			t.Errorf("snapshot=%v: sent to archived target: %v", snapshot, msgs)
		}
		if l := p.labels("c1"); len(l) != 0 {
			t.Errorf("snapshot=%v: target labels changed: %v", snapshot, l)
		}
		if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "changes" {
			t.Errorf("snapshot=%v: reviewer labels = %v", snapshot, l)
		}
		if !p.called("archive", "r1") {
			t.Errorf("snapshot=%v: reviewer not archived", snapshot)
		}
	}
}

func TestVerdictWaitsWhenInspectFails(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "VERDICT: approve")
	p.inspectErr = errors.New("daemon down")
	if err := f.ApplyVerdict(f.agent("r1")); err == nil {
		t.Error("want an error so the next tick retries")
	}
	if len(p.labels("r1")) != 0 || len(p.labels("c1")) != 0 || len(p.sent("c1")) != 0 || p.called("archive", "r1") {
		t.Errorf("acted despite failed inspect: %v", p.calls)
	}
}

func TestTickReadsTrackOnlyFromTheAgentsReply(t *testing.T) {
	// the Fellow prompt has a line "TRACK: direct"; a short plan leaves it in the log tail
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "[User] You are the Fellow...\nTRACK: direct\nAnything else is the build track\n"+
		"[Thought] planning\nPlan: fix the board.\nTIER: risky")
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("want a plan review, calls = %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != "plan-review" {
		t.Errorf("labels = %v", l)
	}
}

func TestTickRecordsBaseCommitOfNewBoardCard(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Second)})
	c.Status = "running"
	f, p := newTestFactory(t, c)
	f.GitOut = func(_ context.Context, dir string, args ...string) (string, error) {
		if dir == c.Cwd && strings.Join(args, " ") == "rev-parse HEAD" {
			return "abc123\n", nil
		}
		return "", errors.New("unexpected git " + strings.Join(args, " "))
	}
	f.Tick()
	if l := p.labels("c1"); l[labelBase] != "abc123" {
		t.Errorf("labels = %v", l)
	}
}

func TestDirectCardNeedsACommitOfItsOwn(t *testing.T) {
	for _, tc := range []struct{ head, stage string }{{"base1", "zach"}, {"new2", "landed"}} {
		c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute), labelBase: "base1"})
		c.Cwd = t.TempDir()
		f, p := newTestFactory(t, c)
		storeLog(t, f, p, "c1", "Answered Zach and landed the work item.\nTRACK: direct")
		f.Git = func(context.Context, string, ...string) error { return nil }
		f.GitOut = gitStub(directOK, map[string]string{"rev-parse HEAD": tc.head + "\n"})
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != tc.stage {
			t.Errorf("head %s: labels = %v", tc.head, l)
		}
	}
}

func TestStuckCardsGoToZach(t *testing.T) {
	since := ago(10 * time.Minute)
	for _, tc := range []struct {
		name   string
		labels map[string]string
		extra  []Agent
		stuck  bool
	}{
		{"building, nothing queued", map[string]string{labelStage: "building", labelSince: since}, nil, true},
		{"building, review queued", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: ago(10 * time.Second)}, nil, false},
		{"plan, not a board card", map[string]string{labelStage: "plan", labelSince: since}, nil, true},
		{"plan review, reviewer running", map[string]string{labelStage: "plan-review", labelSince: since},
			[]Agent{func() Agent { r := reviewer("r1", "c1", "plan"); r.Status = "running"; return r }()}, false},
		{"code review, no reviewer at all after retry", map[string]string{labelStage: "review", labelSince: since, labelNoVerdict: "1"}, nil, true},
		{"code review, reviewer applied after retry", map[string]string{labelStage: "review", labelSince: since, labelNoVerdict: "1"},
			[]Agent{func() Agent { r := reviewer("r1", "c1", "code"); r.Labels[labelPrefix+labelApplied] = "1"; return r }()}, true},
		{"landing, agent never acted", map[string]string{labelStage: "landing", labelSince: since}, nil, true},
		{"building, inside the grace period", map[string]string{labelStage: "building", labelSince: ago(time.Minute)}, nil, false},
		{"needs zach already", map[string]string{labelStage: "zach", labelSince: since}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", tc.labels)
			c.LastActivityAt = t0.Add(-time.Hour)
			if tc.name == "building, inside the grace period" {
				c.LastActivityAt = t0.Add(-time.Minute)
			}
			f, p := newTestFactory(t, append([]Agent{c}, tc.extra...)...)
			f.Tick()
			l := p.labels("c1")
			if got := l[labelStage] == "zach" && l[labelNote] == "stuck"; got != tc.stuck {
				t.Fatalf("stuck = %v, labels = %v", got, l)
			}
			if tc.stuck && l[labelStuckFrom] != tc.labels[labelStage] {
				t.Errorf("stuck-from = %q", l[labelStuckFrom])
			}
		})
	}
}

func TestReviewPending(t *testing.T) {
	running := reviewer("r1", "c1", "code")
	running.Status = "running"
	unapplied := reviewer("r2", "c1", "code")
	applied := reviewer("r3", "c1", "code")
	applied.Labels[labelPrefix+labelApplied] = "1"
	gone := reviewer("r4", "c1", "code")
	gone.ArchivedAt = t0.Add(-time.Minute) // archived without a verdict
	other := reviewer("r5", "c2", "code")
	other.Status = "running"
	for _, tc := range []struct {
		name   string
		agents []Agent
		want   bool
	}{
		{"running", []Agent{running}, true},
		{"stopped, verdict not applied", []Agent{unapplied}, true},
		{"applied", []Agent{applied}, false},
		{"archived without verdict", []Agent{gone}, false},
		{"another card's reviewer", []Agent{other}, false},
		{"none", nil, false},
	} {
		if got := reviewPending(tc.agents, "c1", t0); got != tc.want {
			t.Errorf("%s: reviewPending = %v", tc.name, got)
		}
	}
}

func TestStuckCheckSkipsCardArchivedSinceSnapshot(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSince: ago(10 * time.Minute)}))
	p.archived["c1"] = true
	f.Tick()
	if l := p.labels("c1"); len(l) != 0 {
		t.Errorf("labels changed: %v", l)
	}
}

func TestStoppedUnappliedReviewerGetsAppliedNotStuck(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelSince: ago(10 * time.Minute)}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "building" {
		t.Errorf("labels = %v", l)
	}
}

// withLabels is a with the factory labels l (as recorded by fakePaseo.labels) applied.
func withLabels(a Agent, l map[string]string) Agent {
	out := a
	out.Labels = map[string]string{}
	for k, v := range a.Labels {
		out.Labels[k] = v
	}
	for k, v := range l {
		out.Labels[labelPrefix+k] = v
	}
	return out
}

// longReview is a reviewer session whose final reply spans two text blocks and many lines.
func longReview(t *testing.T, f *Factory, id, of, kind, verdict string, p *fakePaseo) Agent {
	t.Helper()
	r := reviewer(id, of, kind)
	r.Provider, r.SessionID, r.Cwd = "claude", "s-"+id, "/w/"+of
	body := "- FIRST finding: the plan skips the migration\n"
	for i := 0; i < 20; i++ {
		body += fmt.Sprintf("- detail %d\n", i)
	}
	lines := []string{
		`{"type":"user","message":{"content":"review it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + strconv.Quote(body) + `}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"- LAST finding: no test\nTIER: low\nVERDICT: ` + verdict + `"}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-"+of+"/s-"+id+".jsonl", strings.Join(lines, "\n")+"\n")
	return r
}

func TestReviewerNotesReachBuilderWhole(t *testing.T) {
	f, p := newTestFactory(t)
	c := card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "1"})
	f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", "code", "changes", p)})
	f.ApplyVerdict(f.agent("r1"))
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "FIRST finding") || !strings.Contains(msgs[0], "LAST finding") ||
		strings.Contains(msgs[0], "VERDICT") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestReviewPromptTreatsZachsRulingsAsSettled(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Tier: "low"}
	if strings.Contains(ReviewPrompt(c, "code", 7478, "unknown"), "already ruled") {
		t.Error("rule without a ruling")
	}
	c.Decided = true
	if !strings.Contains(ReviewPrompt(c, "code", 7478, "unknown"), "already ruled") {
		t.Error("ruling not passed to the reviewer")
	}
}

func TestNoLiveChangesBeforeCodeReview(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	planPrompt := fmt.Sprintf(newCardPrompt, "req", "example/desk", "", "", "", "")
	for name, s := range map[string]string{
		"newCardPrompt": planPrompt,
		"plan rubric":   ReviewPrompt(c, "plan", 7478, "unknown"),
	} {
		if !strings.Contains(s, "deploy steps") {
			t.Errorf("%s lacks deploy steps requirement: %q", name, s)
		}
	}

	f, p := newTestFactory(t, card("c-dirty", map[string]string{labelStage: "building", labelSource: "board"}))
	dirtyCard, _ := f.card("c-dirty")
	if err := f.askToCommit(dirtyCard, " M a.go"); err != nil {
		t.Fatal(err)
	}

	noLivePrompts := map[string]string{
		"newCardPrompt":  planPrompt,
		"buildMsg":       buildMsg("Plan approved.", "(none)"),
		"reworkMsg plan": reworkMsg("plan", 1, 3, "fix it"),
		"reworkMsg code": reworkMsg("code", 1, 3, "fix it"),
		"askToCommit":    p.sent("c-dirty")[0],
		"arbiter fixes":  arbiterFixMsg("fix it"),
		"code rubric":    ReviewPrompt(c, "code", 7478, "unknown"),
	}
	for name, s := range noLivePrompts {
		if !strings.Contains(s, "live changes") {
			t.Errorf("%s lacks no-live-changes rule: %q", name, s)
		}
	}
	for name, s := range map[string]string{
		"buildMsg":       noLivePrompts["buildMsg"],
		"reworkMsg code": noLivePrompts["reworkMsg code"],
		"askToCommit":    noLivePrompts["askToCommit"],
		"arbiter fixes":  noLivePrompts["arbiter fixes"],
	} {
		if !strings.Contains(s, "deploy steps") {
			t.Errorf("%s does not ask the summary to restate deploy steps: %q", name, s)
		}
	}
	codeRubric := noLivePrompts["code rubric"]
	for _, want := range []string{"deploy steps", "fails part-way", "run twice"} {
		if !strings.Contains(codeRubric, want) {
			t.Errorf("code rubric lacks %q: %q", want, codeRubric)
		}
	}
	for name, s := range map[string]string{
		"landMsg":        landMsg,
		"guardedLandMsg": guardedLandMsg,
	} {
		for _, want := range []string{"deploy steps", "stop and report", "improvise"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q: %q", name, want, s)
			}
		}
	}
}

func TestParseSmallFix(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "- missing sender\nSMALL FIX: add confirmAnswer to factoryMails\nTIER: risky\nVERDICT: changes", "add confirmAnswer to factoryMails"},
		{"markdown and commas", "**SMALL FIX:** `a, b` and c\nTIER: low\nVERDICT: changes", "a; b and c"},
		{"placeholder ignored", "SMALL FIX: <one-line summary of the fixes the builder must apply>\nTIER: low\nVERDICT: changes", ""},
		{"placeholder then real", "SMALL FIX: <summary>\nSMALL FIX: record deliver in factoryMails\n", "record deliver in factoryMails"},
		{"negative values ignored", "SMALL FIX: none\nSMALL FIX: no\nSMALL FIX: false\nSMALL FIX: n/a", ""},
		{"mid-line mention ignored", "A line starting with SMALL FIX: lets the plan go to build.\nTIER: low\nVERDICT: changes", ""},
		{"unprompted aliases ignored", "FIX SIZE: small\nBLOCKER SIZE: small\nVERDICT: changes (small)", ""},
		{"absent", "- design disagreement\nTIER: risky\nVERDICT: changes", ""},
	}
	for _, tc := range cases {
		if got := ParseSmallFix(tc.in); got != tc.want {
			t.Errorf("%s: ParseSmallFix = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPlanReworkCapSmallFixGoesToBuild(t *testing.T) {
	reviewText := "- confirmAnswer is missing from factoryMails\n" +
		"SMALL FIX: add confirmAnswer to factoryMails\nBUILD SIDE EFFECTS: none\nTIER: risky\nVERDICT: changes"

	// Rounds 1 and 2 still go back to plan rework even with SMALL FIX.
	for _, prev := range []string{"0", "1"} {
		f, p := newTestFactory(t,
			card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: prev}),
			reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
		storeLog(t, f, p, "r1", reviewText)
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); l[labelStage] != "plan" || l[labelAutoReview] != "plan" || l[labelPlanFix] != "" {
			t.Fatalf("prev=%s: labels = %v", prev, l)
		}
	}

	// Round 3 (at rework cap) with SMALL FIX goes to building, not Needs Zach, and code review checks the fix.
	f, p := newTestFactory(t,
		card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: "2"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", reviewText)
	storeLog(t, f, p, "c1", "[Assistant] Plan...\nWaits on: abc1234\nTIER: risky")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelVerdict] != "plan fixes" || l[labelAutoReview] != "code" ||
		l[labelPlanRounds] != "3" || l[labelTier] != "risky" ||
		l[labelPlanFix] != "add confirmAnswer to factoryMails" || l[labelWaitsOn] != "abc1234" {
		t.Fatalf("at cap with small fix: labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 {
		t.Fatalf("sent = %v", msgs)
	}
	for _, want := range []string{"Apply these fixes, then build", "confirmAnswer is missing", "live changes", "deploy steps"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("build message lacks %q:\n%s", want, msgs[0])
		}
	}

	// Once in building via plan fixes, planPending is false even if the card later visits Needs Zach.
	built := withLabels(f.agent("c1"), l)
	stuckInZach := withLabels(built, map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	if planPending(stuckInZach) {
		t.Errorf("planPending should be false after moving to build with plan fixes: %v", stuckInZach.Labels)
	}

	// Code reviewer prompt instructs the reviewer to verify the last-round plan fixes.
	f.SetAgents([]Agent{built})
	cCard, _ := f.card("c1")
	codePrompt := ReviewPrompt(cCard, "code", 7478, "unknown")
	for _, want := range []string{"add confirmAnswer to factoryMails", "last plan review", "blocking finding"} {
		if !strings.Contains(codePrompt, want) {
			t.Errorf("code review prompt lacks %q:\n%s", want, codePrompt)
		}
	}
	planPrompt := ReviewPrompt(cCard, "plan", 7478, "unknown")
	if !strings.Contains(planPrompt, "SMALL FIX: <") {
		t.Errorf("plan review prompt does not teach SMALL FIX:\n%s", planPrompt)
	}
	if got := ParseSmallFix(planPrompt); got != "" {
		t.Errorf("plan review prompt matched its own SMALL FIX rule: %q", got)
	}

	// Code review at the rework cap goes to the arbiter even if SMALL FIX is present.
	f, p = newTestFactory(t,
		card("c-code", map[string]string{labelStage: "review", labelSource: "board", labelTier: "risky", labelPlanRounds: "1", labelCodeRounds: "2"}),
		reviewer("r-code", "c-code", "code"))
	storeLog(t, f, p, "r-code", reviewText)
	if err := f.ApplyVerdict(f.agent("r-code")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c-code"); l[labelNote] != arbiterNote || len(p.runsWithLabel(labelArg(labelArbiter, "1"))) != 1 {
		t.Errorf("code review at cap should go to the arbiter: %v", l)
	}
}

func TestPlanReworkCapSmallFixWithSideEffectsWaitsForZach(t *testing.T) {
	// the reviewer did not rule out build side effects: Zach allows the build first, whatever the tier
	archReview := "- missing Ordering line for overlapping card\n" +
		"SMALL FIX: add Ordering line for overlapping card\nTIER: architecture\nVERDICT: changes"
	base := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelTier: "architecture", labelPlanRounds: "2"})
	f, p := newTestFactory(t, base, reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", archReview)
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelAwaiting] != "plan" || l[labelVerdict] != "plan side effects" ||
		l[labelNote] != sideEffectNote || l[labelPlanFix] != "add Ordering line for overlapping card" {
		t.Fatalf("architecture at cap with small fix: labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "add Ordering line") || !strings.Contains(msgs[0], "DECISION:") {
		t.Errorf("summary message = %v", msgs)
	}

	// Path A: Zach approves the architecture plan -> goes to building, keeping plan-fix for code review.
	waiting := withLabels(base, l)
	f, p = newTestFactory(t, waiting)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	al := p.labels("c1")
	if al[labelStage] != "building" || al[labelZachPlan] != "approved" || al[labelAutoReview] != "code" {
		t.Errorf("after Zach approves: labels = %v", al)
	}
	if sent := p.sent("c1"); len(sent) != 1 || !strings.Contains(sent[0], "add Ordering line for overlapping card") {
		t.Errorf("approve build message lacks small fix: %v", sent)
	}
	approvedCard := BuildBoard([]Agent{withLabels(waiting, al)}, nil, t0)[0]
	if !strings.Contains(ReviewPrompt(approvedCard, "code", 7478, "unknown"), "add Ordering line for overlapping card") {
		t.Error("code review prompt after architecture approve lacks small fix")
	}

	// Path B: cap -> small fix -> Zach replies with changes -> stuck in plan -> approve must build, not land.
	f, p = newTestFactory(t, waiting)
	if _, err := f.Act("send", Body{ID: "c1", Message: "shrink the batch size"}); err != nil {
		t.Fatal(err)
	}
	sl := p.labels("c1")
	if sl[labelStage] != "plan" || sl[labelAwaiting] != "" || sl[labelPlanFix] != "" {
		t.Fatalf("after Zach reply with changes: labels = %v", sl)
	}
	revising := withLabels(waiting, sl)
	stuckPlan := withLabels(revising, map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	if !planPending(stuckPlan) {
		t.Fatalf("planPending must stay true when stuck before plan approval: %v", stuckPlan.Labels)
	}
	f.SetAgents([]Agent{stuckPlan})
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1"); got[labelStage] != "building" || got[labelZachPlan] != "approved" {
		t.Errorf("approve on stuck unapproved architecture plan must build, not land: %v", got)
	}
}

// stopsAfterReply plays a card agent answering Zach's message: it acts a minute after the move the
// reply made, then stops with reply as its last message.
func stopsAfterReply(t *testing.T, f *Factory, p *fakePaseo, c Agent, reply string) {
	c.Labels = maps.Clone(c.Labels)
	for k, v := range p.labels(c.ID) {
		c.Labels[labelPrefix+k] = v
	}
	f.Now = func() time.Time { return t0.Add(2 * time.Minute) }
	c.LastActivityAt = t0.Add(time.Minute)
	f.SetAgents([]Agent{c})
	storeLog(t, f, p, c.ID, reply)
}

func TestMessageMidErrandStillFinishesDirect(t *testing.T) {
	for _, tc := range []struct {
		name, source, via, reply string
	}{
		{"board message, then TRACK: direct", "board", "", "Sent Zach the deck with the memos.\nTRACK: direct"},
		{"inbox card, email follow-up, no TIER line", "email", "email", "Sent Zach the fixed deck."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "plan", labelSource: tc.source, labelSince: ago(10 * time.Minute)})
			c.Labels["gmail-thread"] = "t1"
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = gitStub(directOK)
			if _, err := f.Act("send", Body{ID: "c1", Message: "include the memos from this morning", Via: tc.via}); err != nil {
				t.Fatal(err)
			}
			if l := p.labels("c1"); l[labelStage] != "plan" || l[labelAutoReview] != "" {
				t.Fatalf("after the message: labels = %v", l)
			}
			stopsAfterReply(t, f, p, c, tc.reply)
			f.Tick()
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
				t.Fatalf("review started for an errand: %v", runs)
			}
			if l := p.labels("c1"); l[labelStage] != "landed" || l[labelTrack] != "direct" {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestEmailFollowUpAfterDirectFinishStaysDirect(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "email", labelSince: ago(10 * time.Minute)})
	c.Labels["gmail-thread"] = "t1"
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.GitOut = gitStub(directOK)
	storeLog(t, f, p, "c1", "Sent Zach the deck.\nTRACK: direct")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landed" || l[labelTrack] != "direct" {
		t.Fatalf("first pass: labels = %v", l)
	}
	if _, err := f.Act("send", Body{ID: "c1", Message: "The file is corrupted", Via: "email"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "plan" || l[labelAutoReview] != "" {
		t.Fatalf("after the follow-up: labels = %v", l)
	}
	stopsAfterReply(t, f, p, c, "Sent Zach a fixed copy.\nTRACK: direct")
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Fatalf("review started for an errand: %v", runs)
	}
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Errorf("labels = %v", l)
	}
}

func TestDirectCardWithAPlanAfterAFollowUpGetsPlanReview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landed", labelSource: "board", labelTrack: "direct", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("send", Body{ID: "c1", Message: "now change the code too"}); err != nil {
		t.Fatal(err)
	}
	stopsAfterReply(t, f, p, c, "PLAN\nGoal: ...\nTIER: low")
	f.Tick()
	runs := p.runsWithLabel(labelArg(labelReviewOf, "c1"))
	if len(runs) != 1 || !contains(runs[0], labelArg(labelReviewKind, "plan")) {
		t.Fatalf("plan review not started: %v", p.calls)
	}
}

func TestTickFinishesDirectCardQueuedForCodeReview(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		labels      map[string]string
		review      bool
	}{
		{"TRACK: direct, no plan review", "Sent Zach the files.\nTRACK: direct", nil, false},
		{"no TRACK line", "Fixed it and committed.", nil, true},
		{"a plan with TIER", "PLAN\nGoal: ...\nTIER: low\nTRACK: direct", nil, true},
		{"its plan passed review", "Sent Zach the files.\nTRACK: direct", map[string]string{labelPlanRounds: "1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := map[string]string{labelStage: "building", labelSource: "email", labelAutoReview: "code", labelSince: ago(10 * time.Minute)}
			for k, v := range tc.labels {
				l[k] = v
			}
			c := card("c1", l)
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = gitStub(directOK, map[string]string{"status --porcelain": "", "rev-parse HEAD": "bbb\n"})
			storeLog(t, f, p, "c1", tc.reply)
			f.Tick()
			runs := p.runsWithLabel(labelArg(labelReviewOf, "c1"))
			if tc.review {
				if len(runs) != 1 {
					t.Fatalf("code review not started: %v", p.calls)
				}
				return
			}
			if len(runs) != 0 {
				t.Fatalf("code review started for a direct card: %v", runs)
			}
			if got := p.labels("c1"); got[labelStage] != "landed" || got[labelAutoReview] != "" {
				t.Errorf("labels = %v", got)
			}
		})
	}
}

func TestWorktreeNotTmpRule(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	planPrompt := strings.ReplaceAll(fmt.Sprintf(newCardPrompt, "req", "example/desk", "", "", "", ""), "<errands>", "desk")

	f, p := newTestFactory(t, card("c-dirty", map[string]string{labelStage: "building", labelSource: "board"}))
	dirtyCard, _ := f.card("c-dirty")
	if err := f.askToCommit(dirtyCard, " M a.go"); err != nil {
		t.Fatal(err)
	}

	worktreePrompts := map[string]string{
		"newCardPrompt":  planPrompt,
		"buildMsg":       buildMsg("Plan approved.", "(none)"),
		"reworkMsg plan": reworkMsg("plan", 1, 3, "fix it"),
		"reworkMsg code": reworkMsg("code", 1, 3, "fix it"),
		"askToCommit":    p.sent("c-dirty")[0],
		"arbiter fixes":  arbiterFixMsg("fix it"),
	}
	for name, s := range worktreePrompts {
		for _, want := range []string{"worktree", "/tmp", ".factory-scratch/", ".git/info/exclude", "Never run commands from inside .factory-scratch/", "from the worktree root using its literal absolute path", "Never run cd X && git", "use git -C X"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(planPrompt, "work/YYYY-MM-DD-short-name/") {
		t.Errorf("newCardPrompt direct track lacks work/YYYY-MM-DD-short-name/ instruction:\n%s", planPrompt)
	}

	sharedScratchPrompts := map[string]string{
		"delegate builder":  delegateText(nil, false),
		"delegate reviewer": delegateText(nil, true),
		"review plan":       ReviewPrompt(c, "plan", 7478, "unknown"),
		"review code":       ReviewPrompt(c, "code", 7478, "unknown"),
		"review land":       ReviewPrompt(c, "land", 7478, "unknown"),
	}
	for name, s := range sharedScratchPrompts {
		for _, want := range []string{"Never run commands from inside .factory-scratch/", "from the worktree root using its literal absolute path", "Never run cd X && git", "use git -C X"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		for _, forbidden := range []string{"delete .factory-scratch/", "Before landing or finishing", scratchClean} {
			if strings.Contains(s, forbidden) {
				t.Errorf("%s instructs a shared-worktree user to clean up: %q", name, forbidden)
			}
		}
	}

	deleteScratchPrompts := map[string]string{
		"worktreeRule":   worktreeRule,
		"landMsg":        landMsg,
		"guardedLandMsg": guardedLandMsg,
		"landMsgFor":     f.landMsgFor(f.agent("c-code")),
		"rerunLandMsg":   rerunLandMsg(false),
		"rerunGuarded":   rerunLandMsg(true),
	}
	for name, s := range worktreePrompts {
		deleteScratchPrompts[name] = s
	}
	for name, s := range deleteScratchPrompts {
		for _, want := range []string{"delete .factory-scratch/", "retirement", scratchClean, "Never run commands from inside .factory-scratch/", "from the worktree root using its literal absolute path", "Never run cd X && git", "use git -C X"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}

	codeRubric := ReviewPrompt(c, "code", 7478, "unknown")
	for _, want := range []string{"must not use /tmp for work that belongs in the worktree", "t.TempDir()"} {
		if !strings.Contains(codeRubric, want) {
			t.Errorf("code rubric lacks %q:\n%s", want, codeRubric)
		}
	}
}

func TestParseSideEffects(t *testing.T) {
	cases := map[string]string{
		"BUILD SIDE EFFECTS: none":                                "none",
		"**BUILD SIDE EFFECTS:** None.":                           "none",
		"- BUILD SIDE EFFECTS: backfills prod rows\nTIER: risky":  "backfills prod rows",
		"BUILD SIDE EFFECTS: <none, or what building changes>":    "",
		"BUILD SIDE EFFECTS: <x>\nplan\nBUILD SIDE EFFECTS: none": "none",
		"no line here": "",
	}
	for in, want := range cases {
		if got := ParseSideEffects(in); got != want {
			t.Errorf("ParseSideEffects(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only a bare none means building has no side effects; its reason goes on the next line. Anything else on
// the line, even a reason that sounds safe, is a side effect, so no reading of free text can skip Zach.
func TestParseSideEffectsOnlyBareNone(t *testing.T) {
	for _, s := range []string{"none", "None.", "** None.", "`none`", "NOTHING", "n/a", "no"} {
		if got := ParseSideEffects("BUILD SIDE EFFECTS: " + s + "\nBecause code and tests stay in the worktree."); got != "none" {
			t.Errorf("%q = %q, want none", s, got)
		}
	}
	effects := []string{
		// a none with a reason on the same line, even a safe-sounding one
		"none (code, test and README changes in the worktree only; deploy happens only at landing)",
		"none; build uses only worktree changes and fake test data.",
		"none (code and tests in the worktree only; nothing is sent, deployed or written to prod while building)",
		"none (synthetic fixtures and temporary-directory stores only; no scheduler, host config, receipt or service changes before landing)",
		"None — code and tests only.",
		"none: unit tests on fake data",
		"nothing. Only the worktree changes.",
		"none. agents get created only after landing",
		"none. The change is Go code and unit tests only, with read-only queries.",
		"none (tests use temporary folders; mail stays untouched)",
		"none (starting nothing; the UI check uses dry-run, which sends no email and answers no prompts)",
		// reviewers' counterexamples: real effects behind a none
		"none to prod; sends a test email",
		"none outside the worktree; writes one row to the live database",
		"none; sends a test email to Zach",
		"none. posts a test message to Slack",
		"none. archives the old agents",
		"none, once the fix is applied (the dry run moves to landing). As written, the build copies a jar to the phone and runs it.",
		"none for the plan as written. A reworked plan would write production data.",
		"none, except one test email to Zach",
		"none (but it restarts the dev service)",
		"no production changes during build. Landing will migrate outbox state.",
		"writes prod rows",
		"sends a test email",
		"none. Starts a scheduler on the host and publishes a release.",
		"none; rotates the test token and kills the old agent",
		"none; no review needed, sends Zach a test email",
		"none. Deploys at landing, and posts to Slack now",
		"none. Not a big change: emails the team",
		"none. Runs terraform apply on the prod VPC",
		"none. Calls the Stripe refund API",
		"none. Opens a pull request and comments on issues",
		"none. Nothing is written, and it sends a test email",
		"none. Posts to Slack, not email",
		"none. Deploys at landing, and posts to Slack",
		"none. The live factory is untouched, and it posts to Slack now",
		"none. Sends no email but posts to Slack",
		"none. Posts to Slack, and deploys at landing",
		"none. Restarts the live service and mail is untouched",
		"none. Existing agents get relabelled, and any anchor agent gets created, only after landing.",
		"none. The schedule prompt rewrites and the backup happen only in the landing step.",
		"none (the fake-data server runs on scratch state; the live factory, its stores and mail are untouched)",
		"none. Writes to prod, the dev database stays unchanged",
		"none. Sends Zach an email, then the rest happens at landing",
		"none. No new code, just deploys to prod",
		"none. No schema change, only restarts the dev service",
		"none. Nothing big happens until it deploys to prod",
		"none. Changes the DNS records",
		"none. Overwrites the config on the server",
		"none. Reboots the phone",
		"none. Edits the systemd unit and reloads it",
		"none. Texts Zach a link",
		"none. Inserts rows into the Postgres database",
		"none. Texts Zach before the deploy happens at landing",
		"none. Code and tests, and changes production",
		"none (sends nothing to prod, but writes the live table)",
		"none. No schema change, will restart the prod service",
		"none. No new tests, will change the prod config",
		"none. No tests, deploy to prod",
		"none. Nothing faked, the deploy runs on prod",
		"none. No mocks, changes go to prod",
		"none. Sends the email before the deploy happens at landing",
		"none. Writes to prod before it happens at landing",
		"none. Changes the card store",
		"none. Changes data",
		"none. Restarts prod",
	}
	for _, s := range effects {
		if got := ParseSideEffects("BUILD SIDE EFFECTS: " + s); got == "none" || got == "" {
			t.Errorf("%q = %q, want it read as a side effect", s, got)
		}
	}
}

// A reviewer that writes a bare none with its reason on the next line lets the plan build first; the same
// reason on the line stops it for Zach.
func TestBareNoneWithReasonBelowBuildsFirst(t *testing.T) {
	for _, tc := range []struct {
		review string
		build  bool
	}{
		{"BUILD SIDE EFFECTS: none\nCode, tests and README stay in the worktree; the deploy happens only at landing.", true},
		{"BUILD SIDE EFFECTS: none (code, tests and README in the worktree; the deploy happens only at landing)", false},
	} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
			reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "r1", "plan is fine\n"+tc.review+"\nTIER: architecture\nVERDICT: approve")
		storeLog(t, f, p, "c1", "plan: new page\nBUILD SIDE EFFECTS: none\nBuilding changes only the worktree.\nTIER: architecture")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if built := l[labelStage] == "building" && l[labelAwaiting] == ""; built != tc.build {
			t.Errorf("%q: built first = %v, want %v: %v", tc.review, built, tc.build, l)
		}
	}
}

func TestArchitecturePlanWithNoBuildSideEffectsBuildsFirst(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "plan is fine\nBUILD SIDE EFFECTS: none\nTIER: architecture\nVERDICT: approve")
	storeLog(t, f, p, "c1", "plan: new page\nBUILD SIDE EFFECTS: none\nTIER: architecture")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelAwaiting] != "" || l[labelTier] != "architecture" {
		t.Errorf("architecture plan with no build side effects should build: %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Build it now") || !strings.Contains(msgs[0], "before it lands") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestArchitecturePlanStopsEarlyUnlessReviewerFindsNoSideEffects(t *testing.T) {
	cases := map[string][2]string{
		"reviewer has no line":       {"TIER: architecture\nVERDICT: approve", "plan\nBUILD SIDE EFFECTS: none\nTIER: architecture"},
		"reviewer lists one":         {"BUILD SIDE EFFECTS: writes prod rows\nTIER: architecture\nVERDICT: approve", "plan\nBUILD SIDE EFFECTS: none"},
		"builder lists one":          {"BUILD SIDE EFFECTS: none\nTIER: architecture\nVERDICT: approve", "plan\nBUILD SIDE EFFECTS: sends a test email\nTIER: architecture"},
		"reviewer none, builder old": {"BUILD SIDE EFFECTS: none\nTIER: architecture\nVERDICT: nits", "plan\nTIER: architecture"},
	}
	for name, logs := range cases {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
			reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "r1", logs[0])
		storeLog(t, f, p, "c1", logs[1])
		f.ApplyVerdict(f.agent("r1"))
		l := p.labels("c1")
		wantStop := name != "reviewer none, builder old"
		if stopped := l[labelStage] == "zach" && l[labelAwaiting] == "plan"; stopped != wantStop {
			t.Errorf("%s: early stop = %v, want %v: %v", name, stopped, wantStop, l)
		}
	}
}

func TestArchitectureSmallFixWithNoSideEffectsBuilds(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelTier: "architecture", labelPlanRounds: "2"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "SMALL FIX: add Ordering line\nBUILD SIDE EFFECTS: none\nTIER: architecture\nVERDICT: changes")
	storeLog(t, f, p, "c1", "plan\nBUILD SIDE EFFECTS: none")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelPlanFix] != "add Ordering line" || l[labelAwaiting] != "" {
		t.Errorf("labels = %v", l)
	}
}

func TestArchitectureCodeApprovedWithoutZachStopsWithPreview(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "TIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != readyNote(1) || l[labelReady] != "1" || l[labelWriting] == "" {
		t.Errorf("labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "CHANGED: ") || !strings.Contains(msgs[0], "phone") ||
		!strings.Contains(msgs[0], "Do not start a server") || strings.Contains(msgs[0], "file-share") ||
		strings.Contains(msgs[0], "timeout 24h") || !strings.Contains(msgs[0], "DECISION:") || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("ready message = %v", msgs)
	}

	// approving the preview lands it; a reply sends it back to building and code review
	waiting := withLabels(card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1", labelCodeRounds: "1", labelSource: "board"}), l)
	f, p = newTestFactory(t, waiting)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1"); got[labelStage] != "landing" {
		t.Errorf("approve on preview: %v", got)
	}
	f, p = newTestFactory(t, waiting)
	if _, err := f.Act("send", Body{ID: "c1", Message: "make the chart bigger"}); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1"); got[labelStage] != "building" || got[labelAutoReview] != "code" {
		t.Errorf("reply on preview: %v", got)
	}
}
