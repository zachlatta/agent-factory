package factory

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type retirementFixture struct{ home, root, tree string }

func retirementJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func retirementTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func newRetirementFixture(t *testing.T) retirementFixture {
	t.Helper()
	f := retirementFixture{home: t.TempDir(), root: t.TempDir()}
	f.tree = filepath.Join(t.TempDir(), "owned")
	retirementTestGit(t, f.root, "init", "-b", "main")
	retirementTestGit(t, f.root, "-c", "user.name=Test", "-c", "user.email=test@users.noreply.github.com", "commit", "--allow-empty", "-m", "base")
	retirementTestGit(t, f.root, "worktree", "add", "-b", "meaningful-title", f.tree)
	retirementJSON(t, filepath.Join(f.home, ".paseo/projects/workspaces.json"), []any{map[string]any{
		"workspaceId": "owned", "cwd": f.tree, "worktreeRoot": f.tree, "mainRepoRoot": f.root,
		"kind": "worktree", "isPaseoOwnedWorktree": true, "branch": "meaningful-title",
	}})
	f.agent(t, "done", "owned", f.tree, map[string]string{"factory.stage": "landed"})
	return f
}

func (f retirementFixture) agent(t *testing.T, id, workspace, cwd string, labels map[string]string) {
	retirementJSON(t, filepath.Join(f.home, ".paseo/agents/test", id+".json"), map[string]any{
		"id": id, "workspaceId": workspace, "cwd": cwd, "lastStatus": "closed", "archivedAt": "2020-01-01T00:00:00Z", "labels": labels,
	})
}

func (f retirementFixture) inventory(t *testing.T) RetirementInventory {
	t.Helper()
	r := InventoryRetirement(context.Background(), f.home, time.Unix(100, 0))
	if len(r.Workspaces) != 1 {
		t.Fatalf("inventory: %+v", r)
	}
	return r
}

func hasRetirementCheck(r RetirementWorkspace, name, state string) bool {
	return slices.ContainsFunc(r.Checks, func(c RetirementCheck) bool { return c.Name == name && c.State == state })
}

func TestRetirementReadsOldArchivesAndActualBranch(t *testing.T) {
	f := newRetirementFixture(t)
	r := f.inventory(t)
	w := r.Workspaces[0]
	if len(w.Agents) != 1 || w.Agents[0].ID != "done" || w.Branch != "meaningful-title" || w.Head == "" || w.CommonDir == "" {
		t.Fatalf("ownership: %+v", w)
	}
	if !hasRetirementCheck(w, "completion", "unknown") || !hasRetirementCheck(w, "fresh-landing-proof", "unknown") {
		t.Fatalf("labels are not proof: %+v", w.Checks)
	}
	for _, op := range w.Operations {
		if op.State != "refused" {
			t.Fatalf("unsafe operation: %+v", op)
		}
	}
	if r.Allowlist != nil || r.ExecutorEnabled {
		t.Fatal("inventory granted authority")
	}
	if w.Fingerprint == "" || w.Fingerprint != f.inventory(t).Workspaces[0].Fingerprint {
		t.Fatal("unstable fingerprint")
	}
}

func TestRetirementProtectsEveryResource(t *testing.T) {
	f := newRetirementFixture(t)
	w := f.inventory(t).Workspaces[0]
	for _, name := range []string{"active-users", "live-agent-state", "services", "terminals", "setups", "processes", "mounts", "scheduler-owner"} {
		if !hasRetirementCheck(w, name, "unknown") {
			t.Errorf("missing fail-closed check: %s", name)
		}
	}
}

func TestRetirementProtectedAgents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"parked", map[string]string{"factory.stage": "zach", "factory.parked": "yes"}},
		{"unfinished", map[string]string{"factory.stage": "building"}},
		{"anchor", map[string]string{"role": "dispatcher"}},
		{"anchor-with-stage", map[string]string{"role": "dispatcher", "factory.stage": "landed"}},
		{"schedule", map[string]string{"paseo.schedule-id": "schedule"}},
		{"reviewer", map[string]string{"factory.role": "reviewer", "factory.applied": "1", "factory.review-of": "missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetirementFixture(t)
			f.agent(t, "done", "owned", f.tree, tc.labels)
			w := f.inventory(t).Workspaces[0]
			if !hasRetirementCheck(w, "completion", "blocked") {
				t.Fatalf("not protected: %+v", w.Checks)
			}
		})
	}
}

func TestRetirementFindsUsersByPathWithoutWorkspaceID(t *testing.T) {
	f := newRetirementFixture(t)
	f.agent(t, "other", "different", filepath.Join(f.tree, "subdirectory"), map[string]string{"factory.stage": "building"})
	w := f.inventory(t).Workspaces[0]
	if len(w.Agents) != 2 || !hasRetirementCheck(w, "completion", "blocked") {
		t.Fatalf("missed path user: %+v", w)
	}
}

