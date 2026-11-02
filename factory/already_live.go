package factory

import (
	"context"
	"strings"
	"time"
)

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
	if state == publicationLanded && f.hasPublishedWork(a) {
		// A status read failure is not proof that the workspace is absent or clean.
		if hold != nil && strings.Contains(hold[labelNote], "worktree status") {
			return reviewStopped, hold
		}
		return reviewAlreadyLanded, nil
	}
	if hold != nil {
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
		labelStage: "landing", labelAutoReview: "", labelSince: iso(f.Now()),
		labelLiveCompletion: a.L(labelReviewed), labelRemainingAsk: "1", labelNote: "checking completion of published work",
	})
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	return f.send(a.ID, f.completionMsgFor(a, "The reviewed work is already on origin/main. Write a current builder completion report for this request. Verify all planned deployment and acceptance steps and report anything unfinished. Do not publish again."))
}

// A shared main HEAD alone is not evidence that this request published work.
func (f *Factory) hasPublishedWork(a Agent) bool {
	if a.L(labelLandedVerified) != "" {
		return true
	}
	if a.L(labelOwnReviewed) == a.L(labelReviewed) && a.L(labelReviewed) != "" {
		return true
	}
	return f.ownReviewedWork(a)
}

// Only commits created in this linked worktree count. Rebase picks and fast
// forwards can import other requests' commits and do not establish ownership.
// Match patches as well as ids so a rebase does not discard ownership evidence.
func (f *Factory) ownReviewedWork(a Agent) bool {
	base, reviewed := a.L(labelBase), a.L(labelReviewed)
	if base == "" || base == reviewed || a.Cwd == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	linked, err := f.isLinkedWorktree(ctx, a.Cwd)
	if err != nil || !linked {
		return false
	}
	es, err := f.headReflog(ctx, a.Cwd)
	if err != nil {
		return false
	}
	revs, err := f.GitOut(ctx, a.Cwd, "rev-list", "--no-merges", base+".."+reviewed)
	if err != nil {
		return false
	}
	commits := strings.Fields(revs)
	for _, e := range es {
		if e.SHA == base {
			break
		}
		if !ownCommitRe.MatchString(e.Subject) {
			continue
		}
		for _, c := range commits {
			if c == e.SHA {
				return true
			}
		}
		id, err := f.patchID(ctx, a.Cwd, e.SHA)
		if err != nil || id == "" {
			continue
		}
		for _, c := range commits {
			other, err := f.patchID(ctx, a.Cwd, c)
			if err == nil && other == id {
				return true
			}
		}
	}
	return false
}
