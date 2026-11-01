package factory

import (
	"html"
	"strings"
)

// The presented revision and feedback exchange survive replacement and restart.
const (
	labelReadyHead     = "ready-head"
	labelReadyFeedback = "ready-feedback"
	feedbackPrefix     = "feedback answered:"
)

func isFeedbackNote(note string) bool { return strings.HasPrefix(note, feedbackPrefix) }

// feedbackEmail delivers the builder's answer without asking for another Ready summary.
func (f *Factory) feedbackEmail(a Agent, c Card, ask *Ask) email {
	text, _, _ := f.askDetail(c, ask)
	text = plainText(text, c.Repo, a.Repo)
	if ask.Overview != nil {
		text = plainText(ask.Overview.Overview, c.Repo, a.Repo) + "\n\n" + text
	}
	text += "\n\nReply \"yes\" or \"land\" to ship it, \"drop\" to drop it, or reply with feedback."
	return email{Subject: "Answer to your feedback", Text: text,
		HTML: "<div style=\"white-space:pre-wrap\">" + html.EscapeString(text) + "</div>"}
}

// Once finished work was presented, changed code returns for a fresh presentation.
func feedbackReadyReason(a Agent, why string) string {
	if why == "" && a.L(labelReadyFeedback) != "" && a.L(labelReady) != "" {
		if why = a.L(labelAskWhy); why == "" {
			why = "you had feedback on the finished work"
		}
	}
	return why
}
