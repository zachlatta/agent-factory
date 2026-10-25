package factory

import "fmt"

// Cards from Zach's inbox: emails to the agents' address and voice-memo tasks start cards here. The
// zrl-bot dispatcher starts each email or voice-memo agent as a card (factory.source email or voice,
// stage plan, plus its gmail-thread label). Errands finish in one pass as before; code work
// goes through the gates like a board card. The card answers Zach in its email thread: when it lands,
// and whenever it waits on him.

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
		return msg
	}
	return msg + " Then tell Zach by email what changed and anything he must do (" + emailHow(t) +
		"), and archive the thread (modify_thread_labels, remove INBOX)."
}

// boardLink names the board in a message: its link when one is configured.
func (f *Factory) boardLink() string {
	if f.cfg.BoardURL == "" {
		return "the factory board"
	}
	return "the board link " + f.cfg.BoardURL
}

// emailZachMsg asks an inbox card waiting in Needs Zach to tell him, by email, what it needs.
func (f *Factory) emailZachMsg(a Agent, why string) string {
	board := f.boardLink()
	return fmt.Sprintf("Your card on the factory board now waits on Zach (%s). He follows inbox cards by email, so "+
		"tell him: %s. One short email: what you need from him (approve the plan, approve landing, or a decision), "+
		"what happens after, and %s (an Open board button in the html, if there is a link). Do not build or land. "+
		"If he must choose, number each question and letter its options (1A, 1B), recommended first, so he can "+
		"answer \"1B\" by email; on the board the same choices show as QUESTION: blocks. When he answers by "+
		"letter, match it against the questions in your email.\n\n"+
		"If he answers by email: to approve, run this as your last command and stop (the factory then sends "+
		"you the next step):\n"+
		"curl -s -X POST -H 'X-Factory: 1' -d '{\"id\":\"%s\"}' http://127.0.0.1:7478/api/approve\n"+
		"Changes or a decision: do what he says, then stop with a short summary. Stop or drop it: run the same "+
		"command with /api/close instead.", why, emailHow(emailThread(a)), board, a.ID)
}

// newCardEmailNote is added to a board card's prompt when it was started from an email thread.
func newCardEmailNote(thread string) string {
	return "\nThis card was started from Zach's email (zrl-bot thread " + thread + "). When it lands, or if it " +
		"stops without landing, tell him there: " + emailHow(thread) + ".\n"
}
