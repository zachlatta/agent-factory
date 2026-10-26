package factory

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Zach's email answers to a card waiting on him. The zrl-bot dispatcher checks the sender, then POSTs
// his reply here (/api/answer: thread, message_id, text). Only a whole reply from a closed word list
// approves or drops; anything else goes to the agent as his instructions. The factory confirms what
// it did with a one-line reply in the thread.

// ErrNoCard, ErrNotWaiting and ErrSharedThread tell the dispatcher to fall back to its own handling.
var (
	ErrNoCard     = errors.New("no card for this thread")
	ErrNotWaiting = errors.New("the card is not waiting on Zach")
	// ErrSharedThread: two or more cards in the thread wait on Zach, so the reply could be for either
	ErrSharedThread = errors.New("several cards in this thread wait on Zach")
)

var (
	approveWords = []string{"approve", "approved", "yes", "y", "ok", "okay", "lgtm", "go", "go ahead", "ship it"}
	dropWords    = []string{"drop", "drop it", "close", "close it", "cancel"}
	letterReply  = regexp.MustCompile(`^\d[a-f]([ ,]+\d[a-f])*$`)
	letterPair   = regexp.MustCompile(`(\d)([a-f])`)
	quoteHeader  = regexp.MustCompile(`^On .+wrote:$`)
	spaces       = regexp.MustCompile(`\s+`)
)

// replyText is the reply without quoted text or signature, as he wrote it.
func replyText(text string) string {
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, ">") || quoteHeader.MatchString(t) || l == "-- " || t == "--" ||
			strings.HasPrefix(t, "Sent from my") {
			break
		}
		keep = append(keep, l)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// cleanReply is replyText lowercased and single-spaced, with no trailing "." or "!".
func cleanReply(text string) string {
	return strings.TrimRight(strings.ToLower(spaces.ReplaceAllString(replyText(text), " ")), ".! ")
}

// answer is what an email reply does: an Act action and its message. Action "" does nothing.
type answer struct{ Action, Message string }

// parseAnswer applies the reply rule to Zach's reply for ask.
func parseAnswer(text string, ask *Ask) answer {
	clean := cleanReply(text)
	if clean == "" {
		return answer{}
	}
	yesActs := ask.Yes == "approve" || ask.Yes == "recommend"
	switch {
	case slices.Contains(approveWords, clean) && yesActs && actionFor(ask, ask.Yes) != nil:
		return answer{Action: ask.Yes}
	case slices.Contains(dropWords, clean) && actionFor(ask, "close") != nil:
		return answer{Action: "close"}
	case letterReply.MatchString(clean):
		if expanded, ok := expandLetters(clean, ask.Questions); ok {
			// approve carries answers; recommend does not (it applies the brief as written), so on a
			// brief they go to the agent, as the board sends them
			if ask.Yes == "approve" && actionFor(ask, "approve") != nil {
				return answer{Action: "approve", Message: expanded}
			}
			return answer{Action: "send", Message: "Zach's answers:\n" + expanded}
		}
	}
	return answer{Action: "send", Message: replyText(text)}
}

