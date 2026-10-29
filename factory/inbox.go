package factory

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cards from Zach's inbox: emails to the agents' address and voice-memo tasks start cards here. The
// zrl-bot dispatcher starts each one through POST /api/new like a board card (origin email or voice,
// Zach's words as the request, its thread, and an Inbox section on answering there), so all three
// get the same Fellow prompt and gates. Errands finish in one pass; code work goes through the gates.
// The card tells Zach in its email thread when it lands; whenever it waits on him, the factory emails
// him there itself (mail.go).

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

// landMsgFor is the landing instruction; a card with an email thread also reports to Zach there, on its
// first landing only: a work-left round (remaining.go) sends no second email.
func (f *Factory) landMsgFor(a Agent) string {
	msg := landMsg
	if f.guarded(a.Repo) {
		msg = guardedLandMsg
	}
	if readRecovery(a).LandedDiff {
		msg = landedDiffLandNote + msg
	}
	t := emailThread(a)
	if t == "" || a.L(labelWorkLeftRounds) != "" {
		return msg + remainingRule
	}
	link := ""
	if l := f.cardLink(a.ID); l != "" {
		link = " End that email with the board link " + l + "."
	}
	return msg + " Then tell Zach by email what changed and anything he must do (" + emailHow(t) +
		"; " + strings.Join(strings.Fields(plainEmailRule), " ") + "), and archive the thread (modify_thread_labels, remove INBOX)." + link + remainingRule
}

// newCardEmailNote is added to a board card's prompt when it was started from an email thread.
func newCardEmailNote(thread string) string {
	return "\nThis card was started from Zach's email (zrl-bot thread " + thread + "). When it lands, tell him " +
		"there what changed: " + emailHow(thread) + ". Do not email him that work started, stalled or failed: " +
		"the factory shows that on its board.\n"
}

// inboxIntake checks a new card's inbox fields: its factory.source (email or voice for the dispatcher's
// cards, else board) and the dispatcher's own labels as paseo flags. An inbox card names its thread;
// only an inbox card carries an Inbox section or dispatcher labels, and only these, without the
// factory. prefix: source=<InboxSource> (the dispatcher's own agents) and voice-memo=<recording>. The
// card gets the same Fellow prompt as a board card, sub-agent section included.
func inboxIntake(b Body) (string, []string, error) {
	source := "board"
	if b.Origin == "email" || b.Origin == "voice" {
		source = b.Origin
	}
	if source == "board" {
		if strings.TrimSpace(b.Inbox) != "" || len(b.Labels) > 0 {
			return "", nil, errors.New("inbox and labels are only for email and voice cards")
		}
		return source, nil, nil
	}
	if strings.TrimSpace(b.Thread) == "" {
		return "", nil, errors.New("an email or voice card needs its thread")
	}
	keys := make([]string, 0, len(b.Labels))
	for k := range b.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		ok := k == "source" && InboxSource != "" && b.Labels[k] == InboxSource
		if k == "voice-memo" {
			ok = voiceMemoLabel.MatchString(b.Labels[k])
		}
		if !ok {
			return "", nil, fmt.Errorf("label %q is not one the dispatcher may set", k)
		}
		args = append(args, "--label", k+"="+b.Labels[k])
	}
	return source, args, nil
}

// voiceMemoLabel is the dispatcher's voice-memo label value: the recording, slugged.
var voiceMemoLabel = regexp.MustCompile(`^[a-z0-9-]{1,60}$`)
