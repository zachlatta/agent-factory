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
func longPlan() string {
	return "PLAN START\n" + strings.Repeat("a step of the plan\n", 400) + "PLAN END\n\n" +
		"QUESTION: Which store?\n- Postgres (recommended)\n- SQLite\n"
}

func TestBoardCardWaitIsEmailedWithFullDetail(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", longPlan())
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
		"1. Which store?", "A. Postgres (recommended)", "B. SQLite", `"done"`, `"drop"`, `"1A 2B"`,
		"Anything else goes to the agent", "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	if strings.Index(text, "How to answer") > strings.Index(text, "PLAN START") {
		t.Errorf("choices and reply words must appear before the long report:\n%s", text)
	}
	if strings.Index(html, "Open board") > strings.Index(html, "PLAN START") {
		t.Errorf("Open board must appear before the long report in HTML:\n%s", html)
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
	var hasDone bool
	for _, w := range replyWords(askFor(c, f.agent("c2"))) {
		if w.Word == "approve" {
			t.Errorf("a stuck card offers approve: %+v", w)
		}
		if w.Word == "done" && strings.Contains(w.Does, "Mark done") {
			hasDone = true
		}
	}
	if !hasDone {
		t.Error("a card with Mark done must offer the done reply word")
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
	c := inboxCard("c1", map[string]string{labelStage: "zach", labelSource: "email", labelNote: "rework cap reached"})
	f, p, m := mailFactory(t, c)
	f.cfg.ZachEmail = "bot@example.com" // inbox cards reply to the sender in thread, not ZachEmail
	m.threads["t1"] = []threadMsg{{ID: "z1"}, {ID: "s1", LabelIDs: []string{"SENT"}}}
	f.Tick()
	s := m.sends()
	if len(s) != 1 || s[0].Tool != "reply_to_message" || s[0].Args["message_id"] != "z1" || s[0].Args["to"] != nil {
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
	storeLog(t, f, p, "r1", "TIER: architecture\nVERDICT: approve")
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
	storeLog(t, f, p, "c1", "SUMMARY for Zach")
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

func TestNoVerdictEmailLoadsReviewerTranscriptOrExplainsWhy(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "plan-review"})
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
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	text := sends[0].Args["text"].(string)
	for _, want := range []string{
		"The review didn't finish properly",
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
	c2 := card("c2", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "review"})
	r2 := reviewer("r2", "c2", "code")
	r2.Labels[labelPrefix+labelApplied] = "1"
	r2.Provider, r2.SessionID, r2.Cwd = "claude", "missing-id", "/w/c2"
	f2, _, m2 := mailFactory(t, c2, r2)
	f2.Tick()
	sends2 := m2.sends()
	if len(sends2) != 1 {
		t.Fatalf("sends2 = %+v", sends2)
	}
	text2 := sends2[0].Args["text"].(string)
	if !strings.Contains(text2, "could not be read from the transcript") || !strings.Contains(text2, "missing-id") {
		t.Errorf("email text for missing transcript = %s", text2)
	}
}

func TestEmailHeaderKeepsOverviewExceptOnFactoryChecks(t *testing.T) {
	reply := "DECISION: Nothing to decide; it is landed and live.\n" +
		"OVERVIEW: Updated the email layout and tests.\n" +
		"NEEDS FROM ZACH: Nothing.\n\n" +
		"Full build report body."
	for _, tc := range []struct {
		name         string
		agent        Agent
		extra        []Agent
		keepDecision bool
		wantTitle    string
	}{
		{
			name:         "landed with work left",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "landed; still to do: browser check"}),
			keepDecision: false,
			wantTitle:    "Landed, with work left",
		},
		{
			name:         "landed did not say what remains",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "landed; did not say what remains"}),
			keepDecision: false,
			wantTitle:    "Landed: did it finish?",
		},
		{
			name:         "landing not verified",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "landing not verified"}),
			keepDecision: false,
			wantTitle:    "Landing didn't reach main",
		},
		{
			name:         "landed diff differs",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "landed diff differs from the reviewed one: extra commit"}),
			keepDecision: false,
			wantTitle:    "Landed something other than what was reviewed",
		},
		{
			name:         "stuck builder",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"}),
			keepDecision: false,
			wantTitle:    "Stuck in Building",
		},
		{
			name:  "stuck reviewer on permission",
			agent: card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "review"}),
			extra: func() []Agent {
				r := reviewer("r1", "c1", "code")
				r.Status, r.Pending = "running", "Bash"
				return []Agent{r}
			}(),
			keepDecision: false,
			wantTitle:    "Reviewer stuck on a permission prompt",
		},
		{
			name:         "secret scan",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "secret scan found 1 hit"}),
			keepDecision: false,
			wantTitle:    "Secret scan stopped this landing",
		},
		{
			name:         "public repo finished without review",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: "public repo: finished without review"}),
			keepDecision: false,
			wantTitle:    "Pushed to a public repo without review",
		},
		{
			name:         "plan approval",
			agent:        card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build"}),
			keepDecision: true,
			wantTitle:    "Approve this plan?",
		},
		{
			name:         "dependency wait",
			agent:        card("c1", map[string]string{labelStage: "zach", labelNote: depAskNote("d0e0001", depDropped)}),
			keepDecision: true,
			wantTitle:    "The card this waits on was dropped",
		},
		{
			name: "waiting on you attention",
			agent: func() Agent {
				a := card("c1", map[string]string{})
				a.RequiresAttention, a.AttentionReason = true, "question"
				return a
			}(),
			keepDecision: true,
			wantTitle:    "Waiting on you: question",
		},
		{
			name: "finished read result",
			agent: func() Agent {
				a := card("c1", map[string]string{})
				a.RequiresAttention, a.AttentionReason = true, "finished"
				return a
			}(),
			keepDecision: true,
			wantTitle:    finishedTitle,
		},
		{
			name:         "moved here by hand",
			agent:        card("c1", map[string]string{labelStage: "zach"}),
			keepDecision: true,
			wantTitle:    handTitle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := append([]Agent{tc.agent}, tc.extra...)
			f, p, _ := mailFactory(t, agents...)
			storeLog(t, f, p, "c1", reply)
			storeLog(t, f, p, "r1", reply)
			c, ok := f.card("c1")
			if !ok {
				t.Fatal("missing card c1")
			}
			a := f.agent("c1")
			ask := askFor(c, a)
			if ask == nil {
				t.Fatal("nil ask")
			}
			detail, qs, ov := f.askDetail(c, ask)
			ask.Questions, ask.Overview = qs, ov
			em := f.composeEmail(a, c, ask, detail)
			if !strings.Contains(em.Text, tc.wantTitle) || !strings.Contains(em.Text, "Updated the email layout and tests.") {
				t.Errorf("missing title %q or overview in:\n%s", tc.wantTitle, em.Text)
			}
			hasDecision := strings.Contains(em.Text, "Nothing to decide; it is landed and live.") ||
				strings.Contains(em.Text, "Needs from you: Nothing.") ||
				strings.Contains(em.HTML, "Needs from you:")
			if hasDecision != tc.keepDecision {
				t.Errorf("keepDecision = %v, got hasDecision = %v:\n%s", tc.keepDecision, hasDecision, em.Text)
			}
		})
	}
}

func TestEmailIncludesOriginalTaskAndInlineEvidenceWithFallback(t *testing.T) {
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
	if !strings.Contains(text, "Original task:\nMake the Needs Zach emails show the original request and screenshots.") {
		t.Errorf("text lacks Original task:\n%s", text)
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
	storeLog(t, f2, p2, "c2", p.logs["c1"])
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
