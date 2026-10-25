package factory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitRun runs git in dir and fails the test on error. Commits use a fixed identity.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t", "GIT_EDITOR=true", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes path in dir and commits it.
func commitFile(t *testing.T, dir, path, content string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", path)
	gitRun(t, dir, "commit", "-q", "-m", "change "+path)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// repos sets up a bare origin, another agent's clone ("other") and a card's linked worktree ("wt",
// branched off origin/main), the way Paseo cards run.
type repos struct{ main, other, wt string }

func newRepos(t *testing.T) repos {
	t.Helper()
	root := t.TempDir()
	r := repos{main: filepath.Join(root, "main"), other: filepath.Join(root, "other"), wt: filepath.Join(root, "wt")}
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", "up.git")
	gitRun(t, root, "clone", "-q", "up.git", "main")
	commitFile(t, r.main, "README.md", "desk\n")
	gitRun(t, r.main, "push", "-q", "origin", "main")
	gitRun(t, root, "clone", "-q", "up.git", "other")
	gitRun(t, r.main, "worktree", "add", "-q", "-b", "card", r.wt, "origin/main")
	return r
}

// otherPushes lands a commit from another agent on origin/main.
func (r repos) otherPushes(t *testing.T, path string) {
	t.Helper()
	gitRun(t, r.other, "pull", "-q", "--rebase", "origin", "main")
	commitFile(t, r.other, path, path+"\n")
	gitRun(t, r.other, "push", "-q", "origin", "main")
}

// land does what bin/land does: fetch, rebase, push HEAD:main.
func (r repos) land(t *testing.T) {
	t.Helper()
	gitRun(t, r.wt, "fetch", "-q", "origin", "main")
	gitRun(t, r.wt, "rebase", "-q", "--autostash", "origin/main")
	gitRun(t, r.wt, "push", "-q", "origin", "HEAD:main")
}

func realGitFactory(t *testing.T) *Factory {
	f, _ := newTestFactory(t)
	f.Git, f.GitOut = runGit, runGitOut
	return f
}

func TestLandedAsReviewed(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		// after review, before landing
		between func(t *testing.T, r repos)
	}{
		{name: "clean rebase over another agent's push", ok: true, between: func(t *testing.T, r repos) {
			r.otherPushes(t, "bin/tool")
		}},
		{name: "already up to date", ok: true, between: func(t *testing.T, r repos) {}},
		{name: "rebased twice before landing", ok: true, between: func(t *testing.T, r repos) {
			r.otherPushes(t, "x")
			gitRun(t, r.wt, "fetch", "-q", "origin", "main")
			gitRun(t, r.wt, "rebase", "-q", "origin/main")
		}},
		{name: "commit after review", between: func(t *testing.T, r repos) {
			commitFile(t, r.wt, "work/a/extra.txt", "unreviewed\n")
		}},
		{name: "amend after review", between: func(t *testing.T, r repos) {
			gitRun(t, r.wt, "commit", "-q", "--amend", "-m", "amended")
		}},
		{name: "reset to another commit", between: func(t *testing.T, r repos) {
			gitRun(t, r.wt, "reset", "-q", "--hard", "HEAD~1")
			commitFile(t, r.wt, "work/a/README.md", "rewritten\n")
		}},
		{name: "conflict fixed during rebase", between: func(t *testing.T, r repos) {
			r.otherPushes(t, "work/a/README.md")
			gitRun(t, r.wt, "fetch", "-q", "origin", "main")
			cmd := exec.Command("git", "-C", r.wt, "rebase", "-q", "origin/main")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
			if cmd.Run() == nil {
				t.Fatal("want a rebase conflict")
			}
			os.WriteFile(filepath.Join(r.wt, "work/a/README.md"), []byte("merged by hand\n"), 0o644)
			gitRun(t, r.wt, "add", "work/a/README.md")
			gitRun(t, r.wt, "-c", "core.editor=true", "rebase", "--continue")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepos(t)
			reviewed := commitFile(t, r.wt, "work/a/README.md", "the work\n")
			tc.between(t, r)
			r.land(t)
			f := realGitFactory(t)
			if ok, _ := f.landedOnMain(r.wt, ""); !ok {
				t.Fatal("not on main")
			}
			ok, why := f.landedAsReviewed(r.wt, reviewed)
			if ok != tc.ok {
				t.Errorf("landedAsReviewed = %v (%s), want %v; reflog:\n%s", ok, why, tc.ok,
					gitRun(t, r.wt, "reflog", "--format=%gs"))
			}
		})
	}
}

