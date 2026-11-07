package factory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseOverview(t *testing.T) {
	block := "DECISION: Land the retry fix?\nOVERVIEW: Retries a failed upload once. Risk: a double upload.\n" +
		"NEEDS FROM ZACH: Approve landing; it lands within a minute.\n"
	for _, tc := range []struct {
		name, in, rest string
		want           *Overview
	}{
		{"full block on top", block + "\nThe plan in full.\n\nTIER: low",
			"The plan in full.\n\nTIER: low",
			&Overview{Decision: "Land the retry fix?", Overview: "Retries a failed upload once. Risk: a double upload.", Needs: "Approve landing; it lands within a minute."}},
		{"bold keys", "**DECISION:** Pick a store\n**OVERVIEW:** Adds a cache.\n**NEEDS FROM ZACH:** Choose below.\n\nbody",
			"body", &Overview{Decision: "Pick a store", Overview: "Adds a cache.", Needs: "Choose below."}},
		{"short NEEDS key", "DECISION: Go?\nNEEDS: Say yes.\n\nbody", "body", &Overview{Decision: "Go?", Needs: "Say yes."}},
		{"two keys are enough", "OVERVIEW: Adds a cache.\nNEEDS FROM ZACH: Approve.\n", "",
			&Overview{Overview: "Adds a cache.", Needs: "Approve."}},
		{"one key is not a block", "DECISION: we chose SQLite.\n\nbody", "DECISION: we chose SQLite.\n\nbody", nil},
		{"an ordinary heading is not a block", "Decision: keep it.\nOverview: fine.\n", "Decision: keep it.\nOverview: fine.", nil},
		{"multi-line needs with buttons",
			"DECISION: Your call on the code\nOVERVIEW: Two points are open.\nNEEDS FROM ZACH: Tap a button:\n- \"Land as it is\": it lands\n- \"Drop it\": nothing lands\n\nPoints follow.",
			"Points follow.",
			&Overview{Decision: "Your call on the code", Overview: "Two points are open.", Needs: "Tap a button:\n- \"Land as it is\": it lands\n- \"Drop it\": nothing lands"}},
		{"the first block wins over a quoted one",
			block + "\nThe reviewer wrote:\n> DECISION: Reviewer's decision\nDECISION: Quoted decision\nOVERVIEW: quoted\n",
			"The reviewer wrote:\n> DECISION: Reviewer's decision\nDECISION: Quoted decision\nOVERVIEW: quoted",
			&Overview{Decision: "Land the retry fix?", Overview: "Retries a failed upload once. Risk: a double upload.", Needs: "Approve landing; it lands within a minute."}},
	} {
		rest, got := ParseOverview(tc.in)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: overview = %+v, want %+v", tc.name, got, tc.want)
		}
		if rest != tc.rest {
			t.Errorf("%s: rest = %q, want %q", tc.name, rest, tc.rest)
		}
	}
	long := strings.Repeat("word ", 300)
	if _, ov := ParseOverview("DECISION: d\nOVERVIEW: " + long + "\n"); ov == nil || ov.Overview != strings.TrimSpace(long) {
		t.Errorf("a long overview is cut: %+v", ov)
	}
}

func TestAskShowsTheAgentsOverview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	f, p := newTestFactory(t)
	storeLog(t, f, p, "c1", "[User] go\nDECISION: Re-run the build?\nOVERVIEW: The build hung on a flaky test.\n"+
		"NEEDS FROM ZACH: Tap Re-run.\n\nI tried twice.\n\nQUESTION: Which fix?\n- Skip the test (recommended)\n- Fix it\n")
	f.SetAgents([]Agent{c})
	got, _ := f.card("c1")
	ask := got.Ask
	want := &Overview{Decision: "Re-run the build?", Overview: "The build hung on a flaky test.", Needs: "Tap Re-run."}
	if ask == nil || fmt.Sprint(ask.Overview) != fmt.Sprint(want) || ask.Detail != "I tried twice." || len(ask.Questions) != 1 {
		t.Fatalf("ask = %+v", ask)
	}
	// cached with the detail
	f.SetAgents([]Agent{c})
	if got, _ = f.card("c1"); fmt.Sprint(got.Ask.Overview) != fmt.Sprint(want) {
		t.Errorf("cached overview = %+v", got.Ask.Overview)
	}
	// no block: today's view
	storeLog(t, f, p, "c1", "[User] go\nI tried twice.")
	c.LastActivityAt = c.LastActivityAt.Add(time.Minute)
	f.SetAgents([]Agent{c})
	if got, _ = f.card("c1"); got.Ask.Overview != nil || got.Ask.Detail != "I tried twice." {
		t.Errorf("no block: ask = %+v", got.Ask)
	}
}

// Every message after which an agent stops, and which can end up on a Needs Zach card, asks for the
// overview block.

// Every message after which an agent stops, and which can end up on a Needs Zach card, asks for the
// overview block.

// Every message after which an agent stops, and which can end up on a Needs Zach card, asks for the
// overview block.

// Every message after which an agent stops, and which can end up on a Needs Zach card, asks for the
// overview block.
func TestPromptsAskForTheOverview(t *testing.T) {
	f, p := newTestFactory(t)
	c := Card{ID: "c1", Title: "t", Repo: "example/desk"}
	prompts := map[string]string{
		"new card":      fmt.Sprintf(newCardPrompt, "req", "example/desk", "", wholeChangeRule, questionRule),
		"request gate":  ReviewPrompt(c, gateCheck, 7478, "unknown"),
		"design gate":   ReviewPrompt(c, gateCheck, 7478, "unknown"),
		"sign-off gate": ReviewPrompt(c, gateCheck, 7478, "unknown"),
		"rework":        reworkGateMsg([]string{"design yellow"}, 1, 3, "notes"),
		"preview":       readyMsg("you asked for it"),
		"land":          landMsg,
		"guarded land":  guardedLandMsg,
		"ui hold":       uiFeedbackHoldMsg("- the button is small"),
	}
	// the board's own messages: plan changes, commit before review
	for _, tc := range []struct {
		name   string
		labels map[string]string
		act    string
		body   Body
	}{} {
		f.SetAgents([]Agent{card(tc.name, tc.labels)})
		tc.body.ID = tc.name
		if _, err := f.Act(tc.act, tc.body); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		msgs := p.sent(tc.name)
		if len(msgs) == 0 {
			t.Fatalf("%s: nothing sent", tc.name)
		}
		prompts[tc.name] = msgs[len(msgs)-1]
	}
	ac := card("dirty", nil)
	f.SetAgents([]Agent{ac})
	if err := f.askToCommit(Card{ID: "dirty"}, " M a.go", gateCheck); err != nil {
		t.Fatal(err)
	}
	prompts["commit before review"] = p.sent("dirty")[0]
	for name, msg := range prompts {
		if !strings.Contains(msg, "DECISION:") && !strings.Contains(msg, overviewReminder) {
			t.Errorf("%s does not ask for the overview block", name)
		}
	}
	for _, name := range []string{"new card", "request gate", "design gate", "sign-off gate", "preview"} {
		if !strings.Contains(prompts[name], "NEEDS FROM ZACH:") {
			t.Errorf("%s does not teach the whole block", name)
		}
	}
	for _, kind := range []string{gateCheck, gateCheck, gateCheck} {
		if !strings.HasSuffix(ReviewPrompt(c, kind, 7478, "unknown"), "VERDICT: <approve|nits|changes|close>\n") {
			t.Errorf("%s review no longer ends with VERDICT", kind)
		}
	}
}
