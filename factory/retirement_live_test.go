package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type retirementLiveFixture struct {
	f                           retirementFixture
	terminals, scripts, version string
	fail                        string
	calls                       []string
}

func newRetirementLiveFixture(t *testing.T) *retirementLiveFixture {
	return &retirementLiveFixture{f: newRetirementFixture(t), terminals: "[]", scripts: "[]", version: "0.11.4"}
}

func (f *retirementLiveFixture) run(ctx context.Context, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.fail != "" && strings.HasPrefix(call, f.fail) {
		return nil, errors.New("private diagnostic")
	}
	switch call {
	case "--version":
		return []byte(f.version), nil
	case "status --json --home " + filepath.Join(f.f.home, ".paseo"):
		return json.Marshal(map[string]any{"home": filepath.Join(f.f.home, ".paseo"), "connectedDaemon": "reachable", "localDaemon": "running", "daemonVersion": f.version})
	case "terminal ls --all --json --home " + filepath.Join(f.f.home, ".paseo"):
		return []byte(f.terminals), nil
	case "script ls --workspace owned --json --home " + filepath.Join(f.f.home, ".paseo"):
		return []byte(f.scripts), nil
	}
	return nil, fmt.Errorf("unexpected command: %s", call)
}

func (f *retirementLiveFixture) inventory(t *testing.T) RetirementInventory {
	r := f.f.inventory(t)
	observeRetirementLive(context.Background(), &r, f.f.home, f.run, func() time.Time { return time.Unix(200, 0) })
	return r
}

func assertRetirementRefused(t *testing.T, r RetirementInventory) {
	t.Helper()
	if r.ExecutorEnabled || r.Allowlist != nil {
		t.Fatal("live observations granted authority")
	}
	for _, w := range r.Workspaces {
		for _, op := range w.Operations {
			if op.State != "refused" {
				t.Fatal("operation enabled")
			}
		}
	}
}

func TestRetirementLiveEmptyReadsAreScoped(t *testing.T) {
	f := newRetirementLiveFixture(t)
	r := f.inventory(t)
	w := r.Workspaces[0]
	assertRetirementRefused(t, r)
	if len(r.Problems) != 0 || !hasRetirementCheck(w, "exclusion", "blocked") || !hasRetirementCheck(w, "terminals", "observed") {
		t.Fatalf("checks: %+v problems: %v", w.Checks, r.Problems)
	}
	for _, name := range []string{"live-agent-state", "active-users", "services", "setups", "processes", "mounts", "scheduler-owner", "fresh-landing-proof"} {
		if !hasRetirementCheck(w, name, "unknown") {
			t.Errorf("%s became safe", name)
		}
	}
	if len(w.Observations) != 2 {
		t.Fatalf("observations: %+v", w.Observations)
	}
	for _, o := range w.Observations {
		if o.Source == "" || o.Scope == "" || o.At == "" || o.StartedAt == "" {
			t.Fatalf("missing provenance: %+v", o)
		}
	}
	if !w.Observations[0].Complete || w.Observations[1].Complete {
		t.Fatal("projection claimed complete resource coverage")
	}
	if len(f.calls) != 4 {
		t.Fatalf("unexpected reads: %v", f.calls)
	}
}

func TestRetirementLiveTerminalsMatchOwnershipAndOverlap(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, cwd string
		blocked              bool
	}{
		{"exact", "owned", "/elsewhere", true},
		{"different-id", "sibling", "child", true},
		{"no-id", "", "child", true},
		{"elsewhere", "sibling", "/elsewhere", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetirementLiveFixture(t)
			cwd := tc.cwd
			if cwd == "child" {
				cwd = filepath.Join(f.f.tree, "missing-child")
			}
			b, _ := json.Marshal([]any{map[string]any{"id": "terminal", "workspaceId": tc.workspace, "cwd": cwd}})
			f.terminals = string(b)
			r := f.inventory(t)
			if hasRetirementCheck(r.Workspaces[0], "terminals", "blocked") != tc.blocked {
				t.Fatal(r.Workspaces[0].Checks)
			}
			assertRetirementRefused(t, r)
		})
	}
}

func TestRetirementLiveScriptsBlockSharedFolders(t *testing.T) {
	f := newRetirementLiveFixture(t)
	f.scripts = `[{"scriptName":"web","type":"service","lifecycle":"running"}]`
	retirementJSON(t, filepath.Join(f.f.home, ".paseo/projects/workspaces.json"), []any{
		map[string]any{"workspaceId": "owned", "kind": "directory", "cwd": f.f.tree},
		map[string]any{"workspaceId": "sibling", "kind": "directory", "cwd": f.f.tree},
	})
	r := InventoryRetirement(context.Background(), f.f.home, time.Now())
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "script ls --workspace sibling --json --home "+filepath.Join(f.f.home, ".paseo") {
			return []byte("[]"), nil
		}
		return f.run(ctx, args...)
	}
	observeRetirementLive(context.Background(), &r, f.f.home, run, time.Now)
	for _, w := range r.Workspaces {
		if !hasRetirementCheck(w, "services", "blocked") {
			t.Fatalf("missed shared service: %+v", w)
		}
		if !hasRetirementCheck(w, "exclusive-directory", "blocked") {
			t.Fatal("live read rewrote persisted checks")
		}
	}
	assertRetirementRefused(t, r)
}

