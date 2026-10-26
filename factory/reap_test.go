package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// idleFor returns a with its last activity d before t0.
func idleFor(a Agent, d time.Duration) Agent {
	a.LastActivityAt = t0.Add(-d)
	return a
}

func TestReapArchivesDoneCardsAfterGrace(t *testing.T) {
	done := idleFor(card("done1", map[string]string{labelStage: "landed"}), 10*time.Minute)
	fresh := idleFor(card("done2", map[string]string{labelStage: "landed"}), time.Minute)
	busy := idleFor(card("done3", map[string]string{labelStage: "landed"}), time.Hour)
	busy.Status = "running"
	f, p := newTestFactory(t, done, fresh, busy)
	f.reap(t0)
	if !p.archived["done1"] {
		t.Errorf("a Done card idle 10 min should be archived: %v", p.verbs("done1"))
	}
	if p.called("archive", "done2") {
		t.Error("a Done card active a minute ago (a follow-up) should wait out the grace")
	}
	if p.called("archive", "done3") || p.called("stop", "done3") {
		t.Error("a running agent is never reaped")
	}
	if c, _ := f.card("done1"); c.ArchivedAt == "" {
		t.Error("the board should show the archive at once")
	}
}

func TestReapViaTick(t *testing.T) {
	f, p := newTestFactory(t, idleFor(card("done1", map[string]string{labelStage: "landed"}), time.Hour))
	f.Tick()
	if !p.archived["done1"] {
		t.Error("Tick should run the reap")
	}
}

func TestReapLeavesUnlabelledAgents(t *testing.T) {
	mine := idleFor(Agent{ID: "zach-session", Host: "host1", Status: "idle", Labels: map[string]string{},
		CreatedAt: t0.Add(-48 * time.Hour)}, 30*time.Hour)
	f, p := newTestFactory(t, mine)
	f.reap(t0)
	if len(p.verbs("zach-session")) != 0 {
		t.Errorf("agents the factory did not start are only counted: %v", p.verbs("zach-session"))
	}
}

func TestReapArchivesAppliedReviewers(t *testing.T) {
	r1 := reviewer("rev1", "card1", "code")
	r1.Labels[labelPrefix+labelApplied] = "1"
	r2 := reviewer("rev2", "card1", "code")
	r2.Labels[labelPrefix+labelApplied] = "1"
	r3 := reviewer("rev3", "gone1", "land") // verdict not applied yet: ApplyVerdict's job, whatever its target
	c := card("card1", map[string]string{labelStage: "review"})
	f, p := newTestFactory(t, c, r1, r2, r3)
	f.reap(t0)
	if !p.archived["rev1"] || !p.archived["rev2"] {
		t.Errorf("reviewers with nothing left to do should be archived: %v %v", p.verbs("rev1"), p.verbs("rev2"))
	}
	if p.called("archive", "rev3") {
		t.Error("a reviewer whose verdict is pending is left for ApplyVerdict")
	}
}

func TestDropStopsRunningReviewers(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "review"})
	r := reviewer("rev1", "card1", "code")
	r.Status = "running"
	f, p := newTestFactory(t, c, r)
	p.busy["rev1"] = 1 // like Paseo: archive refuses a running agent until it is stopped
	if _, err := f.Act("close", Body{ID: "card1"}); err != nil {
		t.Fatal(err)
	}
	if !p.called("stop", "rev1") || !p.archived["rev1"] {
		t.Errorf("dropping a card should stop and archive its running reviewer: %v", p.verbs("rev1"))
	}
}

func TestReapParksNeedsZachPastTTL(t *testing.T) {
	old := idleFor(card("zach1", map[string]string{labelStage: "zach", labelNote: "approve plan"}), 4*time.Hour)
	young := idleFor(card("zach2", map[string]string{labelStage: "zach"}), time.Hour)
	f, p := newTestFactory(t, old, young)
	f.reap(t0)
	if !p.archived["zach1"] || p.labels("zach1")[labelParked] == "" {
		t.Fatalf("a Needs Zach card idle past the TTL should be parked: %v %v", p.verbs("zach1"), p.labels("zach1"))
	}
	if v := p.verbs("zach1"); slices.Index(v, "update") > slices.Index(v, "archive") {
		t.Errorf("the parked label goes on before the archive: %v", v)
	}
	if p.called("archive", "zach2") {
		t.Error("under the TTL and the cap: stays live")
	}
	c, ok := f.card("zach1")
	if !ok || c.Stage != "zach" || c.ArchivedAt != "" || c.Parked == "" {
		t.Errorf("a parked card stays in Needs Zach, not archived on the board: %+v", c)
	}
	if !slices.Contains(c.Flags, parkedFlag) {
		t.Errorf("flags %v should include %q", c.Flags, parkedFlag)
	}
}

