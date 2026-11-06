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
	text := "[Assistant] looks fine\nTIER: **risky**\nSIGN OFF: none\nVERDICT: approve\n"
	if got := ParseVerdict(text); got != "approve" {
		t.Errorf("verdict = %q", got)
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

func TestCodeApprovedAutoLandsByTier(t *testing.T) {
	// work the factory started lands after the gates when nothing is on the always-ask list
	for _, tier := range []string{"low", "risky"} {
		f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: gateDesign, labelTier: tier, labelRoundsRequest: "1", labelReviewed: reviewedSHA}),
			reviewer("r1", "c1", gateSignoff))
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: "+tier+"\nSIGN OFF: none\nVERDICT: nits")
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

func TestCardBeforeArchivedClosedReviewerStartsOnlyOneReviewer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: gateRequest, labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("r1", "c1", gateRequest)
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
	if l[labelStage] != gateRequest || l[labelReviewerRetries] != "1" {
		t.Errorf("labels = %v", l)
	}
}

func TestFailedAutoRerunRetriesOnNextTick(t *testing.T) {
	c := card("c1", map[string]string{labelStage: gateRequest, labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("r1", "c1", gateRequest)
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "Review interrupted before a verdict.")
	p.runErr = errors.New("paseo run failed")
	if err := f.ApplyVerdict(f.agent("r1")); err == nil {
		t.Fatal("expected StartReview error when paseo run fails")
	}
	l := p.labels("c1")
	if l[labelReviewerRetries] != "1" {
		t.Fatalf("replacement not counted before StartReview: %v", l)
	}
	p.runErr = nil
	rApplied := withLabels(r, p.labels("r1"))
	f.SetAgents([]Agent{withLabels(c, l), rApplied})
	f.Tick()
	l = p.labels("c1")
	if l[labelStage] != gateRequest || l[labelReviewerRetries] != "2" || l[labelStuck] != "" {
		t.Errorf("after failed auto-rerun, next Tick should start the second replacement: labels = %v", l)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 2 {
		t.Errorf("runs = %d, want 2", len(runs))
	}
}

func TestVerdictAndDirtyCommitClearReviewerRetries(t *testing.T) {
	c := card("c1", map[string]string{labelStage: gateRequest, labelSource: "board", labelReviewerRetries: "1"})
	r := reviewer("r1", "c1", gateRequest)
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: changes\n- fix x")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if v, ok := l[labelReviewerRetries]; !ok || v != "" || l[labelStage] != "building" {
		t.Errorf("valid verdict did not clear reviewer retries: %v", l)
	}

	// Same-pass dirty worktree: ApplyVerdict counts a replacement, then StartReview sees a dirty tree and calls
	// askToCommit in the same pass. askToCommit must clear the count so the next code review round gets its
	// own replacements.
	cDirty := card("c2", map[string]string{labelStage: gateDesign, labelSource: "board"})
	cDirty.Cwd = "/w/c2"
	rDirty := reviewer("r2", "c2", gateSignoff)
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
	if v, ok := l2[labelReviewerRetries]; !ok || v != "" || l2[labelStage] != "building" {
		t.Errorf("same-pass askToCommit did not clear reviewer retries: %v", l2)
	}
}

func TestApproveLandsOtherwise(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelReviewed: reviewedSHA}))
	headIs(f, reviewedSHA, nil)
	f.Act("approve", Body{ID: "c1"})
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelNote] != "approved by Zach" {
		t.Errorf("labels = %v", l)
	}
}

func TestTickAppliesFinishedReviewersOnce(t *testing.T) {
	r := reviewer("r1", "c1", gateSignoff)
	done := reviewer("r2", "c1", gateSignoff)
	done.Labels[labelPrefix+labelApplied] = "1"
	young := reviewer("r3", "c1", gateSignoff)
	young.CreatedAt = t0.Add(-10 * time.Second)
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelTier: "low"}), r, done, young)
	storeLog(t, f, p, "r1", "VERDICT: approve")
	f.Tick()
	// r2's verdict is not read again (the idle reap archives it); r3 is too young to apply
	if !p.called("archive", "r1") || p.called("logs", "r2") || p.called("archive", "r3") {
		t.Errorf("calls = %v", p.calls)
	}
}

