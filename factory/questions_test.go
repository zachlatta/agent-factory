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

// Each ask's buttons and what they run: Yes or Land only where they approve, allow or check something,
// and none on asks whose answer is the factory's own action.
func TestAskButtons(t *testing.T) {
	cases := []struct {
		name string
		a    Agent
		want string // each choice as id=action
	}{
		{"architecture plan", card("c", map[string]string{labelStage: "zach", labelAwaiting: "plan"}), "yes=approve no=send drop=close"},
		{"ready to land", card("c", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"}), "land=approve change=send drop=close"},
		{"architecture found in code", card("c", map[string]string{labelStage: "zach", labelNote: "review found an architecture change: approve to land"}), "land=approve change=send drop=close"},
		{"rework cap brief", card("c", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}), "arbiter=arbiter change=send drop=close"},
		{"stuck in its column", card("c", map[string]string{labelStage: "review", labelStuck: stuckReview}), "?"},
		{"close suggested", card("c", map[string]string{labelStage: "zach", labelVerdict: "close"}), "drop=close change=send"},
		{"escalated brief", card("c", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"}), "arbiter=arbiter change=send drop=close"},
		{"agent error stuck", card("c", map[string]string{labelStage: "building", labelStuck: stuckAgentError}), "?"},
		{"landing unverified", card("c", map[string]string{labelStage: "zach", labelNote: "landing not verified"}), "change=send done=done"},
		{"moved by hand", card("c", map[string]string{labelStage: "zach"}), "change=send done=done"},
		{"landed with work left", card("c", map[string]string{labelStage: "zach", labelNote: "landed; still to do: x"}), "change=send done=done"},
		{"open question", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "permission", Labels: map[string]string{}}, "answer=send drop=close"},
		{"finished unread", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}}, "done=done change=send"},
		{"repo picker", card("c", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelCandidates: "example/desk"}),
			"pick:example/desk=approve pick=pickrepo change=send drop=close"},
	}
	for _, c := range cases {
		ask := BuildBoard([]Agent{c.a}, nil, t0)[0].Ask
		if ask == nil {
			t.Errorf("%s: no ask", c.name)
			continue
		}
		var got []string
		for _, ch := range ask.Choices {
			got = append(got, ch.ID+"="+ch.action)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: buttons %q, want %q", c.name, strings.Join(got, " "), c.want)
		}
	}
}

func TestAskQuestionsFromLongReply(t *testing.T) {
	// finished work: the builder's preview is the detail
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1)})
	r := reviewer("r1", "c1", "code")
	r.Labels[labelPrefix+labelApplied] = "1"
	f, p := newTestFactory(t)
	// the question sits past the 6000-character cut of Detail
	storeLog(t, f, p, "r1", "[User] review\nKeep the old flag?\nVERDICT: escalate")
	storeLog(t, f, p, "c1", "[User] brief please\n"+strings.Repeat("x", 7000)+"\nQUESTION: Keep the old flag?\n- Keep it\n- Remove it")
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
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach allowed this build") ||
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
		"new card":    fmt.Sprintf(newCardPrompt, "req", "example/desk", "", tierRules, wholeChangeRule, questionRule),
		"plan review": ReviewPrompt(c, "plan", 7478, "unknown"),
		"code review": ReviewPrompt(c, "code", 7478, "unknown"),
		"arbiter":     ArbiterPrompt(c, "plan", 7478, "unknown", "notes", true),
		"preview":     readyMsg("you asked for it"),
	} {
		if !strings.Contains(p, "QUESTION:") {
			t.Errorf("%s prompt does not teach the QUESTION: format", name)
		}
	}
}
