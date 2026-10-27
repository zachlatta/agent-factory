package factory

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Checks on what an agent's worktree actually did, read from its HEAD reflog. Each card runs in its
// own linked worktree, and git keeps a HEAD reflog per worktree, so the reflog holds only that card's
// moves: its own commits show as "commit…" entries, while other agents' commits reach it only through
// "rebase (pick)" or fast-forward entries. A range like base..HEAD would also hold those other
// commits once the landing has rebased (plan review, round 2).

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

// ownLandingTime finds the last landed version of an own commit made after asked.
// hasOwn distinguishes a no-commit worktree from work that cannot be matched to main.
// Only a complete worktree reflog can prove that no own commit exists. No fetch or write runs.
func (f *Factory) ownLandingTime(ctx context.Context, dir string, asked time.Time) (at time.Time, hasOwn bool, err error) {
	if dir == "" {
		return at, false, errors.New("worktree unknown")
	}
	if asked.IsZero() {
		return at, false, errors.New("request time unknown")
	}
	linked, err := f.isLinkedWorktree(ctx, dir)
	if err != nil {
		return at, false, err
	}
	if !linked {
		return at, false, errors.New("shared reflog")
	}
	const limit = 2000
	out, err := f.GitOut(ctx, dir, "reflog", "show", "--max-count=2001", "--format=%H%x09%gs", "HEAD")
	if err != nil {
		return at, false, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) > limit {
		return at, false, errors.New("reflog limit reached")
	}
	var own, versions []reflogEntry
	complete := false
	seenOwn, seenVersion := map[string]bool{}, map[string]bool{}
	for _, line := range lines {
		sha, subject, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if subject == "" { // initial linked-worktree HEAD entry
			complete = true
		}
		if ownCommitRe.MatchString(subject) && !seenOwn[sha] {
			own = append(own, reflogEntry{sha, subject})
			seenOwn[sha] = true
		} else if strings.Contains(subject, "(pick):") && !seenVersion[sha] {
			versions = append(versions, reflogEntry{sha, subject})
			seenVersion[sha] = true
		}
	}
	commitTime := func(sha string) (time.Time, error) {
		out, err := f.GitOut(ctx, dir, "show", "-s", "--format=%cI", sha)
		if err != nil {
			return time.Time{}, err
		}
		return time.Parse(time.RFC3339, strings.TrimSpace(out))
	}
	onBranch := func(sha string) bool {
		return f.Git(ctx, dir, "merge-base", "--is-ancestor", sha, "HEAD") == nil &&
			f.Git(ctx, dir, "merge-base", "--is-ancestor", sha, "origin/main") == nil
	}
	for _, e := range own {
		made, eerr := commitTime(e.SHA)
		if eerr != nil {
			return at, hasOwn, eerr
		}
		if !made.After(asked) {
			continue
		}
		hasOwn = true
		if onBranch(e.SHA) {
			if made.After(at) {
				at = made.UTC()
			}
			continue
		}
		id, eerr := f.patchID(ctx, dir, e.SHA)
		if eerr != nil {
			return at, hasOwn, eerr
		}
		if id == "" {
			continue
		}
		for _, v := range versions {
			if !onBranch(v.SHA) {
				continue
			}
			other, eerr := f.patchID(ctx, dir, v.SHA)
			if eerr != nil {
				return at, hasOwn, eerr
			}
			if other != id {
				continue
			}
			when, eerr := commitTime(v.SHA)
			if eerr != nil {
				return at, hasOwn, eerr
			}
			if when.After(asked) && when.After(at) {
				at = when.UTC()
			}
		}
	}
	if !hasOwn && !complete {
		return at, false, errors.New("incomplete reflog")
	}
	return at, hasOwn, nil
}

// landingMoveRe matches the reflog entries a plain landing writes: its rebase (pick, start,
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
// reviewed commit is on origin/main by context-free (-U0) patch-id, and nothing but landing moves
// happened in the worktree after review. Like git cherry, git patch-id ignores whitespace-only
// differences; omitting context lines lets a clean rebase over a nearby edit pass while the reflog
// check still rejects any amend, new commit, reset, or conflict fix. An empty patch-id (such as an
// empty commit that was not fast-forwarded onto origin/main) counts as a miss. Call after
// landedOnMain, which fetches.
func (f *Factory) landedAsReviewed(cwd, reviewed string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base, err := f.GitOut(ctx, cwd, "merge-base", "origin/main", reviewed)
	if err != nil {
		return false, "git merge-base: " + err.Error()
	}
	base = strings.TrimSpace(base)
	revs, err := f.GitOut(ctx, cwd, "rev-list", "--reverse", "--no-merges", base+".."+reviewed)
	if err != nil {
		return false, "git rev-list: " + err.Error()
	}
	if commits := strings.Fields(revs); len(commits) > 0 {
		mainOut, err := f.GitOut(ctx, cwd, "rev-list", "--no-merges", base+"..origin/main")
		if err != nil {
			return false, "git rev-list: " + err.Error()
		}
		mainIDs := map[string]bool{}
		for _, c := range strings.Fields(mainOut) {
			id, err := f.patchID(ctx, cwd, c)
			if err != nil {
				return false, err.Error()
			}
			if id != "" {
				mainIDs[id] = true
			}
		}
		for _, c := range commits {
			id, err := f.patchID(ctx, cwd, c)
			if err != nil {
				return false, err.Error()
			}
			if id == "" || !mainIDs[id] {
				return false, "reviewed commit not on origin/main: " + short(c)
			}
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

// patchID returns the context-free (`git diff -U0 <c>^ <c> | git patch-id --stable`) patch-id of c,
// with diff flags pinned so repo or host git config cannot alter the diff format or shorten binary
// blob hashes on the index line.
func (f *Factory) patchID(ctx context.Context, cwd, c string) (string, error) {
	diff, err := f.GitOut(ctx, cwd, "diff", "--no-ext-diff", "--no-textconv", "--no-color",
		"--full-index", "--src-prefix=a/", "--dst-prefix=b/", "-U0", c+"^", c)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	return id, nil
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
		return nil, errors.New("not a linked worktree (its reflog is shared)")
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
