package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecisionHistoryWaitsThenAsks(t *testing.T) {
	for _, stage := range []string{"plan", "landing", "building", "review"} {
		t.Run(stage, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: stage, labelSource: "board", labelSince: ago(10 * time.Minute)})
			c.LastActivityAt = t0.Add(-time.Minute)
			if stage == "building" {
				c.Labels[labelPrefix+labelAutoReview] = "code"
			}
			r := reviewer("r1", "c1", "code")
			f, p := newTestFactory(t, c, r)
			run := func() {
				t.Helper()
				a := c
				if stage == "review" {
					a = r
				}
				if err := f.tickAgent(a, f.Now()); err != nil {
					t.Fatal(err)
				}
			}
			run()
			if p.labels(c.ID)[labelStage] != "" || p.runs != 0 || p.labels(r.ID)[labelApplied] != "" {
				t.Fatalf("missing history advanced card: %v", p.calls)
			}
			f.Now = func() time.Time { return t0.Add(time.Minute) }
			run()
			l := p.labels(c.ID)
			if l[labelStage] != "zach" || !strings.Contains(l[labelNote], "history unavailable") {
				t.Fatalf("missing history did not escalate: %v", p.calls)
			}
			if p.runs != 0 || p.labels(r.ID)[labelApplied] != "" || p.called("archive", r.ID) {
				t.Fatalf("missing history applied or restarted review: %v", p.calls)
			}
			noLogsCalls(t, p)
		})
	}
}

func TestDecisionHistoryLateFileResumesPlan(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	if err := f.tickAgent(c, t0); err != nil {
		t.Fatal(err)
	}
	if p.runs != 0 {
		t.Fatal("review started without history")
	}
	storeLog(t, f, p, c.ID, "Plan.\nTITLE: A clearer title\nTIER: low")
	if err := f.tickAgent(c, t0); err != nil {
		t.Fatal(err)
	}
	if p.labels(c.ID)[labelStage] != "plan-review" {
		t.Fatalf("late file not used: %v", p.calls)
	}
	noLogsCalls(t, p)
}

func TestDecisionHistoryMissingDoesNotNameOrQualifyDirect(t *testing.T) {
	c := card("c1", map[string]string{labelSource: "board"})
	f, p := newTestFactory(t, c)
	f.nameCard(c, "TITLE: Too early")
	if direct, err := f.directReply(c); direct || !errors.Is(err, ErrNoHistory) || p.labels(c.ID)[labelTitled] != "" {
		t.Fatal("missing history used as a reply")
	}
	noLogsCalls(t, p)
}

func TestDecisionHistoryDryRunBlocksLogs(t *testing.T) {
	if paseoRead([]string{"logs", "c1"}) {
		t.Fatal("dry-run permits a session-loading log read")
	}
}

func TestDecisionHistoryReviewerLateFileAndHumanGate(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "asked"}[expired], func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "review", labelPlanRounds: "1"})
			r := reviewer("r1", c.ID, "code")
			f, p := newTestFactory(t, c, r)
			if expired {
				f.Now = func() time.Time { return t0.Add(time.Minute) }
			}
			if err := f.ApplyVerdict(r); err != nil {
				t.Fatal(err)
			}
			if expired {
				for k, v := range p.labels(c.ID) {
					c.Labels[labelPrefix+k] = v
				}
				f.SetAgents([]Agent{c, r})
			}
			storeLog(t, f, p, r.ID, "Finding.\nTIER: low\nVERDICT: changes")
			if err := f.ApplyVerdict(r); err != nil {
				t.Fatal(err)
			}
			if expired {
				if p.labels(r.ID)[labelApplied] != "" || len(p.sent(c.ID)) != 0 {
					t.Fatal("late history answered the human gate")
				}
			} else if p.labels(r.ID)[labelApplied] != "1" || p.labels(c.ID)[labelCodeRounds] != "1" {
				t.Fatalf("late verdict not applied: %v", p.labels(c.ID))
			}
			noLogsCalls(t, p)
		})
	}
}

