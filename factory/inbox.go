package factory

import "fmt"

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
