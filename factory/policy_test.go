package factory

import (
	"context"
	"errors"
	"fmt"
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
	p.logs["r1"] = "[Assistant] plan is sound\nTIER: low\nVERDICT: approve"
	p.logs["c1"] = "[Assistant] Plan ... TIER: low"
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
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
	p.logs["c1"] = "plan: replace the storage layer\nTIER: architecture"
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
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach approved your plan") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestCodeApprovedAutoLandsByTier(t *testing.T) {
	for _, tier := range []string{"low", "risky"} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: tier, labelPlanRounds: "1"}),
			reviewer("r1", "c1", "code"))
		p.logs["r1"] = "TIER: " + tier + "\nVERDICT: nits"
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

func TestArchitectureApprovedByZachAutoLands(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelZachPlan: "approved", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "TIER: architecture\nVERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "landing" {
		t.Errorf("labels = %v", l)
	}
}

func TestCodeReviewFindsArchitectureOnLowCard(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "it swapped the database\nTIER: architecture\nVERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelTier] != "architecture" {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("must not land: %v", p.sent("c1"))
	}
}

func TestUntieredCardGoesToZachAfterReview(t *testing.T) {
	// an existing session Zach sent to review by hand: no plan review, no tier
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review"}), reviewer("r1", "c1", "code"))
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.Contains(l[labelNote], "approve to land") || l[labelTier] != "low" {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("must not land: %v", p.sent("c1"))
	}
}

func TestHandReviewedCardWaitsForZachAfterRework(t *testing.T) {
	// a hand-sent card whose first code review asked for changes now has a tier label, but no plan review
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelCodeRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.Contains(l[labelNote], "approve to land") {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("must not land: %v", p.sent("c1"))
	}
}

func TestChangesGoBackUntilReworkCap(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "[Assistant] - test is missing\nVERDICT: changes"
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
	p.logs["r1"] = "VERDICT: changes"
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != "rework cap reached" {
		t.Errorf("at cap: labels = %v", l)
	}
}

func TestMissingVerdictOffersRerun(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low"}), reviewer("r1", "c1", "code"))
	p.logs["r1"] = "I ran out of time"
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != noVerdictNote || l[labelStuckFrom] != "review" || l[labelVerdict] != "code none" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("no brief without a question: %v", msgs)
	}
	f.SetAgents([]Agent{withLabels(f.agent("c1"), l)})
	if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Errorf("rerun started %d reviews", len(runs))
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
	p.logs["r1"] = "VERDICT: approve"
	f.Tick()
	// r2's verdict is not read again (the idle reap archives it); r3 is too young to apply
	if !p.called("archive", "r1") || p.called("logs", "r2") || p.called("archive", "r3") {
		t.Errorf("calls = %v", p.calls)
	}
}

func TestTickStartsPlanReviewWhenPlannerStops(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
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
			p.logs["c1"] = "Emailed Zach the answer and landed the work item.\nTRACK: direct"
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
			p.logs["c1"] = "Landed.\nREMAINING: none"
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
		for _, want := range []string{"DIFFERENT agent", "TIER:", "VERDICT:", "whole app", "Read-only"} {
			if !strings.Contains(pr, want) {
				t.Errorf("%s prompt lacks %q", kind, want)
			}
		}
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
		p.logs["r1"] = "[Assistant] fix it\nTIER: risky\nVERDICT: changes"
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
	p.logs["r1"] = "VERDICT: approve"
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
	p.logs["c1"] = "[User] You are the Fellow...\nTRACK: direct\nAnything else is the build track\n" +
		"[Thought] planning\nPlan: fix the board.\nTIER: risky"
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
		p.logs["c1"] = "Answered Zach and landed the work item.\nTRACK: direct"
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
		{"code review, no reviewer at all", map[string]string{labelStage: "review", labelSince: since}, nil, true},
		{"code review, reviewer applied", map[string]string{labelStage: "review", labelSince: since},
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
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
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

// longReview is a reviewer session whose final reply spans two text blocks and many lines; its
// `paseo logs --tail 6` holds only the end.
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
	p.logs[id] = "- detail 19\n- LAST finding: no test\nTIER: low\nVERDICT: " + verdict
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

// briefFor applies a final verdict to a card at the edge of the rework cap and returns the labels, the
// message sent to the builder, and the Ask the card then shows.
func briefFor(t *testing.T, labels map[string]string, kind, verdict string) (map[string]string, string, *Ask) {
	t.Helper()
	f, p := newTestFactory(t)
	c := card("c1", labels)
	f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", kind, verdict, p)})
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	msgs := p.sent("c1")
	if len(msgs) != 1 {
		t.Fatalf("sent = %v", msgs)
	}
	return l, msgs[0], BuildBoard([]Agent{withLabels(c, l)}, nil, t0)[0].Ask
}

func TestReworkCapAsksBuilderForBrief(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		labels     map[string]string
		asIs       string
	}{
		{"code", "code", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "2"}, btnLandAsIs},
		{"plan", "plan", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: "2"}, btnBuildAsIs},
	} {
		l, msg, ask := briefFor(t, tc.labels, tc.kind, "changes")
		if l[labelStage] != "zach" || l[labelNote] != "rework cap reached" || l[labelTold] != "" {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
		for _, want := range []string{"FIRST finding", "LAST finding", "not seen this final review", "Do not build"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: brief lacks %q:\n%s", tc.name, want, msg)
			}
		}
		if strings.Contains(msg, "word for word") {
			t.Errorf("%s: no reviewer question to quote at the cap", tc.name)
		}
		// the brief names every button the card shows, by its exact label
		if ask.From != "builder" || actionIDs(ask) != "recommend,reply,approve,close" || ask.Actions[2].Label != tc.asIs {
			t.Errorf("%s: ask = %+v", tc.name, ask)
		}
		for _, x := range ask.Actions {
			if !strings.Contains(msg, `"`+x.Label+`"`) {
				t.Errorf("%s: brief lacks button %q", tc.name, x.Label)
			}
		}
	}
}