func TestTickFinishesDirectCardWithoutReview(t *testing.T) {
	for _, tc := range []struct {
		name, stage, note string
		mergeBase         error
	}{
		{name: "on main", stage: "landed", note: "done in one pass"},
		{name: "not on main: told to land again", stage: "", note: "landing its result again", mergeBase: errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
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
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: gateRequest, labelSince: t0.Add(-10 * time.Second).Format(time.RFC3339)})
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
		{name: "not on main", stage: "building", mergeBase: errors.New("exit status 1"), gitCalls: 2},
		{name: "cwd gone", stage: "building", gone: true},
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
			if tc.stage == "building" && (l[labelNote] != "rebuilding: landing not verified" || l[labelAutoReview] == "" ||
				len(p.sent("c1")) != 1 || !strings.Contains(p.sent("c1")[0], "Rebase on origin/main")) {
				t.Errorf("not sent back to building: %v %v", l, p.sent("c1"))
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

// Changes are whole: callers, tests, docs and README, no old path. Refactoring inside the area of the
// ask does not raise the tier; outside it, it is architecture Zach did not ask for.

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
		{"plan", map[string]string{labelStage: "building", labelSource: "board", labelSince: since}},
		{"auto-review", map[string]string{labelStage: "building", labelAutoReview: gateRequest, labelSince: since}},
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
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
	p.inspectErr = errors.New("daemon down")
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Errorf("review started: %v", runs)
	}
}

func TestVerdictForArchivedTargetIsNotSent(t *testing.T) {
	for _, snapshot := range []bool{true, false} {
		c := card("c1", map[string]string{labelStage: gateRequest, labelSource: "board"})
		if snapshot {
			c.ArchivedAt = t0.Add(-time.Minute)
		}
		f, p := newTestFactory(t, c, reviewer("r1", "c1", gateRequest))
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
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateRequest, labelSource: "board"}),
		reviewer("r1", "c1", gateRequest))
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
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "[User] You are the Fellow...\nTRACK: direct\nAnything else is the build track\n"+
		"[Thought] planning\nPlan: fix the board.\nTIER: risky")
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("want a plan review, calls = %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != gateRequest {
		t.Errorf("labels = %v", l)
	}
}

func TestTickRecordsBaseCommitOfNewBoardCard(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: ago(10 * time.Second)})
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
	for _, tc := range []struct{ head, stage string }{{"base1", ""}, {"new2", "landed"}} {
		c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: ago(10 * time.Minute), labelBase: "base1"})
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

