package factory

import (
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func choiceIDs(ask *Ask) []string {
	var out []string
	for _, c := range ask.Choices {
		out = append(out, c.ID)
	}
	return out
}

// Every ask gets one fixed set of answers: Land / Change / Drop for finished work, Yes / No / Drop for
// decisions; the factory's own problems keep their single action.
func TestAskChoices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		kind   string
		want   []string
	}{
		{"preview", map[string]string{labelNote: readyNote(1)}, askReady, []string{"land", "change", "drop"}},
		{"review passed", map[string]string{labelNote: "review passed: approve to land"}, askReady, []string{"land", "change", "drop"}},
		{"secret scan", map[string]string{labelNote: "secret scan failed: email in x.go:1"}, askReady, []string{"land", "change", "drop"}},
		{"build side effects", map[string]string{labelAwaiting: "plan", labelNote: sideEffectNote}, askDecision, []string{"yes", "no", "change", "drop"}},
		{"published unreviewed", map[string]string{labelNote: "public repo: finished without review"}, askDecision, []string{"yes", "no", "change"}},
		// board-only: the factory's own to settle, answered in the card's details
		{"did not settle", map[string]string{labelNote: arbiterStuckNote}, askProblem, []string{"arbiter", "change", "drop"}},
		{"close", map[string]string{labelNote: "reviewer suggests closing", labelVerdict: "close"}, askProblem, []string{"drop", "change"}},
		{"work left", map[string]string{labelNote: "landed; still to do: x"}, askProblem, []string{"change", "done"}},
	} {
		labels := map[string]string{labelStage: "zach"}
		for k, v := range tc.labels {
			labels[k] = v
		}
		a := card("c1", labels)
		ask := askFor(Card{ID: "c1", Stage: "zach"}, a)
		if ask.Kind != tc.kind || !slices.Equal(choiceIDs(ask), tc.want) {
			t.Errorf("%s: kind %q choices %v, want %q %v", tc.name, ask.Kind, choiceIDs(ask), tc.kind, tc.want)
		}
	}
	repo := askFor(Card{ID: "c1", Stage: "zach"}, card("c1", map[string]string{labelStage: "zach", labelAwaiting: "repo",
		labelCandidates: "a/one b/two"}))
	if repo.Kind != askDecision || !slices.Equal(choiceIDs(repo), []string{"pick:a/one", "pick:b/two", "pick", "change", "drop"}) {
		t.Errorf("repo ask: %q %v", repo.Kind, choiceIDs(repo))
	}
}

var emailWrap = regexp.MustCompile(`Zach's follow-up email \([^)]*\):\n|\n\(end of Zach's email\)`)

// The board's choice runs the same action as the email reply for it.
func TestChooseRunsTheSameActionAsEmail(t *testing.T) {
	for _, tc := range []struct {
		choice, message, email string
		check                  func(t *testing.T, p *fakePaseo)
	}{
		{"yes", "", "approve", func(t *testing.T, p *fakePaseo) {
			if l := p.labels("c1"); l[labelStage] != "building" || l[labelZachPlan] != "approved" {
				t.Errorf("yes: labels %v", l)
			}
		}},
		// No is one tap: the board sends no text, and the agent reads the same plain no either way
		{"no", "", "no", func(t *testing.T, p *fakePaseo) {
			if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "No.") {
				t.Errorf("no: sent %v", s)
			}
		}},
		{"change", "Use SQLite", "Use SQLite", func(t *testing.T, p *fakePaseo) {
			if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "Use SQLite") || !strings.Contains(s[0], "wants changes") {
				t.Errorf("change: sent %v", s)
			}
		}},
		{"drop", "", "drop", func(t *testing.T, p *fakePaseo) {
			if !p.called("archive", "c1") {
				t.Error("drop: not archived")
			}
		}},
	} {
		board, pb, _ := mailFactory(t, planWaiting("c1"))
		if _, err := board.Act("choose", Body{ID: "c1", Choice: tc.choice, Message: tc.message}); err != nil {
			t.Fatalf("board %s: %v", tc.choice, err)
		}
		tc.check(t, pb)
		mail, pm, _ := mailFactory(t, planWaiting("c1"))
		if _, err := mail.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: tc.email}); err != nil {
			t.Fatalf("email %s: %v", tc.email, err)
		}
		tc.check(t, pm)
		// the same words, the email's marked as his email
		var unwrapped []string
		for _, s := range pm.sent("c1") {
			unwrapped = append(unwrapped, emailWrap.ReplaceAllString(s, ""))
		}
		if !slices.Equal(pb.sent("c1"), unwrapped) {
			t.Errorf("%s: the board sent %q, the email %q", tc.choice, pb.sent("c1"), pm.sent("c1"))
		}
	}
}

