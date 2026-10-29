//go:build linux

package factory

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hotIDs(f *Factory) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for id := range f.hot {
		out[id] = true
	}
	return out
}

// The watcher sees an agent file change within a second, including in a directory made after it
// started, and ignores writes that change only activity times.
func TestWatcherMarksChangedAgentsHot(t *testing.T) {
	run := card("c1", map[string]string{labelStage: "building"})
	run.Status = "running"
	f, _ := newTestFactory(t)
	writeAgents(t, f, run)
	f.reload()
	stop := f.startWatch()
	defer stop()

	busy := run
	busy.LastActivityAt = t0.Add(time.Minute)
	writeAgents(t, f, busy)
	time.Sleep(3 * watchDebounce)
	if h := hotIDs(f); len(h) != 0 {
		t.Errorf("activity-only write marked %v hot", h)
	}

	start := time.Now()
	idle := run
	idle.Status = "idle"
	writeAgents(t, f, idle)
	waitFor(t, "c1 hot", func() bool { return hotIDs(f)["c1"] })
	if took := time.Since(start); took > time.Second {
		t.Errorf("c1 hot after %v", took)
	}
	select {
	case <-f.hotKick:
	default:
		t.Error("loop not woken")
	}

	writeAgents(t, f, card("c9", map[string]string{labelStage: "plan"})) // a new directory
	waitFor(t, "c9 hot", func() bool { return hotIDs(f)["c9"] })
}

// A file renamed into place counts as written.
func TestWatcherSeesRenamedFiles(t *testing.T) {
	f, _ := newTestFactory(t)
	writeAgents(t, f, card("c1", map[string]string{labelStage: "plan"}))
	f.reload()
	stop := f.startWatch()
	defer stop()
	moved := card("c1", map[string]string{labelStage: "plan-review"})
	writeAgents(t, f, moved)
	path := agentFile(f.cfg.Home, "c1")
	b, _ := os.ReadFile(path)
	clear(f.hot)
	tmp := filepath.Join(filepath.Dir(path), ".c1.tmp")
	os.WriteFile(tmp, []byte(strings.Replace(string(b), "plan-review", "building", 1)), 0o644)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "c1 hot after rename", func() bool { return hotIDs(f)["c1"] })
}

// With no agent directory the watcher is off and says so; the loop still polls.
func TestRunPollsWithoutWatcher(t *testing.T) {
	f, _ := newTestFactory(t)
	var tick atomic.Int64 // a clock that moves on every read, so each loop's end time differs
	f.Now = func() time.Time { return t0.Add(time.Duration(tick.Add(1)) * time.Millisecond) }
	f.cfg.Poll = 10 * time.Millisecond
	stop := startRun(t, f)
	waitFor(t, "a first poll", func() bool { return f.Health().LastLoopDone != "" })
	first := f.Health().LastLoopDone
	waitFor(t, "another poll", func() bool { return f.Health().LastLoopDone != first })
	stop()
	if logs := strings.Join(f.log, "\n"); !strings.Contains(logs, "agent watcher off") {
		t.Errorf("no watcher-off line in %v", logs)
	}
}

// End to end: a reviewer whose file turns idle has its verdict sent within about a second, with
// no poll in between.
func TestRunActsOnStoppedReviewerWithinASecond(t *testing.T) {
	f, p := threeReviews(t)
	for _, n := range []string{"1", "2", "3"} {
		r := f.agent("r" + n)
		r.Status = "running"
		writeAgents(t, f, r)
	}
	f.reload()
	f.cfg.Poll = time.Hour
	startRun(t, f)
	waitFor(t, "first pass", func() bool { return f.Health().LastLoopDone != "" })
	time.Sleep(50 * time.Millisecond) // the watcher is started before the first pass ends
	start := time.Now()
	r := f.agent("r2")
	r.Status = "idle"
	writeAgents(t, f, r)
	waitFor(t, "c2 sent back", func() bool { return len(p.sent("c2")) == 1 })
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("verdict sent %v after the reviewer stopped", took)
	}
	if len(p.sent("c1")) != 0 || len(p.sent("c3")) != 0 {
		t.Error("running reviewers' cards moved")
	}
}
