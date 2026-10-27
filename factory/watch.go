package factory

import (
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The agent watcher: Paseo writes an agent's file (~/.paseo/agents/<dir>/<id>.json) when its status,
// archive or labels change. The factory watches those files (inotify on Linux, watch_linux.go) so an
// agent that stops is acted on within about a second, not at the next poll; the poll stays as the
// fallback, and is all there is when the watcher cannot run.
//
// A changed file whose status, archive time, attention flag or factory labels differ from the
// board's snapshot marks the agent hot and wakes the loop for a hot pass: reload the files (no
// paseo calls), then the rules for the hot agents only. A Tick in progress checks for hot agents
// between agents and between its queued archives, so a stopped agent waits at most for the call
// in flight. Every agent is handled at most once per pass (tickPass.done).
//
// Paseo calls stay in order: a card's move is check, label, send, and moves of different cards
// share the board snapshot and labels, so they are not run concurrently. What is taken off a
// hand-off's path instead is the reviewer archive, which runs after the pass's moves.

// watchDebounce: changed files are read this long after the first change, so the several writes
// of one update are read once.
const watchDebounce = 250 * time.Millisecond

// agentFileState is the part of an agent file the watcher compares with the snapshot.
type agentFileState struct {
	ID, LastStatus, ArchivedAt, AttentionReason string
	Internal, RequiresAttention                 bool
	Labels                                      map[string]string
}

func factoryLabels(l map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		if strings.HasPrefix(k, labelPrefix) {
			out[k] = v
		}
	}
	return out
}

// changedFor reports whether the file differs from the snapshot's agent a in what the rules read.
func (d agentFileState) changedFor(a Agent) bool {
	return a.Status != d.LastStatus || !a.ArchivedAt.Equal(parseTime(d.ArchivedAt)) ||
		a.RequiresAttention != d.RequiresAttention || a.AttentionReason != d.AttentionReason ||
		!maps.Equal(factoryLabels(a.Labels), factoryLabels(d.Labels))
}

// noteAgentFiles reads the changed agent files and marks hot each factory agent whose file differs
// from the snapshot (or is new). nil means the watcher lost events: a full pass runs.
func (f *Factory) noteAgentFiles(paths []string) {
	if paths == nil {
		f.poke()
		return
	}
	snap := map[string]Agent{}
	for _, a := range f.agents() {
		snap[a.ID] = a
	}
	var hot []string
	for _, p := range paths {
		var d agentFileState
		if readJSON(p, &d) != nil || d.ID == "" || d.Internal {
			continue
		}
		a, ok := snap[d.ID]
		if len(factoryLabels(d.Labels)) == 0 && (!ok || !factoryAgent(a)) {
			continue // not the factory's: the next poll shows it
		}
		if !ok || d.changedFor(a) {
			hot = append(hot, d.ID)
		}
	}
	f.markHot(hot)
}

// markHot queues agents for a hot pass and wakes the loop.
func (f *Factory) markHot(ids []string) {
	if len(ids) == 0 {
		return
	}
	f.mu.Lock()
	for _, id := range ids {
		f.hot[id] = true
	}
	f.mu.Unlock()
	f.hotPoke()
}

func (f *Factory) hotPoke() {
	select {
	case f.hotKick <- struct{}{}:
	default:
	}
}

// tickPass is one Tick's or hot pass's record of the agents it already handled.
type tickPass struct{ done map[string]bool }

// beginPass starts a pass: archives asked for during it (deferArchive) wait until its moves are made.
func (f *Factory) beginPass() *tickPass {
	f.mu.Lock()
	f.archiveQ = &[]string{}
	f.mu.Unlock()
	return &tickPass{done: map[string]bool{}}
}

