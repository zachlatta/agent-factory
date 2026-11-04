package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Numbered answers in the daily digest. The digest opens with the waiting asks one word answers, newest
// first, and digest.json keeps what each sent digest numbered, so a number means what that digest showed
// and never shifts. Zach's reply in its thread reaches POST /api/answer. The whole reply must parse as
// words and numbers ("yes 1 3, drop 2", "1 yes, 2 drop") or nothing acts. Each number is looked up in
// that digest's list and checked again against the live card, then runs the same choice as the card's
// own email answer, through the same gates. The factory replies once in the thread with what was done
// and what was refused. A number is never adopted as the card's thread: later replies there are the
// digest's.

// digestListKeep is how long a sent digest's list answers replies.
const digestListKeep = 30 * 24 * time.Hour

const digestReplyExample = "yes 1 3, drop 2"

// digestList is what one sent digest numbered. Thread is set once the send returns; a digest that may
// have gone out has none, so a reply to it is not recognised rather than resolved against a guess.
type digestList struct {
	Day     string       `json:"day"`
	At      time.Time    `json:"at"`
	Thread  string       `json:"thread,omitempty"`
	Items   []digestItem `json:"items,omitempty"` // number N is Items[N-1]
	Handled []string     `json:"handled,omitempty"`
}

// digestItem is one numbered ask as the digest showed it. A later reply acts only while the live card
// still matches Since, Question and Choices.
type digestItem struct {
	Card     string   `json:"card"`
	Title    string   `json:"title"`
	Since    string   `json:"since,omitempty"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

// keptLists is the lists still within digestListKeep at now.
func keptLists(ls []digestList, now time.Time) []digestList {
	return slices.DeleteFunc(slices.Clone(ls), func(l digestList) bool { return l.At.Before(now.Add(-digestListKeep)) })
}

// waitingOn is card id, by its exact id, while it waits on Zach: not gone, not replaced, not a sub-agent.
func (f *Factory) waitingOn(id string) (Agent, Card, bool) {
	byID := f.byID()
	a, ok := byID[id]
	if !ok || a.gone() || a.L(labelRole) != "" || replacedPredecessor(byID, a) || a.L(labelStage) != "zach" {
		return Agent{}, Card{}, false
	}
	c, ok := f.card(id)
	if !ok || c.ID != id {
		return Agent{}, Card{}, false
	}
	return a, c, true
}

// numberedAsk is the digest's item for a waiting card and its ask, read now, when a number can answer it.
// Numbering a card and acting on a number both ask here, so a card only ever acts under the rule it was
// numbered by: it is emailed, its go answer is Land or Yes and does not close it, it has no questions (Yes
// would take the recommended options) and its builder is not still writing.
func (f *Factory) numberedAsk(a Agent, c Card) (digestItem, *Ask, bool) {
	ask := f.replyAsk(c, a)
	if !mailable(ask) || len(ask.Questions) > 0 || f.stillWriting(a, ask) {
		return digestItem{}, nil, false
	}
	g := goChoice(ask)
	if g == nil || (g.ID != "land" && g.ID != "yes") || g.action == "" || g.action == "close" {
		return digestItem{}, nil, false
	}
	title := c.Title
	if title == "" {
		title = a.Title
	}
	repos := []string{c.Repo, a.Repo}
	title = plainText(title, repos...)
	if title == "" {
		title = "(untitled)"
	}
	it := digestItem{Card: a.ID, Title: title, Since: a.L(labelSince), Question: digestQuestion(a, ask, repos)}
	for _, ch := range ask.Choices {
		it.Choices = append(it.Choices, ch.ID)
	}
	return it, ask, true
}

// digestQuestion is a numbered line's question: short, and with no title to name a bare decision, since
// the line already opens with the title.
func digestQuestion(a Agent, ask *Ask, repos []string) string {
	return truncWords(emailQuestion(a, ask, repos, ""), subjectCap)
}

// numberDecisions moves the asks a number can answer from s.ForYou to s.Decisions, newest first.
func (f *Factory) numberDecisions(s *Sections) {
	waiting := slices.Clone(s.ForYou)
	sortNewest(waiting)
	numbered := map[string]bool{}
	for _, l := range waiting {
		a, c, ok := f.waitingOn(l.ID)
		if !ok {
			continue
		}
		if it, _, ok := f.numberedAsk(a, c); ok {
			s.Decisions = append(s.Decisions, it)
			numbered[l.ID] = true
		}
	}
	s.ForYou = slices.DeleteFunc(slices.Clone(s.ForYou), func(l SectionLine) bool { return numbered[l.ID] })
}

// digestWords are the choices a numbered line offers, in the order it says them.
var digestWords = []struct{ ID, Word string }{{"land", "Land"}, {"yes", "Yes"}, {"no", "No"}, {"drop", "Drop"}}

func offeredWords(choices []string) string {
	var out []string
	for _, w := range digestWords {
		if slices.Contains(choices, w.ID) {
			out = append(out, w.Word)
		}
	}
	if len(out) < 2 {
		return strings.Join(out, "")
	}
	return strings.Join(out[:len(out)-1], ", ") + " or " + out[len(out)-1]
}

const (
	digestTitleCap = 60  // runes of the title on a numbered line
	digestLineCap  = 120 // runes of title and decision
)

// digestLine is a numbered line's words: the title, so that two cards with the same decision read
// differently and a number never answers an unknown change, then the decision.
func digestLine(it digestItem) string {
	title := truncWords(it.Title, digestTitleCap)
	return title + ": " + truncWords(it.Question, digestLineCap-len([]rune(title))-len(": "))
}

// writeDecisions writes the numbered list, at the top of the digest below anything due. The HTML line links to the card; the
// text one stays short.
func (f *Factory) writeDecisions(t, h *strings.Builder, items []digestItem) {
	if len(items) == 0 {
		return
	}
	head, how := "Answer by reply", "Reply with a word and the numbers, like: "+digestReplyExample
	fmt.Fprintf(t, "%s\n%s\n", head, how)
	fmt.Fprintf(h, `<h3 style="margin:16px 0 4px;font-size:15px">%s</h3><p style="margin:0;color:#59636e">%s</p><ol style="margin:4px 0 0;padding-left:24px">`,
		html.EscapeString(head), html.EscapeString(how))
	for i, it := range items {
		words := offeredWords(it.Choices)
		fmt.Fprintf(t, "%d. %s (%s)\n", i+1, digestLine(it), words)
		line := html.EscapeString(digestLine(it))
		if link := f.cardLink(it.Card); link != "" {
			line = `<a href="` + html.EscapeString(link) + `" style="color:#0969da">` + line + `</a>`
		}
		h.WriteString(`<li>` + line + `<span style="color:#59636e"> (` + html.EscapeString(words) + `)</span></li>`)
	}
	t.WriteString("\n")
	h.WriteString("</ol>")
}

// bindDigestThread stores the thread send_email returned on that day's list.
func (f *Factory) bindDigestThread(day string, sent json.RawMessage) error {
	var res struct {
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(sent, &res) != nil || res.ThreadID == "" {
		return errors.New("send_email gave no thread id")
	}
	return f.updateDigestList(func(l digestList) bool { return l.Day == day }, func(l *digestList) { l.Thread = res.ThreadID })
}

// updateDigestList changes the newest list that match accepts, in digest.json.
func (f *Factory) updateDigestList(match func(digestList) bool, change func(*digestList)) error {
	f.digestMu.Lock()
	defer f.digestMu.Unlock()
	d, err := f.readDigest()
	if err != nil {
		return err
	}
	for i := len(d.Lists) - 1; i >= 0; i-- {
		if match(d.Lists[i]) {
			change(&d.Lists[i])
			return f.writeDigest(d)
		}
	}
	return errors.New("no such digest list")
}

// digestListFor is the newest stored list sent in thread.
func (f *Factory) digestListFor(thread string) (digestList, bool) {
	f.digestMu.Lock()
	d, err := f.readDigest()
	f.digestMu.Unlock()
	if err != nil {
		// an unreadable digest.json must not stop answers to every other card
		f.logf("daily digest: reading %v", err)
		return digestList{}, false
	}
	for i := len(d.Lists) - 1; i >= 0; i-- {
		if d.Lists[i].Thread == thread {
			return d.Lists[i], true
		}
	}
	return digestList{}, false
}

// digestAnswer is one number of a reply and the word Zach gave it.
type digestAnswer struct {
	Number int
	Word   string
}

var (
	digestSeparators = regexp.MustCompile(`[\s,;:&]+`)
	digestNumber     = regexp.MustCompile(`^[0-9]{1,4}$`)
)

// digestReplyWord reports whether w answers a number: the reply lists, without the done words, since an
// ask that has one is not numbered.
func digestReplyWord(w string) bool {
	return w == "no" || slices.Contains(approveWords, w) || slices.Contains(landWords, w) || slices.Contains(dropWords, w)
}

// parseDigestReply reads a whole reply as clauses of one form: word then numbers ("yes 1 3, drop 2") or
// numbers then word ("1 yes, 2 drop"). Anything else, a number with no word, or one number given two
// words, is not ok. A word is required because a landing cannot be taken back.
func parseDigestReply(text string) ([]digestAnswer, bool) {
	type token struct {
		number int
		word   string
	}
	var fields []string
	for _, x := range digestSeparators.Split(cleanReply(text), -1) {
		if x = strings.TrimRight(x, ".!"); x != "" && x != "and" {
			fields = append(fields, x)
		}
	}
	var toks []token
	for i := 0; i < len(fields); i++ {
		switch {
		case digestNumber.MatchString(fields[i]):
			n, _ := strconv.Atoi(fields[i])
			toks = append(toks, token{number: n})
		case i+1 < len(fields) && digestReplyWord(fields[i]+" "+fields[i+1]):
			toks = append(toks, token{word: fields[i] + " " + fields[i+1]})
			i++
		case digestReplyWord(fields[i]):
			toks = append(toks, token{word: fields[i]})
		default:
			return nil, false
		}
	}
	if len(toks) == 0 {
		return nil, false
	}
	wordFirst := toks[0].word != ""
	words := map[int]string{}
	assign := func(numbers []int, word string) bool {
		for _, n := range numbers {
			if old, seen := words[n]; seen && old != word {
				return false
			}
			words[n] = word
		}
		return len(numbers) > 0
	}
	var numbers []int
	word := ""
	for _, tk := range toks {
		switch {
		case tk.word == "":
			numbers = append(numbers, tk.number)
		case wordFirst:
			if word != "" && !assign(numbers, word) {
				return nil, false
			}
			numbers, word = nil, tk.word
		default:
			if !assign(numbers, tk.word) {
				return nil, false
			}
			numbers = nil
		}
	}
	if wordFirst && !assign(numbers, word) {
		return nil, false
	}
	if !wordFirst && len(numbers) > 0 {
		return nil, false
	}
	var out []digestAnswer
	for n, w := range words {
		out = append(out, digestAnswer{n, w})
	}
	slices.SortFunc(out, func(a, b digestAnswer) int { return a.Number - b.Number })
	return out, true
}

// answerDigest handles a reply in a digest's thread. ok is false when no stored list has the thread,
// so the card path takes it. The caller holds replies.mu. The message is recorded as handled before
// anything acts, so a repeated POST does nothing and sends nothing; a stop partway leaves the remaining
// numbers unanswered and still waiting on the board.
func (f *Factory) answerDigest(b Body) (res map[string]string, ok bool, err error) {
	list, ok := f.digestListFor(b.Thread)
	if !ok {
		return nil, false, nil
	}
	if slices.Contains(list.Handled, b.MessageID) {
		return map[string]string{"action": "duplicate"}, true, nil
	}
	saved, err := f.replies.read()
	if err != nil {
		return nil, true, err
	}
	if !f.passive() {
		err := f.updateDigestList(func(l digestList) bool { return l.Thread == b.Thread },
			func(l *digestList) { l.Handled = append(l.Handled, b.MessageID) })
		if err != nil {
			return nil, true, err
		}
		if f.replies.done == nil {
			f.replies.done = map[string]bool{}
		}
		f.replies.done[b.Thread+"\x00"+b.MessageID] = true
	}
	answers, parsed := parseDigestReply(b.Text)
	if !parsed {
		f.logf("digest reply not understood: nothing changed")
		f.replyToDigest(b.MessageID, "Nothing changed: I couldn't read that reply. Reply with a word and the numbers, like: "+digestReplyExample)
		return map[string]string{"action": "digest-unparsed"}, true, nil
	}
	var done, refused []string
	for _, ans := range answers {
		line, acted := f.applyDigestAnswer(list, ans, b, saved)
		if acted {
			done = append(done, line)
		} else {
			refused = append(refused, line)
		}
	}
	var parts []string
	if len(done) > 0 {
		parts = append(parts, "Done:\n"+strings.Join(done, "\n"))
	}
	if len(refused) > 0 {
		parts = append(parts, "Refused:\n"+strings.Join(refused, "\n"))
	}
	f.replyToDigest(b.MessageID, strings.Join(parts, "\n\n"))
	f.logf("digest reply: %d done, %d refused", len(done), len(refused))
	return map[string]string{"action": "digest", "done": strconv.Itoa(len(done)), "refused": strconv.Itoa(len(refused))}, true, nil
}

// applyDigestAnswer runs one number of a reply, and returns its line for the reply and whether it acted.
// Anything the card's own answer would stop on, or any way the card differs from what the digest showed,
// refuses it.
func (f *Factory) applyDigestAnswer(list digestList, ans digestAnswer, b Body, saved map[string]pendingReply) (string, bool) {
	if ans.Number < 1 || ans.Number > len(list.Items) {
		return fmt.Sprintf("%d: not a number in that digest.", ans.Number), false
	}
	it := list.Items[ans.Number-1]
	refuse := func(why string) (string, bool) {
		line := fmt.Sprintf("%d. %s: %s", ans.Number, it.Title, why)
		if link := f.cardLink(it.Card); link != "" {
			line += " " + link
		}
		return line, false
	}
	a, c, ok := f.waitingOn(it.Card)
	if !ok {
		return refuse("it has moved on and no longer waits on you.")
	}
	cur, ask, ok := f.numberedAsk(a, c)
	if !ok {
		return refuse("it needs more than one word now.")
	}
	if cur.Since != it.Since || cur.Question != it.Question || !slices.Equal(cur.Choices, it.Choices) {
		return refuse("it has changed since that digest.")
	}
	reply := parseAnswer(ans.Word, ask)
	ch := choiceByID(ask, reply.Choice)
	if ch == nil || !exactAnswer(ans.Word, ask) {
		return refuse(fmt.Sprintf("%q is not one of its answers (%s).", ans.Word, offeredWords(it.Choices)))
	}
	if err := f.markReply(a, b); err != nil {
		f.logf("%s digest answer %s not recorded: %v", short(a.ID), ch.ID, err)
		return refuse("it could not be recorded.")
	}
	if _, err := f.runChoice(a.ID, *ch, Body{Message: reply.Message, Via: "email"}); err != nil {
		f.logf("%s digest answer %s failed: %v", short(a.ID), ch.ID, err)
		return refuse(trunc(plainText(err.Error(), c.Repo, a.Repo), 200))
	}
	if err := f.replies.clearCard(saved, a.ID); err != nil {
		f.logf("%s: clearing a clarification: %v", short(a.ID), err)
	}
	f.logf("%s answered by digest number %d: %s", short(a.ID), ans.Number, ch.ID)
	if err := f.reload(); err != nil { // the line below and the next number read the board as it is now
		f.logf("board reload after a digest answer: %v", err)
	}
	line := fmt.Sprintf("%d. %s: %s", ans.Number, ch.Label, it.Title)
	if state := workingState[f.agent(a.ID).L(labelStage)]; state != "" {
		line += " (now " + state + ")" // a gate can send a Land back to review
	}
	return line, true
}

// replyToDigest answers in the digest's thread. A failed send is only logged: the message is already
// handled, so it is never repeated.
func (f *Factory) replyToDigest(messageID, text string) {
	if f.Mail == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h := `<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;white-space:pre-line">` +
		html.EscapeString(text) + `</div>`
	if _, err := f.sendMail(ctx, "", "reply_to_message", map[string]any{"message_id": messageID, "text": text, "html": h, "omit_quote": true}); err != nil {
		f.logf("telling Zach what his digest reply did: %v", err)
	}
}
