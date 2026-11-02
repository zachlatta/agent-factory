package factory

import (
	"html"
	"strings"
)

// labelLiveMail reserves a completion notification before delivery, using mailOnce's
// retry and uncertain-send protection. Only confirmed complete published work queues it.
const labelLiveMail = "live-mail"
const labelLiveMailed = "live-mailed"

func (f *Factory) mailLiveDone(a Agent) error {
	if a.L(labelStage) != "landed" || a.L(labelLiveMail) != "pending" || a.L(labelLiveMailed) == "sent" || f.Mail == nil || f.mailWaits(a.ID) {
		return nil
	}
	if lineageEmailThread(f.byID(), a) == "" && lineageLabel(f.byID(), a, labelZachThread) == "" && f.cfg.ZachEmail == "" {
		return nil
	}
	reply, err := storedReplyOne(f.cfg.Home, a)
	if err != nil {
		return err
	}
	text := "The work is live and its review passed. Nothing remains.\n\n" + strings.TrimSpace(reply)
	_, err = f.mailOnce(a, labelLiveMailed, "sent", email{Subject: "The work is live", Text: text, HTML: "<p>" + html.EscapeString(text) + "</p>"})
	return err
}
