package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RetirementInventory is observation only. It cannot be submitted to an executor.
// Records contain private local paths and identifiers; keep output outside public repos.
type RetirementInventory struct {
	Version         int                   `json:"version"`
	At              string                `json:"at"`
	ExecutorEnabled bool                  `json:"executorEnabled"`
	Problems        []string              `json:"problems"`
	Workspaces      []RetirementWorkspace `json:"workspaces"`
	Allowlist       []RetirementPermit    `json:"allowlist"`
}

// RetirementPermit is the contract for a separately reviewed executor. No command
// currently accepts permits. Each operation needs explicit authorization and fresh checks.
type RetirementPermit struct {
	WorkspaceID   string `json:"workspaceId"`
	CanonicalPath string `json:"canonicalPath"`
	CommonDir     string `json:"commonDir"`
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	Fingerprint   string `json:"fingerprint"`
	Operation     string `json:"operation"` // agent-archive, workspace-archive, filesystem-remove, branch-remove
	ExpiresAt     string `json:"expiresAt"`
}

type RetirementCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}
type RetirementOperation struct {
	Name  string `json:"name"`
	State string `json:"state"`
}
type RetirementAgent struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Archived bool   `json:"archived"`
}
type RetirementWorkspace struct {
	ID                string                `json:"workspaceId"`
	Kind              string                `json:"kind"`
	CanonicalPath     string                `json:"canonicalPath"`
	CommonDir         string                `json:"commonDir"`
	Branch            string                `json:"branch"`
	RecordedBranch    string                `json:"recordedBranch"`
	Head              string                `json:"head"`
	Archived          bool                  `json:"archived"`
	ExpectedDirectory string                `json:"expectedDirectory"`
	Agents            []RetirementAgent     `json:"agents"`
	Checks            []RetirementCheck     `json:"checks"`
	Operations        []RetirementOperation `json:"operations"`
	Fingerprint       string                `json:"fingerprint"`
}

type retirementRecord struct {
	WorkspaceID, Kind, Cwd, WorktreeRoot, MainRepoRoot, Branch, ArchivedAt string
	IsPaseoOwnedWorktree                                                   bool
}
type retirementAgentRecord struct {
	ID, WorkspaceID, Cwd, LastStatus, ArchivedAt string
	LastActivityAt, LastUserMessageAt            string
	RequiresAttention                            bool
	Labels                                       map[string]string
}

