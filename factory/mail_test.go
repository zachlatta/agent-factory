package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMail records zrl_bot_email_control calls. get_thread answers from threads; send_email starts
// thread "nt1"; errs fails a tool with that error; attachmentErr fails when attachments is set.
type fakeMail struct {
	mu            sync.Mutex
	calls         []mailCall
	threads       map[string][]threadMsg
	errs          map[string]error
	attachmentErr error
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
	if m.attachmentErr != nil && a["attachments"] != nil {
		return nil, m.attachmentErr
	}
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
// histWait is a Needs Zach wait that is still emailed, used where a test needs any such wait.
const histWait = "history unavailable: decision paused"

func longPlan() string {
	return "PLAN START\n" + strings.Repeat("a step of the plan\n", 400) + "PLAN END\n\n" +
		"QUESTION: Which store?\n- Postgres (recommended)\n- SQLite\n"
}

const planNote = sideEffectNote

// planWait is a card waiting for Zach to approve its plan: a decision, so it is emailed.
func planWait(id string, extra map[string]string) Agent {
	l := map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: planNote}
	for k, v := range extra {
		l[k] = v
	}
	return card(id, l)
}

func TestDecisionEmailIsShortAndPlain(t *testing.T) {
	c := planWait("c1", nil)
	c.Title = "Accept schedule handoffs before retirement is turned on"
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", "DECISION: Approve building the handoff checker?\n"+
		"OVERVIEW: Adds a checker in `factory/retire.go` for zachlatta/agent-factory, commit 0f1bc63ab.\n"+
		"NEEDS FROM ZACH: approve the plan.\n\n"+longPlan())
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
	if a["subject"] != "Approve building the handoff checker?" {
		t.Errorf("the subject must be the question: %v", a["subject"])
	}
	for _, want := range []string{"I recommend A.", "Which store?", "A. Postgres (recommended)", "B. SQLite",
		"Reply with a letter", `"yes" to allow & build`, `"drop" to stop it`, "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	body := text[:strings.Index(text, "Open the board:")]
	if n := wordCount(body); n >= 60 {
		t.Errorf("a decision email is under 60 words, got %d:\n%s", n, body)
	}
	for _, banned := range []string{"PLAN START", "factory/retire.go", "zachlatta/", "0f1bc63ab", "`", "QUESTION:",
		"Card", "card c1", "Details", "Original task"} {
		if strings.Contains(text, banned) || strings.Contains(html, banned) {
			t.Errorf("email has %q:\n%s", banned, text)
		}
	}
	if !strings.Contains(html, "Open board") || !strings.Contains(html, `href="https://board.example/#c1"`) {
		t.Errorf("html lacks the board button:\n%s", html)
	}
	if l := p.labels("c1"); l[labelTold] != planNote || l[labelZachThread] != "nt1" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("the agent was messaged: %v", msgs)
	}
}

// Factory copy in every emailed ask avoids pipeline words, and every body stays under 60 words.
func TestEmailedAsksUsePlainWords(t *testing.T) {
	reply := "Built it.\n\nQUESTION: Which colour should the new button be on the phone page?\n" +
		"- A calm blue that matches the rest of the page (recommended)\n- A bright red that stands out from everything\n" +
		"- Green like the old one used to be before\n- Grey so it fades into the page background\n- Black\n- White\n"
	for _, note := range []string{planNote, readyNote(1), "review passed: approve to land", "secret scan found 1 hit",
		"public repo: finished without review"} {
		a := card("c1", map[string]string{labelStage: "zach", labelNote: note})
		if note == planNote {
			a = planWait("c1", nil)
		}
		a.Title = "Board buttons get clearer labels"
		f, p, m := mailFactory(t, a)
		storeLog(t, f, p, "c1", reply)
		f.Tick()
		s := m.sends()
		if len(s) != 1 {
			t.Errorf("%s: sends = %+v", note, s)
			continue
		}
		text := s[0].Args["text"].(string)
		body := text[:strings.Index(text, "Open the board:")]
		if n := wordCount(body); n >= 60 {
			t.Errorf("%s: %d words:\n%s", note, n, body)
		}
		for _, w := range []string{"card", "track", "landing", "fellow", "land "} {
			if strings.Contains(strings.ToLower(s[0].Args["subject"].(string)+" "+body), w) {
				t.Errorf("%s: email uses %q:\n%s\n%s", note, w, s[0].Args["subject"], body)
			}
		}
	}
}

