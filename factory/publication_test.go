package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicationAgent(t *testing.T, f *Factory, r repos, reviewed string) Agent {
	t.Helper()
	dir := filepath.Join(f.cfg.Home, "dev", "example", "repo")
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.main, dir); err != nil {
		t.Fatal(err)
	}
	return Agent{Repo: "example/repo", Labels: map[string]string{labelPrefix + labelReviewed: reviewed}}
}

func TestReviewedPublication(t *testing.T) {
	for _, name := range []string{"exact", "rebased", "absent", "partial", "empty", "missing object", "fetch failure"} {
		t.Run(name, func(t *testing.T) {
			r := newRepos(t)
			f := realGitFactory(t)
			reviewed := commitFile(t, r.wt, "change", "reviewed\n")
			want := publicationLanded
			switch name {
			case "exact":
				r.land(t)
			case "rebased":
				r.otherPushes(t, "other")
				r.land(t)
			case "absent":
				want = publicationAbsent
			case "partial":
				r.land(t)
				reviewed = commitFile(t, r.wt, "second", "second\n")
				want = publicationAbsent
			case "empty":
				gitRun(t, r.wt, "commit", "--allow-empty", "-m", "empty")
				reviewed = gitRun(t, r.wt, "rev-parse", "HEAD")
				want = publicationAbsent
			case "missing object":
				reviewed = strings.Repeat("f", 40)
				want = publicationUnknown
			case "fetch failure":
				r.land(t)
				f.Git = func(context.Context, string, ...string) error { return errors.New("fetch unavailable") }
				want = publicationUnknown
			}
			a := publicationAgent(t, f, r, reviewed)
			// Publication evidence must not need the card's worktree or its reflog.
			gitRun(t, r.main, "worktree", "remove", "--force", r.wt)
			got, why := f.reviewedPublication(a)
			if got != want {
				t.Fatalf("got %v (%s), want %v", got, why, want)
			}
		})
	}
}

func TestReviewedPublicationIncompleteComparison(t *testing.T) {
	for _, tc := range []struct{ name, base string }{{"empty merge base", ""}, {"merge only range", "base"}} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			f.Git = func(context.Context, string, ...string) error { return nil }
			f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
				if args[0] == "merge-base" {
					return tc.base, nil
				}
				return "", nil
			}
			a := Agent{Repo: "example/repo", Labels: map[string]string{labelPrefix + labelReviewed: "reviewed"}}
			if state, why := f.reviewedPublication(a); state != publicationUnknown {
				t.Fatalf("got %v (%s), want unknown", state, why)
			}
		})
	}
}

func TestPublishedWorkRequiresOwnCommitsOrRecordedLanding(t *testing.T) {
	r := newRepos(t)
	f := realGitFactory(t)
	base := gitRun(t, r.wt, "rev-parse", "HEAD")
	reviewed := commitFile(t, r.wt, "own-change", "change\n")
	r.land(t)
	a := publicationAgent(t, f, r, reviewed)
	a.Cwd = r.wt
	a.Labels[labelPrefix+labelBase] = base
	if !f.hasPublishedWork(a) {
		t.Fatal("own commits were not recognized")
	}
	a.Labels[labelPrefix+labelBase] = reviewed
	if f.hasPublishedWork(a) {
		t.Fatal("shared main HEAD counted as own work")
	}
	a.Labels[labelPrefix+labelLandedVerified] = "1"
	if !f.hasPublishedWork(a) {
		t.Fatal("recorded landing was not recognized")
	}
}

func TestPublishedWorkRejectsRebaseWithoutOwnCommits(t *testing.T) {
	r := newRepos(t)
	f := realGitFactory(t)
	base := gitRun(t, r.wt, "rev-parse", "HEAD")
	r.otherPushes(t, "other")
	gitRun(t, r.wt, "fetch", "origin", "main")
	gitRun(t, r.wt, "rebase", "origin/main")
	reviewed := gitRun(t, r.wt, "rev-parse", "HEAD")
	a := publicationAgent(t, f, r, reviewed)
	a.Cwd = r.wt
	a.Labels[labelPrefix+labelBase] = base
	if state, why := f.reviewedPublication(a); state != publicationLanded {
		t.Fatalf("publication=%v: %s", state, why)
	}
	if f.hasPublishedWork(a) {
		t.Fatal("other requests' commits counted as own work after rebase")
	}
	if disposition, hold := f.assessReview(a, r.wt); disposition != reviewEligible || hold != nil {
		t.Fatalf("normal approval path bypassed: disposition=%v hold=%v", disposition, hold)
	}
}

func TestPublishedWorkOwnershipSurvivesRebaseAndWorktreeRemoval(t *testing.T) {
	r := newRepos(t)
	f := realGitFactory(t)
	base := gitRun(t, r.wt, "rev-parse", "HEAD")
	commitFile(t, r.wt, "own", "own change\n")
	r.otherPushes(t, "other")
	r.land(t)
	reviewed := gitRun(t, r.wt, "rev-parse", "HEAD")
	a := publicationAgent(t, f, r, reviewed)
	a.Cwd = r.wt
	a.Labels[labelPrefix+labelBase] = base
	if !f.ownReviewedWork(a) {
		t.Fatal("rebased own patch not recognized")
	}
	a.Labels[labelPrefix+labelOwnReviewed] = reviewed
	gitRun(t, r.main, "worktree", "remove", "--force", r.wt)
	if !f.hasPublishedWork(a) {
		t.Fatal("recorded ownership lost with worktree")
	}
	a.Labels[labelPrefix+labelReviewed] = base
	if f.hasPublishedWork(a) {
		t.Fatal("ownership reused for a different review")
	}
}
