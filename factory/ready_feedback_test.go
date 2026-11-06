package factory

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestReadyFeedbackUnchanged(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1",
		labelCodeRounds: "1", labelReady: "1", labelReadyHead: reviewedSHA, labelReadyFeedback: "exchange-2", labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c, reviewer("r2", "c1", "code"))
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r2", "TIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r2"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || f.agent("c1").L(labelReady) != "1" || !isFeedbackNote(l[labelNote]) || l[labelWriting] != "" {
		t.Fatalf("labels = %v", l)
	}
	if !readyStop(labeled(c, l)) {
		t.Fatal("feedback answer must retain finished-work controls")
	}
}

func TestReadyFeedbackMail(t *testing.T) {
	c := inThread(card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " exchange-2", labelReady: "1"}), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	storeLog(t, f, p, "c1", "DECISION: None.\nOVERVIEW: The preview needs a reload.\nNEEDS FROM ZACH: Nothing.\n\nReload the preview to see the update.")
	f.Tick()
	f.SetAgents([]Agent{labeled(c, p.labels("c1"))})
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %v", sends)
	}
	restarted, _, after := mailFactory(t, labeled(c, p.labels("c1")))
	restarted.Tick()
	if len(after.sends()) != 0 {
		t.Fatal("restart repeated feedback email")
	}
	text := sends[0].Args["text"].(string)
	if !strings.Contains(text, "Reload the preview") || strings.Contains(text, "Ready to try:") {
		t.Fatal(text)
	}
}

func TestFeedbackApprovalLands(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " reply-1", labelZachThread: "nt1", labelReviewed: reviewedSHA})
	f, p, _ := mailFactory(t, c)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", "The preview needs a reload.")
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m3", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if p.labels("c1")[labelStage] != "landing" {
		t.Fatal(p.labels("c1"))
	}
}

func TestFeedbackRevisionGate(t *testing.T) {
	for _, tc := range []struct {
		name, head, dirty string
		fresh             bool
	}{
		{"changed", movedSHA, "", true}, {"unknown", "", "", true}, {"dirty", reviewedSHA, " M code.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1", labelReady: "1", labelReadyHead: tc.head, labelReadyFeedback: "reply-1", labelReviewed: reviewedSHA})
			f, p := newTestFactory(t, c)
			headIs(f, reviewedSHA, nil)
			dirtyIs(f, tc.dirty, nil)
			if err := f.passCode(c, map[string]string{}, "code approve", "architecture", "", ""); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if isFeedbackNote(l[labelNote]) {
				t.Fatal("unsafe suppression", l)
			}
			if tc.fresh && l[labelNote] != readyNote(2) {
				t.Fatal(l)
			}
		})
	}
}

// A feedback reply queues a new generation while retaining the presentation.
func TestReadyFeedbackQueuesReview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelReadyHead: reviewedSHA, labelCodeRounds: "2"})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Explain how to open the preview"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelAutoReview] != "code" || l[labelReadyFeedback] == "" || l[labelCodeRoundsFrom] != "2" {
		t.Fatal(l)
	}
	sent := p.sent("c1")
	if len(sent) != 1 || !strings.Contains(sent[0], "Explain how to open") || !strings.Contains(sent[0], "Do not repeat the Ready summary") {
		t.Fatal(sent)
	}
	for _, want := range []string{"only if his reply leaves nothing for him to decide", "lands with no further ask", "told you to wait or hold", "keep the ask"} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("the rule lacks %q: %s", want, sent[0])
		}
	}
}

