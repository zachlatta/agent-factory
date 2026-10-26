package factory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMail records zrl_bot_email_control calls. get_thread answers from threads; send_email starts
// thread "nt1"; errs fails a tool with that error.
type fakeMail struct {
	mu      sync.Mutex
	calls   []mailCall
	threads map[string][]threadMsg
	errs    map[string]error
}

type mailCall struct {
	Tool string
	Args map[string]any
}

func (m *fakeMail) Call(_ context.Context, tool string, args any) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := json.Marshal(args)
	var a map[string]any
	json.Unmarshal(raw, &a)
	m.calls = append(m.calls, mailCall{tool, a})
	if err := m.errs[tool]; err != nil {
		return nil, err
	}
	switch tool {
	case "get_thread":
		return json.Marshal(map[string]any{"messages": m.threads[a["thread_id"].(string)]})
	case "send_email":
		return json.RawMessage(`{"id":"nm1","thread_id":"nt1"}`), nil
	}
	return json.RawMessage(`{}`), nil
}

// sends are the calls that send mail (send_email, reply_to_message).
func (m *fakeMail) sends() []mailCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mailCall
	for _, c := range m.calls {
		if c.Tool == "send_email" || c.Tool == "reply_to_message" {
			out = append(out, c)
		}
	}
	return out
}

const testZach = "zach@example.com"

func mailFactory(t *testing.T, agents ...Agent) (*Factory, *fakePaseo, *fakeMail) {
	t.Helper()
	f, p := newTestFactory(t, agents...)
	m := &fakeMail{threads: map[string][]threadMsg{}, errs: map[string]error{}}
	f.Mail = m
	f.cfg.ZachEmail = testZach
	f.cfg.BoardURL = "https://board.example/"
	f.mailProbe.at = t0 // no health probe in these tests
	return f, p, m
}

// longPlan is a builder reply longer than the board's 6000-character Detail, ending in a question.
func longPlan() string {
	return "PLAN START\n" + strings.Repeat("a step of the plan\n", 400) + "PLAN END\n\n" +
		"QUESTION: Which store?\n- Postgres (recommended)\n- SQLite\n"
}

func TestBoardCardWaitIsEmailedWithFullDetail(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	f, p, m := mailFactory(t, c)
	p.logs["c1"] = longPlan()
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 || sends[0].Tool != "send_email" {
		t.Fatalf("sends = %+v", sends)
	}
	a := sends[0].Args
	text, html := a["text"].(string), a["html"].(string)
	if to := a["to"].([]any); len(to) != 1 || to[0] != testZach {
		t.Errorf("to = %v", to)
	}
	for _, want := range []string{"Stuck in Building", "PLAN START", "PLAN END", "card c1 (example/desk)",
		"1. Which store?", "A. Postgres (recommended)", "B. SQLite", `"drop"`, `"1A 2B"`,
		"Anything else goes to the agent", "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	if strings.Contains(text, "QUESTION:") {
		t.Error("questions are listed, not left as QUESTION: blocks")
	}
	for _, want := range []string{"PLAN END", "Open board", `href="https://board.example/#c1"`} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if !strings.HasPrefix(a["subject"].(string), "Needs you: card c1") {
		t.Errorf("subject = %v", a["subject"])
	}
	if l := p.labels("c1"); l[labelTold] != "stuck" || l[labelZachThread] != "nt1" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("the agent was messaged: %v", msgs)
	}
}

func TestApproveWordOnlyWhenYesApproves(t *testing.T) {
	plan := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "x"})
	f, _, _ := mailFactory(t, plan)
	c, _ := f.card("c1")
	ask := askFor(c, f.agent("c1"))
	words := replyWords(ask)
	if len(words) != 2 || words[0].Word != "approve" || !strings.Contains(words[0].Does, "Approve plan & build") {
		t.Errorf("plan words = %+v", words)
	}
	stuck := card("c2", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	f, _, _ = mailFactory(t, stuck)
	c, _ = f.card("c2")
	for _, w := range replyWords(askFor(c, f.agent("c2"))) {
		if w.Word == "approve" {
			t.Errorf("a stuck card offers approve: %+v", w)
		}
	}
}

