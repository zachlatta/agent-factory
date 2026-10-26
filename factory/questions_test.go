package factory

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseQuestions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rest string
		qs   []Question
	}{
		{"one question in prose",
			"Plan is ready.\n\nQUESTION: Where should it live?\n- Keep it where it is (recommended)\n- New repo\n\nTIER: low",
			"Plan is ready.\n\n\nTIER: low",
			[]Question{{Text: "Where should it live?", Options: []string{"Keep it where it is (recommended)", "New repo"}}}},
		{"option styles and bold label",
			"**QUESTION:** Pick a port\n1. 7478\n2) 7479\nA) 8080\nb. 9000\n* 9001\nafter",
			"after",
			[]Question{{Text: "Pick a port", Options: []string{"7478", "7479", "8080", "9000", "9001"}}}},
		{"blank line before options",
			"QUESTION: Ship it?\n\n- yes\n- no",
			"",
			[]Question{{Text: "Ship it?", Options: []string{"yes", "no"}}}},
		{"too few options stays prose",
			"QUESTION: Anything else?\n- no\nend",
			"QUESTION: Anything else?\n- no\nend",
			nil},
		{"over 6 options keeps the first 6",
			"QUESTION: n?\n- 1\n- 2\n- 3\n- 4\n- 5\n- 6\n- 7",
			"",
			[]Question{{Text: "n?", Options: []string{"1", "2", "3", "4", "5", "6"}}}},
	}
	for _, c := range cases {
		rest, qs := ParseQuestions(c.in)
		if rest != c.rest || !reflect.DeepEqual(qs, c.qs) {
			t.Errorf("%s: got rest %q qs %+v", c.name, rest, qs)
		}
	}
	// at most 4 questions; the fifth stays in the text
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString("QUESTION: q?\n- a\n- b\n")
	}
	rest, qs := ParseQuestions(b.String())
	if len(qs) != 4 || rest != "QUESTION: q?\n- a\n- b" {
		t.Errorf("5 questions: got %d, rest %q", len(qs), rest)
	}
}

func TestAskYesNo(t *testing.T) {
	cases := []struct {
		name    string
		a       Agent
		yes, no string
	}{
		{"architecture plan", card("c", map[string]string{labelStage: "zach", labelAwaiting: "plan"}), "approve", "changes"},
		{"ready to land", card("c", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"}), "approve", "changes"},
		{"architecture found in code", card("c", map[string]string{labelStage: "zach", labelNote: "review found an architecture change: approve to land"}), "approve", "changes"},
		{"rework cap brief", card("c", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}), "recommend", "changes"},
		{"no verdict", card("c", map[string]string{labelStage: "zach", labelNote: noVerdictNote}), "", ""},
		{"close suggested", card("c", map[string]string{labelStage: "zach", labelVerdict: "close"}), "close", "changes"},
		{"escalated brief", card("c", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"}), "recommend", "changes"},
		{"stuck", card("c", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"}), "", ""},
		{"landing unverified", card("c", map[string]string{labelStage: "zach", labelNote: "landing not verified"}), "say", "say"},
		{"moved by hand", card("c", map[string]string{labelStage: "zach"}), "say", "say"},
		{"landed with work left", card("c", map[string]string{labelStage: "zach", labelNote: "landed; still to do: x"}), "say", "say"},
		{"permission", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "permission", Labels: map[string]string{}}, "say", "say"},
		{"finished unread", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}}, "", ""},
		{"repo picker", card("c", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelCandidates: "example/desk"}), "", ""},
	}
	for _, c := range cases {
		ask := BuildBoard([]Agent{c.a}, nil, t0)[0].Ask
		if ask == nil || ask.Yes != c.yes || ask.No != c.no {
			t.Errorf("%s: ask = %+v, want yes %q no %q", c.name, ask, c.yes, c.no)
		}
	}
}

func TestAskQuestionsFromLongReply(t *testing.T) {
	// an escalation: the builder's brief is the detail
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"})
	r := reviewer("r1", "c1", "code")
	r.Labels[labelPrefix+labelApplied] = "1"
	f, p := newTestFactory(t)
	// the question sits past the 6000-character cut of Detail
	p.logs["r1"] = "[User] review\nKeep the old flag?\nVERDICT: escalate"
	p.logs["c1"] = "[User] brief please\n" + strings.Repeat("x", 7000) + "\nQUESTION: Keep the old flag?\n- Keep it\n- Remove it"
	f.SetAgents([]Agent{c, r})
	got, _ := f.card("c1")
	want := []Question{{Text: "Keep the old flag?", Options: []string{"Keep it", "Remove it"}}}
	if got.Ask == nil || !reflect.DeepEqual(got.Ask.Questions, want) || strings.Contains(got.Ask.Detail, "QUESTION:") {
		t.Fatalf("ask = %+v", got.Ask)
	}
	// served from the cache: questions are still there
	f.SetAgents([]Agent{c, r})
	if got, _ := f.card("c1"); !reflect.DeepEqual(got.Ask.Questions, want) {
		t.Errorf("cached questions = %+v", got.Ask.Questions)
	}
}

func TestApproveCarriesZachsAnswers(t *testing.T) {
	answers := "1. Where should it live?\n   → Keep it where it is"
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelTier: "architecture"}))
	if _, err := f.Act("approve", Body{ID: "c1", Message: answers}); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach approved your plan") ||
		!strings.Contains(msgs[0], answers) || !strings.Contains(msgs[0], "Build it now") {
		t.Errorf("plan approve sent = %v", msgs)
	}

	f, p = newTestFactory(t, card("c2", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"}))
	if _, err := f.Act("approve", Body{ID: "c2", Message: answers}); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c2"); len(msgs) != 1 || !strings.Contains(msgs[0], answers) || !strings.Contains(msgs[0], "Land it now") {
		t.Errorf("land approve sent = %v", msgs)
	}

	// no answers: messages as before
	f, p = newTestFactory(t, card("c3", map[string]string{labelStage: "zach", labelAwaiting: "plan"}))
	if _, err := f.Act("approve", Body{ID: "c3"}); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c3"); len(msgs) != 1 || strings.Contains(msgs[0], "answers") {
		t.Errorf("plain approve sent = %v", msgs)
	}
}

func TestPromptsTeachQuestionFormat(t *testing.T) {
	c := Card{ID: "c1", Title: "t", Repo: "example/desk"}
	for name, p := range map[string]string{
		"new card":    fmt.Sprintf(newCardPrompt, "req", "example/desk", "", tierRules, wholeAppRule, questionRule),
		"plan review": ReviewPrompt(c, "plan", 7478, "unknown"),
		"code review": ReviewPrompt(c, "code", 7478, "unknown"),
		"brief":       briefMsg("code", true, "notes", briefAsk(card("c1", map[string]string{labelNote: "reviewer escalated"}))),
		"summary":     summaryMsg,
	} {
		if !strings.Contains(p, "QUESTION:") {
			t.Errorf("%s prompt does not teach the QUESTION: format", name)
		}
	}
}