func TestStoppedCardsRecoverInTheirColumn(t *testing.T) {
	since := ago(10 * time.Minute)
	for _, tc := range []struct {
		name   string
		labels map[string]string
		extra  []Agent
		want   string // "nudge", "stuck" or "" (nothing)
	}{
		{"building, nothing queued", map[string]string{labelStage: "building", labelSince: since}, nil, "nudge"},
		{"building, review queued", map[string]string{labelStage: "building", labelAutoReview: gateRequest, labelSince: ago(10 * time.Second)}, nil, ""},
		{"plan, not a board card", map[string]string{labelStage: "building", labelSince: since}, nil, "nudge"},
		{"plan review, reviewer running", map[string]string{labelStage: gateRequest, labelSince: since},
			[]Agent{func() Agent { r := reviewer("r1", "c1", gateRequest); r.Status = "running"; return r }()}, ""},
		{"code review, no reviewer after the replacements and arbiters", map[string]string{labelStage: gateDesign, labelSince: since, labelReviewerRetries: "4"}, nil, "stuck"},
		{"code review, reviewer applied after the replacements and arbiters", map[string]string{labelStage: gateDesign, labelSince: since, labelReviewerRetries: "4"},
			[]Agent{func() Agent {
				r := reviewer("r1", "c1", gateSignoff)
				r.Labels[labelPrefix+labelApplied] = "1"
				return r
			}()}, "stuck"},
		{"landing, agent never acted", map[string]string{labelStage: "landing", labelSince: since}, nil, "nudge"},
		{"building, inside the grace period", map[string]string{labelStage: "building", labelSince: ago(time.Minute)}, nil, ""},
		{"needs zach already", map[string]string{labelStage: "zach", labelSince: since}, nil, ""},
		{"already marked stuck", map[string]string{labelStage: "building", labelSince: since, labelStuck: stuckStopped}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.labels[labelArchiveWatch] = "1" // already enrolled; this test checks recovery moves
			c := card("c1", tc.labels)
			c.LastActivityAt = t0.Add(-time.Hour)
			if tc.name == "building, inside the grace period" {
				c.LastActivityAt = t0.Add(-time.Minute)
			}
			f, p, m := mailFactory(t, append([]Agent{c}, tc.extra...)...)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] == "zach" || len(m.sends()) != 0 {
				t.Fatalf("went to Needs Zach or emailed: %v %v", l, m.sends())
			}
			got := ""
			switch {
			case l[labelStuck] != "":
				got = "stuck"
			case len(p.sent("c1")) == 1 && strings.HasPrefix(p.sent("c1")[0], "The factory has no next step"):
				got = "nudge"
			case len(p.sent("c1")) != 0 || len(l) != 0:
				got = fmt.Sprintf("other: %v %v", l, p.sent("c1"))
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStoppedCardIsNudgedThenRerunThenMarkedStuck(t *testing.T) {
	for _, stage := range []string{"building", "landing"} {
		t.Run(stage, func(t *testing.T) {
			since := ago(10 * time.Minute)
			c := card("c1", map[string]string{labelStage: stage, labelSince: since, labelReviewed: reviewedSHA})
			c.LastActivityAt = t0.Add(-time.Hour)
			f, p := newTestFactory(t, c)
			headIs(f, reviewedSHA, nil)
			stopAgain := func() {
				c = withLabels(c, p.labels("c1"))
				c.Labels[labelPrefix+labelSince] = since
				c.Labels[labelPrefix+labelStage] = stage // the re-run's move did not take: still nothing queued
				f.SetAgents([]Agent{c})
				f.Tick()
			}
			f.Tick()
			if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "no next step") {
				t.Fatalf("nudge = %v", msgs)
			}
			stopAgain()
			rerun := len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) == 1
			if stage == "landing" {
				rerun = len(p.sent("c1")) == 2 && strings.Contains(p.sent("c1")[1], "re-running your landing")
			}
			if !rerun {
				t.Fatalf("step not re-run: %v %v", p.calls, p.sent("c1"))
			}
			stopAgain()
			if l := p.labels("c1"); l[labelStuck] != stuckStopped || l[labelStage] == "zach" {
				t.Errorf("not marked stuck in its column: %v", l)
			}
			sent := len(p.sent("c1"))
			stopAgain()
			if len(p.sent("c1")) != sent {
				t.Error("a card marked stuck was nudged again")
			}
		})
	}
}

func TestNudgesStartAfreshAfterTheCardMoves(t *testing.T) {
	since := ago(10 * time.Minute)
	c := card("c1", map[string]string{labelStage: "building", labelSince: since})
	c.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c)
	f.Tick() // nudge 1
	c = withLabels(c, p.labels("c1"))
	c.Labels[labelPrefix+labelStage] = "review"
	c.Labels[labelPrefix+labelSince] = iso(t0)
	c.LastActivityAt = t0
	f.SetAgents([]Agent{c})
	f.Tick() // moved on: forgets the nudge
	if r := readRecovery(withLabels(c, p.labels("c1"))); r.Nudges != 0 {
		t.Fatalf("nudges kept after a move: %+v", r)
	}
}