func TestRetirementSharedDirectoryRecord(t *testing.T) {
	f := newRetirementFixture(t)
	retirementJSON(t, filepath.Join(f.home, ".paseo/projects/workspaces.json"), []any{
		map[string]any{"workspaceId": "owned", "cwd": f.root, "kind": "directory", "isPaseoOwnedWorktree": false},
		map[string]any{"workspaceId": "sibling", "cwd": f.root, "kind": "directory", "isPaseoOwnedWorktree": false},
	})
	f.agent(t, "done", "owned", f.root, map[string]string{"factory.routing": "1", "factory.routed": "real-card"})
	r := InventoryRetirement(context.Background(), f.home, time.Unix(100, 0))
	if len(r.Workspaces) != 2 {
		t.Fatal(r)
	}
	for _, w := range r.Workspaces {
		if w.ExpectedDirectory != "preserve" || !hasRetirementCheck(w, "exclusive-directory", "blocked") {
			t.Fatalf("shared directory: %+v", w)
		}
	}
}

func TestRetirementDirtyIgnoredAndRenamedBranches(t *testing.T) {
	for _, mode := range []string{"untracked", "ignored", "renamed", "primary", "teardown"} {
		t.Run(mode, func(t *testing.T) {
			f := newRetirementFixture(t)
			name := "git-clean"
			switch mode {
			case "untracked":
				os.WriteFile(filepath.Join(f.tree, "scratch"), []byte("work"), 0600)
			case "ignored":
				os.WriteFile(filepath.Join(f.root, ".git/info/exclude"), []byte("scratch\n"), 0600)
				os.WriteFile(filepath.Join(f.tree, "scratch"), []byte("work"), 0600)
			case "renamed":
				retirementTestGit(t, f.tree, "branch", "-m", "new-title")
				name = "branch-ownership"
			case "primary":
				retirementJSON(t, filepath.Join(f.home, ".paseo/projects/workspaces.json"), []any{map[string]any{"workspaceId": "owned", "cwd": f.root, "worktreeRoot": f.root, "mainRepoRoot": f.root, "kind": "worktree", "isPaseoOwnedWorktree": true, "branch": "main"}})
				name = "owned-worktree"
			case "teardown":
				retirementJSON(t, filepath.Join(f.tree, "paseo.json"), map[string]any{"worktree": map[string]any{"teardown": []string{"do-not-run"}}})
				name = "teardown-hooks"
			}
			if !hasRetirementCheck(f.inventory(t).Workspaces[0], name, "blocked") {
				t.Fatalf("missed %s", mode)
			}
		})
	}
}

func TestRetirementMalformedEvidenceBlocksAll(t *testing.T) {
	f := newRetirementFixture(t)
	os.WriteFile(filepath.Join(f.home, ".paseo/agents/test/broken.json"), []byte("{"), 0600)
	r := f.inventory(t)
	if len(r.Problems) == 0 || !hasRetirementCheck(r.Workspaces[0], "complete-records", "blocked") {
		t.Fatalf("incomplete evidence: %+v", r)
	}
}