// expandLetters turns "1b 2a" into the questions and chosen options, as the board sends them. ok is
// false when a number or letter is not among the questions.
func expandLetters(clean string, qs []Question) (string, bool) {
	var b strings.Builder
	for _, m := range letterPair.FindAllStringSubmatch(clean, -1) {
		qi, oi := int(m[1][0]-'1'), int(m[2][0]-'a')
		if qi < 0 || qi >= len(qs) || oi >= len(qs[qi].Options) {
			return "", false
		}
		fmt.Fprintf(&b, "%d. %s\n   → %s\n", qi+1, qs[qi].Text, qs[qi].Options[oi])
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// waitingCard is the one live factory card waiting on Zach in email thread thread. Several cards can
// share a thread (cards started from one email), so only waiting cards count, and a reply that two
// or more of them could be answering acts on none: the dispatcher passes it on as a follow-up.
func (f *Factory) waitingCard(thread string) (Agent, error) {
	var waiting []Agent
	found := false
	for _, a := range f.agents() {
		if a.gone() || a.L(labelStage) == "" || a.L(labelRole) != "" {
			continue
		}
		if a.L(labelZachThread) != thread && emailThread(a) != thread {
			continue
		}
		found = true
		if a.L(labelStage) == "zach" {
			waiting = append(waiting, a)
		}
	}
	switch {
	case len(waiting) == 1:
		return waiting[0], nil
	case len(waiting) > 1:
		return Agent{}, ErrSharedThread
	case found:
		return Agent{}, ErrNotWaiting
	}
	return Agent{}, ErrNoCard
}

// answerByEmail applies Zach's email reply b to the card waiting on him in b.Thread.
func (f *Factory) answerByEmail(b Body) (map[string]string, error) {
	if b.Thread == "" || b.MessageID == "" {
		return nil, errors.New("thread and message_id are required")
	}
	a, err := f.waitingCard(b.Thread)
	if err != nil {
		return nil, err
	}
	if a.L(labelAnswered) == b.MessageID {
		return map[string]string{"card": a.ID, "action": "duplicate"}, nil
	}
	c, ok := f.card(a.ID)
	if !ok {
		return nil, ErrNoCard
	}
	ask := askFor(c, a)
	if ask == nil {
		return nil, ErrNotWaiting
	}
	_, ask.Questions, _ = f.askDetail(c, ask)
	ans := parseAnswer(b.Text, ask)
	if ans.Action == "" {
		return map[string]string{"card": a.ID, "action": "none"}, nil
	}
	// answered first: a repeated POST of this message does nothing
	if err := f.setLabels(a.ID, map[string]string{labelAnswered: b.MessageID}); err != nil {
		return nil, err
	}
	planWait := c.Awaiting == "plan" || planPending(a)
	_, err = f.Act(ans.Action, Body{ID: a.ID, Message: ans.Message, Via: "email"})
	f.confirmAnswer(a, b.MessageID, ackLine(ans.Action, planWait, err, f.cardLink(a.ID)))
	if err != nil {
		return nil, err
	}
	f.logf("%s answered by email: %s", short(a.ID), ans.Action)
	return map[string]string{"card": a.ID, "action": ans.Action}, nil
}

// ackLine is the one-line confirmation of an email answer.
func ackLine(action string, plan bool, err error, link string) string {
	if err != nil {
		s := "That didn't go through: " + trunc(err.Error(), 200) + "."
		if link != "" {
			s += " Answer on the board: " + link
		}
		return s
	}
	switch {
	case action == "close":
		return "Dropped: the card is archived and nothing lands."
	case action == "recommend":
		return "Going with its recommendations: building now."
	case action == "approve" && plan:
		return "Approved: building now."
	case action == "approve":
		return "Approved: landing now."
	case plan:
		return "Sent back: the agent is revising the plan."
	}
	return "Sent to the agent: it will act on it."
}

// confirmAnswer replies in the thread to Zach's answer with line. A failure is only logged.
func (f *Factory) confirmAnswer(a Agent, messageID, line string) {
	if f.Mail == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h := `<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328">` +
		html.EscapeString(line) + `</div>`
	if _, err := f.Mail.Call(ctx, "reply_to_message", map[string]any{"message_id": messageID, "text": line,
		"html": h, "omit_quote": true}); err != nil {
		f.logf("%s: confirming the email answer: %v", short(a.ID), err)
	}
}

// emailFollowUp wraps an email answer as a follow-up block (peers.go), so reviewer prompts and the
// card's request show it like any follow-up email.
func (f *Factory) emailFollowUp(msg string) string {
	return followUpPrefix + f.Now().In(f.cfg.Location).Format(time.RFC1123Z) + followUpSuffix + "\n" + msg + "\n" + emailEnd
}
