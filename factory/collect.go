package factory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadAgents reads this host's Paseo agents straight from ~/.paseo: open ones, plus ones archived
// or closed within window. Those files carry labels, workspace, attention flags and archive time,
// which `paseo ls --json` does not show.
func LoadAgents(home, host string, window time.Duration, now time.Time) ([]Agent, error) {
	cutoff := now.Add(-window)
	var projects []struct{ ProjectID, ProjectKey, DisplayName, CustomName string }
	var workspaces []struct{ WorkspaceID, ProjectID, Branch string }
	readJSON(filepath.Join(home, ".paseo/projects/projects.json"), &projects)
	readJSON(filepath.Join(home, ".paseo/projects/workspaces.json"), &workspaces)
	proj := map[string]int{}
	for i, p := range projects {
		proj[p.ProjectID] = i
	}
	type wsInfo struct{ repo, project, branch string }
	ws := map[string]wsInfo{}
	for _, w := range workspaces {
		info := wsInfo{branch: w.Branch}
		if i, ok := proj[w.ProjectID]; ok {
			p := projects[i]
			if _, r, ok := strings.Cut(p.ProjectKey, "github.com/"); ok {
				info.repo = r
			}
			info.project = p.CustomName
			if info.project == "" {
				info.project = p.DisplayName
			}
		}
		ws[w.WorkspaceID] = info
	}

	files, err := filepath.Glob(filepath.Join(home, ".paseo/agents/*/*.json"))
	if err != nil {
		return nil, err
	}
	repoCache := map[string]string{}
	var all []Agent
	byID := map[string]Agent{}
	keep := map[string]bool{}
	for _, f := range files {
		var d struct {
			ID, Title, Provider, Cwd, WorkspaceID, LastStatus, AttentionReason, LastError string
			CreatedAt, LastActivityAt, LastUserMessageAt, ArchivedAt                      string
			Internal, RequiresAttention                                                   bool
			Labels                                                                        map[string]string
			Config                                                                        struct{ Model string }
			Persistence                                                                   struct{ SessionID string }
		}
		if readJSON(f, &d) != nil || d.ID == "" || d.Internal {
			continue
		}
		a := Agent{ID: d.ID, Host: host, Title: d.Title, Provider: d.Provider, Model: d.Config.Model, Cwd: d.Cwd,
			WorkspaceID: d.WorkspaceID, SessionID: d.Persistence.SessionID, Status: d.LastStatus,
			CreatedAt: parseTime(d.CreatedAt), LastActivityAt: parseTime(d.LastActivityAt),
			LastUserMessageAt: parseTime(d.LastUserMessageAt),
			ArchivedAt:        parseTime(d.ArchivedAt), Labels: d.Labels, RequiresAttention: d.RequiresAttention,
			AttentionReason: d.AttentionReason, LastError: d.LastError}
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		all = append(all, a)
		byID[a.ID] = a
		if a.archived() && a.ArchivedAt.Before(cutoff) && !(a.parked() && a.ArchivedAt.After(now.Add(-parkedKeep))) {
			continue
		}
		if !a.archived() && a.Status == "closed" && a.LastActivityAt.Before(cutoff) {
			continue
		}
		keep[a.ID] = true
	}
	for id := range keep {
		for _, prev := range cardLineage(byID, id) {
			keep[prev.ID] = true
		}
	}
	var out []Agent
	for _, a := range all {
		if !keep[a.ID] {
			continue
		}
		info := ws[a.WorkspaceID]
		a.Repo, a.Project, a.Branch = info.repo, info.project, info.branch
		if a.Repo == "" && a.Cwd != "" {
			if _, ok := repoCache[a.Cwd]; !ok {
				repoCache[a.Cwd] = repoOf(a.Cwd)
			}
			a.Repo = repoCache[a.Cwd]
		}
		out = append(out, a)
	}
	return applyLineageLabels(out), nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// repoOf finds owner/repo from the origin remote of the git checkout containing dir.
func repoOf(dir string) string {
	if _, err := os.Stat(dir); err != nil {
		return "(worktree removed)"
	}
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		cfg := filepath.Join(d, ".git", "config")
		if b, err := os.ReadFile(filepath.Join(d, ".git")); err == nil {
			// worktree: ".git" is a file pointing at <main>/.git/worktrees/<name>
			if g, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: "); ok {
				cfg = filepath.Join(filepath.Dir(filepath.Dir(g)), "config")
			}
		}
		if r := originRepo(cfg); r != "" {
			return r
		}
	}
	return ""
}

func originRepo(cfg string) string {
	f, err := os.Open(cfg)
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && inOrigin && strings.TrimSpace(k) == "url" {
			u := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(v), "/"), ".git")
			u = strings.ReplaceAll(u, ":", "/")
			parts := strings.Split(u, "/")
			if len(parts) >= 2 {
				return parts[len(parts)-2] + "/" + parts[len(parts)-1]
			}
		}
	}
	return ""
}