func TestReadyFeedbackStateCarried(t *testing.T) {
	old := card("old", map[string]string{labelReady: "2", labelReadyHead: reviewedSHA, labelReadyFeedback: "reply-2", labelAskWhy: "requested"})
	next := copyCarriedLabels(old)
	old.Labels[labelPrefix+labelFeedbackHold], old.Labels[labelPrefix+labelFeedbackTold] = "it is an architecture change", "reply-2"
	next = copyCarriedLabels(old)
	for _, k := range []string{labelReady, labelReadyHead, labelReadyFeedback, labelAskWhy, labelFeedbackHold, labelFeedbackTold} {
		if next[labelPrefix+k] != old.L(k) {
			t.Fatalf("%s lost: %v", k, next)
		}
	}
}

const answerNothing = "DECISION: None.\nOVERVIEW: The totals add up.\nNEEDS FROM ZACH: Nothing.\n\nThe totals add up each night."

func TestFeedbackAsksNothing(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		want        bool
	}{
		{"none and nothing", answerNothing, true},
		{"markdown", "**DECISION:** None\n**OVERVIEW:** x.\n**NEEDS FROM ZACH:** Nothing.", true},
		{"none and none", "DECISION: None.\nOVERVIEW: x.\nNEEDS FROM ZACH: None.\nREMAINING: none", true},
		{"a real decision", "DECISION: Should this also cover the phone page?\nOVERVIEW: x.\nNEEDS FROM ZACH: Nothing.", false},
		{"the bare decision", "DECISION: Land this change?\nOVERVIEW: x.\nNEEDS FROM ZACH: Nothing.", false},
		{"a try", "DECISION: None.\nOVERVIEW: x.\nNEEDS FROM ZACH: Try the preview.\nREMAINING: none", false},
		{"a hold", "DECISION: None.\nOVERVIEW: x.\nNEEDS FROM ZACH: Nothing yet; holding until the other work is done.", false},
		{"an ask after nothing", "DECISION: None.\nOVERVIEW: x.\nNEEDS FROM ZACH: Nothing, but reply if you disagree.", false},
		{"a question", answerNothing + "\n\nQUESTION: Which?\n- A\n- B\n", false},
		{"an item for Zach", answerNothing + "\nREMAINING: zach: check the page", false},
		{"an item for an agent", answerNothing + "\nREMAINING: agent: finish the docs", false},
		{"no overview", "The totals add up each night.", false},
		{"no needs line", "DECISION: None.\nOVERVIEW: x.", false},
		{"no decision line", "OVERVIEW: x.\nNEEDS FROM ZACH: Nothing.", false},
		{"empty", "", false},
	} {
		if got := feedbackAsksNothing(tc.reply); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// feedbackCard waits for its review verdict after a feedback answer on the version Zach was shown.
func feedbackCard(extra map[string]string) Agent {
	l := map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "1", labelReady: "1",
		labelReadyHead: reviewedSHA, labelReadyFeedback: "exchange-2", labelReviewed: reviewedSHA, labelAskWhy: "you asked for it"}
	for k, v := range extra {
		l[k] = v
	}
	return card("c1", l)
}

