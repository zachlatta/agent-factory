package factory

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	parentAgentLabel = "paseo.parent-agent-id"
	quietAnchorRole  = "quiet-anchor"
	// Only the hidden anchor uses a synthetic parent, making its bootstrap silent.
	quietBootstrapParent = "factory-quiet-bootstrap"
)

type quietState struct {
	id      string
	checked time.Time
}

func quietAnchor(a Agent) bool { return a.L(labelRole) == quietAnchorRole }
func boardAnchor(a Agent) bool {
	return quietAnchor(a) || (InboxAnchorRole != "" && a.Labels["role"] == InboxAnchorRole)
}

// quietRunLabels reads flags before the final prompt. Values follow Paseo's comma-separated
// label syntax; the prompt must never be interpreted as a flag.
func quietRunLabels(args []string) map[string]string {
	labels := map[string]string{}
	for i := 1; i+1 < len(args)-1; i++ {
		if args[i] != "--label" {
			continue
		}
		for _, pair := range strings.Split(args[i+1], ",") {
			k, v, ok := strings.Cut(pair, "=")
			if ok {
				labels[k] = v
			}
		}
		i++
	}
	return labels
}

func (f *Factory) ensureQuietAnchor() (string, error) {
	f.quietMu.Lock()
	defer f.quietMu.Unlock()
	return f.ensureQuietAnchorLocked()
}

