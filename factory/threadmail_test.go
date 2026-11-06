package factory

import (
	"testing"
	"time"
)

// The factory starts no email of its own for board work: per-card sends go out only in a thread
// Zach started (gmail-thread), and the digest is never sent. A silent card also keeps its
// told-family labels unset, so nothing is marked as emailed when nothing went out.

func inThread(a Agent, thread string) Agent {
	a.Labels[gmailThreadLabel] = thread
	return a
}

const decisionReply = "DECISION: Approve building the handoff checker?\n" +
	"OVERVIEW: Adds a checker.\nNEEDS FROM ZACH: approve the plan.\n\nThe plan.\n\nBUILD SIDE EFFECTS: none\nTIER: low\n"

func TestDecisionEmailOnlyInZachThread(t *testing.T) {
	t.Run("board card sends nothing and is not marked told", func(t *testing.T) {
		f, p, m := mailFactory(t, planWait("c1", nil))
		storeLog(t, f, p, "c1", decisionReply)
		f.Tick()
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("board card emailed Zach: %+v", s)
		}
		if got := p.labels("c1")[labelTold]; got != "" {
			t.Errorf("told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("board card with only a factory thread sends nothing", func(t *testing.T) {
		c := planWait("c1", map[string]string{labelZachThread: "t1"})
		f, p, m := mailFactory(t, c)
		m.threads["t1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}}
		storeLog(t, f, p, "c1", decisionReply)
		f.Tick()
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("zach-thread-only card emailed Zach: %+v", s)
		}
		if got := p.labels("c1")[labelTold]; got != "" {
			t.Errorf("told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("thread card still gets its decision email in the thread", func(t *testing.T) {
		f, p, m := mailFactory(t, inThread(planWait("c1", nil), "t1"))
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		storeLog(t, f, p, "c1", decisionReply)
		f.Tick()
		s := m.sends()
		if len(s) != 1 || s[0].Tool != "reply_to_message" {
			t.Fatalf("sends = %+v, want one reply in the thread", s)
		}
		if got := p.labels("c1")[labelTold]; got == "" {
			t.Errorf("told unset after a sent decision email")
		}
	})
}

func parkedAsk() Agent {
	c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelTrack: "direct",
		labelNote: directAskNote("Send it?"), labelParked: iso(t0), labelSince: ago(time.Hour)})
	c.ArchivedAt = t0
	return c
}

func TestParkedAskEmailOnlyInZachThread(t *testing.T) {
	const reply = "DECISION: Send it?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Answer it.\n\nTRACK: direct"
	t.Run("board card sends nothing", func(t *testing.T) {
		f, p, m := mailFactory(t, parkedAsk())
		storeLog(t, f, p, "c1", reply)
		f.mailParkedAsks(f.agents())
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("parked board card emailed Zach: %+v", s)
		}
		if got := p.labels("c1")[labelTold]; got != "" {
			t.Errorf("told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("thread card still sends", func(t *testing.T) {
		f, p, m := mailFactory(t, inThread(parkedAsk(), "t1"))
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		storeLog(t, f, p, "c1", reply)
		f.mailParkedAsks(f.agents())
		if s := m.sends(); len(s) != 1 || s[0].Tool != "reply_to_message" {
			t.Fatalf("sends = %+v, want one reply in the thread", s)
		}
	})
}

func dueCard() Agent {
	due := t0.Add(48 * time.Hour)
	c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelTrack: "direct",
		labelNote: directAskNote("Send it?"), labelSince: iso(t0), labelDue: iso(due),
		labelTold: directAskNote("Send it?")})
	c.Title = "Fill in the booth form"
	return c
}