func TestLandedAsReviewedCatchesUnlandedReview(t *testing.T) {
	r := newRepos(t)
	reviewed := commitFile(t, r.wt, "work/a/README.md", "the work\n")
	f := realGitFactory(t)
	gitRun(t, r.wt, "fetch", "-q", "origin", "main")
	if ok, why := f.landedAsReviewed(r.wt, reviewed); ok || !strings.Contains(why, "not on origin/main") {
		t.Errorf("got %v %q", ok, why)
	}
}

func TestUnreviewedMovesNeedsTheReviewedCommit(t *testing.T) {
	if _, err := unreviewedMoves([]reflogEntry{{"a", "commit: x"}}, "zzz"); err == nil {
		t.Error("want an error when the reviewed commit is not in the reflog")
	}
}

func TestOwnFilesOutside(t *testing.T) {
	r := newRepos(t)
	f := realGitFactory(t)
	commitFile(t, r.wt, "work/2026-10-25-x/README.md", "answer\n")
	r.otherPushes(t, "bin/other-agents-tool")
	r.land(t)
	if out, err := f.ownFilesOutside(r.wt, "work/"); err != nil || len(out) != 0 {
		t.Fatalf("work item only: %v %v", out, err)
	}
	commitFile(t, r.wt, "bin/mine", "#!/bin/sh\n")
	out, err := f.ownFilesOutside(r.wt, "work/")
	if err != nil || !slices.Equal(out, []string{"bin/mine"}) {
		t.Errorf("own bin change: %v %v", out, err)
	}
	if _, err := f.ownFilesOutside(r.main, "work/"); err == nil {
		t.Error("main checkout: want an error (its reflog is shared)")
	}
	if _, err := f.ownFilesOutside(filepath.Join(r.wt, "nope"), "work/"); err == nil {
		t.Error("missing dir: want an error")
	}
}

func TestDirtyStatus(t *testing.T) {
	r := newRepos(t)
	f := realGitFactory(t)
	ctx := context.Background()
	if d, err := f.dirtyStatus(ctx, r.wt); err != nil || d != "" {
		t.Fatalf("clean: %q %v", d, err)
	}
	os.WriteFile(filepath.Join(r.wt, "scratch.txt"), []byte("x"), 0o644)
	if d, _ := f.dirtyStatus(ctx, r.wt); !strings.Contains(d, "scratch.txt") {
		t.Errorf("untracked file not reported: %q", d)
	}
}

func TestLandingMoves(t *testing.T) {
	// subjects as git 2.53 writes them (checked in a scratch repo, 2026-10-25)
	for subj, want := range map[string]bool{
		"rebase (start): checkout origin/main":                 true,
		"rebase (pick): A":                                     true,
		"rebase (finish): returning to refs/heads/card":        true,
		"pull -q --rebase origin main (pick): C1":              true,
		"merge origin/main: Fast-forward":                      true,
		"pull: Fast-forward":                                   true,
		"checkout: moving from card to card":                   true,
		"reset: moving to HEAD":                                true, // an up-to-date rebase
		"reset: moving to HEAD~1":                              false,
		"commit: more":                                         false,
		"commit (amend): A2":                                   false,
		"rebase (continue): C1":                                false,
		"merge origin/main: Merge made by the 'ort' strategy.": false,
		"cherry-pick: x":                                       false,
	} {
		if got := landingMoveRe.MatchString(subj); got != want {
			t.Errorf("%q: landing move = %v, want %v", subj, got, want)
		}
	}
}
