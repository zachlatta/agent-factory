package factory

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestDecisionWaitChain(t *testing.T) {
	decision := card("ccc0003", map[string]string{labelStage: "zach"})
	middle := card("bbb0002", map[string]string{labelStage: "landing", labelHeld: "ccc0003", labelWaitsOn: "ccc0003"})
	leaf := card("aaa0001", map[string]string{labelStage: "landing", labelHeld: "bbb0002", labelWaitsOn: "bbb0002"})
	agents := []Agent{leaf, middle, decision}
	cards := []Card{{ID: leaf.ID, Stage: "landing"}, {ID: middle.ID, Stage: "landing"}, {ID: decision.ID, Title: "Example task", Stage: "zach"}}
	decorateDecisionWaits(cards, agents)
	for _, c := range cards[:2] {
		if c.DecisionBlocker != decision.ID || c.Stage != "landing" || c.Ask != nil {
			t.Fatalf("held card: %+v", c)
		}
	}
	if cards[2].WaitingCount != 2 {
		t.Fatalf("count: %+v", cards[2])
	}
	for _, labels := range []map[string]string{
		{labelStage: "building"}, {labelStage: "landed"}, {labelStage: "zach", labelLandedVerified: "1"},
		{labelStage: "landing", labelWaitsOn: "aaa0001"},
	} {
		agents[2] = card(decision.ID, labels)
		decorateDecisionWaits(cards, agents)
		if cards[0].DecisionBlocker != "" || cards[2].WaitingCount != 0 {
			t.Fatalf("stale blocker: %+v", cards)
		}
	}
}

func TestDecisionWaitSafety(t *testing.T) {
	d := card("ddd0004", map[string]string{labelStage: "zach"})
	a := card("aaa0001", map[string]string{labelStage: "landing", labelHeld: d.ID, labelWaitsOn: d.ID})
	for _, kind := range []string{"direct", "missing", "dropped", "cycle", "remembered"} {
		t.Run(kind, func(t *testing.T) {
			target, held := d, a
			target.Labels = maps.Clone(d.Labels)
			held.Labels = maps.Clone(a.Labels)
			agents := []Agent{held, target}
			switch kind {
			case "missing":
				agents = agents[:1]
			case "dropped":
				agents[1].ArchivedAt = t0
			case "cycle":
				agents[1] = card(d.ID, map[string]string{labelStage: "landing", labelWaitsOn: a.ID})
			case "remembered":
				agents[0].Labels[labelPrefix+labelWaitsDone] = "1"
			}
			cards := []Card{{ID: a.ID, Stage: "landing", WaitsOn: d.ID}, {ID: d.ID, Stage: "zach", Ask: &Ask{}}}
			decorateDecisionWaits(cards, agents)
			want := 0
			if kind == "direct" {
				want = 1
			}
			if cards[1].WaitingCount != want {
				t.Fatalf("count = %d", cards[1].WaitingCount)
			}
			s := BuildSections(cards, t0, nil)
			if len(s.Working) != 1 || len(s.ForYou) != 1 || len(s.Stuck) != 0 || cards[0].Ask != nil {
				t.Fatalf("sections: %+v", s)
			}
			if want == 1 && (s.Working[0].Blocker != d.ID || !strings.Contains(s.ForYou[0].State, "1 card waits on this")) {
				t.Fatalf("missing explanation: %+v", s)
			}
		})
	}
}

func TestDecisionWaitBoardIsReadOnly(t *testing.T) {
	d := card("ddd0004", map[string]string{labelStage: "zach", labelAwaiting: "plan"})
	a := card("aaa0001", map[string]string{labelStage: "landing", labelHeld: d.ID, labelWaitsOn: d.ID})
	before := maps.Clone(a.Labels)
	cards := BuildBoard([]Agent{a, d}, nil, t0)
	var held Card
	for _, c := range cards {
		if c.ID == a.ID {
			held = c
		}
	}
	if held.Stage != "landing" || held.Ask != nil || held.DecisionBlocker != d.ID {
		t.Fatalf("held: %+v", held)
	}
	if !maps.Equal(a.Labels, before) {
		t.Fatal("board changed labels")
	}
	if mailable(held.Ask) {
		t.Fatal("held card generated a mailable ask")
	}
}

func TestDecisionWaitUsesHeldDependency(t *testing.T) {
	for _, heldNeedsInput := range []bool{true, false} {
		t.Run(fmt.Sprint(heldNeedsInput), func(t *testing.T) {
			heldStage, plannedStage := "building", "zach"
			if heldNeedsInput {
				heldStage, plannedStage = plannedStage, heldStage
			}
			held := card("bbb0002", map[string]string{labelStage: heldStage})
			planned := card("ccc0003", map[string]string{labelStage: plannedStage})
			middle := card("ddd0004", map[string]string{labelStage: "landing", labelHeld: held.ID, labelWaitsOn: planned.ID})
			leaf := card("aaa0001", map[string]string{labelStage: "landing", labelHeld: middle.ID, labelWaitsOn: planned.ID})
			agents := []Agent{leaf, middle, held, planned}
			cards := BuildBoard(agents, nil, t0)
			wantBlocker, wantCount := "", 0
			if heldNeedsInput {
				wantBlocker, wantCount = held.ID, 2
			}
			for _, c := range cards {
				if (c.ID == leaf.ID || c.ID == middle.ID) && c.DecisionBlocker != wantBlocker {
					t.Fatalf("blocker = %q, want %q", c.DecisionBlocker, wantBlocker)
				}
			}
			if h := workflowHealth(agents); h.HeldForInput != wantCount || h.LongestChain != 2 {
				t.Fatalf("health = %+v, want held for input %d and depth 2", h, wantCount)
			}
		})
	}
}
