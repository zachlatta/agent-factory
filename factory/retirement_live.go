package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RetirementLive describes supplemental, non-atomic reads, never permission.
type RetirementLive struct {
	CLIVersion    string `json:"cliVersion"`
	DaemonVersion string `json:"daemonVersion"`
	StartedAt     string `json:"startedAt"`
	At            string `json:"at"`
	DeadlineAt    string `json:"deadlineAt"`
}

// Complete refers only to Scope at the time of this read, not resource safety.
// Counts omit names, command lines and provider configuration.
type RetirementObservation struct {
	Source    string `json:"source"`
	Scope     string `json:"scope"`
	StartedAt string `json:"startedAt"`
	At        string `json:"at"`
	Complete  bool   `json:"complete"`
	Count     int    `json:"count"`
}

type retirementReadCommand func(context.Context, ...string) ([]byte, error)

// InventoryRetirementLive adds opt-in reads from the daemon selected by home's
// .paseo directory. It never uses the board's filtered view or a remote endpoint.
func InventoryRetirementLive(ctx context.Context, home string, now time.Time) RetirementInventory {
	persisted, cancel := context.WithTimeout(ctx, 2*time.Minute)
	r := InventoryRetirement(persisted, home, now)
	if persisted.Err() != nil {
		r.Problems = append(r.Problems, "persisted inventory deadline exceeded or cancelled")
	}
	cancel()
	observeRetirementLive(ctx, &r, home, retirementLiveCommand, time.Now)
	return r
}

const retirementLiveCommandTimeout = 20 * time.Second

func retirementLiveCommand(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, retirementLiveCommandTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "paseo", args...)
	c.WaitDelay = time.Second
	return c.Output()
}

func retirementSetCheck(w *RetirementWorkspace, name, state, reason string) {
	for i := range w.Checks {
		if w.Checks[i].Name == name {
			// A later incomplete read must not erase an observed blocker.
			if w.Checks[i].State != "blocked" {
				w.Checks[i] = RetirementCheck{name, state, reason}
			}
			return
		}
	}
	w.Checks = append(w.Checks, RetirementCheck{name, state, reason})
}