// InventoryRetirement reads durable records without the board's age filter or CLI
// pagination. Dynamic daemon state and successful fetched proof remain unknown:
// the inventory does not fetch, archive, stop anything, or invoke repo scripts.
func InventoryRetirement(ctx context.Context, home string, now time.Time) RetirementInventory {
	r := RetirementInventory{Version: 1, At: iso(now), Problems: []string{}, Workspaces: []RetirementWorkspace{}}
	var records []retirementRecord
	if !retirementRead(filepath.Join(home, ".paseo/projects/workspaces.json"), &records) || records == nil {
		r.Problems = append(r.Problems, "workspace records unavailable or malformed")
	}
	files, problems := retirementAgentFiles(filepath.Join(home, ".paseo/agents"))
	r.Problems = append(r.Problems, problems...)
	var agents []retirementAgentRecord
	seen := map[string]bool{}
	for _, path := range files {
		var a retirementAgentRecord
		if !retirementRead(path, &a) || a.ID == "" || seen[a.ID] || !filepath.IsAbs(a.Cwd) || !slices.Contains([]string{"idle", "closed", "running", "error"}, a.LastStatus) || !retirementTimes(a.ArchivedAt, a.LastActivityAt, a.LastUserMessageAt) {
			r.Problems = append(r.Problems, "agent record malformed, duplicated or unsupported")
			continue
		}
		seen[a.ID] = true
		agents = append(agents, a)
	}
	seen = map[string]bool{}
	for _, record := range records {
		if record.WorkspaceID == "" || seen[record.WorkspaceID] || !slices.Contains([]string{"directory", "worktree", "local_checkout"}, record.Kind) || (record.ArchivedAt != "" && parseTime(record.ArchivedAt).IsZero()) {
			r.Problems = append(r.Problems, "workspace record malformed, duplicated or unsupported")
		}
		seen[record.WorkspaceID] = true
	}
	for _, record := range records {
		w := RetirementWorkspace{ID: record.WorkspaceID, Kind: record.Kind, RecordedBranch: record.Branch, Archived: record.ArchivedAt != "", ExpectedDirectory: "preserve", Agents: []RetirementAgent{}}
		check := func(name, state, reason string) { w.Checks = append(w.Checks, RetirementCheck{name, state, reason}) }
		if len(r.Problems) > 0 {
			check("complete-records", "blocked", "inventory has unreadable or unsupported records")
		} else {
			check("complete-records", "observed", "all persisted records read without an age or page limit")
		}
		path, err := filepath.EvalSymlinks(record.Cwd)
		if err != nil || !filepath.IsAbs(record.Cwd) {
			check("canonical-directory", "blocked", "workspace path unavailable or relative")
		} else {
			w.CanonicalPath = path
			check("canonical-directory", "observed", "resolved existing directory")
		}
		exclusiveState, exclusiveReason := "observed", "no overlapping persisted workspace record"
		for _, other := range records {
			if other.WorkspaceID == record.WorkspaceID {
				continue
			}
			p := retirementPath(other.Cwd)
			if p == "" {
				if exclusiveState != "blocked" {
					exclusiveState, exclusiveReason = "unknown", "another workspace path cannot be resolved"
				}
				continue
			}
			if retirementOverlap(w.CanonicalPath, p) {
				exclusiveState, exclusiveReason = "blocked", "another workspace references an overlapping directory"
			}
		}
		check("exclusive-directory", exclusiveState, exclusiveReason)
		if record.Kind == "local_checkout" {
			check("primary-checkout", "blocked", "primary checkout must be preserved")
		}
		completion := "observed"
		for _, a := range agents {
			if a.WorkspaceID != record.WorkspaceID && !retirementOverlap(w.CanonicalPath, retirementPath(a.Cwd)) {
				continue
			}
			kind, state := retirementCompletion(a, agents)
			w.Agents = append(w.Agents, RetirementAgent{a.ID, kind, a.ArchivedAt != ""})
			if state == "blocked" || completion == "observed" {
				completion = state
			}
			if state != "observed" {
				check("agent-completion", state, kind+": completion requires independent evidence")
			}
		}
		if len(w.Agents) == 0 {
			completion = "unknown"
		}
		check("completion", completion, "completion must be established for every associated agent; labels alone do not prove a landed result")
		for _, name := range []string{"live-agent-state", "active-users", "services", "terminals", "setups", "processes", "mounts", "scheduler-owner"} {
			check(name, "unknown", "requires a complete current resource inventory from its owner")
		}
		if w.CanonicalPath != "" {
			retirementGitChecks(ctx, record, &w, check)
			retirementHooks(record, w.CanonicalPath, check)
		}
		check("fresh-landing-proof", "unknown", "read-only inventory does not fetch; executor must fetch successfully and prove recorded HEAD is landed")
		for _, op := range []string{"agent-archive", "workspace-archive", "filesystem-remove", "branch-remove"} {
			w.Operations = append(w.Operations, RetirementOperation{op, "refused"})
		}
		// Hash only selected evidence, never whole provider configs or credentials.
		b, _ := json.Marshal(struct {
			Record     retirementRecord
			AllRecords []retirementRecord
			Agents     []retirementAgentRecord
			Result     RetirementWorkspace
		}{record, records, retirementEvidence(agents, w.Agents), w})
		h := sha256.Sum256(b)
		w.Fingerprint = hex.EncodeToString(h[:])
		r.Workspaces = append(r.Workspaces, w)
	}
	slices.SortFunc(r.Workspaces, func(a, b RetirementWorkspace) int { return strings.Compare(a.ID, b.ID) })
	return r
}

func retirementTimes(values ...string) bool {
	for _, v := range values {
		if v != "" && parseTime(v).IsZero() {
			return false
		}
	}
	return true
}

