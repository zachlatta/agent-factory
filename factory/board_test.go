package factory

import (
	"testing"
	"time"
)

func TestDerive(t *testing.T) {
	arch := time.Now()
	cases := []struct {
		name string
		a    Agent
		want string
	}{
		{"archived", Agent{ArchivedAt: arch, Labels: map[string]string{labelPrefix + labelStage: "building"}}, "landed"},
		{"label wins", Agent{Status: "running", Labels: map[string]string{labelPrefix + labelStage: "review"}}, "review"},
		{"running", Agent{Status: "running"}, "building"},
		{"unread finish", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished"}, "zach"},
		{"seen and idle", Agent{Status: "idle"}, "landed"},
		{"permission wait", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "permission"}, "zach"},
		{"closed session", Agent{Status: "closed", RequiresAttention: true, AttentionReason: "finished"}, "landed"},
		{"email agent answered by email", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished",
			Labels: map[string]string{"source": "mail-bot"}}, "landed"},
		{"scheduled run finished", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished",
			Labels: map[string]string{"paseo.schedule-id": "x"}}, "landed"},
		{"scheduled run blocked", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "permission",
			Labels: map[string]string{"paseo.schedule-id": "x"}}, "zach"},
	}
	for _, c := range cases {
		if got := Derive(c.a); got != c.want {
			t.Errorf("%s: Derive = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildBoardFoldsChildren(t *testing.T) {
	now := t0
	parent := Agent{ID: "p", Title: "parent", Status: "running", LastActivityAt: now, Labels: map[string]string{}}
	sub := Agent{ID: "s", Title: "sub", Status: "idle", Labels: map[string]string{"paseo.parent-agent-id": "p"}}
	rev := Agent{ID: "r", Title: "rev", Status: "running", Labels: map[string]string{labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewOf: "p", labelPrefix + labelReviewKind: "code"}}
	anchor := Agent{ID: "a", Labels: map[string]string{"role": "mail-anchor"}}
	sched := Agent{ID: "sc", Title: "Nightly check", Status: "running", LastActivityAt: now, Labels: map[string]string{"paseo.schedule-id": "x"}}
	cards := BuildBoard([]Agent{parent, sub, rev, anchor, sched}, nil, now)
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2 (parent, schedule)", len(cards))
	}
	p := cards[0]
	if p.ID != "p" || len(p.Children) != 2 || !p.Reviewing {
		t.Errorf("parent card = %+v", p)
	}
	if cards[1].Source != "schedule" || cards[1].Tier != "routine" {
		t.Errorf("schedule card = %+v", cards[1])
	}
}

func TestBuildBoardFlags(t *testing.T) {
	now := t0
	stalled := Agent{ID: "x", Status: "running", LastActivityAt: now.Add(-30 * time.Minute), Labels: map[string]string{}}
	done := Agent{ID: "y", Status: "idle", LastActivityAt: now, Labels: map[string]string{}}
	cards := BuildBoard([]Agent{stalled, done}, nil, now)
	if len(cards[0].Flags) == 0 || cards[0].Flags[0] != "stalled 30 min" {
		t.Errorf("stalled flags = %v", cards[0].Flags)
	}
	if len(cards[1].Flags) == 0 || cards[1].Flags[0] != "not archived" {
		t.Errorf("done flags = %v", cards[1].Flags)
	}
}

func TestBuildBoardIncludesWaitingIdeas(t *testing.T) {
	ideas := []Idea{{ID: "idea-1", Title: "faster tests", Status: "waiting", Repo: "example/desk", By: "scout", CreatedAt: t0},
		{ID: "idea-2", Title: "done", Status: "started"}}
	cards := BuildBoard(nil, ideas, t0)
	if len(cards) != 1 || cards[0].Stage != "idea" || cards[0].ID != "idea-1" {
		t.Errorf("cards = %+v", cards)
	}
}

func TestBuildBoardIdeaViews(t *testing.T) {
	ideas := []Idea{
		{ID: "idea-new", Title: "second", Summary: "S2", Status: "waiting", CreatedAt: t0.Add(time.Minute)},
		{ID: "idea-old", Title: "first", Summary: "S1", Why: "W1", DoneWhen: "D1", Request: "long brief",
			By: "scout", Status: "waiting", CreatedAt: t0},
		{ID: "idea-run", Title: "running", Summary: "S3", Status: "started", Card: "c1", CreatedAt: t0},
		{ID: "idea-gone", Title: "gone", Status: "dismissed", Card: "", CreatedAt: t0},
	}
	agents := []Agent{
		{ID: "c1", Title: "running", Status: "running", LastActivityAt: t0, Labels: map[string]string{labelPrefix + labelOrigin: "idea"}},
		{ID: "c2", Title: "other", Status: "running", LastActivityAt: t0, Labels: map[string]string{}},
	}
	byID := map[string]Card{}
	for _, c := range BuildBoard(agents, ideas, t0) {
		byID[c.ID] = c
	}
	old := byID["idea-old"].Idea
	if old == nil || old.Summary != "S1" || old.Why != "W1" || old.DoneWhen != "D1" || old.By != "scout" || old.Queue != 1 {
		t.Errorf("idea-old view = %+v", old)
	}
	if v := byID["idea-new"].Idea; v == nil || v.Queue != 2 {
		t.Errorf("idea-new view = %+v", v)
	}
	if v := byID["c1"].Idea; v == nil || v.ID != "idea-run" || v.Summary != "S3" || v.Status != "started" || v.Queue != 0 {
		t.Errorf("started card view = %+v", v)
	}
	if byID["c2"].Idea != nil {
		t.Errorf("card not from an idea got %+v", byID["c2"].Idea)
	}
}

func TestStalledFactoryCardShowsInNeedsZach(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "building"})
	a.Status, a.LastActivityAt = "running", t0.Add(-30*time.Minute)
	c := BuildBoard([]Agent{a}, nil, t0)[0]
	if c.Stage != "zach" || c.Ask == nil || actionIDs(c.Ask) != "stop,reply" {
		t.Errorf("card = %+v ask = %+v", c, c.Ask)
	}
	// an unlabeled stalled agent stays in Building with its flag
	u := Agent{ID: "u", Status: "running", LastActivityAt: t0.Add(-30 * time.Minute), Labels: map[string]string{}}
	if c := BuildBoard([]Agent{u}, nil, t0)[0]; c.Stage != "building" || c.Ask != nil {
		t.Errorf("unlabeled card = %+v", c)
	}
	// a running factory card with recent activity stays put
	a.LastActivityAt = t0.Add(-time.Minute)
	if c := BuildBoard([]Agent{a}, nil, t0)[0]; c.Stage != "building" {
		t.Errorf("active card stage = %q", c.Stage)
	}
}

func TestBuildBoardShowsScoutsInIdeas(t *testing.T) {
	running := Agent{ID: "s1", Title: "Scout: ideas for example/desk", Status: "running", LastActivityAt: t0,
		Labels: map[string]string{labelPrefix + labelRole: "scout"}}
	finished := Agent{ID: "s2", Status: "closed", ArchivedAt: t0, LastActivityAt: t0,
		Labels: map[string]string{labelPrefix + labelRole: "scout"}}
	cards := BuildBoard([]Agent{running, finished}, nil, t0)
	if c := cards[0]; c.Source != "scout" || c.Stage != "idea" || c.Note != "looking for ideas" {
		t.Errorf("running scout = %+v, want it in Ideas", c)
	}
	if c := cards[1]; c.Stage != "landed" {
		t.Errorf("archived scout stage = %q, want landed", c.Stage)
	}
}