func TestReviewPending(t *testing.T) {
	running := reviewer("r1", "c1", gateSignoff)
	running.Status = "running"
	unapplied := reviewer("r2", "c1", gateSignoff)
	applied := reviewer("r3", "c1", gateSignoff)
	applied.Labels[labelPrefix+labelApplied] = "1"
	gone := reviewer("r4", "c1", gateSignoff)
	gone.ArchivedAt = t0.Add(-time.Minute) // archived without a verdict
	other := reviewer("r5", "c2", gateSignoff)
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

func TestReviewPromptTreatsZachsRulingsAsSettled(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Tier: "low"}
	if strings.Contains(ReviewPrompt(c, gateDesign, 7478, "unknown"), "already ruled") {
		t.Error("rule without a ruling")
	}
	c.Decided = true
	if !strings.Contains(ReviewPrompt(c, gateDesign, 7478, "unknown"), "already ruled") {
		t.Error("ruling not passed to the reviewer")
	}
}

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

// throwawayConditions are the conditions every plan prompt must carry.
var throwawayConditions = []string{
	"the build or its tests start and remove them",
	"they run on the build machine only",
	"they use no live data or credentials, not even a temporary copy",
	"bound to loopback or this host's tailnet IP, never a wildcard or public address",
	"no public exposure or persistent host configuration changes",
	"they send no messages and spend no money",
}

// Changes are whole: callers, tests, docs and README, no old path. Refactoring inside the area of the
// ask does not raise the tier; outside it, it is architecture Zach did not ask for.

func TestReviewPromptCarriesRules(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	for _, kind := range []string{gateRequest, gateDesign} {
		pr := ReviewPrompt(c, kind, 7478, "unknown")
		for _, want := range []string{"DIFFERENT agent", "VERDICT:", wholeChangeRule, "is a blocking finding"} {
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

func TestStoppedUnappliedReviewerGetsAppliedNotStuck(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateRequest, labelSource: "board", labelSince: ago(10 * time.Minute)}),
		reviewer("r1", "c1", gateRequest))
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nSIGN OFF: none\nVERDICT: approve")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != gateDesign {
		t.Errorf("labels = %v", l)
	}
}

