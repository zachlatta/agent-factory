package factory

import (
	"context"
	"reflect"
	"slices"
)

// ProcessState defaults to uncertainty so absent evidence cannot authorize recovery.
type ProcessState uint8

const (
	ProcessUncertain ProcessState = iota
	ProcessIdle
	ProcessLiveWork
)

type ProcessInspection struct {
	State  ProcessState
	Reason string // Fixed diagnostic code; never raw process evidence.
}

type ProcessHostLocality uint8

const (
	ProcessHostUnknown ProcessHostLocality = iota
	ProcessHostLocal
	ProcessHostRemote
)

type ProcessTarget struct {
	AgentID            string
	Locality           ProcessHostLocality // Local means the agent's daemon is explicitly known to be local.
	ToolServerCommands [][]string          // Exact executable and argument vectors, supplied by host settings.
}

// ProcessInfo describes one observation. StartTime is the OS process start
// identity, not a sampling timestamp. Command preserves argument boundaries.
type ProcessInfo struct {
	PID                 int
	ParentPID           int
	StartTime           string
	StartOrder          uint64 // OS start ticks since boot, comparable across this table; zero means unknown.
	UserID              string // OS user identity; empty means unknown.
	EnvironmentReadable bool
	Environment         map[string]string
	Command             []string
}

// ProcessSnapshot must cover the full local process table, including runner
// ancestors and descendants. Complete is false on missing table entries, identity reads or exits.
// An unreadable environment is represented by EnvironmentReadable=false.
// Readers must return independently owned observations, not reused mutable data.
type ProcessSnapshot struct {
	Complete  bool
	Processes []ProcessInfo
}

type ProcessReader interface {
	Snapshot(context.Context, ProcessTarget) (ProcessSnapshot, error)
}

// InspectAgentProcesses only classifies evidence; it performs no recovery.
// Two matching observations are not a lock: callers must revalidate before acting.
func InspectAgentProcesses(ctx context.Context, reader ProcessReader, target ProcessTarget) ProcessInspection {
	uncertain := func(reason string) ProcessInspection { return ProcessInspection{Reason: reason} }
	if target.Locality != ProcessHostLocal {
		return uncertain("host_not_local")
	}
	if target.AgentID == "" || reader == nil {
		return uncertain("missing_input")
	}
	var previous map[int]ProcessInfo
	var runner int
	for i := 0; i < 2; i++ {
		if ctx.Err() != nil {
			return uncertain("cancelled")
		}
		snapshot, err := reader.Snapshot(ctx, target)
		if ctx.Err() != nil {
			return uncertain("cancelled")
		}
		if err != nil {
			return uncertain("read_failed")
		}
		relevant, id, ok := processEvidence(snapshot, target.AgentID)
		if !ok {
			return uncertain("incomplete_or_ambiguous")
		}
		if i == 0 {
			previous = relevant
			runner = id
		} else if runner != id || !reflect.DeepEqual(previous, relevant) {
			return uncertain("processes_changed")
		}
	}
	// Only direct children can establish a tool-server exclusion.
	excluded := map[int]bool{}
	for pid, p := range previous {
		if p.ParentPID != runner {
			continue
		}
		for _, command := range target.ToolServerCommands {
			if len(command) > 0 && slices.Equal(p.Command, command) {
				excluded[pid] = true
				break
			}
		}
	}
	// Propagate exclusions through parent links, regardless of inherited environment.
	for changed := true; changed; {
		changed = false
		for pid, p := range previous {
			if !excluded[pid] && excluded[p.ParentPID] {
				excluded[pid] = true
				changed = true
			}
		}
	}
	for pid, p := range previous {
		if pid != runner && p.ParentPID != 0 && !excluded[pid] {
			// Ancestors were retained for identity checking, but are not runner work.
			for parent := p.ParentPID; parent != 0; parent = previous[parent].ParentPID {
				if parent == runner {
					return ProcessInspection{State: ProcessLiveWork, Reason: "live_descendants"}
				}
			}
		}
	}
	return ProcessInspection{State: ProcessIdle, Reason: "stable_idle"}
}

// processEvidence validates the table and retains the runner tree and ancestors.
// Unrelated processes may change between observations. Unreadable environments
// outside the runner tree require evidence ruling out another runner.
func processEvidence(s ProcessSnapshot, agentID string) (map[int]ProcessInfo, int, bool) {
	if !s.Complete {
		return nil, 0, false
	}
	all := make(map[int]ProcessInfo, len(s.Processes))
	for _, p := range s.Processes {
		if p.PID <= 0 || p.ParentPID < 0 || p.StartTime == "" {
			return nil, 0, false
		}
		if _, exists := all[p.PID]; exists {
			return nil, 0, false
		}
		all[p.PID] = p
	}
	runner := 0
	for _, p := range all {
		if p.ParentPID != 0 {
			if _, ok := all[p.ParentPID]; !ok {
				return nil, 0, false
			}
		}
		seen := map[int]bool{}
		for id := p.PID; id != 0; id = all[id].ParentPID {
			if seen[id] {
				return nil, 0, false
			}
			seen[id] = true
		}
		if p.EnvironmentReadable && p.Environment["PASEO_AGENT_ID"] == agentID {
			// A missing parent cannot prove that its environment lacks the id.
			parent, ok := all[p.ParentPID]
			if !ok || !parent.EnvironmentReadable {
				return nil, 0, false
			}
			if parent.Environment["PASEO_AGENT_ID"] != agentID {
				if runner != 0 {
					return nil, 0, false
				}
				runner = p.PID
			}
		}
	}
	if runner == 0 {
		return nil, 0, false
	}
	// Only the runner user's processes can be additional runner candidates.
	// Unknown ownership cannot establish that an unreadable process is unrelated.
	if all[runner].UserID == "" {
		return nil, 0, false
	}
	for _, p := range all {
		if !p.EnvironmentReadable && (p.UserID == "" || p.UserID == all[runner].UserID) {
			// A process predating the runner's parent cannot have inherited
			// this launch's agent id. Unknown or equal start ticks prove nothing.
			parent := all[all[runner].ParentPID]
			if p.UserID == "" || p.StartOrder == 0 || parent.StartOrder == 0 || p.StartOrder >= parent.StartOrder {
				return nil, 0, false
			}
		}
	}
	relevant := map[int]ProcessInfo{}
	for pid, p := range all {
		for id := pid; id != 0; id = all[id].ParentPID {
			if id == runner {
				if len(p.Command) == 0 || !p.EnvironmentReadable {
					return nil, 0, false
				}
				relevant[pid] = p
				break
			}
		}
	}
	for id := all[runner].ParentPID; id != 0; id = all[id].ParentPID {
		relevant[id] = all[id]
	}
	return relevant, runner, true
}