func TestDecisionHistoryPlanMustBeReadBeforeReviewConsumed(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan-review"})
	r := reviewer("r1", c.ID, "plan")
	f, p := newTestFactory(t, c, r)
	storeLog(t, f, p, r.ID, "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(r); err != nil {
		t.Fatal(err)
	}
	if p.labels(r.ID)[labelApplied] != "" || len(p.sent(c.ID)) != 0 {
		t.Fatal("review consumed without plan")
	}
	storeLog(t, f, p, c.ID, "Plan.\nTIER: risky\nWaits on: d0e1234")
	if err := f.ApplyVerdict(r); err != nil {
		t.Fatal(err)
	}
	if l := p.labels(c.ID); l[labelTier] != "risky" || l[labelWaitsOn] != "d0e1234" || l[labelStage] != "building" {
		t.Fatalf("stored plan decisions lost: %v", l)
	}
	noLogsCalls(t, p)
}

func TestDecisionHistoryUnavailableKinds(t *testing.T) {
	for _, kind := range []string{"missing", "no-session", "unsupported", "invalid", "empty"} {
		t.Run(kind, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "plan", labelSource: "email", labelSince: ago(10 * time.Minute)})
			f, p := newTestFactory(t)
			switch kind {
			case "no-session":
				c.SessionID = ""
			case "unsupported":
				c.Provider = "unknown"
			case "invalid":
				c.SessionID = "invalid/session"
			case "empty":
				writeFile(t, f.cfg.Home+"/.claude/projects/-test/"+c.SessionID+".jsonl", "")
			}
			f.SetAgents([]Agent{c})
			f.Now = func() time.Time { return t0.Add(time.Minute) }
			if err := f.tickAgent(c, f.Now()); err != nil {
				t.Fatal(err)
			}
			if p.labels(c.ID)[labelStage] != "zach" || p.labels(c.ID)[labelTrack] != "" || p.runs != 0 {
				t.Fatalf("unavailable history misclassified: %v", p.labels(c.ID))
			}
			noLogsCalls(t, p)
		})
	}
}

func TestDecisionHistoryLandingWaitDoesNotWriteTiming(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error {
		t.Error("verified landing before history was available")
		return nil
	}
	f.GitOut = func(context.Context, string, ...string) (string, error) {
		t.Error("read landing time before history was available")
		return "", errors.New("no history")
	}
	if err := f.tickAgent(c, t0); err != nil {
		t.Fatal(err)
	}
	noLogsCalls(t, p)
}

func TestDecisionHistoryAskExplainsPause(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "history unavailable: decision paused", labelStuckFrom: "review"})
	ask := askFor(Card{Stage: "zach"}, c)
	if ask == nil || !strings.Contains(ask.Title, "History unavailable") || !strings.Contains(ask.Why, "two minutes") || ask.Actions[0].ID != "rerun" {
		t.Fatalf("history wait misrepresented: %+v", ask)
	}
}

func TestDecisionHistoryOutsideLandingReviewEscalates(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "requester absent", true: "requester present"}[present], func(t *testing.T) {
			requester := card("outside", nil)
			r := reviewer("r1", requester.ID, "land")
			f, p := newTestFactory(t, r)
			if present {
				f.SetAgents([]Agent{requester, r})
			}
			if err := f.landReviews.put(LandReview{By: requester.ID, Review: r.ID, Status: "running", Head: "abc1234"}, t0); err != nil {
				t.Fatal(err)
			}
			if err := f.ApplyVerdict(r); err != nil {
				t.Fatal(err)
			}
			pending, _ := f.landReviews.Get(requester.ID)
			if pending.Status != "running" || p.labels(r.ID)[labelApplied] != "" {
				t.Fatal("landing review did not wait for history")
			}
			f.Now = func() time.Time { return t0.Add(time.Minute) }
			if err := f.ApplyVerdict(r); err != nil {
				t.Fatal(err)
			}
			result, _ := f.landReviews.Get(requester.ID)
			if result.Status != "done" || result.Verdict != "escalate" || !strings.Contains(result.Findings, "history unavailable") || result.DoneAt.IsZero() {
				t.Fatalf("outside landing review held indefinitely: %+v", result)
			}
			if p.labels(r.ID)[labelApplied] != "1" || !p.called("archive", r.ID) {
				t.Fatal("finished landing reviewer not retired")
			}
			labels := p.labels(requester.ID)
			if labels[labelStage] != "" || labels[labelNote] != "" || labels[labelStuckFrom] != "" || len(p.sent(requester.ID)) != 0 || p.runs != 0 {
				t.Fatalf("outside requester treated as a board card: %v", labels)
			}
			for k, v := range p.labels(r.ID) {
				r.Labels[labelPrefix+k] = v
			}
			f.Now = func() time.Time { return t0.Add(30 * time.Minute) }
			for range 3 {
				if err := f.tickAgent(r, f.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if p.countCalls("archive", r.ID) != 1 {
				t.Fatal("later ticks repeated the completed landing review")
			}
			noLogsCalls(t, p)
		})
	}
}

