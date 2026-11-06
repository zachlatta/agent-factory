package factory

import (
	"cmp"
	"fmt"
	"html"
	"strings"
	"time"
)

// A direct result that asks Zach something waits on him with this note (finishDirect). Its question
// can carry a DUE: line: the board shows it, and a card with an email thread gets one reminder
// dueReminderLead before it.
const (
	noteDirectAsk     = "asks you: "
	directAskNoteCap  = 200
	directAskFallback = "The agent needs your answer"
	dueReminderLead   = 6 * time.Hour
	directAnswerRule  = "This answers the question you stopped on. Do what it asks and land any new work item " +
		"the way the repo's AGENTS.md says, then end with TRACK: direct. If it now needs a code change, build it, commit on your branch and " +
		"end with the RESULT: line instead."
)

// directAskNote is the wait note of a direct result asking title: one capped line, for a label.
func directAskNote(title string) string {
	return noteDirectAsk + trunc(strings.Join(strings.Fields(title), " "), directAskNoteCap)
}

// directAskStop reports whether a waits on Zach's answer to its direct result.
func directAskStop(a Agent) bool {
	return a.L(labelStage) == "zach" && strings.HasPrefix(a.L(labelNote), noteDirectAsk)
}

// directAskFor is the Ask for a direct result's question. Its work has landed, but what it wants is his
// answer, so Answer comes first. Mark done closes it when no answer is needed after all.
func directAskFor(note string) *Ask {
	return &Ask{Kind: askQuestion, directAsk: true, From: "builder",
		Title: cmp.Or(strings.TrimSpace(strings.TrimPrefix(note, noteDirectAsk)), directAskFallback),
		Why:   "The agent stopped on a question for you. Answer it and the agent carries on; mark it done if it needs no answer.",
		Actions: []Action{{ID: "reply", Label: "Answer", Primary: true, Hint: "Your answer"},
			act("done", "Mark done"), act("close", "Drop")},
		Choices: []Choice{
			{ID: "answer", Label: "Answer", Primary: true, Text: true, Hint: "Your answer", action: "send"},
			{ID: "done", Label: "Mark done", action: "done"},
			dropChoice(),
		}}
}

// parseDue reads a DUE: line's time: RFC 3339, seconds optional. A time with no zone is not accepted.
func parseDue(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// replyDue is the due label for card a's direct reply, "" when it has no usable DUE: line. A bad one
// is logged and ignored: the ask still stops.
func (f *Factory) replyDue(a Agent, reply string) string {
	rest, _ := ParseQuestions(reply)
	_, ov := ParseOverview(rest)
	if ov == nil || ov.Due == "" {
		return ""
	}
	due, ok := parseDue(ov.Due)
	if !ok {
		f.logf("%s DUE line ignored, not an ISO time with a zone: %s", short(a.ID), trunc(ov.Due, 60))
		return ""
	}
	return iso(due)
}

// waitDue is when a's question to Zach is due, or zero. It belongs to the direct ask: a later stop of
// another kind has no due time, even with the label still set.
func waitDue(a Agent) time.Time {
	if !directAskStop(a) {
		return time.Time{}
	}
	return parseTime(a.L(labelDue))
}

// dueText says a due time in plain words, at loc.
func dueText(due, now time.Time, loc *time.Location) string {
	when := due.In(loc).Format("Mon Jan 2, 3:04 PM")
	if !now.Before(due) {
		return "Overdue: was due " + when
	}
	return "Due " + when
}

// dueLine is dueText now, in Zach's time zone.
func (f *Factory) dueLine(due time.Time) string {
	loc := f.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	return dueText(due, f.Now(), loc)
}

// mailParkedAsks emails a direct question whose agent was already archived when it stopped: the Tick's
// Needs Zach move skips an archived agent. mailZach keeps it to one email per note and retries a failed send.
func (f *Factory) mailParkedAsks(agents []Agent) {
	if f.passive() || f.Mail == nil {
		return
	}
	for _, a := range agents {
		if !a.parked() || !directAskStop(a) {
			continue
		}
		c, ok := f.card(a.ID)
		if !ok {
			continue
		}
		if err := f.mailZach(a, c); err != nil {
			f.logf("%s: emailing Zach about its question: %v", short(a.ID), err)
		}
	}
}

// mailDueReminders sends Zach one reminder in the card's thread when a question that has waited since
// before the window is due within dueReminderLead. An ask that arrives inside the window is its own
// reminder, and past the time the board shows it overdue instead. It loops over every agent:
// a parked card is reminded too. One per due time: asking again with the same time does not resend.
func (f *Factory) mailDueReminders(agents []Agent, now time.Time) {
	if f.passive() || f.Mail == nil {
		return
	}
	byID := f.byID()
	for _, a := range agents {
		due := waitDue(a)
		if due.IsZero() || a.gone() || replacedPredecessor(byID, a) {
			continue
		}
		start := due.Add(-dueReminderLead)
		if now.Before(start) || !now.Before(due) || !parseTime(a.L(labelSince)).Before(start) {
			continue
		}
		mark := iso(due)
		if lineageLabel(byID, a, labelDueTold) == mark || f.mailWaits(a.ID) {
			continue
		}
		if lineageEmailThread(byID, a) == "" {
			continue // reminders go only in a thread Zach started
		}
		c, ok := f.card(a.ID)
		if !ok {
			continue
		}
		sent, err := f.mailOnce(a, labelDueTold, mark, f.dueReminderEmail(a, c, due))
		if err != nil {
			f.logf("%s: emailing Zach a due reminder: %v", short(a.ID), err)
			continue
		}
		if sent {
			f.logf("%s: reminded Zach of its due time %s", short(a.ID), mark)
		}
	}
}

// dueReminderEmail is the reminder for card c: the question, when it is due and how to answer.
func (f *Factory) dueReminderEmail(a Agent, c Card, due time.Time) email {
	title := c.Title
	if title == "" {
		title = a.Title
	}
	question := emailQuestion(a, f.replyAsk(c, a), []string{c.Repo, a.Repo}, title)
	lines := []string{
		fmt.Sprintf("Reminder: %s. This still waits on your answer.", f.dueLine(due)),
		question,
		`Reply with your answer, "done" to mark it done, or "drop" to stop it.`,
	}
	var t, h strings.Builder
	h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:16px;line-height:1.5;color:#1f2328;max-width:640px">`)
	for _, l := range lines {
		t.WriteString(l + "\n")
		fmt.Fprintf(&h, "<p>%s</p>", html.EscapeString(l))
	}
	if link := f.cardLink(a.ID); link != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", link)
		fmt.Fprintf(&h, `<p><a href="%s">Open the board</a></p>`, html.EscapeString(link))
	}
	h.WriteString("</div>")
	return email{Subject: "Reminder: " + truncWords(question, subjectCap), Text: t.String(), HTML: h.String()}
}