func observeRetirementLive(ctx context.Context, r *RetirementInventory, home string, run retirementReadCommand, now func() time.Time) {
	// Allow startup/list reads plus one bounded process per persisted workspace.
	// This budget starts after the persisted reader, and parent cancellation wins.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute+time.Duration(len(r.Workspaces))*(retirementLiveCommandTimeout+time.Second))
	defer cancel()
	deadline, _ := ctx.Deadline()
	r.Live = &RetirementLive{StartedAt: iso(now()), DeadlineAt: iso(deadline)}
	defer func() {
		r.Live.At = iso(now())
		if ctx.Err() != nil {
			r.Problems = append(r.Problems, "live inventory deadline exceeded or cancelled")
		}
	}()
	for i := range r.Workspaces {
		retirementSetCheck(&r.Workspaces[i], "exclusion", "unknown", "no supported concurrent-use exclusion established")
	}
	if len(r.Problems) > 0 {
		r.Problems = append(r.Problems, "live reads skipped: persisted inventory incomplete")
		return
	}
	paseoHome := retirementPath(filepath.Join(home, ".paseo"))
	if paseoHome == "" {
		r.Problems = append(r.Problems, "live reads require an absolute daemon home")
		return
	}
	b, err := run(ctx, "--version")
	if err != nil || strings.TrimSpace(string(b)) != "0.11.4" {
		r.Problems = append(r.Problems, "live reads require the inspected CLI version")
		return
	}
	r.Live.CLIVersion = "0.11.4"
	b, err = run(ctx, "status", "--json", "--home", paseoHome)
	var status struct{ Home, ConnectedDaemon, LocalDaemon, DaemonVersion string }
	if err != nil || json.Unmarshal(b, &status) != nil || status.ConnectedDaemon != "reachable" || status.LocalDaemon != "running" || status.DaemonVersion != "0.11.4" || retirementPath(status.Home) != paseoHome {
		r.Problems = append(r.Problems, "live daemon identity or version unavailable or unsupported")
		return
	}
	r.Live.DaemonVersion = status.DaemonVersion
	for i := range r.Workspaces {
		retirementSetCheck(&r.Workspaces[i], "exclusion", "blocked", "inspected daemon API has no supported retirement lease; archive stops resources and may run teardown")
	}

	// The supported terminal list is unpaginated and includes every workspace.
	started := iso(now())
	b, err = run(ctx, "terminal", "ls", "--all", "--json", "--home", paseoHome)
	var terminals []struct{ ID, WorkspaceID, Cwd string }
	if err == nil {
		err = retirementLiveArray(b, &terminals)
	}
	seen := map[string]bool{}
	for _, t := range terminals {
		if t.ID == "" || seen[t.ID] || retirementPath(t.Cwd) == "" {
			err = errors.New("unsupported terminal record")
		}
		seen[t.ID] = true
	}
	if err != nil {
		r.Problems = append(r.Problems, "complete daemon terminal list unavailable or malformed")
	}
	at := iso(now())
	for i := range r.Workspaces {
		w := &r.Workspaces[i]
		o := RetirementObservation{Source: "paseo terminal ls --all", Scope: "all daemon terminals by workspace ID and overlapping canonical path", StartedAt: started, At: at, Complete: err == nil}
		if err == nil {
			for _, t := range terminals {
				if t.WorkspaceID == w.ID || retirementOverlap(w.CanonicalPath, retirementPath(t.Cwd)) {
					o.Count++
				}
			}
			if o.Count > 0 {
				retirementSetCheck(w, "terminals", "blocked", "daemon terminals reference this workspace or an overlapping path")
			} else if w.CanonicalPath != "" {
				retirementSetCheck(w, "terminals", "observed", "no matching terminals in this daemon list at read time; not an exclusion guarantee")
			} else {
				o.Complete = false
			}
		}
		w.Observations = append(w.Observations, o)
	}

	// Query exact IDs, including archived records; never resolve by cwd or slug.
	// A service in a sibling workspace still protects their shared folder.
	failedScripts := 0
	for _, owner := range r.Workspaces {
		started = iso(now())
		b, err = run(ctx, "script", "ls", "--workspace", owner.ID, "--json", "--home", paseoHome)
		var scripts []struct{ ScriptName, Type, Lifecycle string }
		if err == nil {
			err = retirementLiveArray(b, &scripts)
		}
		seen = map[string]bool{}
		for _, s := range scripts {
			if s.ScriptName == "" || seen[s.ScriptName] || !slices.Contains([]string{"service", "script"}, s.Type) || !slices.Contains([]string{"running", "stopped"}, s.Lifecycle) {
				err = errors.New("unsupported script record")
			}
			seen[s.ScriptName] = true
		}
		if err != nil {
			failedScripts++
		}
		at = iso(now())
		for i := range r.Workspaces {
			w := &r.Workspaces[i]
			if w.ID != owner.ID && w.CanonicalPath != "" && owner.CanonicalPath != "" && !retirementOverlap(w.CanonicalPath, owner.CanonicalPath) {
				continue
			}
			// The projection can omit unreadable configuration or runtime entries.
			// A parsed response cannot attest complete service coverage.
			o := RetirementObservation{Source: "paseo script ls --workspace", Scope: "configured-script projection of workspace " + owner.ID + "; configuration and runtime coverage unverified", StartedAt: started, At: at}
			if err == nil {
				for _, s := range scripts {
					if s.Lifecycle == "running" {
						o.Count++
					}
				}
				if o.Count > 0 {
					retirementSetCheck(w, "services", "blocked", "running configured script or service in this workspace or a potentially overlapping folder")
				}
			}
			w.Observations = append(w.Observations, o)
		}
	}
	if failedScripts > 0 {
		r.Problems = append(r.Problems, fmt.Sprintf("configured script list unavailable or malformed for %d workspaces", failedScripts))
	}
}

func retirementLiveArray(b []byte, v any) error {
	// Reject null, wrappers and partial/page envelopes, even when entries is empty.
	if !strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
		return errors.New("expected full array")
	}
	return json.Unmarshal(b, v)
}
