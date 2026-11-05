package factory

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestParseWaitsOn(t *testing.T) {
	for _, tc := range []struct {
		text       string
		wantID     string
		wantBefore bool
	}{
		{"plan\nWaits on: abc1234\nTIER: low", "abc1234", false},
		{"- **Waits on:** `abc1234` (lands first)", "abc1234", false},
		{"waits on: ABC1234", "abc1234", false},
		{"Waits on: abc1234 before build", "abc1234", true},
		{"Waits on: abc1234 before-build", "abc1234", true},
		{"- **Waits on:** `abc1234` **before build**", "abc1234", true},
		{"Waits on: abc1234 (before build)", "abc1234", true},
		{"Waits on: abc1234 — before build", "abc1234", true},
		{"Waits on: abc1234, before build", "abc1234", true},
		{"Waits on: abc1234 before building", "abc1234", true},
		{"Waits on: abc1234 before the build", "abc1234", true},
		{"Waits on: abc1234 before build starts", "abc1234", true},
		{"Waits on: abc1234 to land before build", "abc1234", false},
		{"Waits on: abc1234\nBefore build, run the tests.", "abc1234", false},
		{"Waits on: abc1234\nlater\nWaits on: def5678", "def5678", false},
		{"Waits on: abc1234 before build\nlater\nWaits on: def5678", "def5678", false},
		{"Waits on: abc1234\nlater\nWaits on: def5678 before build", "def5678", true},
		{"Waits on: c1c1c1c (this card)", "", false},
		{"Waits on: c1c1c1c before build", "", false},
		{"Ordering: abc1234 lands first", "", false},
		{"It waits on: abc1234 in prose", "", false},
		{"Waits on: abc12345678", "", false},
		{"no line", "", false},
	} {
		if gotID, gotBefore := ParseWaitsOnSpec(tc.text, "c1c1c1c9999"); gotID != tc.wantID || gotBefore != tc.wantBefore {
			t.Errorf("ParseWaitsOnSpec(%q) = (%q, %v), want (%q, %v)", tc.text, gotID, gotBefore, tc.wantID, tc.wantBefore)
		}
	}
}

func TestPlanApprovalRecordsWaitsOn(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[User] write a plan\nThe plan.\nWaits on: d0e0001\nTIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelWaitsOn] != "d0e0001" || l[labelWaitsBefore] != "" || l[labelStage] != "building" {
		t.Errorf("labels = %v", l)
	}
}

func TestRevisedPlanWithoutWaitsOnClearsIt(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board",
		labelWaitsOn: "d0e0001", labelWaitsDone: "1", labelWaitsBefore: "build"}), reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[User] revise\nThe revised plan, no dependency.\nTIER: low")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelWaitsOn] != "" || l[labelWaitsDone] != "" || l[labelWaitsBefore] != "" {
		t.Errorf("labels = %v", l)
	}
}

// dep is a card named in a Waits on line, in the given state.
func dep(stage string, archived bool) Agent {
	d := card("d0e0001aaaa", map[string]string{labelStage: stage})
	if archived {
		d.ArchivedAt = t0.Add(-time.Minute)
	}
	return d
}

func heldCard(extra map[string]string) Agent {
	l := map[string]string{labelStage: "landing", labelTier: "low", labelPlanRounds: "1", labelWaitsOn: "d0e0001",
		labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)}
	for k, v := range extra {
		l[k] = v
	}
	return card("c1", l)
}

func TestCodeApprovalHoldsWhileDependencyIsInFlight(t *testing.T) {
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001", labelReviewed: reviewedSHA}), reviewer("r1", "c1", "code"), dep("building", false))
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "landing" || l[labelHeld] != "d0e0001" || l[labelNote] != "waits on d0e0001 to land" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("held card was messaged: %v", msgs)
	}
}

func TestCodeApprovalLandsWhenDependencyLanded(t *testing.T) {
	for name, d := range map[string]Agent{"landed": dep("landed", true), "marked": {}} {
		labels := map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelWaitsOn: "d0e0001",
			labelReviewed: reviewedSHA}
		agents := []Agent{reviewer("r1", "c1", "code")}
		if name == "marked" {
			labels[labelWaitsDone] = "1" // landed long ago: no longer in the list
		} else {
			agents = append(agents, d)
		}
		f, p := newTestFactory(t, append(agents, idea("c1", labels))...)
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
		f.ApplyVerdict(f.agent("r1"))
		if l := p.labels("c1"); l[labelStage] != "landing" || l[labelHeld] != "" {
			t.Errorf("%s: labels = %v", name, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
			t.Errorf("%s: sent = %v", name, msgs)
		}
	}
}

