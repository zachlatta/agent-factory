package factory

import "strings"

// labelLiveCompletion ties the current builder completion report to its reviewed commit.
const labelLiveCompletion = "live-completion"

type reviewDisposition int

const (
	reviewEligible reviewDisposition = iota
	reviewAlreadyLanded
	reviewStopped
)

// assessReview keeps publication evidence separate from worktree freshness. A readable
// changed worktree always needs review, even when an older reviewed commit is public.
func (f *Factory) assessReview(a Agent, cwd string) (reviewDisposition, map[string]string) {
	hold := f.reviewHold(a, cwd)
	if strings.TrimSpace(a.L(labelReviewed)) == "" || (hold != nil && hold[labelStage] == "building") {
		return reviewStopped, hold
	}
	state, why := f.reviewedPublication(a)
	if state == publicationLanded {
		// A status read failure is not proof that the workspace is absent or clean.
		if hold != nil && strings.Contains(hold[labelNote], "worktree status") {
			return reviewStopped, hold
		}
		return reviewAlreadyLanded, nil
	}
	if hold != nil || state == publicationUnknown {
		if hold == nil {
			hold = map[string]string{labelStage: "zach", labelNote: unverifiedReviewNote, labelStuckFrom: "review", labelHeld: "", labelAutoReview: ""}
		}
		evidence := "The factory could not verify whether the reviewed work was published."
		if state == publicationAbsent {
			evidence = "The reviewed work is not on origin/main."
		}
		hold[labelNote] = strings.TrimRight(hold[labelNote], ". ") + ". " + evidence + " " + why
		return reviewStopped, hold
	}
	return reviewEligible, nil
}

func (f *Factory) beginLiveCompletion(a Agent) error {
	if a.L(labelLiveCompletion) == a.L(labelReviewed) {
		return nil
	}
	labels := clearHeldApproval(a, map[string]string{
		labelStage: "landing", labelAutoReview: "", labelSince: iso(f.Now()), labelLiveMail: "", labelLiveMailed: "",
		labelLiveCompletion: a.L(labelReviewed), labelRemainingAsk: "1", labelNote: "checking completion of published work",
	})
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	return f.send(a.ID, "The reviewed work is already on origin/main. Write a current builder completion report for this request. Verify all planned deployment and acceptance steps and report anything unfinished. Do not publish again."+remainingRule)
}
