package factory

import (
	"errors"
	"os"
	"strings"
)

const labelArchiveWatch = "archive-watch"
const labelFinished = "finished"
const stuckArchived = "Agent archived before this step finished."
const directWorktreeGoneNote = "done in one pass; worktree removed before its landing was checked"

var errWorktreeGone = errors.New("this card's worktree was removed when its workspace was archived; drop the card or start a new one")

func worktreeGone(cwd string) bool {
	if strings.TrimSpace(cwd) == "" {
		return false
	}
	info, err := os.Stat(cwd)
	return err != nil || !info.IsDir()
}

func archiveStage(a Agent) bool {
	switch a.L(labelStage) {
	case "building", gateRequest, gateDesign, gateSignoff, "plan", "plan-review", "review":
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
		if done, err := f.completeArchivedDirect(a); done || err != nil {
			if err != nil {
				return true, err
			}
			return true, f.retireReviews(a.ID)
		}
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

func (f *Factory) completeArchivedDirect(a Agent) (bool, error) {
	if atoi(a.L(roundsLabel(gateRequest))) > 0 {
		return false, nil
	}
	key := "checked:" + iso(a.LastActivityAt)
	if a.L(labelStuck) == stuckArchived {
		f.mu.Lock()
		seen := f.directArchiveChecked[a.ID] == key
		f.mu.Unlock()
		if seen {
			return false, nil
		}
	}
	direct, err := f.directReply(a)
	if err != nil || !direct {
		if err == nil || !f.Now().Before(historyDeadline(a)) {
			f.mu.Lock()
			if f.directArchiveChecked != nil {
				f.directArchiveChecked[a.ID] = key
			}
			f.mu.Unlock()
		}
		return false, nil
	}
	c, ok := f.card(a.ID)
	if !ok {
		c = Card{ID: a.ID, Short: short(a.ID), Title: a.Title, Host: a.Host, Cwd: a.Cwd, Repo: a.Repo,
			WorkspaceID: a.WorkspaceID}
	}
	if reply, err := f.storedReply(a); err == nil {
		f.nameCard(a, reply)
	}
	return true, f.markDirect(a, c)
}

// Recovery uses the normal reply path and starts a new review generation, keeping finished-work approval.
func (f *Factory) resumeArchived(c Card, message string, attachments []Attachment) error {
	_, err := f.act("send", Body{ID: c.ID, Message: message, Attachments: attachments}, true)
	return err
}