func TestRetirementLeavesGitAndFilesUntouched(t *testing.T) {
	f := newRetirementFixture(t)
	before := retirementTestGit(t, f.root, "show-ref")
	refs := retirementTestGit(t, f.root, "worktree", "list", "--porcelain")
	f.inventory(t)
	if before != retirementTestGit(t, f.root, "show-ref") || refs != retirementTestGit(t, f.root, "worktree", "list", "--porcelain") {
		t.Fatal("inventory mutated git")
	}
	if _, err := os.Stat(f.tree); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementDoesNotRunGitFilters(t *testing.T) {
	f := newRetirementFixture(t)
	retirementTestGit(t, f.root, "config", "filter.inventory.clean", "exit 99")
	w := f.inventory(t).Workspaces[0]
	if !hasRetirementCheck(w, "git-clean", "unknown") {
		t.Fatal("status must not invoke configured filters")
	}
}

func TestRetirementRunningAgentAndTrackedChanges(t *testing.T) {
	f := newRetirementFixture(t)
	retirementJSON(t, filepath.Join(f.home, ".paseo/agents/test/done.json"), map[string]any{"id": "done", "workspaceId": "owned", "cwd": f.tree, "lastStatus": "running", "labels": map[string]string{"factory.stage": "landed"}})
	if !hasRetirementCheck(f.inventory(t).Workspaces[0], "completion", "blocked") {
		t.Fatal("running agent eligible")
	}
	if err := os.WriteFile(filepath.Join(f.tree, "tracked"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	retirementTestGit(t, f.tree, "add", "tracked")
	retirementTestGit(t, f.tree, "-c", "user.name=Test", "-c", "user.email=test@users.noreply.github.com", "commit", "-m", "work")
	if err := os.WriteFile(filepath.Join(f.tree, "tracked"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	w := f.inventory(t).Workspaces[0]
	if !hasRetirementCheck(w, "git-clean", "blocked") || !hasRetirementCheck(w, "local-landing-evidence", "blocked") {
		t.Fatal("dirty or unlanded work missed")
	}
}

func TestRetirementLocalLandingProofDoesNotAuthorizeDeletion(t *testing.T) {
	f := newRetirementFixture(t)
	retirementTestGit(t, f.root, "update-ref", "refs/remotes/origin/main", "HEAD")
	w := f.inventory(t).Workspaces[0]
	if !hasRetirementCheck(w, "local-landing-evidence", "observed") || !hasRetirementCheck(w, "fresh-landing-proof", "unknown") {
		t.Fatal("fresh fetch requirement missing")
	}
	for _, op := range w.Operations {
		if op.State != "refused" {
			t.Fatal("stale proof authorized operation")
		}
	}
}

func TestRetirementRejectsUnenumeratedAgentDirectory(t *testing.T) {
	f := newRetirementFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(f.home, ".paseo/agents/linked")); err != nil {
		t.Fatal(err)
	}
	r := f.inventory(t)
	if len(r.Problems) == 0 || !hasRetirementCheck(r.Workspaces[0], "complete-records", "blocked") {
		t.Fatal("unread inventory treated as complete")
	}
}

func TestRetirementLocalCheckoutIsProtectedWithoutGlobalProblem(t *testing.T) {
	f := newRetirementFixture(t)
	retirementJSON(t, filepath.Join(f.home, ".paseo/projects/workspaces.json"), []any{
		map[string]any{"workspaceId": "owned", "cwd": f.tree, "worktreeRoot": f.tree, "mainRepoRoot": f.root, "kind": "worktree", "isPaseoOwnedWorktree": true, "branch": "meaningful-title"},
		map[string]any{"workspaceId": "primary", "cwd": f.root, "mainRepoRoot": f.root, "kind": "local_checkout", "branch": "main"},
	})
	r := InventoryRetirement(context.Background(), f.home, time.Unix(100, 0))
	if len(r.Problems) != 0 || len(r.Workspaces) != 2 {
		t.Fatalf("supported primary checkout invalidated inventory: %+v", r.Problems)
	}
	for _, w := range r.Workspaces {
		if !hasRetirementCheck(w, "complete-records", "observed") {
			t.Fatal("primary checkout blocked unrelated inventory")
		}
		if w.Kind == "local_checkout" && (w.ExpectedDirectory != "preserve" || !hasRetirementCheck(w, "owned-worktree", "blocked")) {
			t.Fatal("primary checkout not protected")
		}
	}
}

func TestRetirementErrorAgentIsIncludedAndBlocked(t *testing.T) {
	f := newRetirementFixture(t)
	retirementJSON(t, filepath.Join(f.home, ".paseo/agents/test/done.json"), map[string]any{"id": "done", "workspaceId": "owned", "cwd": f.tree, "lastStatus": "error", "labels": map[string]string{"factory.stage": "landed"}})
	r := f.inventory(t)
	w := r.Workspaces[0]
	if len(r.Problems) != 0 || len(w.Agents) != 1 || !hasRetirementCheck(w, "complete-records", "observed") || !hasRetirementCheck(w, "completion", "blocked") {
		t.Fatalf("error agent must be included and protected: %+v", r)
	}
}

func TestRetirementExclusiveDirectoryCheckIsAggregated(t *testing.T) {
	f := newRetirementFixture(t)
	retirementJSON(t, filepath.Join(f.home, ".paseo/projects/workspaces.json"), []any{
		map[string]any{"workspaceId": "one", "cwd": f.root, "kind": "directory"},
		map[string]any{"workspaceId": "two", "cwd": f.root, "kind": "directory"},
		map[string]any{"workspaceId": "three", "cwd": f.root, "kind": "directory"},
		map[string]any{"workspaceId": "unknown", "cwd": "relative", "kind": "directory"},
	})
	r := InventoryRetirement(context.Background(), f.home, time.Unix(100, 0))
	for _, w := range r.Workspaces {
		count := 0
		for _, c := range w.Checks {
			if c.Name == "exclusive-directory" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("workspace %s has %d overlap checks", w.ID, count)
		}
		if w.ID != "unknown" && !hasRetirementCheck(w, "exclusive-directory", "blocked") {
			t.Fatal("unknown path obscured known overlap")
		}
	}
}