func TestHeldCardWithUIFeedbackIsToldToWait(t *testing.T) {
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001", labelReviewed: reviewedSHA}), reviewer("r1", "c1", "code"), dep("review", false))
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "Fine.\n\nUI feedback:\n- the button is small\n\nALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	msgs := p.sent("c1")
	if len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") || !strings.Contains(msgs[0], "the button is small") ||
		!strings.Contains(msgs[0], "d0e0001") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestHeldCardWaitsWhileDependencyInFlight(t *testing.T) {
	// it got the feedback message, replied and stopped: still nothing to do, not a landing to verify
	c := heldCard(nil)
	c.LastActivityAt = t0.Add(-5 * time.Minute)
	f, p := newTestFactory(t, c, dep("review", false))
	gitCalls := 0
	f.Git = func(_ context.Context, _ string, _ ...string) error { gitCalls++; return nil }
	f.Tick()
	if l := p.labels("c1"); len(l) != 0 {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 || gitCalls != 0 {
		t.Errorf("sent = %v, git calls = %d", msgs, gitCalls)
	}
}

func TestHeldCardIsReleasedOnceDependencyLands(t *testing.T) {
	verified := dep("zach", true)
	verified.Labels[labelPrefix+labelLandedVerified] = "1"
	parked := dep("zach", true)
	parked.Labels[labelPrefix+labelParked] = iso(t0)
	parked.Labels[labelPrefix+labelLandedVerified] = "1"
	for name, d := range map[string]Agent{"done": dep("landed", false), "verified closed": verified, "verified parked": parked} {
		t.Run(name, func(t *testing.T) {
			f, p := newTestFactory(t, heldCard(map[string]string{labelReviewed: reviewedSHA}), d)
			headIs(f, reviewedSHA, nil)
			f.Tick()
			l := p.labels("c1")
			if l[labelHeld] != "" || l[labelStage] != "" || l[labelSince] != iso(t0) || l[labelWaitsDone] != "1" {
				t.Errorf("labels = %v", l)
			}
			msgs := p.sent("c1")
			if len(msgs) != 1 || !strings.Contains(msgs[0], "d0e0001") || !strings.Contains(msgs[0], "Land it now") ||
				!strings.Contains(msgs[0], "REMAINING:") {
				t.Fatalf("sent = %v", msgs)
			}
			// the next Tick sees the released card (since = now, not acted yet): nothing more is sent
			released := heldCard(map[string]string{labelHeld: "", labelSince: iso(t0), labelWaitsDone: "1",
				labelReviewed: reviewedSHA})
			f.SetAgents([]Agent{released, d})
			f.Tick()
			if msgs := p.sent("c1"); len(msgs) != 1 {
				t.Errorf("sent again: %v", msgs)
			}
		})
	}
}

func TestHeldCardRebuildsWhenDependencyDroppedOrMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		agents []Agent
		state  string
		says   string
	}{
		"dropped": {[]Agent{dep("building", true)}, depDropped, "was dropped"},
		"closed":  {[]Agent{dep("zach", true)}, depDropped, "was dropped"},
		"missing": {nil, depMissing, "is not on the board"},
	} {
		f, p, m := mailFactory(t, append(tc.agents, heldCard(nil))...)
		f.Tick()
		l := p.labels("c1")
		if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelHeld] != "" || l[labelWaitsOn] != "" ||
			l[labelNote] != "rebuilding: d0e0001 "+tc.state {
			t.Errorf("%s: labels = %v", name, l)
		}
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], tc.says) || !strings.Contains(msgs[0], "rebuild without it") ||
			!strings.Contains(msgs[0], "Code review runs again") {
			t.Errorf("%s: sent = %v", name, msgs)
		}
		if len(m.sends()) != 0 {
			t.Errorf("%s: emailed Zach: %v", name, m.sends())
		}
		// once rebuilt and reviewed, the land prompt is no longer held
		a := withLabels(heldCard(nil), l)
		if id, _ := f.pendingDep(a); id != "" {
			t.Errorf("%s: still waits on %s", name, id)
		}
	}
}

func TestHeldCardIsNotStuck(t *testing.T) {
	c := heldCard(map[string]string{labelSince: ago(time.Hour)})
	c.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c, dep("building", false))
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "" {
		t.Errorf("labels = %v", l)
	}
}

func TestZachApproveHoldsWhileDependencyInFlight(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land",
		labelWaitsOn: "d0e0001", labelReviewed: reviewedSHA}), dep("building", false))
	headIs(f, reviewedSHA, nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelHeld] != "d0e0001" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Errorf("sent = %v", msgs)
	}
}

