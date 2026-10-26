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
			&Overview{"Land the retry fix?", "Retries a failed upload once. Risk: a double upload.", "Approve landing; it lands within a minute."}},
		{"bold keys", "**DECISION:** Pick a store\n**OVERVIEW:** Adds a cache.\n**NEEDS FROM ZACH:** Choose below.\n\nbody",
			"body", &Overview{"Pick a store", "Adds a cache.", "Choose below."}},
		{"short NEEDS key", "DECISION: Go?\nNEEDS: Say yes.\n\nbody", "body", &Overview{Decision: "Go?", Needs: "Say yes."}},
		{"two keys are enough", "OVERVIEW: Adds a cache.\nNEEDS FROM ZACH: Approve.\n", "",
			&Overview{Overview: "Adds a cache.", Needs: "Approve."}},
		{"one key is not a block", "DECISION: we chose SQLite.\n\nbody", "DECISION: we chose SQLite.\n\nbody", nil},
		{"an ordinary heading is not a block", "Decision: keep it.\nOverview: fine.\n", "Decision: keep it.\nOverview: fine.", nil},
		{"multi-line needs with buttons",
			"DECISION: Your call on the code\nOVERVIEW: Two points are open.\nNEEDS FROM ZACH: Tap a button:\n- \"Land as it is\": it lands\n- \"Drop it\": nothing lands\n\nPoints follow.",
			"Points follow.",
			&Overview{"Your call on the code", "Two points are open.", "Tap a button:\n- \"Land as it is\": it lands\n- \"Drop it\": nothing lands"}},
		{"the first block wins over a quoted one",
			block + "\nThe reviewer wrote:\n> DECISION: Reviewer's decision\nDECISION: Quoted decision\nOVERVIEW: quoted\n",
			"The reviewer wrote:\n> DECISION: Reviewer's decision\nDECISION: Quoted decision\nOVERVIEW: quoted",
			&Overview{"Land the retry fix?", "Retries a failed upload once. Risk: a double upload.", "Approve landing; it lands within a minute."}},
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
	p.logs["c1"] = "[User] go\nDECISION: Re-run the build?\nOVERVIEW: The build hung on a flaky test.\n" +
		"NEEDS FROM ZACH: Tap Re-run.\n\nI tried twice.\n\nQUESTION: Which fix?\n- Skip the test (recommended)\n- Fix it\n"
	f.SetAgents([]Agent{c})
	got, _ := f.card("c1")
	ask := got.Ask
	want := &Overview{"Re-run the build?", "The build hung on a flaky test.", "Tap Re-run."}
	if ask == nil || fmt.Sprint(ask.Overview) != fmt.Sprint(want) || ask.Detail != "I tried twice." || len(ask.Questions) != 1 {
		t.Fatalf("ask = %+v", ask)
	}
	// cached with the detail
	f.SetAgents([]Agent{c})
	if got, _ = f.card("c1"); fmt.Sprint(got.Ask.Overview) != fmt.Sprint(want) {
		t.Errorf("cached overview = %+v", got.Ask.Overview)
	}
	// no block: today's view
	p.logs["c1"] = "[User] go\nI tried twice."
	c.LastActivityAt = c.LastActivityAt.Add(time.Minute)
	f.SetAgents([]Agent{c})
	if got, _ = f.card("c1"); got.Ask.Overview != nil || got.Ask.Detail != "I tried twice." {
		t.Errorf("no block: ask = %+v", got.Ask)
	}
}

func TestEmailLeadsWithTheOverview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	f, p, m := mailFactory(t, c)
	p.logs["c1"] = "[User] go\nDECISION: Re-run the build?\nOVERVIEW: The build hung.\nNEEDS FROM ZACH: Tap Re-run.\n\nI tried twice."
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	text, html := sends[0].Args["text"].(string), sends[0].Args["html"].(string)
	if !strings.HasPrefix(text, "Re-run the build?\nThe build hung.\nNeeds from you: Tap Re-run.\n") {
		t.Errorf("text does not lead with the overview:\n%s", text)
	}
	if strings.Contains(text, "DECISION:") || !strings.Contains(text, "I tried twice.") {
		t.Errorf("text = %s", text)
	}
	if !strings.Contains(html, "Re-run the build?") || !strings.Contains(html, "Needs from you") {
		t.Errorf("html lacks the overview: %s", html)
	}
}

