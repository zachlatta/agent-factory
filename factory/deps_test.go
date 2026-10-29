package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestParseWaitsOn(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"plan\nWaits on: abc1234\nTIER: low", "abc1234"},
		{"- **Waits on:** `abc1234` (lands first)", "abc1234"},
		{"waits on: ABC1234", "abc1234"},
		{"Waits on: abc1234\nlater\nWaits on: def5678", "def5678"},
		{"Waits on: c1c1c1c (this card)", ""},
		{"Ordering: abc1234 lands first", ""},
		{"It waits on: abc1234 in prose", ""},
		{"Waits on: abc12345678", ""},
		{"no line", ""},
	} {
		if got := ParseWaitsOn(tc.text, "c1c1c1c9999"); got != tc.want {
			t.Errorf("ParseWaitsOn(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestPlanApprovalRecordsWaitsOn(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[User] write a plan\nThe plan.\nWaits on: d0e0001\nTIER: low")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelWaitsOn] != "d0e0001" || l[labelStage] != "building" {
		t.Errorf("labels = %v", l)
	}
}

func TestRevisedPlanWithoutWaitsOnClearsIt(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board",
		labelWaitsOn: "d0e0001", labelWaitsDone: "1"}), reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[User] revise\nThe revised plan, no dependency.\nTIER: low")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelWaitsOn] != "" || l[labelWaitsDone] != "" {
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
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001"}), reviewer("r1", "c1", "code"), dep("building", false))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
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
		labels := map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelWaitsOn: "d0e0001"}
		agents := []Agent{reviewer("r1", "c1", "code")}
		if name == "marked" {
			labels[labelWaitsDone] = "1" // landed long ago: no longer in the list
		} else {
			agents = append(agents, d)
		}
		f, p := newTestFactory(t, append(agents, card("c1", labels))...)
		storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
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
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1",
		labelWaitsOn: "d0e0001"}), reviewer("r1", "c1", "code"), dep("review", false))
	storeLog(t, f, p, "r1", "Fine.\n\nUI feedback:\n- the button is small\n\nTIER: low\nVERDICT: approve")
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
			f, p := newTestFactory(t, heldCard(nil), d)
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
			released := heldCard(map[string]string{labelHeld: "", labelSince: iso(t0), labelWaitsDone: "1"})
			f.SetAgents([]Agent{released, d})
			f.Tick()
			if msgs := p.sent("c1"); len(msgs) != 1 {
				t.Errorf("sent again: %v", msgs)
			}
		})
	}
}

func TestHeldCardGoesToZachWhenDependencyDroppedOrMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		agents []Agent
		note   string
		title  string
	}{
		"dropped": {[]Agent{dep("building", true)}, "waits on d0e0001: it was dropped", "was dropped"},
		"closed":  {[]Agent{dep("zach", true)}, "waits on d0e0001: it was dropped", "was dropped"},
		"missing": {nil, "waits on d0e0001: no such card found", "find the card"},
	} {
		f, p := newTestFactory(t, append(tc.agents, heldCard(nil))...)
		f.Tick()
		l := p.labels("c1")
		if l[labelStage] != "zach" || l[labelHeld] != "" || l[labelNote] != tc.note {
			t.Errorf("%s: labels = %v", name, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 0 {
			t.Errorf("%s: sent = %v", name, msgs)
		}
		a := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: tc.note})
		ask := askFor(Card{Stage: "zach"}, a)
		if ask == nil || !strings.Contains(ask.Title, tc.title) || ask.Yes != "approve" || mailable(ask) {
			t.Fatalf("%s: ask = %+v", name, ask)
		}
		// Zach's Approve is the answer: it lands, with no second hold
		f.SetAgents(append(tc.agents, a))
		if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); l[labelStage] != "landing" || l[labelHeld] != "" {
			t.Errorf("%s: after approve labels = %v", name, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
			t.Errorf("%s: after approve sent = %v", name, msgs)
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
		labelWaitsOn: "d0e0001"}), dep("building", false))
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
	c := heldCard(nil)
	c.Repo = publicRepo
	f, p := newTestFactory(t, c, dep("landed", false))
	ranges := fakeScan(f, []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, nil)
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

func TestDependencyAskRecoversAfterVerifiedLanding(t *testing.T) {
	for _, note := range []string{depAskNote("d0e0001", depDropped), depAskNote("d0e0001", depMissing)} {
		t.Run(note, func(t *testing.T) {
			d := dep("zach", true)
			d.Labels[labelPrefix+labelLandedVerified] = "1"
			c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: note, labelTold: note})
			f, p := newTestFactory(t, c, d)
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

func TestDependencyRecoveryStaysBlocked(t *testing.T) {
	for _, name := range []string{"unverified", "diff changed", "unrelated ask", "running", "closed", "parked", "running on inspect"} {
		t.Run(name, func(t *testing.T) {
			d := dep("zach", false)
			d.Labels[labelPrefix+labelLandedVerified] = "1"
			c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: depAskNote("d0e0001", depDropped)})
			switch name {
			case "unverified", "diff changed":
				delete(d.Labels, labelPrefix+labelLandedVerified)
				d.Labels[labelPrefix+labelNote] = "landing not verified"
				if name == "diff changed" {
					d.Labels[labelPrefix+labelNote] = "landed diff differs from the reviewed one: changed"
				}
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
			if l := p.labels("c1"); l[labelStage] != "" || l[labelHeld] != "" {
				t.Errorf("moved blocked card: %v", l)
			}
			if len(p.sent("c1")) != 0 {
				t.Error("sent blocked card a land prompt")
			}
		})
	}
}

func TestGuardedDependencyAskRecoveryScansAgain(t *testing.T) {
	for _, clean := range []bool{false, true} {
		c := heldCard(map[string]string{labelStage: "zach", labelHeld: "", labelNote: depAskNote("d0e0001", depDropped)})
		c.Repo = publicRepo
		d := dep("zach", true)
		d.Labels[labelPrefix+labelLandedVerified] = "1"
		f, p := newTestFactory(t, c, d)
		var hits []scan.Hit
		if !clean {
			hits = []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}
		}
		ranges := fakeScan(f, hits, nil)
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
		if l[labelLandedVerified] != "" || l[labelStage] != "zach" {
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