func TestGuardedReleaseScansAgain(t *testing.T) {
	c := heldCard(map[string]string{labelReviewed: reviewedSHA})
	c.Repo = publicRepo
	f, p := newTestFactory(t, c, dep("landed", false))
	ranges := fakeScan(f, []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, nil)
	headIs(f, reviewedSHA, nil)
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan failed") || l[labelHeld] != "" {
		t.Errorf("labels = %v", l)
	}
	if len(*ranges) != 1 || len(p.sent("c1")) != 0 {
		t.Errorf("scans = %v, sent = %v", *ranges, p.sent("c1"))
	}
}

func TestBoardShowsWaitsOn(t *testing.T) {
	for state, d := range map[string]*Agent{"": ptr(dep("building", false)), " (landed)": ptr(dep("landed", true)),
		" (dropped)": ptr(dep("building", true)), " (not found)": nil} {
		agents := []Agent{card("c1", map[string]string{labelStage: "building", labelWaitsOn: "d0e0001"})}
		if d != nil {
			agents = append(agents, *d)
		}
		var c Card
		for _, x := range BuildBoard(agents, nil, t0) {
			if x.ID == "c1" {
				c = x
			}
		}
		if c.WaitsOn != "d0e0001" || !contains(c.Flags, "waits on d0e0001"+state) {
			t.Errorf("%q: waitsOn %q, flags %v", state, c.WaitsOn, c.Flags)
		}
	}
	for state, d := range map[string]*Agent{" before build": ptr(dep("building", false)), " (landed)": ptr(dep("landed", true))} {
		agents := []Agent{card("c1", map[string]string{labelStage: "building", labelWaitsOn: "d0e0001", labelWaitsBefore: "build"})}
		if d != nil {
			agents = append(agents, *d)
		}
		var c Card
		for _, x := range BuildBoard(agents, nil, t0) {
			if x.ID == "c1" {
				c = x
			}
		}
		if c.WaitsOn != "d0e0001" || !contains(c.Flags, "waits on d0e0001"+state) {
			t.Errorf("before-build %q: waitsOn %q, flags %v", state, c.WaitsOn, c.Flags)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A verified landing remains a satisfied dependency after follow-up work or closure.
func TestVerifiedLandingDependency(t *testing.T) {
	for _, archived := range []bool{false, true} {
		d := dep("zach", archived)
		d.Labels[labelPrefix+labelLandedVerified] = "1"
		d.Labels[labelPrefix+labelNote] = "closed by Zach"
		if got := depOf([]Agent{d}, "d0e0001"); got != depLanded {
			t.Errorf("archived=%v: depOf = %q", archived, got)
		}
		if got := depState([]Agent{d}, heldCard(nil), "d0e0001"); got != depLanded {
			t.Errorf("archived=%v: depState = %q", archived, got)
		}
	}
}

// legacyDepNote is the Needs Zach note a dependency wait had before cards decided for themselves.
func legacyDepNote(missing bool) string {
	if missing {
		return "waits on d0e0001: no such card found"
	}
	return "waits on d0e0001: it was dropped"
}

func TestOldDependencyWaitReleasesAfterVerifiedLanding(t *testing.T) {
	for _, note := range []string{legacyDepNote(false), legacyDepNote(true)} {
		t.Run(note, func(t *testing.T) {
			d := dep("zach", true)
			d.Labels[labelPrefix+labelLandedVerified] = "1"
			c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: note, labelTold: note,
				labelReviewed: reviewedSHA})
			f, p := newTestFactory(t, c, d)
			headIs(f, reviewedSHA, nil)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != "landing" || l[labelHeld] != "" || l[labelTold] != "" || l[labelWaitsDone] != "1" || l[labelSince] != iso(t0) || !strings.HasPrefix(l[labelNote], "auto-landing:") {
				t.Errorf("labels = %v", l)
			}
			if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
				t.Fatalf("sent = %v", msgs)
			}
			c.Labels[labelPrefix+labelStage] = "landing"
			c.Labels[labelPrefix+labelNote] = l[labelNote]
			c.Labels[labelPrefix+labelSince] = iso(t0)
			c.Labels[labelPrefix+labelWaitsDone] = "1"
			f.SetAgents([]Agent{c, d})
			f.Tick()
			if len(p.sent("c1")) != 1 {
				t.Error("sent another land prompt")
			}
		})
	}
}

