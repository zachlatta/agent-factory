package factory

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestReplyWordsAreButtons(t *testing.T) {
	agents := map[string]Agent{
		"plan":    card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReviewed: reviewedSHA}),
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
	return card(id, map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReviewed: reviewedSHA,
		labelZachThread: "nt1"})
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
		labelRoundsRequest: "1", labelReviewed: reviewedSHA})
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

func TestEmailAnswerFindsTheWaitingCardInASharedThread(t *testing.T) {
	// cards started from one email share its thread; only the one waiting on Zach is answered
	planning := card("c0", map[string]string{labelStage: "building", labelZachThread: "nt1"})
	planning.CreatedAt = t0.Add(-2 * time.Hour)
	f, p, _ := mailFactory(t, planning, planWaiting("c1"))
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "approve"})
	if err != nil || res.(map[string]string)["card"] != "c1" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if p.labels("c1")[labelStage] != "landing" || len(p.labels("c0")) != 0 {
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

func TestUnclearReplyLeavesWaitingWorkUntouched(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	f.ReplyCheck = func(context.Context, ReplyContext) (string, error) { return "unclear", nil }
	res, err := f.answerByEmail(Body{Thread: "nt1", MessageID: "m2", Text: "Do something else"})
	if err != nil || res["action"] != "clarify" {
		t.Fatalf("%v %v", res, err)
	}
	if len(p.labels("c1")) != 0 || len(p.sent("c1")) != 0 || len(m.sends()) != 1 {
		t.Fatal("waiting work changed")
	}
	f.replies = &replyStore{path: f.replies.path}
	f.ReplyCheck = func(context.Context, ReplyContext) (string, error) {
		t.Fatal("clarification classified")
		return "", nil
	}
	res, err = f.answerByEmail(Body{Thread: "nt1", MessageID: "m3", Text: "answer"})
	if err != nil || res["action"] != "send" {
		t.Fatalf("%v %v", res, err)
	}
}

// readyWaiting is a board card waiting at Ready for you, with only the thread of its approval email.
func readyWaiting(id string) Agent {
	return card(id, map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelZachThread: "nt1", labelReviewed: reviewedSHA})
}

// emailAnswerFactory is a mail factory whose cards pass the reviewed-commit check.
func emailAnswerFactory(t *testing.T, agents ...Agent) (*Factory, *fakePaseo, *fakeMail) {
	t.Helper()
	f, p, m := mailFactory(t, agents...)
	headIs(f, reviewedSHA, nil)
	return f, p, m
}

// A yes by email reports in that thread when the change lands: the land message sent by the same
// answer already carries the email instruction.
func TestEmailYesStoresTheThreadAndLandMessageEmails(t *testing.T) {
	f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[gmailThreadLabel] != "nt1" || l[labelStage] != "landing" {
		t.Fatalf("labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 {
		t.Fatalf("sent = %v", msgs)
	}
	for _, want := range []string{"reply_to_message", "zrl-bot thread nt1", "archive the thread"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("land message lacks %q:\n%s", want, msgs[0])
		}
	}
}

// A board approval of the same card stays quiet.
func TestBoardLandStoresNoThreadAndSendsNoEmailInstruction(t *testing.T) {
	f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "land"}); err != nil {
		t.Fatal(err)
	}
	if _, set := p.labels("c1")[gmailThreadLabel]; set {
		t.Errorf("a board approval stored a thread: %v", p.labels("c1"))
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || strings.Contains(msgs[0], "reply_to_message") || strings.Contains(msgs[0], "archive the thread") {
		t.Errorf("sent = %v", msgs)
	}
}

// A card with a request thread keeps it, with no extra label write.
func TestEmailAnswerKeepsARequestThread(t *testing.T) {
	a := withLabels(readyWaiting("c1"), map[string]string{labelZachThread: ""})
	a.Labels[gmailThreadLabel] = "t9"
	f, p, _ := emailAnswerFactory(t, a)
	if _, err := f.Act("answer", Body{Thread: "t9", MessageID: "m2", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, set := p.labels("c1")[gmailThreadLabel]; set {
		t.Errorf("rewrote the request thread: %v", p.labels("c1"))
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "zrl-bot thread t9") {
		t.Errorf("sent = %v", msgs)
	}
}

// Only an answer that acts stores the thread: a clarifying question and a separate request do not.
func TestEmailReplyThatDoesNotAnswerStoresNoThread(t *testing.T) {
	for _, tc := range []struct{ verdict, action string }{{"unclear", "clarify"}, {"unrelated", "separate"}} {
		t.Run(tc.verdict, func(t *testing.T) {
			f, p, _ := emailAnswerFactory(t, planWaiting("c1"))
			f.ReplyCheck = func(context.Context, ReplyContext) (string, error) { return tc.verdict, nil }
			res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "what about lunch tomorrow"})
			if err != nil || res.(map[string]string)["action"] != tc.action {
				t.Fatalf("res = %v, err = %v", res, err)
			}
			if _, set := p.labels("c1")[gmailThreadLabel]; set {
				t.Errorf("stored a thread: %v", p.labels("c1"))
			}
		})
	}
}