func TestApproveWordOnlyWhenYesApproves(t *testing.T) {
	f, _, _ := mailFactory(t, planWait("c1", nil))
	c, _ := f.card("c1")
	ask := askFor(c, f.agent("c1"))
	words := replyWords(ask)
	if len(words) != 3 || words[0].Word != "yes" || words[0].Does != "allow & build" || words[1].Word != "no" || words[2].Word != "drop" {
		t.Errorf("plan words = %+v", words)
	}
	stuck := card("c2", map[string]string{labelStage: "zach", labelNote: "landed; still to do: x"})
	f, _, _ = mailFactory(t, stuck)
	c, _ = f.card("c2")
	var hasDone bool
	for _, w := range replyWords(askFor(c, f.agent("c2"))) {
		if w.Word == "yes" {
			t.Errorf("a stuck card offers yes: %+v", w)
		}
		hasDone = hasDone || w.Word == "done"
	}
	if !hasDone {
		t.Error("a card with Mark done must offer the done reply word")
	}
}
func TestLaterWaitRepliesInTheCardsThread(t *testing.T) {
	c := planWait("c1", map[string]string{labelTold: "stuck", labelZachThread: "nt1"})
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

	// Even when Zach replied earlier in a board card's thread (e.g. from an older address), later
	// waits on labelZachThread must still address f.cfg.ZachEmail.
	f, _, m = mailFactory(t, c)
	m.threads["nt1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}, {ID: "m2", LabelIDs: []string{"INBOX"}}}
	f.Tick()
	s = m.sends()
	if len(s) != 1 || s[0].Args["message_id"] != "m2" {
		t.Fatalf("sends = %+v", s)
	}
	if to, _ := s[0].Args["to"].([]any); len(to) != 1 || to[0] != testZach {
		t.Errorf("board-card thread reply must set to=%s even after a reply in thread: %v", testZach, s[0].Args["to"])
	}
}

func TestInboxCardIsEmailedInItsThread(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "zach", labelSource: "email", labelNote: readyNote(1)})
	f, p, m := mailFactory(t, c)
	f.cfg.ZachEmail = "bot@example.com" // inbox cards reply to the sender in thread, not ZachEmail
	m.threads["t1"] = []threadMsg{{ID: "z1"}, {ID: "s1", LabelIDs: []string{"SENT"}}}
	f.Tick()
	s := m.sends()
	if len(s) != 1 || s[0].Tool != "reply_to_message" || s[0].Args["message_id"] != "z1" || s[0].Args["to"] != nil {
		t.Fatalf("sends = %+v", s)
	}
	if l := p.labels("c1"); l[labelTold] != readyNote(1) || l[labelZachThread] != "" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("the agent was asked to email: %v", msgs)
	}
}

func TestEmailedOncePerWait(t *testing.T) {
	c := planWait("c1", map[string]string{labelTold: planNote})
	f, p, m := mailFactory(t, c)
	f.Tick()
	if s := m.sends(); len(s) != 0 {
		t.Errorf("told twice: %+v", s)
	}
	// once it leaves Needs Zach, the next wait is emailed even with the same note
	left := withLabels(c, map[string]string{labelStage: "building", labelAwaiting: "", labelSince: ago(10 * time.Second)})
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
	f, p, m := mailFactory(t, planWait("c1", nil))
	f.cfg.ZachEmail = ""
	f.Tick()
	if s := m.sends(); len(s) != 0 || p.labels("c1")[labelTold] != "" {
		t.Errorf("emailed with no address: %+v", s)
	}
}