func TestOldDependencyWaitMovesToRecovery(t *testing.T) {
	for _, name := range []string{"in flight again", "dropped", "unrelated ask", "running", "closed", "parked", "running on inspect"} {
		t.Run(name, func(t *testing.T) {
			d := dep("building", false)
			c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: legacyDepNote(false)})
			switch name {
			case "dropped":
				d.ArchivedAt = t0.Add(-time.Hour)
			case "unrelated ask":
				c.Labels[labelPrefix+labelNote] = "review passed: approve to land"
			case "running":
				c.Status = "running"
			case "closed", "parked":
				c.ArchivedAt = t0.Add(-time.Minute)
				if name == "parked" {
					c.Labels[labelPrefix+labelParked] = iso(t0)
				}
			}
			f, p := newTestFactory(t, c, d)
			if name == "running on inspect" {
				p.status["c1"] = "running"
			}
			f.Tick()
			l := p.labels("c1")
			switch name {
			case "in flight again":
				if l[labelStage] != "landing" || l[labelHeld] != "d0e0001" || len(p.sent("c1")) != 0 {
					t.Errorf("not held again: %v %v", l, p.sent("c1"))
				}
			case "dropped":
				if l[labelStage] != "building" || len(p.sent("c1")) != 1 {
					t.Errorf("not rebuilt: %v %v", l, p.sent("c1"))
				}
			default:
				if l[labelStage] != "" || l[labelHeld] != "" || len(p.sent("c1")) != 0 {
					t.Errorf("moved blocked card: %v %v", l, p.sent("c1"))
				}
			}
		})
	}
}

func TestGuardedDependencyAskRecoveryScansAgain(t *testing.T) {
	for _, clean := range []bool{false, true} {
		c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: legacyDepNote(false),
			labelReviewed: reviewedSHA})
		c.Repo = publicRepo
		d := dep("zach", true)
		d.Labels[labelPrefix+labelLandedVerified] = "1"
		f, p := newTestFactory(t, c, d)
		var hits []scan.Hit
		if !clean {
			hits = []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}
		}
		ranges := fakeScan(f, hits, nil)
		headIs(f, reviewedSHA, nil)
		f.Tick()
		if len(*ranges) != 1 {
			t.Errorf("scans = %v", *ranges)
		}
		if clean {
			if len(p.sent("c1")) != 1 {
				t.Errorf("sent = %v", p.sent("c1"))
			}
		} else if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan failed") || len(p.sent("c1")) != 0 {
			t.Errorf("failed scan released card: labels %v, sent %v", l, p.sent("c1"))
		}
	}
}

func TestFailedLandingDoesNotRecordVerifiedDependency(t *testing.T) {
	for _, changed := range []bool{false, true} {
		c := dep("landing", false)
		c.Cwd = t.TempDir()
		c.Labels[labelPrefix+labelSince] = ago(10 * time.Minute)
		if changed {
			c.Labels[labelPrefix+labelReviewed] = "h1"
		}
		f, p := newTestFactory(t, c)
		f.Git = func(context.Context, string, ...string) error {
			if !changed {
				return errors.New("not on main")
			}
			return nil
		}
		f.GitOut = func(context.Context, string, ...string) (string, error) { return "", errors.New("review not verified") }
		storeLog(t, f, p, c.ID, "REMAINING: none")
		f.Tick()
		l := p.labels(c.ID)
		if l[labelLandedVerified] != "" || l[labelStage] != "building" {
			t.Errorf("failed verification recorded landing: %v", l)
		}
		for k, v := range l {
			c.Labels[labelPrefix+k] = v
		}
		if got := depOf([]Agent{c}, short(c.ID)); got != depWaiting {
			t.Errorf("dependency state = %q", got)
		}
	}
}

