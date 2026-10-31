package factory

const labelArchiveWatch = "archive-watch"
const labelFinished = "finished"
const stuckArchived = "Agent archived before this step finished."

func archiveStage(a Agent) bool {
	switch a.L(labelStage) {
	case "plan", "plan-review", "building", "review":
		return a.L(labelRole) == "" && a.L(labelRouting) == "" && !boardAnchor(a)
	}
	return false
}

// Only enrolled work is classified: older archives have ambiguous completion labels.
func interruptedArchive(a Agent) bool {
	return a.archived() && !a.parked() && archiveStage(a) && a.L(labelArchiveWatch) == "1" &&
		a.L(labelFinished) == "" && a.L(labelReplacedBy) == "" && a.L(labelRouted) == ""
}

func (f *Factory) tickArchive(a Agent) (bool, error) {
	if a.L(labelFinished) == "1" {
		if a.archived() {
			return true, nil
		}
		return true, f.archiveAgent(a.ID)
	}
	if interruptedArchive(a) {
		if err := f.markStuck(a, stuckArchived); err != nil {
			return true, err
		}
		return true, f.retireReviews(a.ID)
	}
	if !a.archived() && !f.handoffBusy(a) && archiveStage(a) && a.L(labelArchiveWatch) != "1" {
		archived, _, err := f.liveState(a.ID)
		if err != nil || archived {
			return true, err
		}
		return false, f.setLabels(a.ID, map[string]string{labelArchiveWatch: "1"})
	}
	return false, nil
}

// Recovery uses the normal reply path, including message formatting and review generation.
func (f *Factory) resumeArchived(c Card, message string, attachments []Attachment) error {
	_, err := f.Act("send", Body{ID: c.ID, Message: message, Attachments: attachments})
	return err
}