func retirementAgentFiles(root string) ([]string, []string) {
	var files, problems []string
	providers, err := os.ReadDir(root)
	if err != nil {
		return nil, []string{"agent inventory unavailable"}
	}
	for _, provider := range providers {
		if !provider.IsDir() {
			problems = append(problems, "unsupported entry in agent inventory")
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, provider.Name()))
		if err != nil {
			problems = append(problems, "agent provider directory unreadable")
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				problems = append(problems, "nested agent directory unsupported")
				continue
			}
			if strings.HasSuffix(entry.Name(), ".json") {
				files = append(files, filepath.Join(root, provider.Name(), entry.Name()))
			}
		}
	}
	if len(files) == 0 {
		problems = append(problems, "agent inventory empty")
	}
	return files, problems
}

func retirementRead(path string, v any) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return readJSON(path, v) == nil
}

// Resolve existing parents too: an agent's cwd may be a now-missing subdirectory.
func retirementPath(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ""
	}
	p := retirementPath(parent)
	if p == "" {
		return ""
	}
	return filepath.Join(p, filepath.Base(path))
}

func retirementOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	within := func(x, y string) bool {
		rel, err := filepath.Rel(x, y)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

func retirementCompletion(a retirementAgentRecord, all []retirementAgentRecord) (string, string) {
	l := a.Labels
	if l["factory.parked"] != "" {
		return "parked", "blocked"
	}
	if a.LastStatus == "running" || a.RequiresAttention {
		return "active-or-waiting", "blocked"
	}
	if a.LastStatus == "error" {
		return "agent-error", "blocked"
	}
	if l["role"] != "" || (l["factory.role"] != "" && l["factory.role"] != "reviewer") {
		return "anchor-or-external-role", "blocked"
	}
	if l["paseo.schedule-id"] != "" {
		return "schedule-external-owner", "blocked"
	}
	if l["factory.routing"] != "" {
		if l["factory.routed"] != "" && l["factory.routed"] != "starting" && a.ArchivedAt != "" {
			return "routed-picker", "observed"
		}
		return "unfinished-picker", "blocked"
	}
	if l["factory.role"] == "reviewer" {
		if l["factory.applied"] != "" && a.ArchivedAt != "" {
			for _, p := range all {
				if p.ID == l["factory.review-of"] && p.Labels["factory.stage"] == "landed" && p.Labels["factory.parked"] == "" && (p.LastStatus == "idle" || p.LastStatus == "closed") && !p.RequiresAttention {
					return "applied-reviewer", "unknown"
				}
			}
		}
		return "reviewer-with-protected-target", "blocked"
	}
	if l["factory.stage"] == "landed" {
		return "completed-card", "unknown"
	}
	return "unfinished-or-unowned", "blocked"
}

func retirementEvidence(all []retirementAgentRecord, selected []RetirementAgent) []retirementAgentRecord {
	var out []retirementAgentRecord
	for _, a := range all {
		if !slices.ContainsFunc(selected, func(s RetirementAgent) bool { return s.ID == a.ID }) {
			continue
		}
		labels := map[string]string{}
		for _, key := range []string{"role", "factory.stage", "factory.role", "factory.applied", "factory.review-of", "factory.parked", "factory.routing", "factory.routed", "paseo.schedule-id"} {
			if a.Labels[key] != "" {
				labels[key] = a.Labels[key]
			}
		}
		a.Labels = labels
		out = append(out, a)
	}
	return out
}

func retirementGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Optional locks and filesystem monitors can write or run external code during status.
	c := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", dir}, args...)...)
	b, err := c.Output()
	return strings.TrimSpace(string(b)), err
}