func TestEscalationAsksBuilderForBriefQuotingReviewer(t *testing.T) {
	l, msg, ask := briefFor(t, map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}, "code", "escalate")
	if l[labelStage] != "zach" || l[labelNote] != "reviewer escalated" || l[labelVerdict] != "code escalate" {
		t.Errorf("labels = %v", l)
	}
	if !strings.Contains(msg, "quoted word for word") || !strings.Contains(msg, "FIRST finding") {
		t.Errorf("brief = %s", msg)
	}
	if actionIDs(ask) != "reply,recommend,close" {
		t.Errorf("ask = %+v", ask)
	}
	for _, x := range ask.Actions {
		if !strings.Contains(msg, `"`+x.Label+`"`) {
			t.Errorf("brief lacks button %q", x.Label)
		}
	}
	if strings.Contains(msg, btnLandAsIs) {
		t.Error("escalated card has no land-as-is button")
	}
}

func TestRecommendAppliesTheBrief(t *testing.T) {
	// plan at the cap: revise to the recommendations, build, code review next
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
		labelVerdict: "plan changes", labelPlanRounds: "3", labelTier: "risky"}))
	if _, err := f.Act("recommend", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelZachPlan] != "approved" || l[labelDecided] != "1" {
		t.Errorf("plan labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Revise the plan to match them") ||
		!strings.Contains(msgs[0], "do NOT land") {
		t.Errorf("plan sent = %v", msgs)
	}

	// code at the cap: apply, then a code review, not landing
	code := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
		labelVerdict: "code changes", labelPlanRounds: "1", labelCodeRounds: "3", labelTier: "low"})
	f, p = newTestFactory(t, code)
	if _, err := f.Act("recommend", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l = p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelDecided] != "1" {
		t.Errorf("code labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Apply them") || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("code sent = %v", msgs)
	}

	// that review asks for changes again: straight back to Zach with a new brief, no loop
	_, msg, _ := briefFor(t, map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "3",
		labelDecided: "1"}, "code", "changes")
	if !strings.Contains(msg, "Write a brief for Zach") {
		t.Errorf("after recommend: %s", msg)
	}
}

func TestApproveOnCodeAtCapLandsAsIs(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
		labelVerdict: "code changes", labelPlanRounds: "1", labelCodeRounds: "3"}))
	f.Act("approve", Body{ID: "c1"})
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelDecided] != "" {
		t.Errorf("labels = %v", l)
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

	f, p := newTestFactory(t,
		card("c-plan", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
			labelVerdict: "plan changes", labelPlanRounds: "3", labelTier: "risky"}),
		card("c-code", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
			labelVerdict: "code changes", labelPlanRounds: "1", labelCodeRounds: "3", labelTier: "low"}),
		card("c-dirty", map[string]string{labelStage: "building", labelSource: "board"}))
	if _, err := f.Act("recommend", Body{ID: "c-plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("recommend", Body{ID: "c-code"}); err != nil {
		t.Fatal(err)
	}
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
		"recommend plan": p.sent("c-plan")[0],
		"recommend code": p.sent("c-code")[0],
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
		"recommend plan": noLivePrompts["recommend plan"],
		"recommend code": noLivePrompts["recommend code"],
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
		"SMALL FIX: add confirmAnswer to factoryMails\nTIER: risky\nVERDICT: changes"

	// Rounds 1 and 2 still go back to plan rework even with SMALL FIX.
	for _, prev := range []string{"0", "1"} {
		f, p := newTestFactory(t,
			card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: prev}),
			reviewer("r1", "c1", "plan"))
		p.logs["r1"] = reviewText
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
	p.logs["r1"] = reviewText
	p.logs["c1"] = "[Assistant] Plan...\nWaits on: abc1234\nTIER: risky"
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

	// Code review at the rework cap still stops in Needs Zach even if SMALL FIX is present.
	f, p = newTestFactory(t,
		card("c-code", map[string]string{labelStage: "review", labelSource: "board", labelTier: "risky", labelPlanRounds: "1", labelCodeRounds: "2"}),
		reviewer("r-code", "c-code", "code"))
	p.logs["r-code"] = reviewText
	if err := f.ApplyVerdict(f.agent("r-code")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c-code"); l[labelStage] != "zach" || l[labelNote] != "rework cap reached" {
		t.Errorf("code review at cap should still stop in zach: %v", l)
	}
}

func TestPlanReworkCapSmallFixOnArchitectureWaitsForZachPlanApproval(t *testing.T) {
	archReview := "- missing Ordering line for overlapping card\n" +
		"SMALL FIX: add Ordering line for overlapping card\nTIER: architecture\nVERDICT: changes"
	base := card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelTier: "architecture", labelPlanRounds: "2"})
	f, p := newTestFactory(t, base, reviewer("r1", "c1", "plan"))
	p.logs["r1"] = archReview
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelAwaiting] != "plan" || l[labelVerdict] != "plan changes" ||
		l[labelNote] != "architecture: approve the plan to build" || l[labelPlanFix] != "add Ordering line for overlapping card" {
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