// Problems the factory or an agent can sort out show on the board only; decisions are emailed.
func TestWhichWaitsAreEmailed(t *testing.T) {
	running := func(a Agent, pending string) Agent { a.Status, a.Pending = "running", pending; return a }
	attention := func(reason string) Agent { // the finished-card ask
		a := card("c1", map[string]string{})
		a.RequiresAttention, a.AttentionReason = true, reason
		return a
	}
	stuckReviewer := reviewer("r1", "c1", "code")
	stuckReviewer.Status, stuckReviewer.Pending = "running", "Bash"
	for _, tc := range []struct {
		name  string
		agent Agent
		extra []Agent
		mail  bool
	}{
		{"landed with work left", card("c1", map[string]string{labelStage: "zach", labelNote: "landed; still to do: browser check"}), nil, false},
		{"landed did it finish", card("c1", map[string]string{labelStage: "zach", labelNote: "landed; did not say what remains"}), nil, false},
		{"landed diff differs, older wait", card("c1", map[string]string{labelStage: "zach", labelNote: "landed diff differs from the reviewed one: x"}), nil, false},
		{"landed diff differs, public scan failed", func() Agent {
			a := card("c1", map[string]string{labelStage: "zach",
				labelNote: "landed diff differs from the reviewed one: x" + landDiffScanMark + "found: tailnet-hostname a.go:3"})
			a.Repo = publicRepo
			return a
		}(), nil, true},
		{"stuck in its column", card("c1", map[string]string{labelStage: "building", labelStuck: stuckStopped}), nil, false},
		{"stuck reviewer", card("c1", map[string]string{labelStage: "review", labelStuck: stuckReview}), []Agent{stuckReviewer}, false},
		{"agent error", card("c1", map[string]string{labelStage: "building", labelStuck: stuckAgentError}), nil, false},
		{"reviewed commit unverified", card("c1", map[string]string{labelStage: "zach", labelNote: noReviewedNote, labelStuckFrom: "review"}), nil, false},
		{"parked changed since review", card("c1", map[string]string{labelStage: "zach", labelNote: parkedStaleNote, labelStuckFrom: "review"}), nil, false},
		{"history unavailable", card("c1", map[string]string{labelStage: "zach", labelNote: "history unavailable: decision paused"}), nil, false},
		{"duplicate close", card("c1", map[string]string{labelStage: "zach", labelVerdict: "close", labelNote: "reviewer suggests closing"}), nil, false},
		{"permission prompt", running(card("c1", map[string]string{labelStage: "zach"}), "Bash"), nil, false},
		{"finished", attention("finished"), nil, false},
		{"allow a build", planWait("c1", nil), nil, true},
		{"preview", card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1)}), nil, true},
		{"secret scan", card("c1", map[string]string{labelStage: "zach", labelNote: "secret scan found 1 hit"}), nil, true},
		{"older brief", card("c1", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}), nil, false},
		{"did not settle", card("c1", map[string]string{labelStage: "zach", labelNote: arbiterStuckNote}), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, m := mailFactory(t, append([]Agent{tc.agent}, tc.extra...)...)
			storeLog(t, f, p, "c1", "DECISION: Pick a colour?\nOVERVIEW: Changes the button.\nNEEDS FROM ZACH: choose.\n\nReport.")
			c, ok := f.card("c1")
			if !ok || c.Ask == nil {
				t.Fatalf("no ask: %+v", c)
			}
			if c.Ask.BoardOnly == tc.mail {
				t.Errorf("BoardOnly = %v for %q", c.Ask.BoardOnly, c.Ask.Title)
			}
			f.Tick()
			if got := len(m.sends()) == 1; got != tc.mail {
				t.Errorf("emailed = %v, want %v: %+v", got, tc.mail, m.sends())
			}
		})
	}
}

// A plan approval is emailed with its own question even when the agent wrote that it needs nothing.
func TestNeedsNothingDecisionUsesTheAsksQuestion(t *testing.T) {
	// a plan approval is emailed even when the agent wrote that it needs nothing
	f, p, m := mailFactory(t, planWait("c2", nil))
	storeLog(t, f, p, "c2", "DECISION: None before building.\nOVERVIEW: x.\nNEEDS FROM ZACH: Nothing now.\n\nPlan.")
	f.Tick()
	if s := m.sends(); len(s) != 1 || s[0].Args["subject"] != "Allow this build?" {
		t.Errorf("a plan approval must be emailed with its own question: %+v", s)
	}
}

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"Fix `factory/policy.go:119-135` in zachlatta/agent-factory (0f1bc63).":        "Fix in agent-factory.",
		"Both live in example/other-repo now":                                          "Both live in other-repo now",
		"Try https://github.com/example/other-repo/pull/1":                             "Try https://github.com/example/other-repo/pull/1",
		"Wi-Fi/cellular and/or the agent 1b2c3d4e-0000-4000-8000-000000000001 stopped": "Wi-Fi/cellular and/or the agent stopped",
		"see https://example.com/a/b and README.md":                                    "see https://example.com/a/b and",
		"Ship the board deadline change":                                               "Ship the board deadline change",
	} {
		if got := plainText(in, "zachlatta/agent-factory"); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}