func TestDueReminderOnlyInZachThread(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	now := due.Add(-time.Hour)
	const reply = "DECISION: Send it?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Answer it.\n\nTRACK: direct"
	t.Run("board card sends nothing", func(t *testing.T) {
		f, p, m := mailFactory(t, dueCard())
		storeLog(t, f, p, "c1", reply)
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("board card emailed a due reminder: %+v", s)
		}
		if got := p.labels("c1")[labelDueTold]; got != "" {
			t.Errorf("due-told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("thread card still sends", func(t *testing.T) {
		f, p, m := mailFactory(t, inThread(dueCard(), "t1"))
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		storeLog(t, f, p, "c1", reply)
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if s := m.sends(); len(s) != 1 || s[0].Tool != "reply_to_message" {
			t.Fatalf("sends = %+v, want one reply in the thread", s)
		}
		if got := p.labels("c1")[labelDueTold]; got != iso(due) {
			t.Errorf("due-told = %q, want %q", got, iso(due))
		}
	})
}

func TestStuckPromptEmailOnlyInZachThread(t *testing.T) {
	t.Run("board card sends nothing", func(t *testing.T) {
		f, p, m := mailFactory(t, stuckCard("c1", 21*time.Minute))
		f.mailStuckPrompts(f.agents(), t0)
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("board card emailed a stuck-prompt alert: %+v", s)
		}
		if got := p.labels("c1")[labelPermitTold]; got != "" {
			t.Errorf("permit-told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("thread card still sends", func(t *testing.T) {
		f, p, m := mailFactory(t, inThread(stuckCard("c1", 21*time.Minute), "t1"))
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		f.mailStuckPrompts(f.agents(), t0)
		if s := m.sends(); len(s) != 1 || s[0].Tool != "reply_to_message" {
			t.Fatalf("sends = %+v, want one reply in the thread", s)
		}
		if got := p.labels("c1")[labelPermitTold]; got == "" {
			t.Errorf("permit-told unset after a sent stuck-prompt email")
		}
	})
}

func TestFeedbackGoesAheadOnlyInZachThread(t *testing.T) {
	t.Run("board card sends nothing", func(t *testing.T) {
		c := feedbackCard(nil)
		f, p, m := mailFactory(t, c)
		storeLog(t, f, p, "c1", answerNothing)
		f.mailFeedbackGoesAhead(c)
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("board card emailed a goes-ahead answer: %+v", s)
		}
		if got := p.labels("c1")[labelFeedbackTold]; got != "" {
			t.Errorf("feedback-told = %q, want unset when nothing was sent", got)
		}
	})
	t.Run("thread card still sends", func(t *testing.T) {
		c := inThread(feedbackCard(nil), "t1")
		f, p, m := mailFactory(t, c)
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		storeLog(t, f, p, "c1", answerNothing)
		f.mailFeedbackGoesAhead(c)
		if s := m.sends(); len(s) != 1 || s[0].Tool != "reply_to_message" {
			t.Fatalf("sends = %+v, want one reply in the thread", s)
		}
		if got := p.labels("c1")[labelFeedbackTold]; got != "exchange-2" {
			t.Errorf("feedback-told = %q, want the exchange", got)
		}
	})
}

// The Tick never sends the daily digest anymore, even at digest time on a busy day.
func TestTickSendsNoDigest(t *testing.T) {
	f, m, _ := digestFactory(t, 18, 0)
	f.Tick()
	if n := len(digests(m)); n != 0 {
		t.Fatalf("Tick sent %d digests", n)
	}
}

// mailOnce itself refuses a card with no email thread: neither a factory-started thread nor
// FACTORY_ZACH_EMAIL is enough, and no label is set for a send that never happens.
func TestMailOnceNeedsAnEmailThread(t *testing.T) {
	msg := email{Subject: "Hi", Text: "hello", HTML: "<p>hello</p>"}
	for name, extra := range map[string]map[string]string{
		"address only":        nil,
		"factory thread only": {labelZachThread: "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			c := planWait("c1", extra)
			f, p, m := mailFactory(t, c)
			m.threads["t1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}}
			sent, err := f.mailOnce(c, labelTold, "note", msg)
			if err != nil || sent {
				t.Fatalf("mailOnce = %v, %v; want a quiet no-send", sent, err)
			}
			if s := m.sends(); len(s) != 0 {
				t.Fatalf("sends = %+v", s)
			}
			if got := p.labels("c1")[labelTold]; got != "" {
				t.Errorf("told = %q, want unset when nothing was sent", got)
			}
		})
	}
}