func TestReapKeepsNeedsZachWithPendingReviewer(t *testing.T) {
	c := idleFor(card("zach1", map[string]string{labelStage: "zach"}), 5*time.Hour)
	r := reviewer("rev1", "zach1", "code")
	r.Status = "running"
	r.LastActivityAt = t0
	f, p := newTestFactory(t, c, r)
	f.reap(t0)
	if p.called("archive", "zach1") {
		t.Error("a card with a reviewer still coming is not parked")
	}
}

func TestReapCapParksLongestIdleFirst(t *testing.T) {
	var agents []Agent
	for i := range 5 {
		// zach0 idle 50 min ... zach4 idle 90 min; zach5 idle 10 min
		agents = append(agents, idleFor(card(fmt.Sprintf("zach%d", i), map[string]string{labelStage: "zach"}),
			time.Duration(50+10*i)*time.Minute))
	}
	agents = append(agents, idleFor(card("zach5", map[string]string{labelStage: "zach"}), 10*time.Minute))
	f, p := newTestFactory(t, agents...)
	f.cfg.IdleCap = 3
	f.reap(t0)
	var parked []string
	for _, a := range agents {
		if p.archived[a.ID] {
			parked = append(parked, a.ID)
		}
	}
	if !slices.Equal(parked, []string{"zach2", "zach3", "zach4"}) {
		t.Errorf("6 idle agents over a cap of 3: park the 3 longest idle; parked %v", parked)
	}
	if p.archived["zach5"] {
		t.Error("cards idle under 30 min are never parked by the cap")
	}
}

func TestReapLimitPerTick(t *testing.T) {
	var agents []Agent
	for i := range reapPerTick + 5 {
		agents = append(agents, idleFor(card(fmt.Sprintf("d%02d", i), map[string]string{labelStage: "landed"}), time.Hour))
	}
	f, p := newTestFactory(t, agents...)
	f.reap(t0)
	if n := p.countCalls("archive", ""); n != reapPerTick {
		t.Errorf("archived %d in one Tick, want %d", n, reapPerTick)
	}
}

func TestSendResumesParkedCard(t *testing.T) {
	c := idleFor(card("zach1", map[string]string{labelStage: "zach"}), 4*time.Hour)
	f, p := newTestFactory(t, c)
	f.reap(t0)
	if !p.archived["zach1"] {
		t.Fatal("not parked")
	}
	if _, err := f.Act("send", Body{ID: "zach1", Message: "go ahead"}); err != nil {
		t.Fatal(err)
	}
	if p.archived["zach1"] {
		t.Error("the send should unarchive it (Paseo unarchives on send)")
	}
	if got := p.labels("zach1")[labelParked]; got != "" {
		t.Errorf("parked label should be cleared, got %q", got)
	}
	if c, _ := f.card("zach1"); c.Parked != "" || c.ArchivedAt != "" {
		t.Errorf("the board should show it live again: %+v", c)
	}
}

func TestReapClearsStaleParkedLabel(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "building", labelParked: ago(time.Hour)})
	c.Status = "running"
	f, p := newTestFactory(t, c)
	f.reap(t0)
	if got, ok := p.labels("card1")[labelParked]; !ok || got != "" {
		t.Errorf("a live agent with a parked label should have it cleared: %v", p.labels("card1"))
	}
}

func TestParkedTargetGetsVerdict(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "zach", labelParked: ago(time.Hour), labelPlanRounds: "1"})
	c.ArchivedAt = t0.Add(-time.Hour)
	r := reviewer("rev1", "card1", "code")
	f, p := newTestFactory(t, c, r)
	p.archived["card1"] = true
	p.logs["rev1"] = "Looks good.\nVERDICT: approve"
	if err := f.ApplyVerdict(f.agent("rev1")); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("card1")) == 0 {
		t.Errorf("a parked card is waiting, not dropped: the verdict should reach it: %v", p.verbs("card1"))
	}
}

