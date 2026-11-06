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
			c := card("c1", map[string]string{labelStage: "building", labelSource: "board"})
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
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
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
	c := card("c1", map[string]string{labelStage: "landing", labelRoundsRequest: "1"})
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
	storeLog(t, f, p, "c1", "Landed.\nREMAINING: none")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Errorf("labels = %v", l)
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
