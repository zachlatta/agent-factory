package factory

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func sectionIDs(ls []SectionLine) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}

// Sections sorts the board into what waits on Zach, what is working, what is stuck and what shipped today.
func TestBuildSections(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, loc)
	at := func(h, m int) string { return time.Date(2026, 10, 27, h, m, 0, 0, loc).Format(time.RFC3339) }
	yesterday := time.Date(2026, 10, 26, 23, 30, 0, 0, loc).Format(time.RFC3339)
	ready := &Ask{Kind: askReady, Title: "Try it, then approve landing?", Overview: &Overview{Decision: "Ship the phone board?"}}
	problem := &Ask{Kind: askProblem, Title: "Landing didn't reach main"}
	done := func(id, title, landed string) Card {
		return Card{ID: id, Title: title, Stage: "landed", Source: "board",
			Timing: &TimingView{LandedAt: landed, Stages: []StageSpan{{Stage: "landed", Start: landed}}}}
	}
	cards := []Card{
		{ID: "z1", Title: "Phone board", Stage: "zach", Source: "board", Ask: ready},
		{ID: "z2", Title: "Broken landing", Stage: "zach", Source: "board", Ask: problem},
		{ID: "w1", Title: "Retries", Stage: "building", Source: "board"},
		{ID: "w2", Title: "Hung", Stage: "review", Source: "board", Flags: []string{"stalled 40 min"}},
		{ID: "w3", Title: "Follower", Stage: "landing", Source: "board", WaitsOn: "w1abcde", Flags: []string{"waits on w1abcde"}},
		{ID: "w1abcdef", Title: "Retries 2", Stage: "plan", Source: "board"},
		done("d1", "Shipped this morning", at(9, 15)),
		done("d2", "Shipped last night", yesterday),
		{ID: "d3", Title: "Dropped", Stage: "landed", Source: "board", Note: "closed by Zach",
			Timing: &TimingView{Stages: []StageSpan{{Stage: "landed", Start: at(10, 0)}}}},
		{ID: "e1", Title: "Email errand", Stage: "landed", Source: "email",
			Timing: &TimingView{RepliedAt: at(11, 0), Stages: []StageSpan{{Stage: "landed", Start: at(11, 0)}}}},
		{ID: "e2", Title: "Unanswered errand", Stage: "landed", Source: "email",
			Timing: &TimingView{NoReply: true, RequestedAt: at(8, 0), Stages: []StageSpan{{Stage: "landed", Start: at(12, 0)}}}},
		{ID: "r1", Title: "Hourly feed", Stage: "landed", Source: "schedule", Tier: "routine", CreatedAt: at(12, 0)},
		{ID: "r2", Title: "Hourly feed", Stage: "building", Source: "schedule", Tier: "routine", CreatedAt: at(17, 0)},
		{ID: "i1", Title: "An idea", Stage: "idea", Source: "idea"},
	}
	s := BuildSections(cards, now, loc)
	if got := sectionIDs(s.ForYou); !slices.Equal(got, []string{"z1"}) {
		t.Errorf("forYou = %v", got)
	}
	if s.ForYou[0].State != "Ship the phone board?" {
		t.Errorf("forYou state = %q, want the agent's decision line", s.ForYou[0].State)
	}
	if got := sectionIDs(s.Stuck); !slices.Equal(got, []string{"z2", "w2"}) {
		t.Errorf("stuck = %v", got)
	}
	if got := sectionIDs(s.Working); !slices.Equal(got, []string{"w1", "w3", "w1abcdef"}) {
		t.Errorf("working = %v", got)
	}
	states := map[string]string{}
	for _, l := range append(s.Working, s.Stuck...) {
		states[l.ID] = l.State
	}
	for id, want := range map[string]string{"w1": "building", "w3": "waiting on Retries 2", "w1abcdef": "planning",
		"w2": "no activity for 40 min", "z2": "Landing didn't reach main"} {
		if states[id] != want {
			t.Errorf("%s state = %q, want %q", id, states[id], want)
		}
	}
	if got := sectionIDs(s.Shipped); !slices.Equal(got, []string{"e1", "d1"}) {
		t.Errorf("shipped today = %v, want newest first, no drops, nothing from yesterday", got)
	}
	if got := sectionIDs(s.NoAnswer); !slices.Equal(got, []string{"e2"}) {
		t.Errorf("noAnswer = %v", got)
	}
	if got := sectionIDs(s.Routine); !slices.Equal(got, []string{"r2", "r1"}) {
		t.Errorf("routine = %v", got)
	}
}

// "Today" is the local day in Zach's time zone, across midnight and the daylight saving change.
func TestSectionsToday(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	for _, c := range []struct {
		now, at time.Time
		want    bool
	}{
		{time.Date(2026, 11, 1, 18, 0, 0, 0, loc), time.Date(2026, 11, 1, 0, 30, 0, 0, loc), true}, // DST ends at 2 AM
		{time.Date(2026, 11, 1, 18, 0, 0, 0, loc), time.Date(2026, 10, 31, 23, 59, 0, 0, loc), false},
		// 03:50 UTC on the 27th is 23:50 ET on the 26th
		{time.Date(2026, 10, 27, 0, 5, 0, 0, loc), time.Date(2026, 10, 27, 3, 50, 0, 0, time.UTC), false},
		{time.Date(2026, 10, 27, 23, 55, 0, 0, loc), time.Date(2026, 10, 28, 3, 50, 0, 0, time.UTC), true},
	} {
		if got := sameDay(c.at, c.now, loc); got != c.want {
			t.Errorf("sameDay(%v, %v) = %v, want %v", c.at, c.now, got, c.want)
		}
	}
}

// Other parts of the factory add requests that never got an answer (a watchdog's flags, say).
func TestSectionsAddNoAnswer(t *testing.T) {
	f, _ := newTestFactory(t)
	f.AddNoAnswer(func(time.Time) []SectionLine {
		return []SectionLine{{Title: "Order batteries", State: "no answer yet", At: t0.Format(time.RFC3339)}}
	})
	s := f.sections()
	if len(s.NoAnswer) != 1 || s.NoAnswer[0].Title != "Order batteries" {
		t.Errorf("noAnswer = %+v", s.NoAnswer)
	}
}

// The board API carries the sections, so the page and the digest read the same lists.
func TestBoardAPISections(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1)}),
		card("c2", map[string]string{labelStage: "building"}))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var body struct{ Sections Sections }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := sectionIDs(body.Sections.ForYou); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("forYou = %v", got)
	}
	if got := sectionIDs(body.Sections.Working); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("working = %v", got)
	}
}

// A card that automatic recovery marked stuck stays in its column but is listed under Stuck, with why.
func TestSectionsListRecoveryStuckCards(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, loc)
	cards := []Card{
		{ID: "s1", Title: "Retries", Stage: "building", Source: "board", Flags: []string{stuckFlagPrefix + stuckAgentError}},
		{ID: "w1", Title: "Fine", Stage: "building", Source: "board"},
	}
	s := BuildSections(cards, now, loc)
	if len(s.Stuck) != 1 || s.Stuck[0].ID != "s1" || s.Stuck[0].State != "stuck: "+stuckAgentError {
		t.Errorf("stuck = %+v", s.Stuck)
	}
	if len(s.Working) != 1 || s.Working[0].ID != "w1" {
		t.Errorf("working = %+v", s.Working)
	}
}