func TestParkedCardGetsNoAutomaticMove(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "zach", labelParked: ago(time.Hour), labelSince: ago(2 * time.Hour)})
	c.ArchivedAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c)
	before := len(p.verbs("card1")) // the board reads its last message for the Needs Zach ask
	if err := f.tickAgent(f.agent("card1"), t0); err != nil {
		t.Fatal(err)
	}
	f.reap(t0)
	if len(p.verbs("card1")) != before {
		t.Errorf("the factory never wakes a parked card on its own: %v", p.verbs("card1"))
	}
}

func TestEmailAnswerFindsParkedCard(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "zach", labelParked: ago(time.Hour), labelZachThread: "thr1"})
	c.ArchivedAt = t0.Add(-time.Hour)
	f, _ := newTestFactory(t, c)
	a, err := f.waitingCard("thr1")
	if err != nil || a.ID != "card1" {
		t.Errorf("an email reply should find the parked card: %v %v", a.ID, err)
	}
}

func TestDeriveParked(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "zach", labelParked: ago(time.Hour)})
	c.ArchivedAt = t0.Add(-time.Hour)
	if got := Derive(c); got != "zach" {
		t.Errorf("Derive(parked) = %q, want zach", got)
	}
	c.Labels[labelPrefix+labelParked] = ""
	if got := Derive(c); got != "landed" {
		t.Errorf("Derive(archived) = %q, want landed", got)
	}
}

func TestLoadAgentsKeepsParkedPastWindow(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".paseo/agents/p1")
	os.MkdirAll(dir, 0o755)
	write := func(id string, labels map[string]string) {
		b, _ := json.Marshal(map[string]any{"id": id, "lastStatus": "idle", "archivedAt": t0.Add(-3 * 24 * time.Hour).Format(time.RFC3339),
			"createdAt": t0.Add(-4 * 24 * time.Hour).Format(time.RFC3339), "labels": labels})
		os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644)
	}
	write("parked1", map[string]string{labelPrefix + labelStage: "zach", labelPrefix + labelParked: "x"})
	write("old1", map[string]string{labelPrefix + labelStage: "landed"})
	agents, err := LoadAgents(home, "host1", 12*time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range agents {
		ids = append(ids, a.ID)
	}
	if !slices.Equal(ids, []string{"parked1"}) {
		t.Errorf("loaded %v, want only the parked card", ids)
	}
}

func TestHealthCountsAgents(t *testing.T) {
	c1 := idleFor(card("card1", map[string]string{labelStage: "zach"}), 2*time.Hour)
	r1 := reviewer("rev1", "card1", "code")
	c2 := card("card2", map[string]string{labelStage: "building"})
	c2.Status = "running"
	c3 := card("card3", map[string]string{labelStage: "zach", labelParked: ago(time.Hour)})
	c3.ArchivedAt = t0.Add(-time.Hour)
	other := Agent{ID: "other1", Host: "host1", Status: "idle", Labels: map[string]string{}}
	f, _ := newTestFactory(t, c1, r1, c2, c3, other)
	h := f.Health().Agents
	if h.Live != 4 || h.Running != 1 || h.Idle != 3 || h.Parked != 1 {
		t.Errorf("counts %+v", h)
	}
	if h.ByKind["card"] != 2 || h.ByKind["reviewer"] != 1 || h.ByKind["other"] != 1 {
		t.Errorf("by kind %v", h.ByKind)
	}
	if h.IdleCap != defaultIdleCap || h.IdleTTL != defaultIdleTTL.String() {
		t.Errorf("limits %v %v", h.IdleTTL, h.IdleCap)
	}
	var got *CardAgents
	for i := range h.Cards {
		if h.Cards[i].ID == "card1" {
			got = &h.Cards[i]
		}
	}
	if got == nil || got.Live != 2 || got.Idle != 2 || got.IdleMin != 120 {
		t.Errorf("card1 per-card count %+v", got)
	}
}