func TestEmailAnswerRepeatedStoresTheThreadOnce(t *testing.T) {
	f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
	for range 2 {
		if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, c := range p.calls {
		if slices.Contains(c, gmailThreadLabel+"=nt1") {
			n++
		}
	}
	if n != 1 || len(p.sent("c1")) != 1 {
		t.Errorf("thread writes = %d, sent = %v", n, p.sent("c1"))
	}
}

func TestEmailAnswerInDryRunStoresNoThread(t *testing.T) {
	f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
	f.cfg.DryRun = true
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if _, set := p.labels("c1")[gmailThreadLabel]; set {
		t.Errorf("dry run stored a thread: %v", p.labels("c1"))
	}
}

// When the thread cannot be stored or read back the answer is not marked, so a retry of the same
// message still works; a landing that went out now would never email.
func TestEmailAnswerFailsWhenTheThreadIsNotStored(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
		f.p = failThreadWrite{f.p}
		if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err == nil {
			t.Fatal("no error")
		}
		if p.labels("c1")[labelAnswered] != "" || len(p.sent("c1")) != 0 {
			t.Errorf("labels = %v, sent = %v", p.labels("c1"), p.sent("c1"))
		}
	})
	t.Run("reload", func(t *testing.T) {
		f, p, _ := emailAnswerFactory(t, readyWaiting("c1"))
		f.p = p          // no agent files written
		f.cfg.Home = "[" // the agent files cannot be listed
		if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err == nil {
			t.Fatal("no error")
		}
		if p.labels("c1")[labelAnswered] != "" || len(p.sent("c1")) != 0 {
			t.Errorf("labels = %v, sent = %v", p.labels("c1"), p.sent("c1"))
		}
	})
}

// failThreadWrite is a paseo whose write of the email thread label fails.
type failThreadWrite struct{ Paseo }

func (p failThreadWrite) Run(ctx context.Context, args ...string) (string, error) {
	if slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, gmailThreadLabel+"=") }) {
		return "", errors.New("paseo agent update failed")
	}
	return p.Paseo.Run(ctx, args...)
}

func TestEmailRelatedTextGoesToTheAgentAsAFollowUp(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	f.ReplyCheck = func(context.Context, ReplyContext) (string, error) { return "related", nil }
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "ok, but drop step 3\n\n> old"}); err != nil {
		t.Fatal(err)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], followUpPrefix) ||
		!strings.Contains(msgs[0], "ok, but drop step 3\n"+emailEnd) {
		t.Fatalf("sent = %v", msgs)
	}
	if fus := followUpsOf(msgs[0]); len(fus) != 1 || fus[0].Text != "ok, but drop step 3" {
		t.Errorf("follow-ups = %+v", fus)
	}
	if l := p.labels("c1"); l[labelStage] != "building" {
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

func TestParseAnswer(t *testing.T) {
	qs := []Question{{Text: "Which store?", Options: []string{"Postgres", "SQLite"}},
		{Text: "When?", Options: []string{"Now", "Later"}}}
	mk := func(a *Ask) *Ask { a.Choices = choicesFor(a); return a }
	plan := mk(&Ask{Kind: askDecision, Actions: []Action{primary("approve", "Allow & build"),
		reply("Send back", ""), act("close", "Drop it")}, Questions: qs})
	onePlan := mk(&Ask{Kind: askDecision, Actions: []Action{primary("approve", "Allow & build"),
		reply("Send back", ""), act("close", "Drop it")}, Questions: qs[:1]})
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
		{plan, "no", "no", ""},
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
		{plan, "done", "change", "done"},
		{plan, "ok, but drop step 3", "change", "ok, but drop step 3"},
		{plan, "yes to 1A, no to 2", "change", "yes to 1A"},
		{plan, "Please drop the second point and build", "change", "drop the second point"},
		{plan, "1B 2A", "yes", "Which store?\n   → SQLite"},
		{plan, "1b, 2b", "yes", "When?\n   → Later"},
		{plan, "yes", "yes", "1. Which store?\n   → Postgres\n2. When?\n   → Now"}, // yes takes the recommendations
		{onePlan, "B", "yes", "Which store?\n   → SQLite"},                         // one question: a letter alone
		{onePlan, "a.", "yes", "→ Postgres"},
		{plan, "B", "change", "B"}, // two questions: a lone letter is not an answer
		{onePlan, "G", "change", "G"},
		{plan, "3C", "change", "3C"},
		{plan, "wait\n\n> approve", "change", "wait"},
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