func TestDecisionHistoryReplySupersedesPausedReview(t *testing.T) {
	for _, tc := range []struct {
		name               string
		lateFile, archived bool
	}{
		{"history stays missing", false, false}, {"history appears", true, false},
		{"archived history stays missing", false, true}, {"archived history appears", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "review", labelSource: "board", labelTier: "low", labelPlanRounds: "1", labelVerdict: "plan approve"})
			r := reviewer("r1", c.ID, "code")
			if tc.archived {
				r = archived(r)
			}
			f, p := newTestFactory(t, c, r)
			p.archived[r.ID] = tc.archived
			f.p = hookPaseo{Paseo: p, onCall: func(args []string) {
				if len(args) > 0 && args[0] == "send" && p.labels(r.ID)[labelApplied] != "1" {
					t.Error("new instructions sent before superseding the old review")
				}
			}}
			f.Now = func() time.Time { return t0.Add(time.Minute) }
			if err := f.ApplyVerdict(r); err != nil {
				t.Fatal(err)
			}
			c = withLabels(c, p.labels(c.ID))
			f.SetAgents([]Agent{c, r})
			if _, err := f.Act("send", Body{ID: c.ID, Message: "Fix the issue first"}); err != nil {
				t.Fatal(err)
			}
			c, r = withLabels(c, p.labels(c.ID)), withLabels(r, p.labels(r.ID))
			c.Status, p.status[c.ID] = "running", "running"
			f.SetAgents([]Agent{c, r})
			if tc.lateFile {
				storeLog(t, f, p, r.ID, "TIER: low\nVERDICT: approve")
			}
			if err := f.tickAgent(r, f.Now()); err != nil {
				t.Fatal(err)
			}
			if p.labels(c.ID)[labelStage] != "building" || len(p.sent(c.ID)) != 1 {
				t.Fatalf("old review moved card while replying: %v", p.labels(c.ID))
			}
			c.Status, p.status[c.ID] = "idle", "idle"
			c.LastActivityAt = t0.Add(2 * time.Minute)
			f.Now = func() time.Time { return t0.Add(3 * time.Minute) }
			storeLog(t, f, p, c.ID, "Fixed the issue.\nTIER: low")
			f.SetAgents([]Agent{c, r})
			if err := f.tickAgent(r, f.Now()); err != nil {
				t.Fatal(err)
			}
			if p.labels(c.ID)[labelStage] != "building" {
				t.Fatalf("old review paused the fresh reply again: %v", p.labels(c.ID))
			}
			if err := f.tickAgent(c, f.Now()); err != nil {
				t.Fatal(err)
			}
			if p.labels(r.ID)[labelVerdict] != "superseded" || p.labels(c.ID)[labelCodeRounds] != "" || p.labels(c.ID)[labelStage] != "review" || p.runs != 1 {
				t.Fatalf("reply did not get a fresh review: %v, old reviewer: %v", p.labels(c.ID), p.labels(r.ID))
			}
			noLogsCalls(t, p)
		})
	}
}
