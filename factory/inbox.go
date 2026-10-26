package factory

import (
	"fmt"
	"time"
)

// Cards from Zach's inbox: emails to the agents' address and voice-memo tasks start cards here. The
// zrl-bot dispatcher starts each email or voice-memo agent as a card (factory.source email or voice,
// stage plan, plus its gmail-thread label). Errands finish in one pass as before; code work
// goes through the gates like a board card. The card tells Zach in its email thread when it lands;
// whenever it waits on him, the factory emails him there itself (mail.go).

// gmailThreadLabel is the zrl-bot dispatcher's label (no factory. prefix) naming a card's email thread.
const gmailThreadLabel = "gmail-thread"

// fromInbox: the zrl-bot dispatcher started this card for an email or a voice memo.
func fromInbox(a Agent) bool {
	s := a.L(labelSource)
	return s == "email" || s == "voice"
}

// emailThread is the zrl-bot Gmail thread the card reports in, or "".
func emailThread(a Agent) string { return a.Labels[gmailThreadLabel] }

func emailHow(thread string) string {
	return fmt.Sprintf("reply_to_message to Zach's latest message in zrl-bot thread %s (read it with get_thread; "+
		"follow the zrl-bot-email skill: short, plain, html plus text)", thread)
}

// landMsgFor is the landing instruction; a card with an email thread also reports to Zach there.
func (f *Factory) landMsgFor(a Agent) string {
	msg := landMsg
	if f.guarded(a.Repo) {
		msg = guardedLandMsg
	}
	t := emailThread(a)
	if t == "" {
		return msg + remainingRule
	}
	return msg + " Then tell Zach by email what changed and anything he must do (" + emailHow(t) +
		"), and archive the thread (modify_thread_labels, remove INBOX)." + remainingRule
}

// newCardEmailNote is added to a board card's prompt when it was started from an email thread.
func newCardEmailNote(thread string) string {
	return "\nThis card was started from Zach's email (zrl-bot thread " + thread + "). When it lands, or if it " +
		"stops without landing, tell him there: " + emailHow(thread) + ".\n"
}

// Sub-agents on inbox cards. The dispatcher writes an email or voice card's prompt, so the factory
// adds the sub-agent section (delegateRule) itself: one note in the card's first minutes, while its
// agent still runs its first turn, or else appended to the next message the factory sends it.
// factory.delegated marks it sent. A message to a running agent interrupts the command it is running,
// so the note goes only to a young card (still reading its skills); a message to a stopped one would
// start an extra turn whose reply replaces the TRACK or TIER reply the factory reads, so never then.

// delegateWindow is how long after an inbox card starts the factory sends it the sub-agent note.
const delegateWindow = 2 * time.Minute

const delegateNote = "Factory note: you may use sub-agents on this card, as below. No reply needed; continue your current task.\n"

// tickDelegate sends a young, running email or voice card in plan its sub-agent section once.
func (f *Factory) tickDelegate(a Agent, now time.Time) {
	if !fromInbox(a) || a.archived() || a.Status != "running" || a.L(labelStage) != "plan" ||
		a.L(labelDelegated) != "" || now.Sub(a.CreatedAt) > delegateWindow {
		return
	}
	// the snapshot can be a poll old: never wake a card that has stopped since
	if archived, running, err := f.liveState(a.ID); err != nil || archived || !running {
		return
	}
	if err := f.send(a.ID, delegateNote); err != nil {
		f.logf("%s sub-agent note not sent: %v", short(a.ID), err)
		return
	}
	f.logf("%s got the sub-agent section", short(a.ID))
}

// withDelegateRule appends the sub-agent section to a message for an email or voice card that has
// not had it, and names the agent to mark; any other message is unchanged.
func (f *Factory) withDelegateRule(id, msg string) (string, string) {
	a, ok := f.agentOK(id)
	if !ok || !fromInbox(a) || a.L(labelDelegated) != "" {
		return msg, ""
	}
	if st := a.L(labelStage); st == "landing" || st == "landed" {
		return msg, ""
	}
	return msg + "\n" + f.delegateRule(false), a.ID
}