func TestReviewerNotesReachBuilderWhole(t *testing.T) {
	f, p := newTestFactory(t)
	c := card("c1", map[string]string{labelStage: gateDesign, labelTier: "low", labelRoundsRequest: "1", labelRoundsDesign: "1"})
	f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", gateDesign, "changes", p)})
	f.ApplyVerdict(f.agent("r1"))
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "FIRST finding") || !strings.Contains(msgs[0], "LAST finding") ||
		strings.Contains(msgs[0], "VERDICT") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestMessageMidErrandStillFinishesDirect(t *testing.T) {
	for _, tc := range []struct {
		name, source, via, reply string
	}{
		{"board message, then TRACK: direct", "board", "", "Sent Zach the deck with the memos.\nTRACK: direct"},
		{"inbox card, email follow-up, no TIER line", "email", "email", "Sent Zach the fixed deck."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelSource: tc.source, labelSince: ago(10 * time.Minute)})
			c.Labels["gmail-thread"] = "t1"
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = gitStub(directOK, map[string]string{"rev-list origin/main..HEAD": ""})
			if _, err := f.Act("send", Body{ID: "c1", Message: "include the memos from this morning", Via: tc.via}); err != nil {
				t.Fatal(err)
			}
			if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != gateRequest {
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
	c := card("c1", map[string]string{labelStage: "building", labelSource: "email", labelSince: ago(10 * time.Minute)})
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
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != gateRequest {
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

func TestTickFinishesDirectCardQueuedForTheGates(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		labels      map[string]string
		review      bool
	}{
		{"TRACK: direct, no plan review", "Sent Zach the files.\nTRACK: direct", nil, false},
		{"no TRACK line", "Fixed it and committed.", nil, true},
		{"it already passed the request gate", "Sent Zach the files.\nTRACK: direct", map[string]string{labelRoundsRequest: "1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := map[string]string{labelStage: "building", labelSource: "email", labelAutoReview: gateRequest, labelSince: ago(10 * time.Minute)}
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

// Only a bare none means building has no side effects; its reason goes on the next line. Anything else on
// the line, even a reason that sounds safe, is a side effect, so no reading of free text can skip Zach.

// The planner, plan reviewer and plan arbiter all read the same exception: every condition, the naming duty
// on services a plan adds or runs itself, the short note for an existing suite, and that anything else stops.

func TestNewCardPromptDirectTrackPDWMutation(t *testing.T) {
	flat := strings.Join(strings.Fields(newCardPrompt), " ")
	for _, want := range []string{
		"Direct track: the request is an errand that changes nothing",
		"Anything else (code, config, infra, deletes, money, bulk sends, or messages to anyone but Zach sent without a PDW review) is the build track:",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("newCardPrompt lacks %q", want)
		}
	}
	if strings.Contains(flat, "messages to anyone but Zach) is the build track") {
		t.Error("newCardPrompt still has unqualified messages to anyone but Zach on the build track")
	}
}

// Changes are whole: callers, tests, docs and README, no old path. Refactoring inside the area of the
// ask does not raise the tier; outside it, it is architecture Zach did not ask for.

// Only a bare none means building has no side effects; its reason goes on the next line. Anything else on
// the line, even a reason that sounds safe, is a side effect, so no reading of free text can skip Zach.

// The planner, plan reviewer and plan arbiter all read the same exception: every condition, the naming duty
// on services a plan adds or runs itself, the short note for an existing suite, and that anything else stops.

func TestWorktreeNotTmpRule(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	planPrompt := strings.ReplaceAll(fmt.Sprintf(newCardPrompt, "req", "example/desk", "", "", ""), "<errands>", "desk")

	f, p := newTestFactory(t, card("c-dirty", map[string]string{labelStage: "building", labelSource: "board"}))
	dirtyCard, _ := f.card("c-dirty")
	if err := f.askToCommit(dirtyCard, " M a.go", gateRequest); err != nil {
		t.Fatal(err)
	}

	worktreePrompts := map[string]string{
		"newCardPrompt": planPrompt,
		"reworkMsg":     reworkGateMsg(gateDesign, 1, 3, "fix it"),
		"askToCommit":   p.sent("c-dirty")[0],
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
		"review code":       ReviewPrompt(c, gateDesign, 7478, "unknown"),
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

	codeRubric := ReviewPrompt(c, gateDesign, 7478, "unknown")
	for _, want := range []string{"must not use /tmp for work that belongs in the worktree", "t.TempDir()"} {
		if !strings.Contains(codeRubric, want) {
			t.Errorf("code rubric lacks %q:\n%s", want, codeRubric)
		}
	}
}

func TestNoLiveChangesBeforeCodeReview(t *testing.T) {
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	planPrompt := fmt.Sprintf(newCardPrompt, "req", "example/desk", "", "", "")
	for name, s := range map[string]string{
		"newCardPrompt": planPrompt,
	} {
		if !strings.Contains(s, "deploy steps") {
			t.Errorf("%s lacks deploy steps requirement: %q", name, s)
		}
	}

	f, p := newTestFactory(t, card("c-dirty", map[string]string{labelStage: "building", labelSource: "board"}))
	dirtyCard, _ := f.card("c-dirty")
	if err := f.askToCommit(dirtyCard, " M a.go", gateRequest); err != nil {
		t.Fatal(err)
	}

	noLivePrompts := map[string]string{
		"newCardPrompt": planPrompt,
		"reworkMsg":     reworkGateMsg(gateDesign, 1, 3, "fix it"),
		"askToCommit":   p.sent("c-dirty")[0],
		"code rubric":   ReviewPrompt(c, gateDesign, 7478, "unknown"),
	}
	for name, s := range noLivePrompts {
		if !strings.Contains(s, "live changes") {
			t.Errorf("%s lacks no-live-changes rule: %q", name, s)
		}
	}
	for name, s := range map[string]string{
		"reworkMsg code": noLivePrompts["reworkMsg"],
		"askToCommit":    noLivePrompts["askToCommit"],
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
