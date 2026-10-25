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

func TestInboxCardTellsZachOncePerWait(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "zach", labelSource: "email", labelNote: "rework cap reached"})
	f, p := newTestFactory(t, c)
	f.Tick()
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "rework cap reached") || !strings.Contains(msgs[0], "/api/approve") {
		t.Fatalf("sent = %v", msgs)
	}
	if l := p.labels("c1"); l[labelTold] != "rework cap reached" {
		t.Errorf("labels = %v", l)
	}
	c.Labels[labelPrefix+labelTold] = "rework cap reached"
	f2, p2 := newTestFactory(t, c)
	f2.Tick()
	if msgs := p2.sent("c1"); len(msgs) != 0 {
		t.Errorf("told twice: %v", msgs)
	}
}

func TestBoardCardInNeedsZachSendsNothing(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}))
	f.Tick()
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("sent = %v", msgs)
	}
}

func TestInboxArchitecturePlanIsEmailedOnce(t *testing.T) {
	f, p := newTestFactory(t, inboxCard("c1", map[string]string{labelStage: "plan-review", labelSource: "email"}),
		reviewer("r1", "c1", "plan"))
	p.logs["r1"] = "TIER: architecture\nVERDICT: approve"
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelTold] != l[labelNote] {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "reply_to_message") {
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