func TestFeedbackAnswerGoesAheadOrWaits(t *testing.T) {
	const passes = "ALWAYS ASK: none\nTIER: low\nVERDICT: approve"
	const goesAhead = "Nothing more is needed from you. It goes ahead on its own."
	const replyPrompt = "Reply \"yes\" or \"land\""
	for _, tc := range []struct {
		name, reply, review, tier, asked string
		extra                            map[string]string
		landing                          bool
		line                             string
		hold                             string
	}{
		{name: "asks nothing", reply: answerNothing, review: passes, tier: "low", landing: true, line: goesAhead},
		{name: "asks something", reply: "DECISION: Land the totals?\nOVERVIEW: x.\nNEEDS FROM ZACH: Reply yes.", review: passes, tier: "low",
			line: replyPrompt + " to ship it"},
		{name: "architecture", reply: answerNothing, review: passes, tier: "architecture",
			line: "This still needs your yes before it goes ahead: it is an architecture change. " + replyPrompt, hold: "it is an architecture change"},
		{name: "reviewer says it needs him", reply: answerNothing, review: "ALWAYS ASK: it rewrites a gate\nTIER: low\nVERDICT: approve", tier: "low",
			line: "This still needs your yes before it goes ahead: the reviewer says: it rewrites a gate.", hold: "the reviewer says: it rewrites a gate"},
		{name: "reviewer silent", reply: answerNothing, review: "TIER: low\nVERDICT: approve", tier: "low",
			line: "the reviewer did not say whether it needs you", hold: "the reviewer did not say whether it needs you"},
		{name: "the arbiter wants him", reply: answerNothing, review: passes, tier: "low", asked: "the agent that settled its review wants you to decide",
			line: "the agent that settled its review wants you to decide", hold: "the agent that settled its review wants you to decide"},
		{name: "no plan review", reply: answerNothing, review: passes, tier: "low", extra: map[string]string{labelPlanRounds: ""},
			line: "it had no plan review", hold: "it had no plan review"},
		{name: "a reason from the Ready stop holds", reply: answerNothing, review: passes, tier: "low",
			extra: map[string]string{labelAskWhy: "you asked for it; the reviewer says: it rewrites a gate"},
			line:  "the reviewer says: it rewrites a gate", hold: "the reviewer says: it rewrites a gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inThread(feedbackCard(tc.extra), "t1")
			f, p, m := mailFactory(t, c)
			m.threads["t1"] = []threadMsg{{ID: "m1"}}
			headIs(f, reviewedSHA, nil)
			storeLog(t, f, p, "c1", tc.reply)
			if err := f.passCode(c, map[string]string{}, "code approve", tc.tier, tc.review, tc.asked); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if (l[labelStage] == "landing") != tc.landing {
				t.Fatalf("labels = %v", l)
			}
			if !tc.landing {
				if !isFeedbackNote(l[labelNote]) || l[labelFeedbackHold] != tc.hold {
					t.Fatalf("labels = %v", l)
				}
				if tc.hold != "" && !strings.Contains(l[labelAskWhy], tc.hold) {
					t.Fatalf("board reason = %q", l[labelAskWhy])
				}
				f.SetAgents([]Agent{labeled(c, l)})
				f.Tick()
			}
			s := m.sends()
			if len(s) != 1 {
				t.Fatalf("sends = %+v", s)
			}
			text := s[0].Args["text"].(string)
			if !strings.Contains(text, tc.line) || !strings.Contains(s[0].Args["html"].(string), html.EscapeString(tc.line)) {
				t.Errorf("lacks %q:\n%s", tc.line, text)
			}
			if tc.landing && strings.Contains(text, replyPrompt) {
				t.Errorf("asks after asking nothing:\n%s", text)
			}
			if strings.Contains(text, "NEEDS FROM ZACH") {
				t.Errorf("raw overview:\n%s", text)
			}
		})
	}
}

// The normal form of an answer sits only in OVERVIEW: the email must carry it.
func TestFeedbackGoesAheadCarriesAnAnswerOnlyInTheOverview(t *testing.T) {
	c := inThread(feedbackCard(nil), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", "DECISION: None.\nOVERVIEW: The totals include refunds.\nNEEDS FROM ZACH: Nothing.")
	if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
		t.Fatal(err)
	}
	s := m.sends()
	if len(s) != 1 {
		t.Fatalf("sends = %+v", s)
	}
	want := "The totals include refunds.\n\nNothing more is needed from you. It goes ahead on its own."
	if text := s[0].Args["text"].(string); !strings.Contains(text, want) {
		t.Errorf("lacks the answer:\n%s", text)
	}
}

// An answer stripped to punctuation is not printed.
func TestFeedbackEmailSkipsAnOverviewStrippedToPunctuation(t *testing.T) {
	c := inThread(feedbackCard(nil), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", "DECISION: None.\nOVERVIEW: `factory/mail.go`.\nNEEDS FROM ZACH: Nothing.")
	if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
		t.Fatal(err)
	}
	s := m.sends()
	if len(s) != 1 {
		t.Fatalf("sends = %+v", s)
	}
	if text := s[0].Args["text"].(string); !strings.HasPrefix(text, feedbackGoesAhead) {
		t.Errorf("starts with a stripped line:\n%s", text)
	}
}

