package factory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

// A parked card Zach approves while its dependency is still in flight waits archived in Landing.
// His approval (factory.held-approved) lets the factory release it once the dependency lands, with
// the usual freshness and scan gates; a parked card without that approval is never woken.

// parkedHeld is a parked (archived) landing card held on d0e0001, approved by Zach unless approved is false.
func parkedHeld(t *testing.T, approved bool, extra map[string]string) (*Factory, *fakePaseo, func(Agent)) {
	t.Helper()
	l := map[string]string{labelReviewed: reviewedSHA, labelParked: ago(time.Hour)}
	if approved {
		l[labelHeldApproved] = ago(30 * time.Minute)
	}
	for k, v := range extra {
		l[k] = v
	}
	c := heldCard(l)
	c.Status = "closed"
	c.ArchivedAt = t0.Add(-time.Hour)
	// on disk, as Paseo keeps it: the release re-reads the card's labels from its file
	f, d := newDiskFactory(t, c)
	d.archived["c1"] = true
	return f, d.fakePaseo, func(dep Agent) {
		writeAgents(t, f, dep)
		f.SetAgents([]Agent{c, dep})
	}
}

func TestApprovedParkedHeldCardIsReleasedWhenDependencyLands(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("landed", false))
	headIs(f, reviewedSHA, nil)
	f.Tick()
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") || !strings.Contains(msgs[0], "d0e0001") {
		t.Fatalf("sent = %v", msgs)
	}
	if l := p.labels("c1"); l[labelHeld] != "" || l[labelHeldApproved] != "" || l[labelParked] != "" {
		t.Errorf("labels = %v", l)
	}
}

func TestUnapprovedParkedHeldCardIsNeverWoken(t *testing.T) {
	f, p, with := parkedHeld(t, false, nil)
	with(dep("landed", false))
	headIs(f, reviewedSHA, nil)
	f.Tick()
	if len(p.sent("c1")) != 0 || p.labels("c1")[labelStage] != "" {
		t.Errorf("woken: sent %v labels %v", p.sent("c1"), p.labels("c1"))
	}
}

func TestApprovedParkedHeldCardWaitsWhileDependencyInFlight(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("building", false))
	headIs(f, reviewedSHA, nil)
	f.Tick()
	if len(p.sent("c1")) != 0 || len(p.labels("c1")) != 0 {
		t.Errorf("moved: sent %v labels %v", p.sent("c1"), p.labels("c1"))
	}
}

func TestApprovedParkedHeldCardDroppedDependencyAsksZach(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("building", true))
	headIs(f, reviewedSHA, nil)
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || !isDepAskNote(l[labelNote]) || l[labelHeld] != "" || l[labelHeldApproved] != "" {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("sent = %v", p.sent("c1"))
	}
}

// A parked card cannot wait for a fresh review in a working column while archived: it stops for
// Zach with Re-run the review instead, and nothing is sent.
func TestApprovedParkedHeldCardWithMovedHeadStopsForReview(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("landed", false))
	headIs(f, movedSHA, nil)
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelStuckFrom] != "review" || !strings.Contains(l[labelNote], "changed since review") ||
		l[labelAutoReview] != "" || l[labelHeld] != "" || l[labelHeldApproved] != "" {
		t.Errorf("labels = %v", l)
	}
	noLandPrompt(t, "moved", p)
	if len(p.sent("c1")) != 0 {
		t.Errorf("woken: %v", p.sent("c1"))
	}
}

func TestApprovedParkedHeldCardGitFailureFailsClosed(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("landed", false))
	headIs(f, "", errors.New("git broke"))
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], unverifiedReviewNote) || l[labelHeldApproved] != "" {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("woken: %v", p.sent("c1"))
	}
}

func TestApprovedParkedHeldPublicCardScanHitStops(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("landed", false))
	c := f.agent("c1")
	c.Repo = publicRepo
	f.SetAgents([]Agent{c, dep("landed", false)})
	ranges := fakeScan(f, []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, nil)
	headIs(f, reviewedSHA, nil)
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan failed") || l[labelHeldApproved] != "" {
		t.Errorf("labels = %v", l)
	}
	if len(*ranges) != 1 || len(p.sent("c1")) != 0 {
		t.Errorf("scans %v sent %v", *ranges, p.sent("c1"))
	}
}

// Zach's approval of a parked card whose dependency is still in flight records that he approved it.
func TestApprovingAParkedCardHeldOnADependencyRecordsTheApproval(t *testing.T) {
	c := card("c1", approvedLabels(map[string]string{labelWaitsOn: "d0e0001", labelParked: ago(time.Hour)}))
	c.Status = "closed"
	c.ArchivedAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c, dep("building", false))
	p.archived["c1"] = true
	headIs(f, reviewedSHA, nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "landing" || l[labelHeld] != "d0e0001" || l[labelHeldApproved] == "" {
		t.Errorf("labels = %v", l)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("woken before its dependency landed: %v", p.sent("c1"))
	}
}