func TestSideEffectEmailWaitsForTheDecision(t *testing.T) {
	f, p, m := mailFactory(t, card("c1", map[string]string{labelStage: "plan-review"}), reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: writes prod rows\nTIER: architecture\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelWriting] == "" || l[labelTold] != "" {
		t.Fatalf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.HasPrefix(msgs[0], sideEffectMsg) {
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
	storeLog(t, f, p, "c1", "DECISION: Approve building the new page?\nOVERVIEW: SUMMARY for Zach.\nNEEDS FROM ZACH: approve.")
	f.SetAgents([]Agent{done})
	f.Tick()
	if s := m.sends(); len(s) != 1 || !strings.Contains(s[0].Args["text"].(string), "SUMMARY for Zach") ||
		s[0].Args["subject"] != "Approve building the new page?" {
		t.Errorf("sends = %+v", s)
	}
}

func TestReviewCapGoesToTheArbiterWithoutEmail(t *testing.T) {
	labels := map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "2", labelSource: "email"}
	f, p, m := mailFactory(t)
	c := inboxCard("c1", labels)
	f.SetAgents([]Agent{c, longReview(t, f, "r1", "c1", "code", "changes", p)})
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "review" || l[labelNote] != arbiterNote || len(p.sent("c1")) != 0 {
		t.Errorf("labels = %v sent = %v", l, p.sent("c1"))
	}
	if s := m.sends(); len(s) != 0 {
		t.Errorf("emailed Zach about agents disagreeing: %+v", s)
	}
}

func TestFailedSendRetriesAfterBackoff(t *testing.T) {
	c := planWait("c1", nil)
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
	c := planWait("c1", nil)
	f, p, m := mailFactory(t, c)
	m.errs["send_email"] = ErrMailMaybeSent
	f.Tick()
	if l := p.labels("c1"); l[labelTold] != planNote {
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

func TestCloseVerdictIsBoardOnlyAndExplainsWhy(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelVerdict: "close", labelNote: "reviewer suggests closing"})
	r := reviewer("r1", "c1", "plan")
	r.Labels[labelPrefix+labelApplied] = "1"
	r.Provider, r.SessionID, r.Cwd = "claude", "s-oom", "/w/c1"
	f, _, m := mailFactory(t, c, r)
	lines := []string{
		`{"type":"user","message":{"content":"Review this plan."}}`,
		`{"type":"assistant","timestamp":"2026-10-26T04:03:09Z","message":{"content":[{"type":"thinking","thinking":"Checking git log."},{"type":"tool_use","id":"u1","name":"Bash","input":{"command":"git log"}}]}}`,
		`{"type":"user","timestamp":"2026-10-26T04:03:10Z","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"RAW_TOOL_RESULT_SECRET"}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-c1/s-oom.jsonl", strings.Join(lines, "\n")+"\n")
	f.SetAgents([]Agent{c, r})
	f.Tick()
	if s := m.sends(); len(s) != 0 {
		t.Fatalf("a close verdict is board-only: %+v", s)
	}
	text := boardDetail(t, f, "c1")
	for _, want := range []string{
		"Stopped mid-run before writing a reply (last tool: Bash at 00:03:09 EDT).",
		"Last reasoning:\nChecking git log.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("email text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "RAW_TOOL_RESULT_SECRET") || strings.Contains(text, "(The agent's last message could not be read. Open the board to see it.)") {
		t.Errorf("email text leaked raw tool output or used bare fallback:\n%s", text)
	}

	// Missing transcript file says why it could not be read.
	c2 := card("c2", map[string]string{labelStage: "zach", labelVerdict: "close", labelNote: "reviewer suggests closing"})
	r2 := reviewer("r2", "c2", "code")
	r2.Labels[labelPrefix+labelApplied] = "1"
	r2.Provider, r2.SessionID, r2.Cwd = "claude", "missing-id", "/w/c2"
	f2, _, _ := mailFactory(t, c2, r2)
	text2 := boardDetail(t, f2, "c2")
	if !strings.Contains(text2, "could not be read from the transcript") || !strings.Contains(text2, "missing-id") {
		t.Errorf("email text for missing transcript = %s", text2)
	}
}

// boardDetail is the full detail the board shows for card id's ask.
func boardDetail(t *testing.T, f *Factory, id string) string {
	t.Helper()
	c, ok := f.card(id)
	if !ok || c.Ask == nil {
		t.Fatalf("no ask on %s", id)
	}
	detail, _, _ := f.askDetail(c, c.Ask)
	return detail
}

func TestEmailIncludesInlineEvidenceWithFallback(t *testing.T) {
	dir := t.TempDir()
	evDir := filepath.Join(dir, ".factory-evidence")
	if err := os.Mkdir(evDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evDir, "before.png"), pngBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evDir, "after.png"), pngBuf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	c := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build"})
	c.Provider, c.SessionID, c.Cwd = "claude", "s-req", dir
	f, p, m := mailFactory(t, c)
	replyBody := `DECISION: Approve the email layout?\nOVERVIEW: Adds the original request and screenshots.\nNEEDS FROM ZACH: Approve or ask for changes.\n\nBEFORE: before.png\nAFTER: after.png\n\nDetailed plan text.`
	sessionLines := `{"type":"user","message":{"content":"You are the Fellow on Zach's agent factory board: you own this card from plan to landing.\n\nZach's request:\nMake the Needs Zach emails show the original request and screenshots.\n\nWork in this repo (example/desk). Read its AGENTS.md and README first."}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + replyBody + `"}]}}` + "\n"
	writeFile(t, filepath.Join(f.cfg.Home, ".claude", "projects", strings.ReplaceAll(dir, "/", "-"), "s-req.jsonl"), sessionLines)
	storeLog(t, f, p, "c1", strings.ReplaceAll(replyBody, `\n`, "\n"))
	f.SetAgents([]Agent{c})
	f.Tick()

	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	text, html := sends[0].Args["text"].(string), sends[0].Args["html"].(string)
	if !strings.Contains(text, "Before screenshot attached.") || sends[0].Args["subject"] != "Approve the email layout?" {
		t.Errorf("text lacks the screenshots or the question:\n%s", text)
	}
	if strings.Contains(text, "BEFORE: before.png") || strings.Contains(html, "BEFORE: before.png") {
		t.Errorf("raw BEFORE: line was not stripped:\n%s", text)
	}
	if !strings.Contains(html, `src="cid:before"`) || !strings.Contains(html, `src="cid:after"`) {
		t.Errorf("html lacks inline cid images:\n%s", html)
	}
	atts, ok := sends[0].Args["attachments"].([]any)
	if !ok || len(atts) != 2 {
		t.Fatalf("attachments = %#v", sends[0].Args["attachments"])
	}

	// If sending with attachments fails (e.g. size limit), deliver retries without attachments.
	c2 := card("c2", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build"})
	c2.Cwd = dir
	f2, p2, m2 := mailFactory(t, c2)
	m2.attachmentErr = errors.New("attachment too large")
	storeLog(t, f2, p2, "c2", strings.ReplaceAll(replyBody, `\n`, "\n"))
	f2.Tick()
	sends2 := m2.sends()
	if len(sends2) != 2 {
		t.Fatalf("expected retry without attachments, got %d sends: %+v", len(sends2), sends2)
	}
	if sends2[1].Args["attachments"] != nil || strings.Contains(sends2[1].Args["html"].(string), `src="cid:before"`) {
		t.Errorf("fallback send still included attachments or cid img: %+v", sends2[1].Args)
	}
	if p2.labels("c2")[labelTold] == "" {
		t.Errorf("fallback send did not mark card told: %v", p2.labels("c2"))
	}
}

// Every factory email ends with the board link, on the one path all of them take (sendMail).
func TestEveryEmailHasTheBoardLink(t *testing.T) {
	f, _, m := mailFactory(t, planWaiting("c1"))
	ctx := context.Background()
	if _, err := f.sendMail(ctx, "c1", "reply_to_message", map[string]any{"message_id": "m1", "text": "Done.", "html": "<p>Done.</p>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sendMail(ctx, "", "send_email", map[string]any{"text": "Today", "html": "<p>Today</p>"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sendMail(ctx, "c1", "reply_to_message", map[string]any{"text": "See https://board.example/#c1 now"}); err != nil {
		t.Fatal(err)
	}
	s := m.sends()
	if len(s) != 3 {
		t.Fatalf("sends = %+v", s)
	}
	for i, want := range []string{"https://board.example/#c1", "https://board.example/", "https://board.example/#c1"} {
		text := s[i].Args["text"].(string)
		if !strings.Contains(text, want) || strings.Count(text, "board.example") != 1 {
			t.Errorf("email %d text = %q, want one link %s", i, text, want)
		}
		if h, ok := s[i].Args["html"].(string); ok && !strings.Contains(h, `href="`+want+`"`) {
			t.Errorf("email %d html = %q", i, h)
		}
	}
}
