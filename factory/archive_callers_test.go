package factory

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Change daemon state between eligibility and archive, or acknowledge a write
// whose individual read cannot verify an archive timestamp.
type archiveCallerPaseo struct {
	mu                          sync.Mutex
	fake                        *fakePaseo
	id                          string
	runningAt, reads, postReads int
	unverified, wrote           bool
}

func (p *archiveCallerPaseo) Run(ctx context.Context, args ...string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	target := len(args) > 1 && args[1] == p.id
	if target && args[0] == "inspect" {
		p.reads++
		if p.reads == p.runningAt {
			p.fake.mu.Lock()
			p.fake.status[p.id] = "running"
			p.fake.busy[p.id] = 1
			p.fake.mu.Unlock()
		}
		if p.wrote {
			p.postReads++
		}
	}
	out, err := p.fake.Run(ctx, args...)
	if target && args[0] == "archive" && err == nil {
		p.wrote = true
	}
	if target && args[0] == "inspect" && p.wrote && p.unverified && err == nil {
		var state archiveState
		if err := json.Unmarshal([]byte(out), &state); err != nil {
			return "", err
		}
		state.ArchivedAt = ""
		b, err := json.Marshal(state)
		return string(b), err
	}
	return out, err
}

func TestAutomaticArchiveCallersPreserveProtections(t *testing.T) {
	for _, caller := range []string{"reap-done", "reap-reviewer", "reap-park", "verdict-live", "verdict-dropped", "verdict-land", "scout"} {
		for _, failure := range []string{"fresh-running", "unverified"} {
			t.Run(caller+"/"+failure, func(t *testing.T) {
				a := idleFor(card("subject", map[string]string{labelStage: "landed"}), time.Hour)
				target := card("target", map[string]string{labelStage: "plan-review", labelTier: "low"})
				switch caller {
				case "reap-reviewer":
					a = reviewer("subject", "target", "code")
					a.Labels[labelPrefix+labelApplied] = "1"
				case "reap-park":
					a = idleFor(card("subject", map[string]string{labelStage: "zach"}), 4*time.Hour)
				case "verdict-live", "verdict-dropped":
					a = reviewer("subject", "target", "plan")
				case "verdict-land":
					a = reviewer("subject", "target", "land")
				case "scout":
					a.Labels = map[string]string{labelPrefix + labelRole: "scout"}
				}
				if caller == "verdict-dropped" {
					target.ArchivedAt = t0.Add(-time.Minute)
				}
				f, p := newTestFactory(t, a, target)
				p.logs["subject"] = "TIER: low\nVERDICT: approve"
				if caller == "verdict-land" {
					if err := f.landReviews.put(LandReview{By: "target", Review: "subject", Status: "running"}, t0); err != nil {
						t.Fatal(err)
					}
				}
				daemon := &archiveCallerPaseo{fake: p, id: "subject", unverified: failure == "unverified"}
				if failure == "fresh-running" {
					daemon.runningAt = 1
					// Reap's eligibility read must pass before the agent starts running.
					if strings.HasPrefix(caller, "reap-") {
						daemon.runningAt = 2
					}
				}
				f.p = daemon
				switch {
				case strings.HasPrefix(caller, "reap-"):
					f.reap(t0)
				case strings.HasPrefix(caller, "verdict-"):
					_ = f.ApplyVerdict(a)
				default:
					_ = f.tickAgent(a, t0)
				}
				if p.called("stop", "subject") {
					t.Fatal("automatic archive stopped active work")
				}
				if f.agent("subject").archived() {
					t.Fatal("failed automatic archive marked snapshot")
				}
				if failure == "fresh-running" {
					if daemon.reads < daemon.runningAt {
						t.Fatal("did not reach the fresh status check")
					}
					if p.called("archive", "subject") {
						t.Fatal("automatic archive attempted on fresh running state")
					}
				} else {
					if !daemon.wrote {
						t.Fatal("did not exercise archive write")
					}
					if daemon.postReads != 1 {
						t.Fatalf("post-archive inspection count = %d, want 1", daemon.postReads)
					}
					if !strings.Contains(strings.Join(f.log, "\n"), "archive not verified") {
						t.Fatal("verification failure was not reported")
					}
				}
				if strings.HasPrefix(caller, "reap-") && f.lastReap != nil && len(f.lastReap.Archived)+len(f.lastReap.Parked) > 0 {
					t.Fatal("failed archive reported as reaped or parked")
				}
			})
		}
	}
}
