package factory

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestParseAnswer(t *testing.T) {
	qs := []Question{{Text: "Which store?", Options: []string{"Postgres", "SQLite"}},
		{Text: "When?", Options: []string{"Now", "Later"}}}
	mk := func(a *Ask) *Ask { a.Choices = choicesFor(a); return a }
	plan := mk(&Ask{Kind: askDecision, Actions: []Action{primary("approve", "Allow & build"),
		reply("Send back", ""), act("close", "Drop it")}, Questions: qs})
	onePlan := mk(&Ask{Kind: askDecision, Actions: []Action{primary("approve", "Allow & build"),
		reply("Send back", ""), act("close", "Drop it")}, Questions: qs[:1]})
	settle := settleAsk()
	settle.Kind = askProblem
	settle = mk(settle)
	rerun := mk(&Ask{Kind: askProblem, Actions: []Action{primary("rerun", "Re-run"), reply("Reply", "")}})
	landedLeft := mk(&Ask{Kind: askProblem, Actions: []Action{reply("Send it on", ""), act("done", "Mark done")}})
	open := mk(&Ask{Kind: askQuestion, Actions: []Action{{ID: "reply", Label: "Answer", Primary: true}, act("close", "Drop it")}})
	openQs := mk(&Ask{Kind: askQuestion, Actions: open.Actions, Questions: qs[:1]})
	for _, tc := range []struct {
		ask        *Ask
		text, want string
		msg        string // substring of the message, if any
	}{
		{plan, "Approve.", "yes", ""},
		{plan, "LGTM!", "yes", ""},
		{plan, "  go   ahead ", "yes", "1. Which store?\n   → Postgres"},
		{plan, "no", "no", "No."},
		{settle, "yes", "change", "yes"}, // no Yes button on a review that did not settle (it is board-only)
		{rerun, "yes", "change", "yes"},
		{plan, "drop it", "drop", ""},
		{rerun, "drop", "change", "drop"},
		{landedLeft, "done", "done", ""},
		{landedLeft, "Mark done!", "done", ""},
		{landedLeft, "mark it done", "done", ""},
		{open, "done", "answer", "done"}, // an open question has only Answer and Drop
		{open, "yes", "answer", "yes"},   // not a yes/no question: his yes is his answer
		{open, "drop", "drop", ""},
		{openQs, "B", "answer", "Which store?\n   → SQLite"}, // its options answer through Answer
		{plan, "done", "no", "done"},
		{plan, "ok, but drop step 3", "no", "ok, but drop step 3"},
		{plan, "yes to 1A, no to 2", "no", "yes to 1A"},
		{plan, "Please drop the second point and build", "no", "drop the second point"},
		{plan, "1B 2A", "yes", "Which store?\n   → SQLite"},
		{plan, "1b, 2b", "yes", "When?\n   → Later"},
		{plan, "yes", "yes", "1. Which store?\n   → Postgres\n2. When?\n   → Now"}, // yes takes the recommendations
		{onePlan, "B", "yes", "Which store?\n   → SQLite"},                         // one question: a letter alone
		{onePlan, "a.", "yes", "→ Postgres"},
		{plan, "B", "no", "B"}, // two questions: a lone letter is not an answer
		{onePlan, "G", "no", "G"},
		{plan, "3C", "no", "3C"},
		{plan, "wait\n\n> approve", "no", "wait"},
		{plan, "approve\n\nOn Sun, Oct 25, 2026 at 7:42 PM zrl bot wrote:\n> Needs you", "yes", ""},
		{plan, "approve\n-- \nZach", "yes", ""},
		{plan, "Approve\n\nSent from my iPhone", "yes", ""},
		{plan, "  \n> approve", "", ""},
	} {
		got := parseAnswer(tc.text, tc.ask)
		if got.Choice != tc.want || !strings.Contains(got.Message, tc.msg) {
			t.Errorf("parseAnswer(%q) = %+v, want %s with %q", tc.text, got, tc.want, tc.msg)
		}
		if strings.Contains(got.Message, ">") {
			t.Errorf("parseAnswer(%q) kept quoted text: %q", tc.text, got.Message)
		}
	}
}