func retirementGitChecks(ctx context.Context, r retirementRecord, w *RetirementWorkspace, check func(string, string, string)) {
	common, err := retirementGit(ctx, w.CanonicalPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		check("git-ownership", "blocked", "cannot resolve Git common directory")
		return
	}
	w.CommonDir = retirementPath(common)
	w.Head, err = retirementGit(ctx, w.CanonicalPath, "rev-parse", "--verify", "HEAD")
	if err != nil {
		check("git-head", "blocked", "cannot resolve HEAD")
	}
	w.Branch, err = retirementGit(ctx, w.CanonicalPath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || w.Branch == "" || w.Branch != r.Branch {
		check("branch-ownership", "blocked", "actual branch is detached or differs from workspace record")
	} else {
		check("branch-ownership", "observed", "record matches actual checked-out branch")
	}
	// Status can invoke a configured clean/process filter. Do not run it when
	// filters exist or when their absence cannot be established.
	filters, filterErr := retirementGit(ctx, w.CanonicalPath, "config", "--get-regexp", `^filter\.`)
	var exitErr *exec.ExitError
	noFilters := filters == "" && errors.As(filterErr, &exitErr) && exitErr.ExitCode() == 1
	status := ""
	if noFilters {
		status, err = retirementGit(ctx, w.CanonicalPath, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	}
	if !noFilters {
		check("git-clean", "unknown", "configured filters or unreadable filter configuration; status skipped")
	} else if err != nil {
		check("git-clean", "unknown", "cannot inspect tracked, untracked and ignored state")
	} else if status != "" {
		check("git-clean", "blocked", "tracked changes, untracked files or ignored files present")
	} else {
		check("git-clean", "observed", "no tracked changes, untracked files or ignored files")
	}
	root, err := filepath.EvalSymlinks(r.MainRepoRoot)
	top, topErr := retirementGit(ctx, w.CanonicalPath, "rev-parse", "--show-toplevel")
	rootCommon, rootErr := retirementGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	list, listErr := retirementGit(ctx, root, "worktree", "list", "--porcelain")
	registered := false
	for _, block := range strings.Split(list, "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "worktree ") && retirementPath(strings.TrimPrefix(lines[0], "worktree ")) == w.CanonicalPath && slices.Contains(lines, "HEAD "+w.Head) && slices.Contains(lines, "branch refs/heads/"+w.Branch) {
			registered = true
		}
	}
	if r.Kind != "worktree" || !r.IsPaseoOwnedWorktree || err != nil || root == w.CanonicalPath || topErr != nil || retirementPath(top) != w.CanonicalPath || retirementPath(r.WorktreeRoot) != w.CanonicalPath || rootErr != nil || retirementPath(rootCommon) != w.CommonDir || listErr != nil || !registered || w.Branch == "main" || w.Branch == "master" {
		check("owned-worktree", "blocked", "primary, directory, shared, protected branch or inconsistent Git/workspace placement")
	} else {
		w.ExpectedDirectory = "remove-only-after-approval"
		check("owned-worktree", "observed", "owned linked worktree matches Git registration and repository")
	}
	if w.Head != "" {
		if _, err := retirementGit(ctx, w.CanonicalPath, "merge-base", "--is-ancestor", w.Head, "refs/remotes/origin/main"); err != nil {
			check("local-landing-evidence", "blocked", "HEAD absent from local origin/main or evidence unavailable")
		} else {
			check("local-landing-evidence", "observed", "local ancestry only; a successful fresh fetch is still required")
		}
	}
}

func retirementHooks(r retirementRecord, path string, check func(string, string, string)) {
	for _, dir := range []string{path, r.MainRepoRoot} {
		if dir == "" {
			continue
		}
		config := filepath.Join(dir, "paseo.json")
		_, err := os.Lstat(config)
		if os.IsNotExist(err) {
			continue
		}
		var v struct {
			Worktree struct {
				Teardown json.RawMessage `json:"teardown"`
			} `json:"worktree"`
		}
		if !retirementRead(config, &v) {
			check("teardown-hooks", "unknown", "configuration unreadable or unsupported")
			return
		}
		var commands []string
		if len(v.Worktree.Teardown) > 0 && (json.Unmarshal(v.Worktree.Teardown, &commands) != nil || len(commands) > 0) {
			check("teardown-hooks", "blocked", "teardown configuration present; scripts must never run during retirement")
			return
		}
	}
	check("teardown-hooks", "observed", "no teardown commands in inspected workspace and repository configurations")
}
