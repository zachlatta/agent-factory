package factory

import (
	"context"
	"strings"
	"time"
)

// Publication evidence describes only whether the recorded reviewed work reached main.
// It says nothing about subsequent changes or unfinished deployment steps.
type publicationState int

const (
	publicationUnknown publicationState = iota
	publicationAbsent
	publicationLanded
)

// reviewedPublication uses the main checkout so retired worktrees remain verifiable.
// A failed fetch never permits conclusions from a potentially stale remote ref.
func (f *Factory) reviewedPublication(a Agent) (publicationState, string) {
	reviewed := strings.TrimSpace(a.L(labelReviewed))
	if reviewed == "" {
		return publicationUnknown, "no recorded reviewed commit"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cwd := f.repoDir(a.Repo)
	if err := f.Git(ctx, cwd, "fetch", "-q", "origin", "main"); err != nil {
		return publicationUnknown, "fetching origin/main: " + err.Error()
	}
	return f.reviewedPatches(ctx, cwd, reviewed)
}

// reviewedPatches checks a complete context-free patch comparison after a successful fetch.
// A missing object or incomplete comparison is unknown, not proof of absence.
func (f *Factory) reviewedPatches(ctx context.Context, cwd, reviewed string) (publicationState, string) {
	base, err := f.GitOut(ctx, cwd, "merge-base", "origin/main", reviewed)
	if err != nil {
		return publicationUnknown, "git merge-base: " + err.Error()
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return publicationUnknown, "git merge-base returned no commit"
	}
	if base == reviewed {
		return publicationLanded, ""
	}
	revs, err := f.GitOut(ctx, cwd, "rev-list", "--reverse", "--no-merges", base+".."+reviewed)
	if err != nil {
		return publicationUnknown, "git rev-list: " + err.Error()
	}
	commits := strings.Fields(revs)
	if len(commits) == 0 {
		return publicationUnknown, "reviewed range has no comparable commits"
	}
	if len(commits) > 0 {
		mainOut, err := f.GitOut(ctx, cwd, "rev-list", "--no-merges", base+"..origin/main")
		if err != nil {
			return publicationUnknown, "git rev-list: " + err.Error()
		}
		mainIDs := map[string]bool{}
		for _, c := range strings.Fields(mainOut) {
			id, err := f.patchID(ctx, cwd, c)
			if err != nil {
				return publicationUnknown, err.Error()
			}
			if id != "" {
				mainIDs[id] = true
			}
		}
		missing := ""
		for _, c := range commits {
			id, err := f.patchID(ctx, cwd, c)
			if err != nil {
				return publicationUnknown, err.Error()
			}
			if id == "" || !mainIDs[id] {
				missing = c
			}
		}
		if missing != "" {
			return publicationAbsent, "reviewed commit not on origin/main: " + short(missing)
		}
	}
	return publicationLanded, ""
}