// Every reply word an email offers is a button the ask has, for every kind of ask: the word parses
// into one of the ask's choices, and no word is offered without its button.
func TestReplyWordsAreButtons(t *testing.T) {
	agents := map[string]Agent{
		"plan":    card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "x"}),
		"preview": card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1)}),
		"brief":   card("c1", map[string]string{labelStage: "zach", labelNote: "rework cap reached", labelVerdict: "code changes"}),
		"close":   card("c1", map[string]string{labelStage: "zach", labelVerdict: "close"}),
		"stuck":   card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"}),
		"left":    card("c1", map[string]string{labelStage: "zach", labelNote: "landed; still to do: x"}),
		"repo":    card("c1", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelCandidates: "a/one b/two"}),
	}
	open := card("c1", map[string]string{})
	open.RequiresAttention, open.AttentionReason = true, "question"
	agents["open"] = open
	for name, a := range agents {
		ask := askFor(Card{ID: "c1", Stage: "zach"}, a)
		ask.Questions = []Question{{Text: "Q?", Options: []string{"x", "y"}}}
		for _, w := range replyWords(ask) {
			text := w.Word
			if text == "1A 2B" {
				text = "1A"
			}
			got := parseAnswer(text, ask)
			if choiceByID(ask, got.Choice) == nil {
				t.Errorf("%s: reply %q offered (%s) but parses to no button: %+v", name, w.Word, w.Does, got)
			}
		}
		for _, word := range []string{"approve", "no", "done", "drop", "free text"} {
			got := parseAnswer(word, ask)
			if got.Choice != "" && choiceByID(ask, got.Choice) == nil {
				t.Errorf("%s: %q parses to %q, which is not a button", name, word, got.Choice)
			}
		}
		if choiceByID(ask, "done") == nil {
			for _, w := range replyWords(ask) {
				if w.Word == "done" {
					t.Errorf("%s: offers done without a Mark done button", name)
				}
			}
		}
	}
}

func planWaiting(id string) Agent {
	return card(id, map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build",
		labelZachThread: "nt1"})
}

func TestEmailApproveBuildsWithoutAnEcho(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "Approve."})
	if err != nil || res.(map[string]string)["action"] != "approve" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAnswered] != "m2" || l[labelZachPlan] != "approved" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach allowed this build") {
		t.Errorf("sent = %v", msgs)
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("an answer that worked must not be echoed: %+v", s)
	}
}

func TestEmailAnswerRepeatedIsIgnored(t *testing.T) {
	a := withLabels(planWaiting("c1"), map[string]string{labelAnswered: "m2"})
	f, p, m := mailFactory(t, a)
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "approve"})
	if err != nil || res.(map[string]string)["action"] != "duplicate" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if len(p.sent("c1")) != 0 || len(m.sends()) != 0 {
		t.Error("a repeated answer acted again")
	}
}

func TestEmailFreeTextGoesToTheAgentAsAFollowUp(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "ok, but drop step 3\n\n> old"}); err != nil {
		t.Fatal(err)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "wants changes") || !strings.Contains(msgs[0], followUpPrefix) ||
		!strings.Contains(msgs[0], "ok, but drop step 3\n"+emailEnd) {
		t.Fatalf("sent = %v", msgs)
	}
	if fus := followUpsOf(msgs[0]); len(fus) != 1 || fus[0].Text != "ok, but drop step 3" {
		t.Errorf("follow-ups = %+v", fus)
	}
	if l := p.labels("c1"); l[labelStage] != "plan" {
		t.Errorf("labels = %v", l)
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("an answer that worked must not be echoed: %+v", s)
	}
}

func TestEmailDropClosesTheCard(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "Drop it"}); err != nil {
		t.Fatal(err)
	}
	if !p.called("archive", "c1") {
		t.Error("not archived")
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("an answer that worked must not be echoed: %+v", s)
	}
}

func TestEmailDoneMarksCardDone(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: "landed; still to do: browser check",
		labelZachThread: "nt1"})
	f, p, m := mailFactory(t, a)
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "done"})
	if err != nil || res.(map[string]string)["action"] != "done" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if l := p.labels("c1"); l[labelStage] != "landed" || l[labelNote] != "marked done by Zach" {
		t.Errorf("labels = %v", l)
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("an answer that worked must not be echoed: %+v", s)
	}
}