func TestPlanApprovalWithWaitsBeforeBuildHoldsWhileDependencyInFlight(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"), dep("building", false))
	storeLog(t, f, p, "r1", "Check edge case X in foo.go.\nSMALL FIX: guard nil pointer\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: changes")
	storeLog(t, f, p, "c1", "[User] write a plan\nThe plan.\nWaits on: d0e0001 before build\nBUILD SIDE EFFECTS: none\nTIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelWaitsOn] != "d0e0001" || l[labelWaitsBefore] != "build" ||
		l[labelHeld] != "d0e0001" || l[labelAutoReview] != "" || l[labelNote] != "waits on d0e0001 before build" {
		t.Fatalf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 0 {
		t.Fatalf("held build was messaged: %v", msgs)
	}
	if _, err := os.Stat(f.heldBuildPath("c1")); err != nil {
		t.Fatalf("held build file missing while held: %v", err)
	}
	// While held in building for an hour, it is not nudged or marked stuck.
	held := withLabels(f.agent("c1"), l)
	held.Labels[labelPrefix+labelSince] = ago(time.Hour)
	held.LastActivityAt = t0.Add(-time.Hour)
	f.SetAgents([]Agent{held, dep("building", false)})
	f.Tick()
	if len(p.sent("c1")) != 0 || p.labels("c1")[labelStuck] != "" {
		t.Fatalf("held before-build card was nudged or marked stuck: sent=%v labels=%v", p.sent("c1"), p.labels("c1"))
	}
	// Once the blocker lands, the deferred build message (including full reviewer notes and small fixes) is sent.
	f.SetAgents([]Agent{held, dep("landed", true)})
	f.Tick()
	rl := p.labels("c1")
	if rl[labelStage] != "building" || rl[labelHeld] != "" || rl[labelAutoReview] != "code" || rl[labelWaitsDone] != "1" ||
		rl[labelNote] != "building: d0e0001 landed" {
		t.Fatalf("release labels = %v", rl)
	}
	if _, err := os.Stat(f.heldBuildPath("c1")); !os.IsNotExist(err) {
		t.Fatalf("held build file not removed after release: %v", err)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Card d0e0001, which this card waited on before building, has landed") ||
		!strings.Contains(msgs[0], "git fetch origin main && git rebase origin/main") ||
		!strings.Contains(msgs[0], "The plan review found only small, concrete blocking fixes") ||
		!strings.Contains(msgs[0], "Check edge case X in foo.go.") || !strings.Contains(msgs[0], "guard nil pointer") {
		t.Fatalf("release sent = %v", msgs)
	}
}

func TestPlanApprovalWithWaitsBeforeBuildStartsWhenDependencyAlreadyLanded(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"), dep("landed", true))
	storeLog(t, f, p, "r1", "Looks good.\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[User] write a plan\nThe plan.\nWaits on: d0e0001 before build\nBUILD SIDE EFFECTS: none\nTIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelHeld] != "" || l[labelAutoReview] != "code" || l[labelWaitsDone] != "1" {
		t.Fatalf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Build it now, as planned.") {
		t.Fatalf("sent = %v", msgs)
	}
}

func TestPlanApprovalWithWaitsBeforeBuildAndSideEffectsWaitsForZachThenHolds(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"), dep("building", false))
	storeLog(t, f, p, "r1", "SMALL FIX: add timeout\nBUILD SIDE EFFECTS: sends test email\nTIER: low\nVERDICT: changes")
	storeLog(t, f, p, "c1", "[User] write a plan\nThe plan.\nWaits on: d0e0001 before build\nBUILD SIDE EFFECTS: sends test email\nTIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelAwaiting] != "plan" || l[labelWaitsOn] != "d0e0001" ||
		l[labelWaitsBefore] != "build" || l[labelPlanFix] != "add timeout" {
		t.Fatalf("side effect stop labels = %v", l)
	}
	if len(p.sent("c1")) != 1 {
		t.Fatalf("expected side effect stop prompt, got %v", p.sent("c1"))
	}
	stopped := withLabels(f.agent("c1"), l)
	f.SetAgents([]Agent{stopped, dep("building", false)})
	if _, err := f.Act("approve", Body{ID: "c1", Message: "1A: use staging"}); err != nil {
		t.Fatal(err)
	}
	al := p.labels("c1")
	if al[labelStage] != "building" || al[labelHeld] != "d0e0001" || al[labelAutoReview] != "" ||
		al[labelZachPlan] != "approved" || al[labelHeldApproved] == "" || al[labelNote] != "waits on d0e0001 before build" {
		t.Fatalf("approve labels = %v", al)
	}
	if al[labelPlanFix] != "add timeout" {
		t.Fatalf("approve overwrote labelPlanFix: %q", al[labelPlanFix])
	}
	if len(p.sent("c1")) != 1 {
		t.Fatalf("approve messaged held build before blocker landed: %v", p.sent("c1"))
	}
	held := withLabels(stopped, al)
	f.SetAgents([]Agent{held, dep("landed", true)})
	f.Tick()
	rl := p.labels("c1")
	if rl[labelHeld] != "" || rl[labelHeldApproved] != "" || !labelSent(rl, labelHeldApproved) ||
		rl[labelAutoReview] != "code" || rl[labelWaitsDone] != "1" {
		t.Fatalf("release labels = %v", rl)
	}
	msgs := p.sent("c1")
	if len(msgs) != 2 || !strings.Contains(msgs[1], "Zach allowed this build.") ||
		!strings.Contains(msgs[1], "1A: use staging") || !strings.Contains(msgs[1], "add timeout") {
		t.Fatalf("release sent = %v", msgs)
	}
}

func TestHeldBeforeBuildDroppedOrMissingDependencyUsesBeforeBuildWording(t *testing.T) {
	for name, tc := range map[string]struct {
		agents []Agent
		state  string
		says   string
	}{
		"dropped": {[]Agent{dep("building", true)}, depDropped, "was dropped"},
		"missing": {nil, depMissing, "is not on the board"},
	} {
		c := card("c1", map[string]string{labelStage: "building", labelTier: "low", labelPlanRounds: "1",
			labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)})
		f, p, m := mailFactory(t, append(tc.agents, c)...)
		if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "Check X.")); err != nil {
			t.Fatal(err)
		}
		f.Tick()
		l := p.labels("c1")
		if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelHeld] != "" ||
			l[labelWaitsOn] != "" || l[labelWaitsBefore] != "" || l[labelNote] != "rebuilding: d0e0001 "+tc.state {
			t.Errorf("%s: labels = %v", name, l)
		}
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], tc.says) ||
			!strings.Contains(msgs[0], "which your approved plan waited on before building") ||
			!strings.Contains(msgs[0], "Nothing is built yet") || !strings.Contains(msgs[0], "Check X.") ||
			strings.Contains(msgs[0], "say so and change nothing") || strings.Contains(msgs[0], "Code review runs again") {
			t.Errorf("%s: sent = %v", name, msgs)
		}
		if len(m.sends()) != 0 {
			t.Errorf("%s: emailed Zach: %v", name, m.sends())
		}
	}
}

