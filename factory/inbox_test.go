package factory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// inboxCard is a card the zrl-bot dispatcher started for an email (or voice memo) in thread "t1".
func inboxCard(id string, labels map[string]string) Agent {
	a := card(id, labels)
	a.Labels["gmail-thread"] = "t1"
	return a
}

func TestInboxCardWithoutPlanFinishesDirect(t *testing.T) {
	for _, src := range []string{"email", "voice"} {
		c := inboxCard("c1", map[string]string{labelStage: "plan", labelSource: src, labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
		c.Cwd = t.TempDir()
		f, p := newTestFactory(t, c)
		f.Git = func(context.Context, string, ...string) error { return nil }
		f.GitOut = gitStub(directOK)                                                        // no factory.base on inbox cards: the reflog still shows its own commits
		storeLog(t, f, p, "c1", "Replied to Zach with the answer and archived the thread.") // no TRACK or TIER line
		f.Tick()
		if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
			t.Fatalf("%s: review started for an errand: %v", src, runs)
		}
		if l := p.labels("c1"); l[labelStage] != "landed" {
			t.Errorf("%s: labels = %v", src, l)
		}
	}
}

func TestInboxCardWithPlanGetsPlanReview(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "plan", labelSource: "email", labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "PLAN\nGoal: ...\nTIER: risky")
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("plan review not started: %v", p.calls)
	}
}

func TestInboxCardComesToZachThenLandsAndEmails(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "review", labelSource: "email", labelTier: "low", labelPlanRounds: "1"})
	f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || !isReadyNote(l[labelNote]) {
		t.Fatalf("he asked for it, so he sees it first: %v", l)
	}
	f, p = newTestFactory(t, withLabels(c, l))
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") || !strings.Contains(msgs[0], "reply_to_message") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestBoardCardLandMessageHasNoEmail(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if msgs := p.sent("c1"); len(msgs) != 1 || strings.Contains(msgs[0], "reply_to_message") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestNewCardCanCarryEmailThread(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "fix the thing", Repo: "zachlatta/zrl-bot-email-control", Thread: "t9"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel("gmail-thread=t9")
	if len(runs) != 1 || !strings.Contains(runs[0][len(runs[0])-1], "reply_to_message") {
		t.Fatalf("runs = %v", p.calls)
	}
	f2, p2 := newTestFactory(t)
	f2.Act("new", Body{Request: "fix the thing"})
	for _, c := range p2.calls {
		if c[0] == "run" && strings.Contains(c[len(c)-1], "reply_to_message") {
			t.Errorf("board card prompt mentions email: %v", c[len(c)-1])
		}
	}
}