// deferArchive archives a finished reviewer or scout after the pass's moves, or now outside a pass.
func (f *Factory) deferArchive(id string) error {
	f.mu.Lock()
	if q := f.archiveQ; q != nil {
		*q = append(*q, id)
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()
	return f.archiveStoppedAgent(id)
}

// drainArchives runs the pass's queued archives, handling hot agents between them, and ends the
// queue. Archives left when the factory stops are the reap's: it archives applied reviewers.
func (f *Factory) drainArchives(p *tickPass) {
	for {
		f.mu.Lock()
		q := f.archiveQ
		if q == nil || len(*q) == 0 || f.stopping.Load() {
			f.archiveQ = nil
			f.mu.Unlock()
			return
		}
		id := (*q)[0]
		*q = (*q)[1:]
		f.mu.Unlock()
		f.tickHot(p)
		f.archiveStoppedAgent(id) // logs its own error
	}
}

// endPass drops a queue an early return left behind.
func (f *Factory) endPass() {
	f.mu.Lock()
	f.archiveQ = nil
	f.mu.Unlock()
}

// takeHot removes and returns the hot agents this pass has not handled yet. Ones it already handled
// stay hot for the next pass, which is woken.
func (f *Factory) takeHot(p *tickPass) []string {
	f.mu.Lock()
	var ids []string
	again := false
	for id := range f.hot {
		if p.done[id] {
			again = true
			continue
		}
		ids = append(ids, id)
		delete(f.hot, id)
	}
	f.mu.Unlock()
	if again {
		f.hotPoke()
	}
	sort.Strings(ids)
	return ids
}

// tickHot rereads the agent files (no paseo calls) and applies the rules to the hot agents.
func (f *Factory) tickHot(p *tickPass) {
	ids := f.takeHot(p)
	if len(ids) == 0 {
		return
	}
	if err := f.reload(); err != nil {
		f.logf("hot pass: reading agents: %v", err)
	}
	now := f.Now()
	for _, id := range ids {
		if f.stopping.Load() {
			return
		}
		a, ok := f.agentByID(id)
		if !ok {
			continue
		}
		p.done[id] = true
		if err := f.tickAgent(a, now); err != nil {
			f.logf("rule error on %s: %v", short(a.ID), err)
		}
	}
}

// hotPass is the loop's pass after a watcher wake: only the hot agents, then their archives.
func (f *Factory) hotPass() {
	p := f.beginPass()
	defer f.endPass()
	f.tickHot(p)
	f.drainArchives(p)
}

// agentByID is the snapshot's agent with exactly this id.
func (f *Factory) agentByID(id string) (Agent, bool) {
	for _, a := range f.agents() {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// graceDue is when the earliest grace window of the rules ends (a reviewer's first minute, a card's
// 45 s after a move), and the agents waiting on it: the loop wakes then instead of at the next poll.
func graceDue(agents []Agent, now time.Time) (time.Time, []string) {
	var due time.Time
	var ids []string
	add := func(id string, at time.Time) {
		if at.Before(now) {
			return
		}
		switch {
		case due.IsZero() || at.Before(due):
			due, ids = at, []string{id}
		case at.Equal(due):
			ids = append(ids, id)
		}
	}
	for _, a := range agents {
		if a.archived() {
			continue
		}
		if a.L(labelRole) == "reviewer" {
			if a.L(labelApplied) == "" && (a.Status == "idle" || a.Status == "closed") {
				add(a.ID, a.CreatedAt.Add(time.Minute+time.Second))
			}
			continue
		}
		since := parseTime(a.L(labelSince))
		if a.L(labelStage) != "" && a.stopped() && !since.IsZero() && a.LastActivityAt.After(since) {
			add(a.ID, since.Add(actedGrace+time.Second))
		}
	}
	return due, ids
}

// debouncer gathers changed paths and hands them on once, watchDebounce after the first.
type debouncer struct {
	mu    sync.Mutex
	paths map[string]bool
	lost  bool
	timer *time.Timer
	flush func([]string)
}

func (d *debouncer) add(paths []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if paths == nil {
		d.lost = true
	}
	for _, p := range paths {
		d.paths[p] = true
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(watchDebounce, d.fire)
	}
}

func (d *debouncer) fire() {
	d.mu.Lock()
	paths, lost := d.paths, d.lost
	d.paths, d.lost, d.timer = map[string]bool{}, false, nil
	d.mu.Unlock()
	if lost {
		d.flush(nil)
	}
	if len(paths) > 0 {
		d.flush(slicesSorted(paths))
	}
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func slicesSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// startWatch watches the agent files for the loop; the returned func stops it. When the watcher
// cannot start, the loop polls only.
func (f *Factory) startWatch() func() {
	d := &debouncer{paths: map[string]bool{}, flush: f.noteAgentFiles}
	stop, err := watchDir(filepath.Join(f.cfg.Home, ".paseo/agents"), d.add, func(err error) {
		f.logf("agent watcher stopped, polling every %v: %v", f.cfg.Poll, err)
	})
	if err != nil {
		f.logf("agent watcher off, polling every %v: %v", f.cfg.Poll, err)
		return func() {}
	}
	return func() { stop(); d.stop() }
}
