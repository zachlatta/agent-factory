package factory

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestArchiveStopsRunningAgentThenArchives(t *testing.T) {
	for _, tc := range []struct {
		action string
		body   Body
	}{
		{"archive", Body{ID: "c1"}},
		{"close", Body{ID: "c1"}},
		{"stage", Body{ID: "c1", Stage: "landed"}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "plan", labelSource: "board"})
			c.Status = "running"
			f, p := newTestFactory(t, c)
			p.busy["c1"] = 1 // Paseo refuses to archive a running agent
			if _, err := f.Act(tc.action, tc.body); err != nil {
				t.Fatal(err)
			}
			v := slices.DeleteFunc(p.verbs("c1"), func(s string) bool { return s == "update" })
			if !slices.Equal(v, []string{"inspect", "archive", "stop", "inspect", "archive", "inspect"}) {
				t.Errorf("paseo calls on c1 = %v, want inspected stop/retry and archive verification", v)
			}
			if a := f.agent("c1"); !a.archived() {
				t.Error("snapshot not marked archived")
			}
			if got, _ := f.card("c1"); got.ArchivedAt == "" {
				t.Error("board card not marked archived")
			}
		})
	}
}

func TestArchiveRetriesWhileAgentWindsDown(t *testing.T) {
	f, p := newTestFactory(t, card("c1", nil))
	p.busy["c1"] = 2
	if _, err := f.Act("archive", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if !p.archived["c1"] || !f.agent("c1").archived() {
		t.Errorf("not archived after retries: %v", p.calls)
	}

	f, p = newTestFactory(t, card("c1", nil))
	p.busy["c1"] = 100
	if _, err := f.Act("archive", Body{ID: "c1"}); err == nil || !strings.Contains(err.Error(), "currently running") {
		t.Errorf("err = %v, want currently running", err)
	}
	if f.agent("c1").archived() {
		t.Error("snapshot marked archived after a failed archive")
	}
}

func TestTickAfterArchiveStartsNoReview(t *testing.T) {
	// idle, acted since its last move: without the archive this card gets a plan review
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("archive", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Errorf("review started for an archived card: %v", runs)
	}
}

func TestArchiveMarkSurvivesStaleRefresh(t *testing.T) {
	c := card("c1", nil)
	f, _ := newTestFactory(t, c)
	if _, err := f.Act("archive", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	stale := []Agent{c}
	f.SetAgents(stale) // a Refresh that read the agent file before Paseo wrote the archive
	if !f.agent("c1").archived() {
		t.Error("stale refresh undid the archive mark")
	}
	if stale[0].archived() {
		t.Error("SetAgents wrote into the caller's slice")
	}
	f.Now = func() time.Time { return t0.Add(11 * time.Minute) }
	f.SetAgents(stale)
	if f.agent("c1").archived() {
		t.Error("archive mark did not expire")
	}
}

func TestReplyKeepsCardWhereTheAnswerBelongs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     map[string]string
		stage      string // "" = no stage label written
		autoReview string
	}{
		{"landing stays landing", map[string]string{labelStage: "landing", labelNote: "auto-landing (low)", labelVerdict: "code approve", labelPlanRounds: "1"}, "landing", ""},
		{"plan at rework cap goes back to plan", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached", labelVerdict: "plan changes", labelPlanRounds: "3"}, "plan", ""},
		{"after plan passed, code review queued", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached", labelVerdict: "code changes", labelPlanRounds: "1", labelCodeRounds: "3"}, "building", "code"},
		{"unlabeled done card, code review queued", map[string]string{labelStage: "landed", labelSource: "board", labelNote: "done in one pass"}, "building", "code"},
		{"direct card done, back to plan", map[string]string{labelStage: "landed", labelSource: "board", labelNote: "done in one pass", labelTrack: "direct"}, "plan", ""},
		{"direct card not on main, back to plan", map[string]string{labelStage: "zach", labelSource: "email", labelNote: "landing not verified", labelTrack: "direct"}, "plan", ""},
		{"board errand on its first pass stays in plan", map[string]string{labelStage: "plan", labelSource: "board"}, "plan", ""},
		{"inbox errand on its first pass stays in plan", map[string]string{labelStage: "plan", labelSource: "email"}, "plan", ""},
		{"direct card whose plan passed, code review queued", map[string]string{labelStage: "landed", labelSource: "email", labelTrack: "direct", labelPlanRounds: "1"}, "building", "code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t, card("c1", tc.labels))
			if _, err := f.Act("send", Body{ID: "c1", Message: "what do you need from me?"}); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if l[labelStage] != tc.stage || l[labelAutoReview] != tc.autoReview || l[labelSince] == "" {
				t.Errorf("labels = %v", l)
			}
			if len(p.sent("c1")) != 1 {
				t.Errorf("sent = %v", p.sent("c1"))
			}
		})
	}
}

func TestReplyToUnlabeledCardSetsNoStage(t *testing.T) {
	a := Agent{ID: "c1", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}}
	f, p := newTestFactory(t, a)
	if _, err := f.Act("send", Body{ID: "c1", Message: "thanks, one more thing"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); len(l) != 0 {
		t.Errorf("labels = %v", l)
	}
}

func TestReplyThenTickFinishesLanding(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelPlanRounds: "1"})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Act("send", Body{ID: "c1", Message: "why is this under landing?"})
	// the agent answers and stops a minute later
	c.Labels = map[string]string{}
	for k, v := range p.labels("c1") {
		c.Labels[labelPrefix+k] = v
	}
	c.Labels[labelPrefix+labelStage] = "landing"
	f.Now = func() time.Time { return t0.Add(2 * time.Minute) }
	c.LastActivityAt = t0.Add(time.Minute)
	f.SetAgents([]Agent{c})
	f.Git = func(context.Context, string, ...string) error { return nil }
	p.logs["c1"] = "Landed.\nREMAINING: none"
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Errorf("labels = %v", l)
	}
}

