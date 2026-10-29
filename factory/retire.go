package factory

import (
	"fmt"
	"maps"
	"strconv"
	"time"
)

// A retired reviewer (retiredReviewer) has nothing left to say: its verdict was superseded or given
// up on, and an applied reviewer is never applied again. It is stopped and archived when it is
// retired (retireReviewer, retireReviews). When that fails, the reap tries again:
//
//   - A stop interrupts a live run, but a run record left "running" after its process ended (a
//     daemon or host restart mid-turn) has nothing to interrupt and stays "running". So when a stop
//     does not settle it, a retired reviewer is archived with --force: Paseo interrupts any live run
//     and archives the record, keeping its session. Only retired reviewers are forced; every other
//     archive refuses a running agent.
//   - Each failed try is counted on the reviewer (factory.retire-tries, factory.retire-failed-at), so
//     a restart keeps the count. The next try waits retireWaits after the last; after retireMaxTries the
//     reap stops trying and the card shows retireStuckFlag. Nothing deletes the reviewer.
//   - At most retirePerReap tries per reap, within the reap's own budget.
//   - A reviewer someone messaged after it was retired (inConversation) is never stopped, forced or
//     retried: that is a conversation, not a review. Only a stopped one is archived. Retiring writes
//     factory.retired with the time, the boundary for that check. A reviewer retired without it (by
//     an older factory) has no known retirement time, so it counts as untouched only when its own
//     session file shows that nobody ever wrote to it after its prompt (onlyPrompted); the time of
//     that check is then stored as its factory.retired. An unreadable session protects it.
const (
	labelRetired     = "retired"          // on a retired reviewer: when it was retired, written with its applied verdict
	labelRetireTries = "retire-tries"     // on a retired reviewer: failed stop-and-archive tries so far
	labelRetireAt    = "retire-failed-at" // on a retired reviewer: when the last of those tries failed
	retirePerReap    = 2
	retireStuckFlag  = "retired reviewer could not be archived"
)

// retireWaits[n-1] is the wait after the nth failed try.
var retireWaits = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour}

var retireMaxTries = len(retireWaits) + 1

// retiredReviewer: a reviewer whose review was retired, superseded or given up on with no verdict.
// Nothing it says later is applied, so it is not reviewing even while it runs.
func retiredReviewer(a Agent) bool {
	v := a.L(labelVerdict)
	return a.L(labelRole) == "reviewer" && a.L(labelApplied) != "" && (v == "superseded" || v == "")
}

// retireLabels marks a reviewer retired at now with verdict ("superseded", or "" for one given up on).
func retireLabels(verdict string, now time.Time) map[string]string {
	return map[string]string{labelApplied: "1", labelVerdict: verdict, labelRetired: iso(now)}
}

// inConversation reports whether retired reviewer a may be in a conversation, so it must not be
// interrupted: it got a message after it was retired, or, with no factory.retired marker, its
// session does not show its prompt as its only message. An unknown last-message time counts too.
func (f *Factory) inConversation(a Agent) bool {
	if a.LastUserMessageAt.IsZero() {
		return true
	}
	if at := parseTime(a.L(labelRetired)); !at.IsZero() {
		return a.LastUserMessageAt.After(at)
	}
	return !f.onlyPrompted(a)
}

// onlyPrompted reports whether agent a's session file has exactly one user message (its prompt),
// and Paseo's last-message time is no later than that message's, so no message is on its way
// either. Any failure to tell says no.
func (f *Factory) onlyPrompted(a Agent) bool {
	evs, err := storedEvents(f.cfg.Home, a)
	if err != nil {
		return false
	}
	lines, ts := map[int]bool{}, ""
	for _, e := range evs {
		if e.Kind == "user" {
			lines[e.Line], ts = true, e.Ts
		}
	}
	at := parseTime(ts)
	return len(lines) == 1 && !at.IsZero() && !a.LastUserMessageAt.After(at)
}

// retireTry is a retired reviewer's failed archive tries and the time of the last.
type retireTry struct {
	n  int
	at time.Time
}

// retireTriesOf is reviewer a's count: this process's, or the one stored on it by an earlier process.
func (f *Factory) retireTriesOf(a Agent) retireTry {
	f.mu.Lock()
	t, ok := f.retireTries[a.ID]
	f.mu.Unlock()
	if ok {
		return t
	}
	n, err := strconv.Atoi(a.L(labelRetireTries))
	if err != nil || n < 0 {
		return retireTry{}
	}
	return retireTry{n: n, at: parseTime(a.L(labelRetireAt))}
}

// retireDue reports whether the reap should try retired reviewer a again now.
func (f *Factory) retireDue(a Agent, now time.Time) bool {
	t := f.retireTriesOf(a)
	switch {
	case t.n == 0:
		return true
	case t.n >= retireMaxTries:
		if f.noteOnce("retire-gave-up "+a.ID, "") {
			f.logf("reviewer %s retired but still not archived after %d tries; archive it by hand", short(a.ID), t.n)
		}
		return false
	}
	return !now.Before(t.at.Add(retireWaits[t.n-1]))
}

// archiveRetired stops and archives retired reviewer a, forcing the archive when a stop does not
// settle it, and counts a failed try for the reap. One in a conversation (inConversation) is only
// archived if it has stopped.
func (f *Factory) archiveRetired(a Agent) error {
	if f.inConversation(a) {
		return f.archiveWith(a.ID, false, false)
	}
	if a.L(labelRetired) == "" {
		// retired by an older factory and just checked against its session: that check is the boundary
		if err := f.setLabels(a.ID, map[string]string{labelRetired: iso(f.Now())}); err != nil {
			return err
		}
	}
	err := f.archiveWith(a.ID, true, true)
	if err == nil {
		f.mu.Lock()
		delete(f.retireTries, a.ID)
		f.mu.Unlock()
		return nil
	}
	t := f.retireTriesOf(a)
	t = retireTry{n: t.n + 1, at: f.Now()}
	f.mu.Lock()
	f.retireTries[a.ID] = t
	f.mu.Unlock()
	if lerr := f.setLabels(a.ID, map[string]string{labelRetireTries: fmt.Sprint(t.n), labelRetireAt: iso(t.at)}); lerr != nil {
		f.logf("reviewer %s: counting its archive try: %v", short(a.ID), lerr)
	}
	return err
}

// withRetireLabels is r with labels just written on it, before the next Refresh reads them.
func withRetireLabels(r Agent, labels map[string]string) Agent {
	r.Labels = maps.Clone(r.Labels)
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	for k, v := range labels {
		r.Labels[labelPrefix+k] = v
	}
	return r
}
