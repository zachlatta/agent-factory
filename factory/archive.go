package factory

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Only stable releases with the complete archive lookup are accepted.
func fixedArchiveVersion(version string) bool {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return false
	}
	var nums [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return false
		}
		nums[i] = n
	}
	return nums[0] > 0 || nums[1] >= 13
}

type archiveState struct {
	ID, Status string
	Archived   bool
	ArchivedAt string
}

func (f *Factory) inspectArchive(id string) (archiveState, error) {
	out, err := f.paseo("inspect", id, "--json")
	if err != nil {
		return archiveState{}, err
	}
	var state archiveState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return state, fmt.Errorf("archive inspect: %w", err)
	}
	if state.ID != id {
		return state, fmt.Errorf("archive inspect identity mismatch for %s", short(id))
	}
	if state.Status == "" {
		return state, fmt.Errorf("archive inspect missing status for %s", short(id))
	}
	return state, nil
}
func (s archiveState) verified() bool {
	at, err := time.Parse(time.RFC3339Nano, s.ArchivedAt)
	return s.Archived && err == nil && !at.IsZero()
}

// Explicit close/drop may stop an agent. A parked card only leaves its parked state.
func (f *Factory) archiveAgent(id string) error { return f.archiveChecked(id, true) }

// Automatic completion never stops active work or clears a parked card.
func (f *Factory) archiveStoppedAgent(id string) error {
	err := f.archiveChecked(id, false)
	if err != nil {
		f.logf("%s archive: %v", short(id), err)
	}
	return err
}
func (f *Factory) archiveChecked(id string, allowStop bool) error {
	if a, ok := f.agentOK(id); ok {
		id = a.ID
		if a.parked() {
			if allowStop {
				return f.unpark(id)
			}
			return nil
		}
	}
	state, err := f.inspectArchive(id)
	if err != nil {
		return err
	}
	if state.Archived {
		if !state.verified() {
			return fmt.Errorf("archive timestamp missing or invalid for %s", short(id))
		}
		f.markArchived(id)
		return nil
	}
	if state.Status == "running" && !allowStop {
		return fmt.Errorf("agent %s is currently running", short(id))
	}
	_, err = f.paseo("archive", id)
	if err != nil && allowStop && strings.Contains(err.Error(), "currently running") {
		if _, err := f.paseo("stop", id); err != nil {
			return err
		}
		for try := 1; try <= archiveTries; try++ {
			state, err = f.inspectArchive(id)
			if err != nil {
				return err
			}
			if state.Status == "running" {
				err = fmt.Errorf("agent %s is currently running", short(id))
			} else {
				_, err = f.paseo("archive", id)
			}
			if err == nil || !strings.Contains(err.Error(), "currently running") {
				break
			}
			if try < archiveTries {
				f.Sleep(time.Second)
			}
		}
	}
	if err != nil {
		return err
	}
	if !f.cfg.DryRun {
		state, err = f.inspectArchive(id)
		if err != nil {
			return err
		}
		if !state.verified() {
			return fmt.Errorf("archive not verified for %s", short(id))
		}
	}
	f.markArchived(id)
	return nil
}
