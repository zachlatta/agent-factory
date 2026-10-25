package factory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Checks on what an agent's worktree actually did, read from its HEAD reflog. Each card runs in its
// own linked worktree, and git keeps a HEAD reflog per worktree, so the reflog holds only that card's
// moves: its own commits show as "commit…" entries, while other agents' commits reach it only through
// "rebase (pick)" or fast-forward entries. A range like base..HEAD would also hold those other
// commits once bin/land has rebased (plan review, round 2).

// reflogEntry is one HEAD reflog line: the commit HEAD moved to, and git's reason.
type reflogEntry struct{ SHA, Subject string }

// headReflog returns the worktree's HEAD reflog, newest first.
func (f *Factory) headReflog(ctx context.Context, dir string) ([]reflogEntry, error) {
	out, err := f.GitOut(ctx, dir, "reflog", "show", "--format=%H%x09%gs", "HEAD")
	if err != nil {
		return nil, err
	}
	var es []reflogEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, subj, ok := strings.Cut(line, "\t"); ok {
			es = append(es, reflogEntry{sha, subj})
		}
	}
	return es, nil
}

// landingMoveRe matches the reflog entries a plain landing writes: bin/land's rebase (pick, start,
// finish; also "pull … --rebase"), a checkout, a fast-forward, or the no-op reset an up-to-date
// rebase records. Anything else after review (a commit, an amend, "rebase (continue)" after a
// conflict fix, a real merge or reset) changed what lands.
var landingMoveRe = regexp.MustCompile(`^(?:(?:rebase(?: -i)?|pull\b[^(]*) \((?:start|pick|finish)\): |checkout: |(?:merge|pull)\b.*: Fast-forward$|reset: moving to HEAD$)`)

// ownCommitRe matches entries that made a commit of the worktree's own: new commits, amends, and
// rebased commits whose conflicts the agent resolved.
var ownCommitRe = regexp.MustCompile(`^(?:commit|cherry-pick|revert|rebase(?: -i)? \((?:continue|edit|reword|squash|fixup)\))\b`)

// unreviewedMoves lists the reflog entries made after HEAD was last at reviewed that are not plain
// landing moves. An error means the reflog does not show the reviewed commit at all.
func unreviewedMoves(es []reflogEntry, reviewed string) ([]string, error) {
	for i, e := range es {
		if e.SHA != reviewed {
			continue
		}
		var bad []string
		for _, n := range es[:i] {
			if !landingMoveRe.MatchString(n.Subject) {
				bad = append(bad, short(n.SHA)+" "+n.Subject)
			}
		}
		return bad, nil
	}
	return nil, fmt.Errorf("reviewed commit %s is not in the worktree's reflog", short(reviewed))
}

// landedAsReviewed reports whether a landed card put exactly its reviewed commits on main: every
// reviewed commit is on origin/main (patch-equivalent, so rebased is fine), and nothing but landing
// moves happened in the worktree after review. Call after landedOnMain, which fetches.
func (f *Factory) landedAsReviewed(cwd, reviewed string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, cwd, "cherry", "origin/main", reviewed)
	if err != nil {
		return false, "git cherry: " + err.Error()
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") {
			return false, "reviewed commit not on origin/main: " + short(strings.TrimSpace(line[1:]))
		}
	}
	es, err := f.headReflog(ctx, cwd)
	if err != nil {
		return false, "reading reflog: " + err.Error()
	}
	bad, err := unreviewedMoves(es, reviewed)
	if err != nil {
		return false, err.Error()
	}
	if len(bad) > 0 {
		return false, "changed after review: " + strings.Join(bad, "; ")
	}
	return true, ""
}

// isLinkedWorktree reports whether dir is a `git worktree add` checkout (its git dir differs from the
// common one), so its HEAD reflog is its own.
func (f *Factory) isLinkedWorktree(ctx context.Context, dir string) (bool, error) {
	out, err := f.GitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		return false, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return false, errors.New("unexpected rev-parse output")
	}
	return lines[0] != lines[1], nil
}

// ownFilesOutside returns the files that the worktree's own commits (ownCommitRe) changed outside
// prefix, e.g. "work/". Other agents' commits that arrived by rebase are not counted.
func (f *Factory) ownFilesOutside(cwd, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	linked, err := f.isLinkedWorktree(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, errors.New("not a linked worktree, so its reflog is shared")
	}
	es, err := f.headReflog(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if len(es) == 0 {
		return nil, errors.New("empty reflog")
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range es {
		if !ownCommitRe.MatchString(e.Subject) || seen[e.SHA] {
			continue
		}
		seen[e.SHA] = true
		files, err := f.GitOut(ctx, cwd, "show", "--name-only", "--format=", e.SHA)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(strings.TrimSpace(files), "\n") {
			if p != "" && !strings.HasPrefix(p, prefix) && !seen["file:"+p] {
				seen["file:"+p] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// dirtyStatus returns `git status --porcelain` for dir (untracked files included): empty when clean.
func (f *Factory) dirtyStatus(ctx context.Context, dir string) (string, error) {
	out, err := f.GitOut(ctx, dir, "status", "--porcelain")
	return strings.TrimSpace(out), err
}