func TestHeldBeforeBuildKeepHoldOnZachReply(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c, dep("building", false))
	if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "(none)")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("send", Body{ID: "c1", Message: "keep it small"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	answered := withLabels(c, l)
	if l[labelStage] != "building" || answered.L(labelHeld) != "d0e0001" || l[labelAutoReview] != "" {
		t.Fatalf("after send labels = %v (held=%q)", l, answered.L(labelHeld))
	}
	// Agent answered Zach and stopped while blocker is still in flight: stays held, no code review starts.
	answered.LastActivityAt = t0.Add(time.Minute)
	f.Now = func() time.Time { return t0.Add(2 * time.Minute) }
	f.SetAgents([]Agent{answered, dep("building", false)})
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelRole, "reviewer"))) != 0 || len(p.sent("c1")) != 1 {
		t.Fatalf("started review or sent message while still held: calls=%v", p.calls)
	}
	// Once blocker lands, build starts.
	f.SetAgents([]Agent{answered, dep("landed", true)})
	f.Tick()
	if rl := p.labels("c1"); rl[labelHeld] != "" || rl[labelAutoReview] != "code" || len(p.sent("c1")) != 2 {
		t.Fatalf("after blocker landed: labels=%v sent=%v", rl, p.sent("c1"))
	}
}

func TestLandingHeldCardMovedByHandToBuildingIsNotTreatedAsBeforeBuildHold(t *testing.T) {
	c := heldCard(map[string]string{labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c, dep("building", false))
	if _, err := f.Act("stage", Body{ID: "c1", Stage: "building"}); err != nil {
		t.Fatal(err)
	}
	moved := withLabels(c, p.labels("c1"))
	if heldBeforeBuild(moved) {
		t.Fatalf("hand-moved landing card treated as heldBeforeBuild: %v", moved.Labels)
	}
	f.SetAgents([]Agent{moved, dep("landed", true)})
	f.Tick()
	if len(p.sent("c1")) != 0 {
		t.Fatalf("hand-moved landing card released as before-build hold: %v", p.sent("c1"))
	}
}

func TestArbiterAndSettledPlanStopsKeepBeforeBuildHold(t *testing.T) {
	plan := "The plan.\nWaits on: d0e0001 before build\nBUILD SIDE EFFECTS: none\nTIER: low"
	for name, stop := range map[string]func(*testing.T, *Factory, *fakePaseo){
		"arbiter escalate": func(t *testing.T, f *Factory, _ *fakePaseo) {
			if err := f.applyArbiter(f.agent("c1"), "plan", "escalate", "Zach should decide.", "low", plan, plan, map[string]string{}); err != nil {
				t.Fatal(err)
			}
		},
		"arbiter changes without fixes": func(t *testing.T, f *Factory, _ *fakePaseo) {
			if err := f.applyArbiter(f.agent("c1"), "plan", "changes", "Still needs rethinking.", "low", plan, plan, map[string]string{}); err != nil {
				t.Fatal(err)
			}
		},
		"arbitrated plan review stop": func(t *testing.T, f *Factory, p *fakePaseo) {
			c := withLabels(f.agent("c1"), map[string]string{labelPlanRounds: "3", labelArbitrated: "plan"})
			r := reviewer("r1", "c1", "plan")
			f.SetAgents([]Agent{c, r, dep("building", false)})
			storeLog(t, f, p, "r1", "Still disagree.\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: changes")
			if err := f.ApplyVerdict(f.agent("r1")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}), dep("building", false))
			storeLog(t, f, p, "c1", "[User] write a plan\n"+plan)
			stop(t, f, p)
			l := p.labels("c1")
			if l[labelStage] != "zach" || l[labelWaitsOn] != "d0e0001" || l[labelWaitsBefore] != "build" {
				t.Fatalf("after stop: labels = %v", l)
			}
			f.SetAgents([]Agent{withLabels(f.agent("c1"), l), dep("building", false)})
			before := len(p.sent("c1"))
			if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
				t.Fatal(err)
			}
			al := p.labels("c1")
			if al[labelStage] != "building" || al[labelHeld] != "d0e0001" || al[labelAutoReview] != "" || len(p.sent("c1")) != before {
				t.Fatalf("after approve: labels = %v sent = %d (want %d)", al, len(p.sent("c1")), before)
			}
		})
	}
}