func TestApproveUnpassedPlanBuildsInsteadOfLanding(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
		labelVerdict: "plan changes", labelPlanRounds: "3", labelTier: "architecture"}))
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelZachPlan] != "approved" {
		t.Errorf("labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Zach approved your plan") || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestRerunRestartsTheStuckStep(t *testing.T) {
	for _, tc := range []struct{ from, kind, stage string }{
		{"plan", "plan", "plan-review"}, {"plan-review", "plan", "plan-review"},
		{"building", "code", "review"}, {"review", "code", "review"}, {"landing", "", "landing"},
	} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: tc.from}))
		if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
			t.Fatalf("%s: %v", tc.from, err)
		}
		runs := p.runsWithLabel(labelArg(labelReviewOf, "c1"))
		if tc.kind == "" {
			if len(runs) != 0 || len(p.sent("c1")) != 1 || !strings.Contains(p.sent("c1")[0], "Land it now") {
				t.Errorf("%s: calls = %v", tc.from, p.calls)
			}
		} else if len(runs) != 1 || !contains(runs[0], labelArg(labelReviewKind, tc.kind)) {
			t.Errorf("%s: runs = %v", tc.from, runs)
		}
		if l := p.labels("c1"); l[labelStage] != tc.stage {
			t.Errorf("%s: labels = %v", tc.from, l)
		}
	}
}

func TestClosingVerifiedLandingPreservesDependency(t *testing.T) {
	c := dep("zach", false)
	c.Labels[labelPrefix+labelLandedVerified] = "1"
	f, p := newTestFactory(t, c)
	if _, err := f.Act("close", Body{ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if !p.called("archive", c.ID) {
		t.Fatal("card not archived")
	}
	// Apply only the labels actually written by the close action.
	for k, v := range p.labels(c.ID) {
		c.Labels[labelPrefix+k] = v
	}
	c.ArchivedAt = t0
	if c.L(labelNote) != "closed by Zach" || depOf([]Agent{c}, short(c.ID)) != depLanded {
		t.Errorf("closed verified landing no longer satisfies dependency: %v", c.Labels)
	}
}