// An answer's questions print whole with their letters, or the email sends him to the board with no yes.
func TestFeedbackEmailQuestions(t *testing.T) {
	const reply = "DECISION: Which store?\nOVERVIEW: The totals include refunds.\nNEEDS FROM ZACH: Choose one.\n\n"
	for _, tc := range []struct {
		name, question string
		want, not      []string
	}{
		{"whole", "QUESTION: Which store?\n- Keep the old one (recommended)\n- Move now\n",
			[]string{"1. Which store?", "A. Keep the old one (recommended)", "B. Move now", `Reply "yes" or "land"`, "Letters like 1A answer the questions."}, []string{"too long"}},
		{"too long", "QUESTION: Which store?\n- " + strings.Repeat("store ", 70) + "(recommended)\n- Move now\n",
			[]string{"1 question is too long for email. Open the board to answer.", `Reply "drop" to drop it, or reply with feedback.`}, []string{"A. ", `"yes"`, `"land"`, "Letters"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inThread(feedbackCard(nil), "t1")
			f, p, m := mailFactory(t, c)
			m.threads["t1"] = []threadMsg{{ID: "m1"}}
			headIs(f, reviewedSHA, nil)
			storeLog(t, f, p, "c1", reply+tc.question)
			if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
				t.Fatal(err)
			}
			f.SetAgents([]Agent{labeled(c, p.labels("c1"))})
			f.Tick()
			s := m.sends()
			if len(s) != 1 {
				t.Fatalf("sends = %+v", s)
			}
			text := s[0].Args["text"].(string)
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("lacks %q:\n%s", w, text)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(text, w) {
					t.Errorf("has %q:\n%s", w, text)
				}
			}
		})
	}
}

// One answer email per exchange, across a retried verdict and a restart; a new exchange gets its own.
func TestFeedbackGoesAheadMailsOnce(t *testing.T) {
	const passes = "ALWAYS ASK: none\nTIER: low\nVERDICT: approve"
	c := inThread(feedbackCard(nil), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", answerNothing)
	if err := f.passCode(c, map[string]string{}, "code approve", "low", passes, ""); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelFeedbackTold] != "exchange-2" {
		t.Fatalf("labels = %v", l)
	}
	again := labeled(c, l)
	f.SetAgents([]Agent{again})
	if err := f.passCode(again, map[string]string{}, "code approve", "low", passes, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(m.sends()); n != 1 {
		t.Fatalf("a retry emailed again: %d", n)
	}
	restarted, q, after := mailFactory(t, again)
	headIs(restarted, reviewedSHA, nil)
	storeLog(t, restarted, q, "c1", answerNothing)
	after.threads["t1"] = []threadMsg{{ID: "m1"}}
	if err := restarted.passCode(again, map[string]string{}, "code approve", "low", passes, ""); err != nil {
		t.Fatal(err)
	}
	if len(after.sends()) != 0 {
		t.Fatal("a restart emailed again")
	}
	if q.labels("c1")[labelStage] != "landing" {
		t.Fatalf("labels = %v", q.labels("c1"))
	}
	next := labeled(again, map[string]string{labelReadyFeedback: "exchange-3"})
	if err := restarted.passCode(next, map[string]string{}, "code approve", "low", passes, ""); err != nil {
		t.Fatal(err)
	}
	if len(after.sends()) != 1 {
		t.Fatal("a new exchange gets its own answer")
	}
}

func TestFeedbackGoesAheadMailFailureStillLands(t *testing.T) {
	c := inThread(feedbackCard(nil), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", answerNothing)
	m.errs["reply_to_message"] = errors.New("mailbox down")
	if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landing" {
		t.Fatalf("labels = %v", l)
	}
}

func TestFeedbackAskingNothingOnChangedCodeGetsAFreshReady(t *testing.T) {
	c := feedbackCard(map[string]string{labelReadyHead: movedSHA})
	f, p, m := mailFactory(t, c)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", answerNothing)
	if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelNote] != readyNote(2) || l[labelFeedbackHold] != "" {
		t.Fatalf("labels = %v", l)
	}
	if len(m.sends()) != 0 {
		t.Fatalf("sends = %+v", m.sends())
	}
}