func TestSteerKeepsBeforeBuildHold(t *testing.T) {
	f, p := dupFactory(t, "building", "idle", map[string]string{
		labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001",
		labelAutoReview: "", labelPlanRounds: "1", labelSince: ago(10 * time.Minute),
	})
	closeDup(t, f)
	f.tickSteers()
	l := p.labels(keepID)
	a := labeled(f.agent(keepID), l)
	if a.L(labelAutoReview) != "" || a.L(labelHeld) != "d0e0001" {
		t.Fatalf("hold broken after steer: autoreview=%q held=%q", a.L(labelAutoReview), a.L(labelHeld))
	}
	a.Status, a.LastActivityAt = "idle", t0.Add(time.Minute)
	f.Now = func() time.Time { return t0.Add(2 * time.Minute) }
	f.SetAgents([]Agent{a, dep("building", false)})
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelRole, "reviewer"))); n != 0 {
		t.Fatalf("started %d code review(s) on an unbuilt held card", n)
	}
}

type errSend struct{ *fakePaseo }

func (p errSend) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "send" {
		return "", errors.New("paseo unavailable")
	}
	return p.fakePaseo.Run(ctx, args...)
}

func TestFailedSendOnBeforeBuildReleaseAndDropRetriesAndMissingWorktreeStops(t *testing.T) {
	for name, blocker := range map[string]Agent{
		"release": dep("landed", true),
		"drop":    dep("building", true),
	} {
		t.Run(name+" retries after failed send", func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelTier: "low", labelPlanRounds: "1",
				labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)})
			f, p := newTestFactory(t, c, blocker)
			if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "MARKER-NOTES")); err != nil {
				t.Fatal(err)
			}
			f.p = errSend{p}
			f.Tick()
			if l := p.labels("c1"); (l[labelHeld] != "" && l[labelHeld] != "d0e0001") || l[labelAutoReview] == "code" {
				t.Fatalf("failed send cleared hold or armed review: %v", l)
			}
			if _, err := os.Stat(f.heldBuildPath("c1")); err != nil {
				t.Fatalf("failed send deleted held build file: %v", err)
			}
			f.p = p
			f.SetAgents([]Agent{labeled(c, p.labels("c1")), blocker})
			f.Tick()
			msgs := p.sent("c1")
			if len(msgs) != 1 || !strings.Contains(msgs[0], "MARKER-NOTES") {
				t.Fatalf("retry sent = %v", msgs)
			}
			if l := p.labels("c1"); l[labelHeld] != "" || l[labelAutoReview] != "code" {
				t.Fatalf("retry labels = %v", l)
			}
			if _, err := os.Stat(f.heldBuildPath("c1")); !os.IsNotExist(err) {
				t.Fatalf("held build file left after retry: %v", err)
			}
		})
		t.Run(name+" stops for Zach on missing worktree", func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelTier: "low", labelPlanRounds: "1",
				labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)})
			f, p := newTestFactory(t, c, blocker)
			if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "MARKER-NOTES")); err != nil {
				t.Fatal(err)
			}
			f.p = missingCwdSend{p}
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != "zach" || l[labelHeld] != "" || l[labelAutoReview] != "" {
				t.Fatalf("missing worktree labels = %v", l)
			}
			if _, err := os.Stat(f.heldBuildPath("c1")); !os.IsNotExist(err) {
				t.Fatalf("held build file left after worktree stop: %v", err)
			}
			stopped := labeled(c, l)
			if err := f.tickAgent(stopped, f.Now()); err != nil {
				t.Fatalf("next tickAgent after missing worktree stop: %v", err)
			}
		})
	}
}

func TestApproveBeforeBuildHoldNeedsWorktree(t *testing.T) {
	for _, st := range []string{"", "d0e0001"} {
		l := map[string]string{labelStage: "zach", labelAwaiting: "plan", labelSource: "board",
			labelVerdict: "plan side effects", labelParked: ago(time.Hour)}
		if st != "" {
			l[labelWaitsOn], l[labelWaitsBefore] = st, "build"
		}
		c := card("c1", l)
		c.Status, c.ArchivedAt, c.Cwd = "closed", t0.Add(-time.Hour), t.TempDir()+"/gone"
		f, p := newTestFactory(t, c, dep("building", false))
		_, err := f.Act("approve", Body{ID: "c1"})
		if !errors.Is(err, errWorktreeGone) {
			t.Errorf("waits-on=%q: err = %v; want %v", st, err, errWorktreeGone)
		}
		if len(p.labelUpdates("c1")) != 0 {
			t.Errorf("waits-on=%q: updated labels on missing worktree: %v", st, p.labels("c1"))
		}
	}
}