// newRun is the prompt and flags of the one card run newCard started.
func newRun(t *testing.T, p *fakePaseo) []string {
	t.Helper()
	var runs [][]string
	for _, c := range p.calls {
		if c[0] == "run" {
			runs = append(runs, c)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	return runs[0]
}

func hasArg(args []string, flag, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == v {
			return true
		}
	}
	return false
}

func TestEmailAndVoiceCardsGetTheBoardPrompt(t *testing.T) {
	fb, pb := newTestFactory(t)
	if _, err := fb.Act("new", Body{Request: "Order batteries", Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	board := newRun(t, pb)
	boardPrompt := board[len(board)-1]
	old := InboxSource
	InboxSource = "example-inbox"
	defer func() { InboxSource = old }()
	for _, src := range []string{"email", "voice"} {
		f, p := newTestFactory(t)
		inbox := "Answering in the thread\n- Date: Sun, 04 Oct 2026 09:00:00 -0400\nReply to message m1."
		_, err := f.Act("new", Body{Request: "Order batteries", Repo: "example/desk", Title: "Order batteries", Origin: src,
			Thread: "t9", Inbox: inbox, Labels: map[string]string{"source": "example-inbox", "voice-memo": "rec-1"}})
		if err != nil {
			t.Fatal(err)
		}
		run := newRun(t, p)
		prompt := run[len(run)-1]
		head, _, _ := strings.Cut(boardPrompt, "Sub-agents.")
		if !strings.HasPrefix(prompt, head) {
			t.Errorf("%s: prompt does not start with the board prompt:\n%s", src, prompt)
		}
		if requestOf(prompt) != "Order batteries" {
			t.Errorf("%s: request = %q", src, requestOf(prompt))
		}
		if !strings.Contains(prompt, "Sub-agents. Zach allows") || !strings.HasSuffix(strings.TrimSpace(prompt), "Reply to message m1.") {
			t.Errorf("%s: prompt lacks the sub-agent section or the inbox section last:\n%s", src, prompt)
		}
		for _, want := range []string{labelArg(labelSource, src), "gmail-thread=t9", "source=example-inbox", "voice-memo=rec-1", labelArg(labelTitled, "given")} {
			if !hasArg(run, "--label", want) {
				t.Errorf("%s: no label %s in %v", src, want, run)
			}
		}
		if k, v := askedOf(prompt); k != "email-date" || v != "Sun, 04 Oct 2026 09:00:00 -0400" {
			t.Errorf("%s: asked = %s %q", src, k, v)
		}
	}
}

func TestNewCardRejectsBadInboxFields(t *testing.T) {
	cases := map[string]Body{
		"inbox on a board card":  {Request: "x", Repo: "example/desk", Inbox: "reply in thread"},
		"labels on a board card": {Request: "x", Repo: "example/desk", Labels: map[string]string{"source": "example-inbox"}},
		"voice card, no thread":  {Request: "x", Repo: "example/desk", Origin: "voice"},
		"unknown label":          {Request: "x", Repo: "example/desk", Origin: "email", Thread: "t9", Labels: map[string]string{"factory.stage": "landed"}},
		"bad label value":        {Request: "x", Repo: "example/desk", Origin: "email", Thread: "t9", Labels: map[string]string{"source": "board"}},
		"inbox card, auto repo":  {Request: "x", Repo: autoRepo, Origin: "email", Thread: "t9"},
	}
	for name, b := range cases {
		f, p := newTestFactory(t)
		if _, err := f.Act("new", b); err == nil {
			t.Errorf("%s: accepted", name)
		}
		for _, c := range p.calls {
			if c[0] == "run" {
				t.Errorf("%s: started %v", name, c)
			}
		}
	}
}

func TestFactoryMessagesAreSentUnchanged(t *testing.T) {
	inbox := inboxCard("c1", map[string]string{labelStage: "plan", labelSource: "email"})
	board := card("b1", map[string]string{labelStage: "plan", labelSource: "board"})
	f, p := newTestFactory(t, inbox, board)
	for _, id := range []string{"c1", "b1"} {
		if err := f.send(id, "Plan approved. Build it."); err != nil {
			t.Fatal(err)
		}
		if m := p.sent(id); len(m) != 1 || m[0] != "Plan approved. Build it." {
			t.Errorf("%s got %v", id, m)
		}
	}
}

// An email or voice request started through /api/new keeps what the intake gave it when the card's next
// stage starts in a fresh agent: Zach's words verbatim, the dispatcher's section, its source, thread
// and own labels, and the one sub-agent section its first prompt already has.
func TestInboxIntakeCarriesIntoFreshStages(t *testing.T) {
	f, p := newTestFactory(t)
	request := "Rename the weekly report to \"Friday notes\".\n\n  Keep the old link working;  it is in two emails."
	inbox := "Answering in the thread\n- Recorded: Sun Oct 4, 9:00 AM (EDT), 41 seconds\nReply to message m1."
	if _, err := f.Act("new", Body{Request: request, Repo: "example/desk", Title: "Rename the weekly report", Origin: "voice",
		Thread: "t9", Inbox: inbox, Labels: map[string]string{"source": InboxSource, "voice-memo": "rec-1"}}); err != nil {
		t.Fatal(err)
	}
	run := newRun(t, p)
	first := run[len(run)-1]
	a := card("c1", nil)
	for i := 0; i+1 < len(run); i++ {
		if run[i] == "--label" {
			k, v, _ := strings.Cut(run[i+1], "=")
			a.Labels[k] = v
		}
	}
	a.Labels[labelPrefix+labelStage] = "plan-review"
	a.Labels[labelPrefix+labelTier] = "low"
	a.WorkspaceID = "ws-1"
	user, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": first}})
	plan, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{
		{"type": "text", "text": "PLAN: rename it and keep a redirect\nBUILD SIDE EFFECTS: none\nTIER: low"}}}})
	writeFile(t, f.cfg.Home+"/.claude/projects/-test/"+testSession("c1")+".jsonl", string(user)+"\n"+string(plan)+"\n")
	r := reviewer("r1", "c1", "plan")
	f.SetAgents([]Agent{a, r})
	storeLog(t, f, p, "r1", "plan is sound\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 {
		t.Fatalf("no fresh stage agent: %v", p.calls)
	}
	stage := runs[0]
	for _, want := range []string{labelArg(labelSource, "voice"), gmailThreadLabel + "=t9", "source=" + InboxSource,
		"voice-memo=rec-1", labelArg(labelTitled, "given")} {
		if !hasArg(stage, "--label", want) {
			t.Errorf("the fresh agent lacks label %s: %v", want, stage)
		}
	}
	prompt := stage[len(stage)-1]
	if !strings.Contains(prompt, request) || requestOf(handoffSection(prompt, "Original instructions")) != request {
		t.Errorf("Zach's words are not carried verbatim:\n%s", prompt)
	}
	for want, n := range map[string]int{inbox: 1, "Sub-agents. Zach allows": 1} {
		if got := strings.Count(prompt, want); got != n {
			t.Errorf("%q is in the fresh agent's prompt %d times, want %d:\n%s", want, got, n, prompt)
		}
	}
	// the board shows the fresh agent as the same voice card, with the same request
	next := card("new-agent-1", nil)
	for i := 0; i+1 < len(stage); i++ {
		if stage[i] == "--label" {
			k, v, _ := strings.Cut(stage[i+1], "=")
			next.Labels[k] = v
		}
	}
	old := withLabels(a, p.labels(a.ID))
	cards := BuildBoard([]Agent{old, next}, nil, t0)
	var got *Card
	for i := range cards {
		if cards[i].ID == "new-agent-1" {
			got = &cards[i]
		}
	}
	if got == nil || got.Source != "voice memo" {
		t.Fatalf("the fresh agent's card = %+v", got)
	}
}
