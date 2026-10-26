package factory

import (
	"sort"
	"time"
)

// The idle reap (run by every Tick) frees the memory of factory agents nobody is waiting on. An
// idle agent stays loaded in Paseo until it is archived; archiving closes it, and a later
// `paseo send` unarchives it and reloads its session from disk.
//
//   - A Done card is archived once it has been idle doneGrace, so a follow-up message right after
//     landing still finds it live. A reviewer is archived once its verdict is applied (ApplyVerdict
//     does so too; this catches a failed archive there).
//   - A Needs Zach card is parked (factory.parked, then archived) when it has been idle
//     Config.IdleTTL, or when more than Config.IdleCap factory agents are live and idle: then the
//     longest idle go first, none idle under capMinIdle. A parked card keeps its column, and Zach's
//     reply (board or email) wakes it (Factory.send). The factory never wakes it on its own.
//   - Cards in other columns are not parked: the factory moves them on, or to Needs Zach, within
//     minutes. Running agents and agents the factory did not start are never touched, only counted.
//   - At most reapPerTick archives per Tick, so a backlog cannot hold the loop.
const (
	defaultIdleTTL = 3 * time.Hour
	defaultIdleCap = 10
	doneGrace      = 5 * time.Minute
	capMinIdle     = 30 * time.Minute
	reapPerTick    = 20
	parkedKeep     = 14 * 24 * time.Hour // a parked card stays on the board this long (collect.go)
	parkedFlag     = "parked: resumes on reply"
)

// ReapReport is the last reap that changed anything, for /api/health.
type ReapReport struct {
	At       string   `json:"at"`
	Archived []string `json:"archived,omitempty"`
	Parked   []string `json:"parked,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// idleSince is how long stopped agent a has been idle: since its last activity or its last move.
func idleSince(a Agent, now time.Time) time.Duration {
	ref := a.LastActivityAt
	if s := parseTime(a.L(labelSince)); s.After(ref) {
		ref = s
	}
	return now.Sub(ref)
}

// factoryAgent: an agent the factory started or moves (a card, reviewer, scout or picker).
func factoryAgent(a Agent) bool { return a.L(labelStage) != "" || a.L(labelRole) != "" }

func (f *Factory) reap(now time.Time) {
	agents := f.agents()
	rep := ReapReport{At: iso(now)}
	budget := reapPerTick
	archive := func(a Agent, park bool) bool {
		if budget == 0 || f.stopping.Load() {
			return false
		}
		budget--
		// the snapshot can be a poll old: Zach may have messaged it or dropped it since
		if live, err := f.stillStopped(a); !live {
			if err != nil {
				rep.Errors = append(rep.Errors, short(a.ID)+": "+err.Error())
			}
			return false
		}
		if park {
			if err := f.setLabels(a.ID, map[string]string{labelParked: iso(now)}); err != nil {
				rep.Errors = append(rep.Errors, short(a.ID)+": "+err.Error())
				return false
			}
			f.mu.Lock()
			f.parkedNow[a.ID] = iso(now)
			f.mu.Unlock()
		}
		if err := f.archiveStoppedAgent(a.ID); err != nil {
			// a parked label left on a live agent is cleared by the next reap
			rep.Errors = append(rep.Errors, short(a.ID)+": "+err.Error())
			f.mu.Lock()
			delete(f.parkedNow, a.ID)
			f.mu.Unlock()
			return false
		}
		if park {
			rep.Parked = append(rep.Parked, short(a.ID))
		} else {
			rep.Archived = append(rep.Archived, short(a.ID))
		}
		return true
	}

	var waiting []Agent // Needs Zach cards that could be parked
	idleLive := 0
	for _, a := range agents {
		if a.L(labelParked) != "" && !a.archived() {
			// woken by a send whose label clear failed, or a park whose archive failed
			if err := f.setLabels(a.ID, map[string]string{labelParked: ""}); err != nil {
				rep.Errors = append(rep.Errors, short(a.ID)+": "+err.Error())
			}
		}
		if !factoryAgent(a) || !a.stopped() {
			continue
		}
		switch {
		case a.L(labelRole) == "reviewer":
			// one with no verdict applied yet is ApplyVerdict's (it archives after applying)
			if a.L(labelApplied) != "" && archive(a, false) {
				continue
			}
		case a.L(labelRole) != "":
			// scouts and pickers' own rules archive them
		case a.L(labelStage) == "landed":
			if idleSince(a, now) >= doneGrace && archive(a, false) {
				continue
			}
		case a.L(labelStage) == "zach" && !reviewPending(agents, a.ID, now):
			waiting = append(waiting, a)
		}
		if a.Status == "idle" {
			idleLive++
		}
	}

	sort.SliceStable(waiting, func(i, j int) bool { return idleSince(waiting[i], now) > idleSince(waiting[j], now) })
	for _, a := range waiting {
		idle := idleSince(a, now)
		if idle < f.cfg.IdleTTL && (idleLive <= f.cfg.IdleCap || idle < capMinIdle) {
			continue
		}
		if archive(a, true) {
			f.logf("%s parked: idle %s", short(a.ID), idle.Round(time.Minute))
			idleLive--
		}
	}

	if len(rep.Archived)+len(rep.Parked)+len(rep.Errors) > 0 {
		f.mu.Lock()
		f.lastReap = &rep
		f.mu.Unlock()
		if len(rep.Archived) > 0 {
			f.logf("reap archived %d idle agents", len(rep.Archived))
		}
	}
}

// liveGone asks Paseo (not the snapshot) whether agent a has been archived for good: archived and
// not parked. The parked label comes from the snapshot, which the reap updates when it parks.
func (f *Factory) liveGone(a Agent) (bool, error) {
	archived, _, err := f.liveState(a.ID)
	return archived && a.L(labelParked) == "", err
}