func TestRetirementLivePreservesEvidenceAndFilesystem(t *testing.T) {
	f := newRetirementLiveFixture(t)
	f.scripts = `[{"scriptName":"web","type":"service","lifecycle":"stopped"}]`
	r := f.f.inventory(t)
	fingerprint := r.Workspaces[0].Fingerprint
	refs := retirementTestGit(t, f.f.root, "show-ref")
	trees := retirementTestGit(t, f.f.root, "worktree", "list", "--porcelain")
	path := filepath.Join(f.f.home, ".paseo/projects/workspaces.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	observeRetirementLive(context.Background(), &r, f.f.home, f.run, time.Now)
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("workspace records changed")
	}
	if r.Workspaces[0].Fingerprint != fingerprint || !hasRetirementCheck(r.Workspaces[0], "services", "unknown") {
		t.Fatal("supplemental evidence granted safety")
	}
	if refs != retirementTestGit(t, f.f.root, "show-ref") || trees != retirementTestGit(t, f.f.root, "worktree", "list", "--porcelain") {
		t.Fatal("Git changed")
	}
	if _, err := os.Stat(f.f.tree); err != nil {
		t.Fatal(err)
	}
	assertRetirementRefused(t, r)
}

func TestRetirementLiveIncompletePersistedInventorySkipsDaemon(t *testing.T) {
	f := newRetirementLiveFixture(t)
	r := f.f.inventory(t)
	r.Problems = append(r.Problems, "unread record")
	observeRetirementLive(context.Background(), &r, f.f.home, f.run, time.Now)
	if len(f.calls) != 0 || !hasRetirementCheck(r.Workspaces[0], "exclusion", "unknown") {
		t.Fatal("mixed incomplete inventory with live reads")
	}
}

func TestRetirementLiveCanonicalTerminalPath(t *testing.T) {
	f := newRetirementLiveFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.f.tree, alias); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]any{map[string]any{"id": "terminal", "workspaceId": "other", "cwd": alias}})
	f.terminals = string(b)
	r := f.inventory(t)
	if !hasRetirementCheck(r.Workspaces[0], "terminals", "blocked") {
		t.Fatal("missed canonical path overlap")
	}
}

func TestRetirementLiveFailuresStayUnknown(t *testing.T) {
	for _, mode := range []string{"terminal-error", "script-error", "null", "truncated", "paged", "bad-terminal", "duplicate", "bad-script", "new-version", "status-error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newRetirementLiveFixture(t)
			check := "terminals"
			switch mode {
			case "terminal-error":
				f.fail = "terminal"
			case "script-error":
				f.fail = "script"
				check = "services"
			case "null":
				f.terminals = "null"
			case "truncated":
				f.terminals = "["
			case "paged":
				f.terminals = `{"entries":[],"nextCursor":"more"}`
			case "bad-terminal":
				f.terminals = `[{"id":"t","cwd":"relative"}]`
			case "duplicate":
				f.terminals = `[{"id":"t","cwd":"/example"},{"id":"t","cwd":"/example"}]`
			case "bad-script":
				f.scripts = `[{"scriptName":"web","type":"service","lifecycle":"new-state"}]`
				check = "services"
			case "new-version":
				f.version = "future"
			case "status-error":
				f.fail = "status"
			}
			r := f.f.inventory(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			observeRetirementLive(ctx, &r, f.f.home, f.run, time.Now)
			if len(r.Problems) == 0 || !hasRetirementCheck(r.Workspaces[0], check, "unknown") {
				t.Fatalf("unsafe failure: %+v", r)
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "private diagnostic") {
				t.Fatal("raw error disclosed")
			}
			assertRetirementRefused(t, r)
		})
	}
}

func TestRetirementLiveWrongDaemonHomeIsRefused(t *testing.T) {
	f := newRetirementLiveFixture(t)
	r := f.f.inventory(t)
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "status" {
			return []byte(`{"home":"/different","connectedDaemon":"reachable","localDaemon":"running","daemonVersion":"0.11.4"}`), nil
		}
		if args[0] != "--version" {
			t.Fatalf("read mismatched daemon: %v", args)
		}
		return f.run(ctx, args...)
	}
	observeRetirementLive(context.Background(), &r, f.f.home, run, time.Now)
	if len(r.Problems) == 0 || !hasRetirementCheck(r.Workspaces[0], "exclusion", "unknown") {
		t.Fatal(r)
	}
}