func TestFeedbackAskingNothingStopsForAFailedScan(t *testing.T) {
	c := feedbackCard(nil)
	f, p, m := mailFactory(t, c)
	f.cfg.Guarded = []string{c.Repo}
	headIs(f, reviewedSHA, nil)
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		return []scan.Hit{{Rule: "fake-rule", File: "a.go", Line: 3}}, nil
	}
	storeLog(t, f, p, "c1", answerNothing)
	if err := f.passCode(c, map[string]string{}, "code approve", "low", "ALWAYS ASK: none", ""); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan") {
		t.Fatalf("labels = %v", l)
	}
	if len(m.sends()) != 0 {
		t.Fatalf("sends = %+v", m.sends())
	}
}

// The board says why a held answer still waits, whether or not there is something to try.
func TestFeedbackHoldOnTheBoard(t *testing.T) {
	for _, preview := range []bool{false, true} {
		c := card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " exchange-2", labelReady: "1",
			labelFeedbackHold: "it is an architecture change", labelAskWhy: "it is an architecture change"})
		f, _ := newTestFactory(t, c)
		if preview {
			if err := f.previews.put(Preview{Card: "c1", State: previewReady, URL: "https://preview.example/"}); err != nil {
				t.Fatal(err)
			}
		}
		board, _ := f.card("c1")
		ask := askFor(board, c)
		ask.Overview = &Overview{Decision: "None.", Needs: "Nothing."}
		f.fillReady(ask, "c1", "")
		if !strings.Contains(ask.Overview.Needs, "This still needs your yes before it goes ahead: it is an architecture change.") {
			t.Errorf("preview %v: needs = %q", preview, ask.Overview.Needs)
		}
		if !strings.Contains(ask.Why, "It comes to you because it is an architecture change.") {
			t.Errorf("preview %v: why = %q", preview, ask.Why)
		}
	}
}

// New feedback starts a new exchange with no hold from the last one.
func TestNewFeedbackClearsTheHold(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " exchange-2", labelReady: "1", labelReadyHead: reviewedSHA,
		labelFeedbackHold: "it is an architecture change"})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Explain how to open the preview"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelFeedbackHold] != "" || l[labelReadyFeedback] == "" {
		t.Fatal(l)
	}
}

// Only "you asked for it" is waived, and in one rule with finishedWorkReason.
func TestFeedbackGateSharesFinishedWorkReasons(t *testing.T) {
	const passes = "ALWAYS ASK: none"
	c := feedbackCard(nil)
	f, _, _ := mailFactory(t, c)
	if got := f.finishedWorkReason(c, "low", passes, ""); got != "you asked for it" {
		t.Errorf("reason = %q", got)
	}
	if got := f.feedbackGate(c, "low", passes, ""); got != "" {
		t.Errorf("gate = %q", got)
	}
	again := feedbackCard(map[string]string{labelAskWhy: feedbackAgainWhy})
	if got := f.feedbackGate(again, "low", passes, ""); got != "" {
		t.Errorf("gate after a second round = %q", got)
	}
	if got := f.feedbackGate(c, "architecture", "ALWAYS ASK: a gate", "wants you"); got != "wants you; it is an architecture change; the reviewer says: a gate" {
		t.Errorf("gate = %q", got)
	}
}