func TestEmailApproveOnPublicRepoStillScans(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land", labelVerdict: "code approve",
		labelPlanRounds: "1", labelReviewed: reviewedSHA})
	a.Labels[gmailThreadLabel] = "t9"
	a.Repo = "zachlatta/agent-factory"
	f, p, m := mailFactory(t, a)
	headIs(f, reviewedSHA, nil)
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		return []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, nil
	}
	_, err := f.Act("answer", Body{Thread: "t9", MessageID: "m2", Text: "land"})
	if err == nil || !strings.Contains(err.Error(), "secret scan failed") {
		t.Fatalf("a scan hit must stop an emailed approve: %v", err)
	}
	if l := p.labels("c1"); l[labelStage] == "landing" {
		t.Errorf("landed past a scan hit: %v", l)
	}
	s := m.sends()
	if len(s) != 1 || s[0].Args["message_id"] != "m2" ||
		s[0].Args["text"] != "That reply didn't go through. Answer on the board: https://board.example/#c1" {
		t.Errorf("a failed answer gets one plain line: %+v", s)
	}
}

func TestAnswerAPIStatusTellsTheDispatcherToFallBack(t *testing.T) {
	building := card("c2", map[string]string{labelStage: "building", labelZachThread: "nt2"})
	f, _, _ := mailFactory(t, planWaiting("c1"), building)
	h := f.Handler()
	for thread, want := range map[string]int{"nope": 404, "nt2": 409} {
		req := httptest.NewRequest("POST", "/api/answer", strings.NewReader(`{"thread":"`+thread+`","message_id":"m1","text":"approve"}`))
		req.Header.Set("X-Factory", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("thread %s: status %d, want %d (%s)", thread, w.Code, want, w.Body)
		}
	}
	if !errors.Is(func() error { _, err := f.Act("answer", Body{Thread: "nope", MessageID: "m"}); return err }(), ErrNoCard) {
		t.Error("unknown thread is not ErrNoCard")
	}
}

// A review that did not settle offers no questions (the arbiter rules now), so letters are Zach's own words.
func TestEmailLetterAnswersOnABriefReachTheAgent(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: "rework cap reached", labelVerdict: "code changes",
		labelZachThread: "nt1"})
	f, p, m := mailFactory(t, a)
	storeLog(t, f, p, "c1", "Brief.\n\nQUESTION: Which fix?\n- The small one (recommended)\n- The big one\n")
	c, _ := f.card("c1")
	if ask := askFor(c, f.agent("c1")); choiceByID(ask, "arbiter") == nil || choiceByID(ask, "arbiter").action != "arbiter" || goChoice(ask) != nil {
		t.Fatalf("ask = %+v", ask)
	}
	if c.Ask == nil || len(c.Ask.Questions) != 0 {
		t.Fatalf("stale questions offered next to the arbiter: %+v", c.Ask)
	}
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "1B"})
	if err != nil || res.(map[string]string)["action"] != "send" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "1B") || strings.Contains(msgs[0], "→") {
		t.Errorf("sent = %v", msgs)
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("an answer that worked must not be echoed: %+v", s)
	}
}

func TestEmailAnswerFindsTheWaitingCardInASharedThread(t *testing.T) {
	// cards started from one email share its thread; only the one waiting on Zach is answered
	planning := card("c0", map[string]string{labelStage: "plan", labelZachThread: "nt1"})
	planning.CreatedAt = t0.Add(-2 * time.Hour)
	f, p, _ := mailFactory(t, planning, planWaiting("c1"))
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "approve"})
	if err != nil || res.(map[string]string)["card"] != "c1" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if p.labels("c1")[labelStage] != "building" || len(p.labels("c0")) != 0 {
		t.Errorf("c0 = %v, c1 = %v", p.labels("c0"), p.labels("c1"))
	}
}

func TestEmailAnswerActsOnNoneWhenTwoCardsWait(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"), planWaiting("c2"))
	_, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "approve"})
	if !errors.Is(err, ErrSharedThread) {
		t.Fatalf("err = %v", err)
	}
	if len(p.labels("c1")) != 0 || len(p.labels("c2")) != 0 || len(m.sends()) != 0 {
		t.Errorf("acted on a shared thread: c1 = %v, c2 = %v, mail = %+v", p.labels("c1"), p.labels("c2"), m.sends())
	}
	req := httptest.NewRequest("POST", "/api/answer", strings.NewReader(`{"thread":"nt1","message_id":"m2","text":"approve"}`))
	req.Header.Set("X-Factory", "1")
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Errorf("status %d, want 409 so the dispatcher passes it on", w.Code)
	}
}
