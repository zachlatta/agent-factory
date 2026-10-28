package factory

import (
	"strings"
	"testing"
)

func TestParseMustFix(t *testing.T) {
	for in, want := range map[string]string{
		"MUST FIX: add the test\n- MUST FIX: name the caller, too\nVERDICT: changes": "add the test; name the caller; too",
		"**MUST FIX:** `x`":          "x",
		"MUST FIX: none":             "",
		"MUST FIX: <a concrete fix>": "",
		"no line":                    "",
	} {
		if got := ParseMustFix(in); got != want {
			t.Errorf("ParseMustFix(%q) = %q, want %q", in, got, want)
		}
	}
	for _, kind := range []string{"plan", "code"} {
		if got := ParseMustFix(ArbiterPrompt(Card{ID: "c1"}, kind, 7478, "unknown", "notes", false)); got != "" {
			t.Errorf("%s arbiter prompt matched its own MUST FIX rule: %q", kind, got)
		}
	}
}

// At the rework cap (without a small fix) and on an escalation, an arbiter agent rules: never a brief for Zach.
func TestReviewThatDoesNotSettleGoesToTheArbiter(t *testing.T) {
	for _, tc := range []struct {
		name, kind, verdict string
		labels              map[string]string
	}{
		{"plan cap", "plan", "changes", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: "2"}},
		{"code cap", "code", "changes", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "2"}},
		{"plan escalation", "plan", "escalate", map[string]string{labelStage: "plan-review", labelSource: "board"}},
		{"code escalation", "code", "escalate", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}},
	} {
		f, p := newTestFactory(t)
		c := card("c1", tc.labels)
		storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
		f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", tc.kind, tc.verdict, p)})
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] == "zach" || l[labelNote] != arbiterNote || l[labelArbitrated] != tc.kind || l[labelWriting] != "" {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 0 {
			t.Errorf("%s: builder asked for a brief: %v", tc.name, msgs)
		}
		runs := p.runsWithLabel(labelArg(labelArbiter, "1"))
		if len(runs) != 1 {
			t.Fatalf("%s: arbiter runs = %v", tc.name, runs)
		}
		prompt := runs[0][len(runs[0])-1]
		for _, want := range []string{"Arbiter", "LAST finding", "FIRST finding", "VERDICT: <approve|changes|escalate|close>",
			labelArg(labelReviewKind, tc.kind)} {
			if !strings.Contains(strings.Join(runs[0], " "), want) {
				t.Errorf("%s: arbiter run lacks %q", tc.name, want)
			}
		}
		if tc.verdict == "escalate" && !strings.Contains(prompt, "only Zach can decide") {
			t.Errorf("%s: arbiter not told about the escalation", tc.name)
		}
	}
}

// arbiterRules applies an arbiter's ruling to card c and returns its labels and the messages sent to it.
func arbiterRules(t *testing.T, c Agent, kind, ruling string) (map[string]string, []string, *fakePaseo) {
	t.Helper()
	arb := reviewer("a1", c.ID, kind)
	arb.Labels[labelPrefix+labelArbiter] = "1"
	f, p := newTestFactory(t, c, arb)
	storeLog(t, f, p, c.ID, "Plan: make the requested change.\nBUILD SIDE EFFECTS: none\nTIER: low")
	storeLog(t, f, p, "a1", ruling)
	if err := f.ApplyVerdict(f.agent("a1")); err != nil {
		t.Fatal(err)
	}
	return p.labels(c.ID), p.sent(c.ID), p
}

func TestArbiterRulingOnAPlan(t *testing.T) {
	at := func() Agent {
		return card("c1", map[string]string{labelStage: "plan-review", labelSource: "board", labelPlanRounds: "3",
			labelArbitrated: "plan", labelNote: arbiterNote})
	}
	// approve: build, with no new round
	l, msgs, _ := arbiterRules(t, at(), "plan", "Builder is right.\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelPlanRounds] != "" || l[labelVerdict] != "plan approve" {
		t.Errorf("approve: labels = %v", l)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "arbiter") || !strings.Contains(msgs[0], "Build it now") {
		t.Errorf("approve: sent = %v", msgs)
	}
	// changes with MUST FIX: build with the fixes, which code review checks
	l, msgs, _ = arbiterRules(t, at(), "plan", "MUST FIX: add the migration test\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: changes")
	if l[labelStage] != "building" || l[labelPlanFix] != "add the migration test" || l[labelVerdict] != "plan fixes" {
		t.Errorf("changes: labels = %v", l)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "add the migration test") {
		t.Errorf("changes: sent = %v", msgs)
	}
	built := withLabels(at(), l)
	if planPending(built) {
		t.Error("a plan the arbiter passed is still pending")
	}
	// escalate: a side effect Zach must allow before building
	l, msgs, _ = arbiterRules(t, at(), "plan", "It writes production rows.\nBUILD SIDE EFFECTS: writes prod rows\nTIER: risky\nVERDICT: escalate")
	if l[labelStage] != "zach" || l[labelAwaiting] != "plan" || l[labelNote] != sideEffectNote || !planPending(withLabels(at(), l)) {
		t.Errorf("escalate: labels = %v", l)
	}
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0], sideEffectMsg) {
		t.Errorf("escalate: sent = %v", msgs)
	}
	// approve, but it found a side effect: Zach allows the build first
	l, _, _ = arbiterRules(t, at(), "plan", "BUILD SIDE EFFECTS: sends a test email to a customer\nTIER: low\nVERDICT: approve")
	if l[labelStage] != "zach" || l[labelNote] != sideEffectNote {
		t.Errorf("approve with a side effect: labels = %v", l)
	}
	// changes without a fix, or no verdict: the board shows it; no second arbiter
	for _, ruling := range []string{"I cannot tell.\nTIER: low\nVERDICT: changes", "ran out of time"} {
		l, msgs, p := arbiterRules(t, at(), "plan", ruling)
		if l[labelStage] != "zach" || l[labelNote] != arbiterStuckNote || len(msgs) != 0 || len(p.runsWithLabel(labelArg(labelArbiter, "1"))) != 0 {
			t.Errorf("%q: labels = %v sent = %v", ruling, l, msgs)
		}
	}
	// close: as any reviewer's close
	if l, _, _ := arbiterRules(t, at(), "plan", "Duplicate.\nTIER: low\nVERDICT: close"); l[labelStage] != "zach" || l[labelVerdict] != "close" {
		t.Errorf("close: labels = %v", l)
	}
}