func TestDroppingHeldBuildCardRemovesSavedBuildPrompt(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001"})
	f, _ := newTestFactory(t, c, dep("building", false))
	if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "(none)")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("close", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.heldBuildPath("c1")); !os.IsNotExist(err) {
		t.Fatalf("held build file left after close: %v", err)
	}
}

func TestApproveBeforeBuildWhenBlockerAlreadyLandedStartsImmediately(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelSource: "board",
		labelVerdict: "plan side effects", labelWaitsOn: "d0e0001", labelWaitsBefore: "build"})
	f, p := newTestFactory(t, c, dep("landed", true))
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelHeld] != "" || l[labelAutoReview] != "code" || l[labelWaitsDone] != "1" {
		t.Fatalf("labels = %v", l)
	}
	if len(p.sent("c1")) != 1 {
		t.Fatalf("sent = %v", p.sent("c1"))
	}
}

func TestPlanSmallFixRuleExcludesWaitsOnBeforeBuild(t *testing.T) {
	p := ReviewPrompt(Card{ID: "c1", Title: "Card", Repo: "o/r"}, "plan", 7478, "unknown")
	if strings.Contains(p, "or Waits on: line") {
		t.Errorf("plan review prompt still lists Waits on: as a small fix:\n%s", p)
	}
	if !strings.Contains(p, "not a missing Waits on: <id> before build line") {
		t.Errorf("plan review prompt does not exclude Waits on: <id> before build from small fixes:\n%s", p)
	}
}

func TestBeforeBuildHandMovedToLandingUsesPostBuildDroppedWording(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c, dep("building", true))
	f.Tick()
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "which your approved plan lands after") ||
		strings.Contains(msgs[0], "Nothing is built yet") {
		t.Fatalf("hand-moved landing card got before-build dropped wording: %v", msgs)
	}
}

func TestBeforeBuildStageHandoffOnReleaseAndDrop(t *testing.T) {
	setup := func(t *testing.T, blocker Agent) (*Factory, *fakePaseo) {
		a := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAgent: "opus",
			labelTier: "low", labelBase: "base1", labelTitled: "given", labelPlanRounds: "1",
			labelWaitsOn: "d0e0001", labelWaitsBefore: "build", labelHeld: "d0e0001",
			labelVerdict: "plan approve", labelNote: "waits on d0e0001 before build", labelSince: ago(10 * time.Minute)})
		a.WorkspaceID = "ws-1"
		f, p := newTestFactory(t, a, blocker)
		storeLog(t, f, p, a.ID, planLog+"\nWaits on: d0e0001 before build")
		f.GitOut = gitStub(map[string]string{"log --oneline origin/main..HEAD": "", "diff --stat origin/main...HEAD": ""})
		if err := f.saveHeldBuild("c1", buildMsg("Plan approved (low tier).", "MARKER-NOTES")); err != nil {
			t.Fatal(err)
		}
		return f, p
	}

	t.Run("release carries waits-before to fresh builder", func(t *testing.T) {
		f, p := setup(t, dep("landed", true))
		f.Tick()
		runs := stageRuns(p)
		if len(runs) != 1 {
			t.Fatalf("runs = %v", runs)
		}
		args := strings.Join(runs[0], " ")
		for _, want := range []string{
			"--label " + labelPrefix + labelWaitsBefore + "=build",
			"--label " + labelPrefix + labelWaitsOn + "=d0e0001",
			"--label " + labelPrefix + labelWaitsDone + "=1",
			"--label " + labelPrefix + labelAutoReview + "=code",
			"MARKER-NOTES",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("fresh builder args missing %q:\n%s", want, args)
			}
		}
		if strings.Contains(args, "--label "+labelPrefix+labelHeld+"=") {
			t.Errorf("fresh builder carried held label:\n%s", args)
		}
	})

	t.Run("drop clears waits-before on fresh builder", func(t *testing.T) {
		f, p := setup(t, dep("building", true))
		f.Tick()
		runs := stageRuns(p)
		if len(runs) != 1 {
			t.Fatalf("runs = %v", runs)
		}
		args := strings.Join(runs[0], " ")
		for _, bad := range []string{labelHeld + "=", labelWaitsOn + "=", labelWaitsBefore + "="} {
			if strings.Contains(args, "--label "+labelPrefix+bad) {
				t.Errorf("fresh builder carried %s:\n%s", bad, args)
			}
		}
		if !strings.Contains(args, "Nothing is built yet") || !strings.Contains(args, "MARKER-NOTES") {
			t.Errorf("fresh builder prompt missing drop notes:\n%s", args)
		}
	})
}