func TestLaterWaitRepliesInTheCardsThread(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "landing not verified", labelTold: "stuck",
		labelZachThread: "nt1"})
	f, _, m := mailFactory(t, c)
	m.threads["nt1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}}
	f.Tick()
	s := m.sends()
	if len(s) != 1 || s[0].Tool != "reply_to_message" || s[0].Args["message_id"] != "m1" || s[0].Args["omit_quote"] != true {
		t.Fatalf("sends = %+v", s)
	}
	if to, _ := s[0].Args["to"].([]any); len(to) != 1 || to[0] != testZach {
		t.Errorf("a reply to the mailbox's own message must be addressed to Zach: %v", s[0].Args["to"])
	}

	// Zach wrote last: answer his message, which goes back to him
	f, _, m = mailFactory(t, c)
	m.threads["nt1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}, {ID: "m2", LabelIDs: []string{"INBOX"}}}
	f.Tick()
	if s := m.sends(); len(s) != 1 || s[0].Args["message_id"] != "m2" || s[0].Args["to"] != nil {
		t.Errorf("sends = %+v", s)
	}
}

func TestInboxCardIsEmailedInItsThread(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "zach", labelSource: "email", labelNote: "rework cap reached"})
	f, p, m := mailFactory(t, c)
	f.cfg.ZachEmail = "" // inbox cards need no address
	m.threads["t1"] = []threadMsg{{ID: "z1"}, {ID: "s1", LabelIDs: []string{"SENT"}}}
	f.Tick()
	s := m.sends()
	if len(s) != 1 || s[0].Tool != "reply_to_message" || s[0].Args["message_id"] != "z1" {
		t.Fatalf("sends = %+v", s)
	}
	if l := p.labels("c1"); l[labelTold] != "rework cap reached" || l[labelZachThread] != "" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("the agent was asked to email: %v", msgs)
	}
}

func TestEmailedOncePerWait(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan", labelTold: "stuck"})
	f, p, m := mailFactory(t, c)
	f.Tick()
	if s := m.sends(); len(s) != 0 {
		t.Errorf("told twice: %+v", s)
	}
	// once it leaves Needs Zach, the next wait is emailed even with the same note
	left := withLabels(c, map[string]string{labelStage: "building", labelSince: ago(10 * time.Second)})
	f, p, _ = mailFactory(t, left)
	f.Tick()
	if l := p.labels("c1"); l[labelTold] != "" || !contains(p.verbs("c1"), "update") {
		t.Errorf("told not cleared: %v", l)
	}
}

func TestNoEmailForHandMovesOrWithoutAddress(t *testing.T) {
	for name, c := range map[string]Agent{
		"hand move":                  card("c1", map[string]string{labelStage: "zach"}),
		"hand move with an old note": card("c1", map[string]string{labelStage: "zach", labelNote: "auto-landing (low)"}),
	} {
		f, _, m := mailFactory(t, c)
		f.Tick()
		if s := m.sends(); len(s) != 0 {
			t.Errorf("%s: emailed %+v", name, s)
		}
	}
	f, p, m := mailFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"}))
	f.cfg.ZachEmail = ""
	f.Tick()
	if s := m.sends(); len(s) != 0 || p.labels("c1")[labelTold] != "" {
		t.Errorf("emailed with no address: %+v", s)
	}
}