// Callers hold quietMu. No provider task is launched in passive mode.
func (f *Factory) ensureQuietAnchorLocked() (string, error) {
	if f.passive() {
		return "", fmt.Errorf("quiet anchor unavailable in passive mode")
	}
	if f.quiet.id != "" && f.Now().Sub(f.quiet.checked) < 5*time.Second {
		return f.quiet.id, nil
	}
	dir := filepath.Join(f.cfg.StateDir, "quiet-anchor")
	check := func(id string) (bool, error) {
		s, err := f.inspect(id)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return false, nil
			}
			return false, err
		}
		if s.Archived || s.Status == "closed" {
			return false, nil
		}
		if s.Id != id || s.Cwd != dir || s.Status == "" {
			return false, fmt.Errorf("quiet anchor identity or directory mismatch")
		}
		return true, nil
	}
	if f.quiet.id != "" {
		live, err := check(f.quiet.id)
		if err != nil {
			return "", fmt.Errorf("checking quiet anchor: %w", err)
		}
		if live {
			f.quiet.checked = f.Now()
			return f.quiet.id, nil
		}
	}
	// Read disk as well as the last board snapshot, including at startup before the first refresh.
	agents, err := LoadAgents(f.cfg.Home, f.cfg.Host, f.cfg.Window, f.Now())
	if err != nil {
		return "", err
	}
	agents = append(agents, f.agents()...)
	for _, a := range agents {
		if !quietAnchor(a) || a.archived() || a.Status == "closed" || a.Cwd != dir || a.WorkspaceID == "" {
			continue
		}
		live, err := check(a.ID)
		if err != nil {
			return "", fmt.Errorf("checking quiet anchor: %w", err)
		}
		if live {
			f.quiet = quietState{id: a.ID, checked: f.Now()}
			return a.ID, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	args := append(reviewerRunArgs(), "--new-workspace", "local", "--cwd", dir,
		"--title", "Factory quiet anchor", "--label", labelArg(labelRole, quietAnchorRole),
		"--label", parentAgentLabel+"="+quietBootstrapParent, "--background", "--quiet",
		"You are an idle notification anchor. Reply OK once. Do not use tools or start any work.")
	out, err := f.rawPaseo(args...)
	if err != nil {
		return "", fmt.Errorf("creating quiet anchor: %w", err)
	}
	id := lastLine(out)
	if id == "" {
		return "", fmt.Errorf("quiet anchor returned no identity")
	}
	// Remember the identity even if verification fails, so retries never duplicate an uncertain launch.
	f.quiet = quietState{id: id}
	live, err := check(id)
	if err != nil {
		return "", fmt.Errorf("verifying quiet anchor: %w", err)
	}
	if !live {
		return "", fmt.Errorf("quiet anchor is not live in its dedicated directory")
	}
	f.quiet.checked = f.Now()
	return id, nil
}

// quietLaunch is the single boundary for every factory-started provider turn. Sub-agents launched
// from an agent's shell already receive their real parent label from Paseo.
func (f *Factory) quietLaunch(args []string) ([]string, error) {
	id, err := f.ensureQuietAnchor()
	if err != nil {
		return nil, err
	}
	labels := quietRunLabels(args)
	parent := labels[parentAgentLabel]
	if parent == "" {
		parent = id
	}
	n := len(args) - 1
	out := append([]string{}, args[:n]...)
	out = append(out, "--label", parentAgentLabel+"="+parent, "--label", labelArg(labelQuiet, id), args[n])
	return out, nil
}

func quietManaged(agents []Agent) map[string]bool {
	managed := map[string]bool{}
	for _, a := range agents {
		if !boardAnchor(a) && (factoryAgent(a) || a.L(labelQuiet) != "") {
			managed[a.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, a := range agents {
			if !boardAnchor(a) && !managed[a.ID] && managed[a.Labels[parentAgentLabel]] {
				managed[a.ID] = true
				changed = true
			}
		}
	}
	return managed
}

func (f *Factory) reconcileQuiet() error {
	if f.passive() || f.stopping.Load() {
		return nil
	}
	agents := f.agents()
	managed := quietManaged(agents)
	if len(managed) == 0 {
		return nil
	}
	id, err := f.ensureQuietAnchor()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if f.stopping.Load() {
			return nil
		}
		if managed[a.ID] && !a.archived() && a.Status != "closed" {
			if a.Labels[parentAgentLabel] == quietParent(a, id, agents) && a.L(labelQuiet) == id {
				continue
			}
			live, err := f.inspect(a.ID)
			if err != nil {
				return err
			}
			if live.Archived || live.Status == "closed" {
				continue
			}
			if err := f.quietAgent(a, id, agents, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func quietParent(a Agent, id string, agents []Agent) string {
	parent := a.Labels[parentAgentLabel]
	for _, p := range agents {
		if p.ID == parent && quietAnchor(p) {
			parent = id
			break
		}
	}
	if parent == "" || parent == a.L(labelQuiet) {
		parent = id
	}
	return parent
}

func (f *Factory) quietAgent(a Agent, id string, agents []Agent, force bool) error {
	parent := quietParent(a, id, agents)
	if !force && a.Labels[parentAgentLabel] == parent && a.L(labelQuiet) == id {
		return nil
	}
	if _, err := f.rawPaseo("agent", "update", a.ID, "--label", parentAgentLabel+"="+parent,
		"--label", labelArg(labelQuiet, id)); err != nil {
		return fmt.Errorf("quieting %s: %w", short(a.ID), err)
	}
	// Keep this snapshot idempotent until the next disk refresh. Never mutate a published map.
	f.mu.Lock()
	for i, old := range f.list {
		if old.ID == a.ID {
			f.list = append([]Agent{}, f.list...)
			a = old
			a.Labels = maps.Clone(old.Labels)
			if a.Labels == nil {
				a.Labels = map[string]string{}
			}
			a.Labels[parentAgentLabel] = parent
			a.Labels[labelPrefix+labelQuiet] = id
			f.list[i] = a
			break
		}
	}
	f.mu.Unlock()
	return nil
}

func (f *Factory) quietBeforeSend(id string) error {
	if f.passive() {
		return nil
	}
	agents := f.agents()
	a, ok := f.agentOK(id)
	if !ok || !quietManaged(agents)[a.ID] {
		return nil
	}
	anchor, err := f.ensureQuietAnchor()
	if err != nil {
		return err
	}
	// A dispatcher may have detached this agent since the snapshot. Always relabel before sending.
	return f.quietAgent(a, anchor, agents, true)
}

func quietBoundaries(agents []Agent, anchors map[string]bool) map[string]bool {
	out := maps.Clone(anchors)
	if out == nil {
		out = map[string]bool{}
	}
	for _, a := range agents {
		if boardAnchor(a) {
			out[a.ID] = true
		}
	}
	return out
}
