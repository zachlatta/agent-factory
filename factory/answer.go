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
// approves or drops; anything else goes to the agent as his instructions. A reply that worked gets no
// email back (the board shows the new state); one that did nothing gets one plain line saying so.

// ErrNoCard, ErrNotWaiting and ErrSharedThread tell the dispatcher to fall back to its own handling.
var (
	ErrNoCard     = errors.New("no card for this thread")
	ErrNotWaiting = errors.New("the card is not waiting on Zach")
	// ErrSharedThread: two or more cards in the thread wait on Zach, so the reply could be for either
	ErrSharedThread = errors.New("several cards in this thread wait on Zach")
)

var (
	approveWords = []string{"approve", "approved", "yes", "y", "ok", "okay", "lgtm", "go", "go ahead", "ship it"}
	doneWords    = []string{"done", "mark done", "mark it done"}
	dropWords    = []string{"drop", "drop it", "close", "close it", "cancel"}
	letterReply  = regexp.MustCompile(`^\d[a-f]([ ,]+\d[a-f])*$`)
	letterPair   = regexp.MustCompile(`(\d)([a-f])`)
	loneLetter   = regexp.MustCompile(`^[a-f]$`)
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

// answer is what an email reply does: one of the ask's Choices (choose.go), the same buttons the
// board shows, and its message. Choice "" does nothing.
type answer struct{ Choice, Message string }

// parseAnswer applies the reply rule to Zach's reply for ask. A reply word acts only where the ask
// has the button it stands for (replyWords lists the same ones); anything else is the ask's text
// choice (Change, or his Answer).
func parseAnswer(text string, ask *Ask) answer {
	if ask.Ready != nil {
		return readyAnswer(text, ask.Questions)
	}
	clean := cleanReply(text)
	if clean == "" {
		return answer{}
	}
	if len(ask.Questions) == 1 && loneLetter.MatchString(clean) {
		clean = "1" + clean // one question: its letter alone answers it
	}
	goCh, textCh, dropCh := goChoice(ask), textChoice(ask), dropOf(ask)
	switch {
	case slices.Contains(approveWords, clean) && goCh != nil:
		// on an approve, yes takes the recommended (first) option of every question, as the email says
		if goCh.action == "approve" && len(ask.Questions) > 0 {
			return answer{Choice: goCh.ID, Message: recommendedAnswers(ask.Questions)}
		}
		return answer{Choice: goCh.ID}
	case clean == "no" && choiceByID(ask, "no") != nil:
		return answer{Choice: "no"}
	case slices.Contains(doneWords, clean) && choiceByID(ask, "done") != nil:
		return answer{Choice: "done"}
	case slices.Contains(dropWords, clean) && dropCh != nil:
		return answer{Choice: dropCh.ID}
	case letterReply.MatchString(clean):
		if expanded, ok := expandLetters(clean, ask.Questions); ok {
			if goCh != nil && goCh.action == "approve" {
				return answer{Choice: goCh.ID, Message: expanded}
			}
			if textCh != nil {
				return answer{Choice: textCh.ID, Message: "Zach's answers:\n" + expanded}
			}
		}
	}
	if textCh == nil {
		return answer{}
	}
	return answer{Choice: textCh.ID, Message: replyText(text)}
}

// recommendedAnswers is every question answered with its first (recommended) option.
func recommendedAnswers(qs []Question) string {
	var b strings.Builder
	for i := range qs {
		fmt.Fprintf(&b, "%da ", i+1)
	}
	expanded, _ := expandLetters(strings.TrimSpace(b.String()), qs)
	return expanded
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
	byID := f.byID()
	for _, a := range f.agents() {
		if a.gone() || a.L(labelStage) == "" || a.L(labelRole) != "" || replacedPredecessor(byID, a) {
			continue
		}
		if !lineageHasThread(byID, a, thread) {
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
	if lineageHasAnswered(f.byID(), a, b.MessageID) {
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
	if ans.Choice == "" && strings.TrimSpace(replyText(b.Text)) == "" {
		return map[string]string{"card": a.ID, "action": "none"}, nil
	}
	ch := choiceByID(ask, ans.Choice)
	if ch == nil {
		return nil, ErrNotWaiting // no button for it: the dispatcher passes the reply on as a follow-up
	}
	// answered first: a repeated POST of this message does nothing
	if err := f.setLabels(a.ID, map[string]string{labelAnswered: b.MessageID}); err != nil {
		return nil, err
	}
	_, err = f.runChoice(a.ID, *ch, Body{Message: ans.Message, Via: "email"})
	if err != nil {
		// his answer did nothing: say so once, plainly; a reply that worked gets no echo
		f.logf("%s email answer %s failed: %v", short(a.ID), ch.ID, err)
		f.answerFailed(a, b.MessageID)
		return nil, err
	}
	f.logf("%s answered by email: %s", short(a.ID), ch.ID)
	return map[string]string{"card": a.ID, "action": ch.action}, nil
}

// answerFailed replies in the thread to Zach's answer that it did not go through, with the board
// link. A failure to send is only logged.
func (f *Factory) answerFailed(a Agent, messageID string) {
	if f.Mail == nil {
		return
	}
	line := "That reply didn't go through."
	if link := f.cardLink(a.ID); link != "" {
		line += " Answer on the board: " + link
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h := `<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328">` +
		html.EscapeString(line) + `</div>`
	if _, err := f.sendMail(ctx, a.ID, "reply_to_message", map[string]any{"message_id": messageID, "text": line,
		"html": h, "omit_quote": true}); err != nil {
		f.logf("%s: telling Zach his email answer failed: %v", short(a.ID), err)
	}
}

// emailFollowUp wraps an email answer as a follow-up block (peers.go), so reviewer prompts and the
// card's request show it like any follow-up email.
func (f *Factory) emailFollowUp(msg string) string {
	return followUpPrefix + f.Now().In(f.cfg.Location).Format(time.RFC1123Z) + followUpSuffix + "\n" + msg + "\n" + emailEnd
}