// Zach's held approval covers one release: Done, Close, Archive and a manual move cancel it, so a
// later hold or park can never release on an old approval.
func TestCancellingAnApprovedHeldCardClearsTheApproval(t *testing.T) {
	for _, tc := range []struct {
		action string
		body   Body
	}{
		{"done", Body{}},
		{"close", Body{}},
		{"archive", Body{}},
		{"stage", Body{Stage: "zach"}},
		{"stage", Body{Stage: "landed"}},
	} {
		f, p, with := parkedHeld(t, true, nil)
		with(dep("building", false))
		headIs(f, reviewedSHA, nil)
		tc.body.ID = "c1"
		if _, err := f.Act(tc.action, tc.body); err != nil {
			t.Fatalf("%s %s: %v", tc.action, tc.body.Stage, err)
		}
		if l := p.labels("c1"); l[labelHeldApproved] != "" || !labelSent(l, labelHeldApproved) {
			t.Errorf("%s %s: approval kept: %v", tc.action, tc.body.Stage, l)
		}
		if len(p.sent("c1")) != 0 {
			t.Errorf("%s %s: woken: %v", tc.action, tc.body.Stage, p.sent("c1"))
		}
	}
}

// An approve that lands at once (its dependency already landed), or that a stale HEAD sends back to
// review, uses up any earlier held approval.
func TestApproveConsumesAnEarlierHeldApproval(t *testing.T) {
	for name, head := range map[string]string{"lands": reviewedSHA, "stale": movedSHA} {
		c := card("c1", approvedLabels(map[string]string{labelWaitsOn: "d0e0001", labelHeldApproved: ago(time.Hour)}))
		f, p := newTestFactory(t, c, dep("landed", false))
		headIs(f, head, nil)
		if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); !labelSent(l, labelHeldApproved) || l[labelHeldApproved] != "" {
			t.Errorf("%s: approval kept: %v", name, l)
		}
	}
}

// labelSent reports whether k was written (possibly cleared) in the merged labels m.
func labelSent(m map[string]string, k string) bool { _, ok := m[k]; return ok }

// A cancellation after the tick's snapshot leaves the card archived, so the stale snapshot still
// looks like an approved parked hold. The release re-reads the card's current state first and
// does nothing: no message, no label write.
func TestStaleApprovedParkedSnapshotIsNotReleasedAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		action string
		body   Body
	}{
		{"close", Body{}},
		{"done", Body{}},
		{"archive", Body{}},
		{"stage", Body{Stage: "zach"}},
		{"stage", Body{Stage: "landed"}},
	} {
		name := tc.action + " " + tc.body.Stage
		f, p, with := parkedHeld(t, true, nil)
		with(dep("landed", false))
		headIs(f, reviewedSHA, nil)
		stale := f.agent("c1")
		oldCard, ok := f.card("c1")
		if !ok || !heldApprovedParked(stale) {
			t.Fatalf("%s: setup: %+v", name, stale.Labels)
		}
		tc.body.ID = "c1"
		if _, err := f.Act(tc.action, tc.body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		before := len(p.labelUpdates("c1"))
		f.tickHeld(stale, oldCard)
		if msgs := p.sent("c1"); len(msgs) != 0 {
			t.Errorf("%s: stale snapshot woke the card: %v", name, msgs)
		}
		if after := p.labelUpdates("c1"); len(after) != before {
			t.Errorf("%s: stale snapshot moved the card: %v", name, after[before:])
		}
	}
}

// The fresh check keeps a failed inspection a retryable error that moves nothing.
func TestApprovedParkedHeldReleaseInspectionFailureMovesNothing(t *testing.T) {
	f, p, with := parkedHeld(t, true, nil)
	with(dep("landed", false))
	headIs(f, reviewedSHA, nil)
	p.inspectErr = errors.New("paseo down")
	c, _ := f.card("c1")
	if err := f.tickHeld(f.agent("c1"), c); err == nil {
		t.Error("no error")
	}
	if len(p.sent("c1")) != 0 || len(p.labelUpdates("c1")) != 0 {
		t.Errorf("moved: sent %v labels %v", p.sent("c1"), p.labelUpdates("c1"))
	}
}

// labelUpdates lists the `agent update` calls for id.
func (p *fakePaseo) labelUpdates(id string) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out [][]string
	for _, c := range p.calls {
		if len(c) >= 3 && c[0] == "agent" && c[1] == "update" && c[2] == id {
			out = append(out, c)
		}
	}
	return out
}
