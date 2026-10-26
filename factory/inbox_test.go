package factory

import (
	"context"
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
		f.GitOut = gitStub(directOK)                                              // no factory.base on inbox cards: the reflog still shows its own commits
		p.logs["c1"] = "Replied to Zach with the answer and archived the thread." // no TRACK or TIER line
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
	p.logs["c1"] = "PLAN\nGoal: ...\nTIER: risky"
	f.Tick()
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Fatalf("plan review not started: %v", p.calls)
	}
}

func TestInboxCardLandsThenEmailsZach(t *testing.T) {
	f, p := newTestFactory(t, inboxCard("c1", map[string]string{labelStage: "review", labelSource: "email", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
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
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
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

// runningInbox is an email or voice card the dispatcher started age ago, still on its first turn.
func runningInbox(src string, age time.Duration, labels map[string]string) Agent {
	l := map[string]string{labelStage: "plan", labelSource: src}
	for k, v := range labels {
		l[k] = v
	}
	a := inboxCard("c1", l)
	a.Status, a.CreatedAt, a.LastActivityAt = "running", t0.Add(-age), t0
	return a
}

func TestInboxCardGetsSubagentRuleFromFactory(t *testing.T) {
	for _, src := range []string{"email", "voice"} {
		f, p := newTestFactory(t, runningInbox(src, 30*time.Second, nil))
		p.status["c1"] = "running"
		f.Tick()
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], "Factory note") || !strings.Contains(msgs[0], "Sub-agents. Zach allows") ||
			!strings.Contains(msgs[0], "At most 6 sub-agents") {
			t.Fatalf("%s: sent = %v", src, msgs)
		}
		if p.labels("c1")[labelDelegated] == "" {
			t.Errorf("%s: no %s label: %v", src, labelDelegated, p.labels("c1"))
		}
	}
}

func TestSubagentRuleSentOnlyOnce(t *testing.T) {
	f, p := newTestFactory(t, runningInbox("email", 30*time.Second, map[string]string{labelDelegated: ago(10 * time.Second)}))
	p.status["c1"] = "running"
	f.Tick()
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("sent again: %v", msgs)
	}
}

func TestSubagentRuleNotSentLateOrToStoppedOrBoardCards(t *testing.T) {
	cases := map[string]struct {
		a    Agent
		live string // what paseo inspect reports just before the send
	}{
		"older than the window":  {runningInbox("email", 5*time.Minute, nil), "running"},
		"stopped since the poll": {runningInbox("email", 30*time.Second, nil), "idle"},
		"board card":             {runningInbox("board", 30*time.Second, nil), "running"},
		"building":               {runningInbox("email", 30*time.Second, map[string]string{labelStage: "building"}), "running"},
	}
	for name, c := range cases {
		f, p := newTestFactory(t, c.a)
		p.status["c1"] = c.live
		f.Tick()
		if msgs := p.sent("c1"); len(msgs) != 0 {
			t.Errorf("%s: sent = %v", name, msgs)
		}
	}
}

func TestFactoryMessageToInboxCardCarriesSubagentRuleOnce(t *testing.T) {
	inbox := inboxCard("c1", map[string]string{labelStage: "plan", labelSource: "email"})
	done := inboxCard("c2", map[string]string{labelStage: "plan", labelSource: "voice", labelDelegated: ago(time.Hour)})
	board := card("b1", map[string]string{labelStage: "plan", labelSource: "board"})
	sub := Agent{ID: "s1", Status: "idle", Labels: map[string]string{"paseo.parent-agent-id": "c1"}}
	f, p := newTestFactory(t, inbox, done, board, sub)
	for _, id := range []string{"c1", "c2", "b1", "s1"} {
		if err := f.send(id, "Plan approved. Build it."); err != nil {
			t.Fatal(err)
		}
	}
	if m := p.sent("c1"); len(m) != 1 || !strings.HasPrefix(m[0], "Plan approved. Build it.") || !strings.Contains(m[0], "Sub-agents. Zach allows") {
		t.Errorf("inbox card got %v", m)
	}
	if p.labels("c1")[labelDelegated] == "" {
		t.Errorf("inbox card not marked: %v", p.labels("c1"))
	}
	for _, id := range []string{"c2", "b1", "s1"} {
		if m := p.sent(id); len(m) != 1 || m[0] != "Plan approved. Build it." {
			t.Errorf("%s got %v", id, m)
		}
	}
}