// One answer clears the ask everywhere: once the card has moved on, a later answer from the other
// surface does nothing.
func TestChooseThenEmailIsNotWaiting(t *testing.T) {
	f, p, _ := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "yes"}); err != nil {
		t.Fatal(err)
	}
	after := withLabels(planWaiting("c1"), p.labels("c1")) // the card as the next read of its file sees it
	f, p, m := mailFactory(t, after)
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m3", Text: "drop"}); err != ErrNotWaiting {
		t.Errorf("email after a board answer: %v, want ErrNotWaiting", err)
	}
	if p.called("archive", "c1") || len(m.sends()) != 0 {
		t.Error("the late email answer acted or sent mail")
	}
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "drop"}); err == nil || p.called("archive", "c1") {
		t.Errorf("a board answer on a card that no longer waits must fail: %v", err)
	}
}

func TestChooseRefusesUnknownAndEmptyChoices(t *testing.T) {
	f, _, _ := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "land"}); err == nil {
		t.Error("land is not a choice on a plan decision")
	}
	ready := card("c2", map[string]string{labelStage: "zach", labelNote: readyNote(1)})
	f, _, _ = mailFactory(t, ready)
	if _, err := f.Act("choose", Body{ID: "c2", Choice: "change"}); err == nil {
		t.Error("change with no text must ask for the text")
	}
}

// An agent's open question is not a yes/no question: it has Answer (its options answer through it)
// and Drop, never Yes or No, and an empty Answer is refused.
func TestOpenQuestionIsAnswered(t *testing.T) {
	a := card("c1", map[string]string{})
	a.RequiresAttention, a.AttentionReason = true, "question"
	f, p, _ := mailFactory(t, a)
	c, _ := f.card("c1")
	ask := askFor(c, f.agent("c1"))
	if ask.Kind != askQuestion || !slices.Equal(choiceIDs(ask), []string{"answer", "drop"}) || ask.BoardOnly {
		t.Fatalf("open question: kind %q choices %v boardOnly %v", ask.Kind, choiceIDs(ask), ask.BoardOnly)
	}
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "answer"}); err == nil {
		t.Error("an empty answer must be refused")
	}
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "answer", Message: "Zach's answers:\n1. Q\n   → A"}); err != nil {
		t.Fatal(err)
	}
	if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "→ A") || strings.Contains(s[0], "Yes.") {
		t.Errorf("sent %v", s)
	}
}

// The board's choice goes through the action API like any other button.
func TestChooseAPI(t *testing.T) {
	f, p, _ := mailFactory(t, planWaiting("c1"))
	req := httptest.NewRequest("POST", "/api/choose", strings.NewReader(`{"id":"c1","choice":"yes"}`))
	req.Header.Set("X-Factory", "1")
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != 200 || p.labels("c1")[labelStage] != "building" {
		t.Errorf("POST /api/choose: %d %s, labels %v", w.Code, w.Body, p.labels("c1"))
	}
}

// One rule decides For you: every ask that is not board-only is ready, a decision or a question, with
// the fixed buttons; every board-only ask is a problem, shown under Working on it.
func TestEveryAskHasOneKind(t *testing.T) {
	attention := card("c1", map[string]string{})
	attention.RequiresAttention, attention.AttentionReason = true, "question"
	agents := []Agent{attention}
	for _, l := range []map[string]string{
		{labelNote: readyNote(1)}, {labelNote: "review passed: approve to land"}, {labelNote: "secret scan failed: x"},
		{labelAwaiting: "plan", labelNote: sideEffectNote}, {labelNote: "public repo: finished without review"},
		{labelAwaiting: "repo", labelCandidates: "a/one"},
		{labelNote: arbiterStuckNote}, {labelVerdict: "close"}, {labelNote: "stuck", labelStuckFrom: "plan"},
		{labelNote: "landing not verified"},
		{labelNote: "landed; still to do: x"}, {labelNote: "landed; did not say what remains"}, {},
	} {
		labels := map[string]string{labelStage: "zach"}
		for k, v := range l {
			labels[k] = v
		}
		agents = append(agents, card("c1", labels))
	}
	for _, a := range agents {
		ask := askFor(Card{ID: "c1", Stage: "zach"}, a)
		switch {
		case ask.BoardOnly && ask.Kind != askProblem:
			t.Errorf("%q: board-only but kind %q", ask.Title, ask.Kind)
		case !ask.BoardOnly && ask.Kind == askProblem:
			t.Errorf("%q: For you but kind problem", ask.Title)
		case len(ask.Choices) == 0:
			t.Errorf("%q: no buttons", ask.Title)
		}
	}
}