func TestArchitecturePlanEmailWaitsForTheSummary(t *testing.T) {
	f, p, m := mailFactory(t, card("c1", map[string]string{labelStage: "plan-review"}), reviewer("r1", "c1", "plan"))
	p.logs["r1"] = "TIER: architecture\nVERDICT: approve"
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelWriting] == "" || l[labelTold] != "" {
		t.Fatalf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || msgs[0] != summaryMsg {
		t.Errorf("sent = %v", msgs)
	}
	// the builder has not written the summary yet: no email
	waiting := withLabels(card("c1", nil), l)
	f.SetAgents([]Agent{waiting})
	f.Tick()
	if s := m.sends(); len(s) != 0 {
		t.Fatalf("emailed before the summary: %+v", s)
	}
	// it wrote it: one email carrying it
	done := waiting
	done.LastActivityAt = t0.Add(2 * time.Minute)
	f.Now = func() time.Time { return t0.Add(3 * time.Minute) }
	p.logs["c1"] = "SUMMARY for Zach"
	f.SetAgents([]Agent{done})
	f.Tick()
	if s := m.sends(); len(s) != 1 || !strings.Contains(s[0].Args["text"].(string), "SUMMARY for Zach") ||
		!strings.Contains(s[0].Args["text"].(string), "Approve this plan?") {
		t.Errorf("sends = %+v", s)
	}
}

func TestBriefEmailWaitsForTheBrief(t *testing.T) {
	labels := map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "2", labelSource: "email"}
	f, p, m := mailFactory(t)
	c := inboxCard("c1", labels)
	f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", "code", "changes", p)})
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	msgs := p.sent("c1")
	if l[labelTold] != "" || l[labelWriting] == "" || len(msgs) != 1 || strings.Contains(msgs[0], "/api/") {
		t.Errorf("labels = %v sent = %v", l, msgs)
	}
	f.SetAgents([]Agent{withLabels(c, l)})
	f.Tick()
	if s := m.sends(); len(s) != 0 {
		t.Errorf("emailed before the brief: %+v", s)
	}
}

func TestFailedSendRetriesAfterBackoff(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	f, p, m := mailFactory(t, c)
	m.errs["send_email"] = errors.New("send_email: exit 1: server down")
	f.Tick()
	if l := p.labels("c1"); l[labelTold] != "" {
		t.Errorf("told kept after a failed send: %v", l)
	}
	f.Tick()
	if n := len(m.sends()); n != 1 {
		t.Errorf("retried within the backoff: %d sends", n)
	}
	delete(m.errs, "send_email")
	f.Now = func() time.Time { return t0.Add(mailRetryAfter + time.Second) }
	f.Tick()
	if n := len(m.sends()); n != 2 {
		t.Errorf("not retried after the backoff: %d sends", n)
	}
}

func TestTimedOutSendIsNotRetried(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	f, p, m := mailFactory(t, c)
	m.errs["send_email"] = ErrMailMaybeSent
	f.Tick()
	if l := p.labels("c1"); l[labelTold] != "stuck" {
		t.Errorf("a send that may have gone out must stay told: %v", l)
	}
}

func TestGmailIDsAreSafeLabelValues(t *testing.T) {
	// Gmail message and thread ids are 16 hex digits: no comma, which would split a paseo --label
	msg, thread := strings.Repeat("a1", 8), strings.Repeat("0f", 8)
	if arg := labelArg(labelAnswered, msg); arg != labelPrefix+labelAnswered+"="+msg {
		t.Errorf("labelArg = %q", arg)
	}
	f, p := newTestFactory(t, card("c1", nil))
	if err := f.setLabels("c1", map[string]string{labelAnswered: msg, labelZachThread: thread}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelAnswered] != msg || l[labelZachThread] != thread {
		t.Errorf("labels = %v", l)
	}
}

func TestHealthReportsMailWithoutFailing(t *testing.T) {
	f, _, m := mailFactory(t)
	f.mailProbe.at = time.Time{}
	m.errs["get_mailbox_info"] = errors.New("pdw: not logged in")
	f.probeMail()
	h := f.Health()
	if !strings.Contains(h.Mail, "not logged in") || !h.Live {
		t.Errorf("health = %+v", h)
	}
}