// Every message after which an agent stops, and which can end up on a Needs Zach card, asks for the
// overview block.
func TestPromptsAskForTheOverview(t *testing.T) {
	f, p := newTestFactory(t)
	c := Card{ID: "c1", Title: "t", Repo: "example/desk"}
	prompts := map[string]string{
		"new card":     fmt.Sprintf(newCardPrompt, "req", "example/desk", "", tierRules, wholeAppRule, questionRule),
		"plan review":  ReviewPrompt(c, "plan", 7478, "unknown"),
		"code review":  ReviewPrompt(c, "code", 7478, "unknown"),
		"build":        buildMsg("Plan approved.", "(none)"),
		"rework plan":  reworkMsg("plan", 1, 3, "notes"),
		"rework code":  reworkMsg("code", 1, 3, "notes"),
		"summary":      summaryMsg,
		"brief":        briefMsg("code", true, "notes", briefAsk(card("c1", map[string]string{labelNote: "reviewer escalated"}))),
		"land":         landMsg,
		"guarded land": guardedLandMsg,
		"ui hold":      uiFeedbackHoldMsg("- the button is small"),
	}
	// the board's own messages: plan changes, recommendations (plan and code), commit before review
	for _, tc := range []struct {
		name   string
		labels map[string]string
		act    string
		body   Body
	}{
		{"plan changes", map[string]string{labelStage: "zach", labelAwaiting: "plan"}, "send", Body{Message: "smaller"}},
		{"recommend plan", map[string]string{labelStage: "zach", labelNote: "rework cap reached", labelVerdict: "plan changes",
			labelSource: "board"}, "recommend", Body{}},
		{"recommend code", map[string]string{labelStage: "zach", labelNote: "rework cap reached", labelVerdict: "code changes"},
			"recommend", Body{}},
	} {
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
	if err := f.askToCommit(Card{ID: "dirty"}, " M a.go"); err != nil {
		t.Fatal(err)
	}
	prompts["commit before review"] = p.sent("dirty")[0]
	for name, msg := range prompts {
		if !strings.Contains(msg, "DECISION:") && !strings.Contains(msg, overviewReminder) {
			t.Errorf("%s does not ask for the overview block", name)
		}
	}
	for _, name := range []string{"new card", "plan review", "code review", "summary", "brief"} {
		if !strings.Contains(prompts[name], "NEEDS FROM ZACH:") {
			t.Errorf("%s does not teach the whole block", name)
		}
	}
	for _, kind := range []string{"plan", "code"} {
		if !strings.HasSuffix(ReviewPrompt(c, kind, 7478, "unknown"), "TIER: <low|risky|architecture>\nVERDICT: <approve|nits|changes|escalate|close>\n") {
			t.Errorf("%s review no longer ends with TIER and VERDICT", kind)
		}
	}
}

// The brief's NEEDS FROM ZACH lines name every board button by its exact label.
func TestBriefNamesEveryButtonInItsNeeds(t *testing.T) {
	ask := briefAsk(card("c1", map[string]string{labelNote: "rework cap reached", labelVerdict: "code changes"}))
	msg := briefMsg("code", false, "notes", ask)
	i := strings.Index(msg, "NEEDS FROM ZACH:")
	if i < 0 {
		t.Fatal("no NEEDS FROM ZACH: in the brief")
	}
	for _, x := range ask.Actions {
		if !strings.Contains(msg[i:], `"`+x.Label+`"`) {
			t.Errorf("button %q is not named after NEEDS FROM ZACH:", x.Label)
		}
	}
}