func TestArbiterRulingOnCode(t *testing.T) {
	at := func(mk func(string, map[string]string) Agent) Agent {
		return mk("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "3",
			labelArbitrated: "code", labelNote: arbiterNote})
	}
	// approve on factory-started work: it lands
	l, msgs, _ := arbiterRules(t, at(idea), "code", "Fine.\nALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if l[labelStage] != "landing" || len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
		t.Errorf("idea approve: labels = %v sent = %v", l, msgs)
	}
	// approve on work Zach asked for: he sees it finished
	l, msgs, _ = arbiterRules(t, at(card), "code", "Fine.\nALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if l[labelStage] != "zach" || l[labelNote] != previewNote || len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("Zach's approve: labels = %v sent = %v", l, msgs)
	}
	// changes with MUST FIX: one more round, whose review checks the fixes
	l, msgs, _ = arbiterRules(t, at(idea), "code", "MUST FIX: handle the empty list\nALWAYS ASK: none\nTIER: low\nVERDICT: changes")
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelPlanFix] != "handle the empty list" {
		t.Errorf("changes: labels = %v", l)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0], "handle the empty list") || !strings.Contains(msgs[0], "do NOT land") {
		t.Errorf("changes: sent = %v", msgs)
	}
	fixed, _ := newTestFactory(t, withLabels(at(idea), l))
	fc, _ := fixed.card("c1")
	if !strings.Contains(ReviewPrompt(fc, "code", 7478, "unknown"), "handle the empty list") {
		t.Error("the next code review does not check the arbiter's fixes")
	}
	// that round fails again: the board, not a second arbiter
	f, p := newTestFactory(t, withLabels(at(idea), map[string]string{labelStage: "review", labelCodeRounds: "3"}), reviewer("r2", "c1", "code"))
	storeLog(t, f, p, "r2", "still broken\nTIER: low\nVERDICT: changes")
	f.ApplyVerdict(f.agent("r2"))
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != arbiterStuckNote || len(p.runsWithLabel(labelArg(labelArbiter, "1"))) != 0 {
		t.Errorf("second failure: labels = %v", l)
	}
	// escalate on factory-started work: it comes to Zach with the reason
	l, msgs, _ = arbiterRules(t, at(idea), "code", "ALWAYS ASK: it rewrites the landing gate\nTIER: low\nVERDICT: escalate")
	if l[labelStage] != "zach" || l[labelNote] != previewNote || !strings.Contains(l[labelAskWhy], "rewrites the landing gate") {
		t.Errorf("escalate: labels = %v", l)
	}
	if len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") {
		t.Errorf("escalate: sent = %v", msgs)
	}
}

// Zach can hand a review that did not settle (or an older brief) to an arbiter from the board or by email.
func TestZachHandsTheReviewToAnArbiter(t *testing.T) {
	for _, note := range []string{arbiterStuckNote, "rework cap reached", "reviewer escalated"} {
		c := card("c1", map[string]string{labelStage: "zach", labelNote: note, labelVerdict: "plan changes", labelSource: "board",
			labelArbitrated: "plan"})
		f, p := newTestFactory(t, c)
		if _, err := f.Act("arbiter", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		runs := p.runsWithLabel(labelArg(labelArbiter, "1"))
		if len(runs) != 1 || !strings.Contains(strings.Join(runs[0], " "), labelArg(labelReviewKind, "plan")) {
			t.Errorf("%s: arbiter runs = %v", note, runs)
		}
		if l := p.labels("c1"); l[labelStage] != "plan-review" || l[labelArbitrated] != "plan" {
			t.Errorf("%s: labels = %v", note, l)
		}
	}
}

func TestArbiterPromptRules(t *testing.T) {
	c := Card{ID: "c1", Title: "t", Repo: "example/desk", Cwd: "/w/c1"}
	plan := ArbiterPrompt(c, "plan", 7478, "public", "the last review", false)
	code := ArbiterPrompt(c, "code", 7478, "public", "the last review", true)
	for name, p := range map[string]string{"plan": plan, "code": code} {
		for _, want := range []string{"the last review", "Read-only", "MUST FIX: <", "TIER: <low|risky|architecture>\nVERDICT: <approve|changes|escalate|close>\n",
			"PUBLIC", "paseo logs c1"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s arbiter prompt lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(plan, "BUILD SIDE EFFECTS: <") || !strings.Contains(code, "ALWAYS ASK: <") || !strings.Contains(code, "git -C /w/c1 diff") {
		t.Error("arbiter prompts lack their gate's line")
	}
}
