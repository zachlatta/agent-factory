package factory

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
)

// The presented revision and feedback exchange survive replacement and restart.
const (
	labelReadyHead     = "ready-head"
	labelReadyFeedback = "ready-feedback"
	labelFeedbackHold  = "feedback-hold" // why an answer that asks nothing still waits for his yes
	labelFeedbackTold  = "feedback-told" // the exchange whose goes-ahead answer was emailed
	feedbackPrefix     = "feedback answered:"
	feedbackAgainWhy   = "you had feedback on the finished work"
	feedbackGoesAhead  = "Nothing more is needed from you. It goes ahead on its own."

	feedbackWithheldClosing = "Reply \"drop\" to drop it, or reply with feedback."
)

func isFeedbackNote(note string) bool { return strings.HasPrefix(note, feedbackPrefix) }

// feedbackHoldLine says that an answer asking nothing still waits for his yes, and why.
func feedbackHoldLine(why string) string {
	return fmt.Sprintf("This still needs your yes before it goes ahead: %s. Reply \"yes\" or \"land\", \"drop\", or feedback.", strings.TrimRight(why, ". "))
}

// feedbackEmail delivers the builder's answer without asking for another Ready summary.
func (f *Factory) feedbackEmail(a Agent, c Card, ask *Ask) email {
	closing := "Reply \"yes\" or \"land\" to ship it, \"drop\" to drop it, or reply with feedback."
	if hold := a.L(labelFeedbackHold); hold != "" {
		closing = feedbackHoldLine(plainText(hold, c.Repo, a.Repo))
	}
	return f.feedbackMail(a, c, ask, closing)
}

// feedbackMail is the answer, its questions (whole, or a pointer to the board) and closing, built from the
// agent's reply: the overview and questions come from there, so every caller's email carries them.
func (f *Factory) feedbackMail(a Agent, c Card, ask *Ask, closing string) email {
	repos := []string{c.Repo, a.Repo}
	detail, qs, ov := f.askDetail(c, ask)
	var parts []string
	if ov != nil {
		parts = append(parts, plainText(ov.Overview, repos...))
	}
	parts = append(parts, plainText(detail, repos...))
	shown, whole := emailQuestions(qs, repos)
	switch {
	case !whole:
		parts = append(parts, questionsForBoard(len(qs)))
		closing = feedbackWithheldClosing
	case len(qs) > 0:
		parts = append(parts, strings.Join(questionLines(shown), "\n"))
		closing += " Letters like 1A answer the questions."
	}
	parts = slices.DeleteFunc(append(parts, closing), func(p string) bool { return !readable(p) })
	text := strings.Join(parts, "\n\n")
	return email{Subject: "Answer to your feedback", Text: text,
		HTML: "<div style=\"white-space:pre-wrap\">" + html.EscapeString(text) + "</div>"}
}

// mailFeedbackGoesAhead emails t's answer to the feedback once per exchange before t moves on. A failed
// send is logged and does not hold the card back: he asked for nothing to wait on him here.
func (f *Factory) mailFeedbackGoesAhead(t Agent) {
	exchange := t.L(labelReadyFeedback)
	byID := f.byID()
	if t.L(labelFeedbackTold) == exchange || f.Mail == nil ||
		(lineageEmailThread(byID, t) == "" && lineageLabel(byID, t, labelZachThread) == "" && f.cfg.ZachEmail == "") {
		return
	}
	m := f.feedbackMail(t, Card{ID: t.ID, Repo: t.Repo}, readyAsk(""), feedbackGoesAhead)
	if _, err := f.mailOnce(t, labelFeedbackTold, exchange, m); err != nil {
		f.logf("%s: feedback answer not emailed: %v", short(t.ID), err)
	}
}

var feedbackNoneRe = regexp.MustCompile(`(?i)^(none|nothing)$`)

func saysNone(s string) bool { return feedbackNoneRe.MatchString(strings.Trim(s, "*_. \t\n")) }

// feedbackAsksNothing reports whether a builder's answer leaves Zach nothing to do, strictly enough for
// the change to land on it: DECISION and NEEDS FROM ZACH are a bare None or Nothing, with no question and
// no REMAINING item of any kind. Anything else keeps the ask.
func feedbackAsksNothing(reply string) bool {
	rest, qs := ParseQuestions(reply)
	if len(qs) > 0 {
		return false
	}
	_, ov := ParseOverview(rest)
	if ov == nil || !saysNone(ov.Decision) {
		return false
	}
	// a REMAINING line right under NEEDS FROM ZACH runs on into its text
	needs := slices.DeleteFunc(strings.Split(ov.Needs, "\n"), remainingRe.MatchString)
	if !saysNone(strings.Join(needs, "\n")) {
		return false
	}
	items, _ := ParseRemaining(reply)
	return len(items) == 0
}

// feedbackGate says why a passed answer that asks nothing still waits for Zach, or "". He has seen this
// exact version, so only "you asked for it" is waived; what stopped it at Ready for you for any other
// reason still holds, even if a later review round did not repeat it.
func (f *Factory) feedbackGate(t Agent, tier, review, asked string) string {
	why := f.finishedWorkReasons(t, tier, review, asked, true)
	for _, w := range strings.Split(t.L(labelAskWhy), "; ") {
		if w != "" && w != askedForItWhy && w != feedbackAgainWhy && !slices.Contains(why, w) {
			why = append(why, w)
		}
	}
	return strings.Join(why, "; ")
}

// Once finished work was presented, changed code returns for a fresh presentation.
func feedbackReadyReason(a Agent, why string) string {
	if why == "" && a.L(labelReadyFeedback) != "" && a.L(labelReady) != "" {
		if why = a.L(labelAskWhy); why == "" {
			why = feedbackAgainWhy
		}
	}
	return why
}
