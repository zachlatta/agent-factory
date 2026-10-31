package factory

const labelArchiveWatch = "archive-watch"
const labelFinished = "finished"
const stuckArchived = "Agent archived before this step finished."

func archiveStage(a Agent) bool {
	switch a.L(labelStage) {
	case "plan", "plan-review", "building", "review":
		return a.L(labelRole) == "" && !boardAnchor(a)
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

// Recovery is an explicit instruction. Queue the existing review gate after the new turn.
func (f *Factory) resumeArchived(c Card, message string, attachments []Attachment) error {
	g := f.gate(c.ID)
	g.mu.Lock()
	defer g.mu.Unlock()
	a := f.agent(c.ID)
	if err := f.retireReviews(c.ID); err != nil {
		return err
	}
	if _, err := f.chat(c.ID, message, attachments); err != nil {
		return err
	}
	labels := answerLabels(c, a, iso(f.Now()), f.guarded(c.Repo))
	labels[labelStuck] = ""
	labels[labelFinished] = ""
	labels[labelStage] = "building"
	labels[labelAutoReview] = "code"
	if a.L(labelStage) == "plan" || a.L(labelStage) == "plan-review" {
		labels[labelStage] = "plan"
		labels[labelAutoReview] = "plan"
	}
	if err := f.setLabels(c.ID, labels); err != nil {
		return err
	}
	f.unmarkArchived(c.ID)
	g.followUp = f.Now()
	f.delivered(c.ID, false)
	return nil
}
