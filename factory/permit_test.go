package factory

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// stuckCard is card id running on a permission prompt, its last activity idle ago.
func stuckCard(id string, idle time.Duration) Agent {
	c := card(id, map[string]string{labelStage: "building", labelSince: ago(time.Hour)})
	c.Status, c.Pending, c.PendingIDs, c.LastActivityAt = "running", "Bash: rm -rf build", []string{"q-" + id}, t0.Add(-idle)
	return c
}

// A factory agent still on a refused prompt with no activity for 20 minutes gets Zach one email, once per
// move; the factory never approves the prompt (the fake paseo panics on permit allow).
func TestStuckPromptEmailsZachOnce(t *testing.T) {
	f, p, m := mailFactory(t, stuckCard("c1", 19*time.Minute))
	f.Tick()
	if len(m.sends()) != 0 {
		t.Fatalf("emailed before %v: %v", permitTimeout, m.sends())
	}
	f2, p2, m2 := mailFactory(t, stuckCard("c1", 21*time.Minute))
	f2.Tick()
	sends := m2.sends()
	if len(sends) != 1 || sends[0].Tool != "send_email" {
		t.Fatalf("sends = %v, want one new thread", sends)
	}
	text := sends[0].Args["text"].(string)
	for _, want := range []string{"card c1", "Bash: rm -rf build", "never approves", "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("email lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(sends[0].Args["subject"].(string), "permission prompt") {
		t.Errorf("subject = %v", sends[0].Args["subject"])
	}
	mark := "building@" + ago(time.Hour)
	if got := p2.labels("c1")[labelPermitTold]; got != mark {
		t.Errorf("permit-told = %q, want %q", got, mark)
	}
	if p2.labels("c1")[labelZachThread] != "nt1" {
		t.Error("the new thread is not kept for later emails")
	}
	// the next pass sees the mark: no second email for this move
	again := stuckCard("c1", 40*time.Minute)
	again.Labels[labelPrefix+labelPermitTold] = mark
	f3, _, m3 := mailFactory(t, again)
	f3.Tick()
	if len(m3.sends()) != 0 {
		t.Errorf("emailed twice for one move: %v", m3.sends())
	}
	if len(p.labels("c1")) != 0 {
		t.Errorf("labels set before the timeout: %v", p.labels("c1"))
	}
	if p2.countCalls("permit", "deny") != 1 {
		t.Errorf("deny calls = %d, want the one refusal", p2.countCalls("permit", "deny"))
	}
}

// A later move of the card that gets stuck again is emailed again.
func TestStuckPromptEmailsAgainAfterAMove(t *testing.T) {
	c := stuckCard("c1", 30*time.Minute)
	c.Labels[labelPrefix+labelPermitTold] = "plan@" + ago(3*time.Hour)
	f, _, m := mailFactory(t, c)
	f.mailStuckPrompts(f.agents(), t0)
	if len(m.sends()) != 1 {
		t.Fatalf("sends = %v, want one for the new move", m.sends())
	}
}

// A reviewer or sub-agent of a card is reported in the card's thread, once for both; an agent the
// factory did not start is never reported.
func TestStuckPromptOfHelpersGoesToTheirCard(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review", labelSince: ago(time.Hour), labelZachThread: "th1"})
	r := reviewer("r1", "c1", "code")
	r.Status, r.Pending, r.LastActivityAt = "running", "Bash: git push", t0.Add(-25*time.Minute)
	sub := Agent{ID: "s1", Status: "running", Pending: "Edit: y", CreatedAt: t0.Add(-time.Hour),
		LastActivityAt: t0.Add(-25 * time.Minute), Labels: map[string]string{parentAgentLabel: "c1"}}
	mine := Agent{ID: "u1", Status: "running", Pending: "Bash: z", CreatedAt: t0.Add(-time.Hour),
		LastActivityAt: t0.Add(-25 * time.Minute), Labels: map[string]string{}}
	f, p, m := mailFactory(t, c, r, sub, mine)
	m.threads["th1"] = []threadMsg{{ID: "m1", LabelIDs: []string{"SENT"}}}
	f.mailStuckPrompts(f.agents(), t0)
	sends := m.sends()
	if len(sends) != 1 || sends[0].Tool != "reply_to_message" {
		t.Fatalf("sends = %v, want one reply in the card's thread", sends)
	}
	if !strings.Contains(sends[0].Args["text"].(string), "helper agent") {
		t.Errorf("email = %v", sends[0].Args["text"])
	}
	if p.labels("c1")[labelPermitTold] == "" || p.labels("r1")[labelPermitTold] != "" {
		t.Error("the mark belongs on the card")
	}
	if o, ok := promptOwner(f.byID(), cardOwners(f.agents(), nil), mine); ok {
		t.Errorf("owner of an outside agent = %s", o.ID)
	}
	// the email goes to the card the board files the helper under, at any depth
	deep := Agent{ID: "d1", Status: "running", CreatedAt: t0.Add(-time.Hour), Labels: map[string]string{parentAgentLabel: "r1"}}
	agents := append(f.agents(), deep)
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	owners := cardOwners(agents, nil)
	if o, ok := promptOwner(byID, owners, deep); !ok || o.ID != "c1" || owners["d1"] != "c1" {
		t.Errorf("owner of a reviewer's sub-agent = %s (board: %s), want c1", o.ID, owners["d1"])
	}
}

// A failed send clears the mark, so a later pass tries again.
func TestStuckPromptFailedSendClearsMark(t *testing.T) {
	f, p, m := mailFactory(t, stuckCard("c1", 30*time.Minute))
	m.errs["send_email"] = errors.New("mailbox down")
	f.mailStuckPrompts(f.agents(), t0)
	if p.labels("c1")[labelPermitTold] != "" {
		t.Error("a failed send kept the mark")
	}
}

func TestDryRunNeverEmailsStuckPrompts(t *testing.T) {
	f, _, m := mailFactory(t, stuckCard("c1", 30*time.Minute))
	f.cfg.DryRun = true
	f.mailStuckPrompts(f.agents(), t0)
	if len(m.calls) != 0 {
		t.Errorf("a dry run emailed: %v", m.calls)
	}
}
