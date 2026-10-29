package factory

import (
	"testing"
	"time"
)

// A card marked done (or moved to Done) stays done: no review queued or running before that, and no
// late verdict, brings it back. Only a new message from Zach reopens it.

// queuedAfterFollowUp is a card whose builder answered a follow-up and stopped, with a code review
// queued: a Tick would start it.
func queuedAfterFollowUp() Agent {
	return card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelSource: "board",
		labelPlanRounds: "1", labelSince: ago(10 * time.Minute)})
}

// finish runs the terminal action: Done, or a move to the Done column.
func finish(t *testing.T, f *Factory, how string) {
	t.Helper()
	var err error
	if how == "done" {
		_, err = f.Act("done", Body{ID: "c1"})
	} else {
		_, err = f.Act("stage", Body{ID: "c1", Stage: "landed"})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func staysDone(t *testing.T, name string, p *fakePaseo) {
	t.Helper()
	if l := p.labels("c1"); l[labelStage] != "landed" || l[labelAutoReview] != "" {
		t.Errorf("%s: labels = %v", name, l)
	}
	if runs := p.runsWithLabel(labelArg(labelRole, "reviewer")); len(runs) != 0 {
		t.Errorf("%s: review started on a finished card: %v", name, runs)
	}
}

func TestDoneCancelsAQueuedReview(t *testing.T) {
	for _, how := range []string{"done", "stage"} {
		for _, stale := range []bool{false, true} {
			name := map[bool]string{false: how + "/fresh list", true: how + "/list from before"}[stale]
			t.Run(name, func(t *testing.T) {
				c := queuedAfterFollowUp()
				f, p := newTestFactory(t, c)
				f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
				finish(t, f, how)
				if !stale {
					f.SetAgents([]Agent{merged(c, p.labels("c1"))})
				}
				f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
				f.Tick()
				staysDone(t, name, p)
			})
		}
	}
}

// A reviewer running when the card was finished is retired; its late verdict, even from a list read
// before, does not move the card.
func TestLateVerdictAfterDoneDoesNotReviveTheCard(t *testing.T) {
	for _, how := range []string{"done", "stage"} {
		t.Run(how, func(t *testing.T) {
			rev := reviewer("r1", "c1", "code")
			rev.Status = "running"
			land := reviewer("r2", "c1", "land")
			land.Status = "running"
			c := freshCard(false, requested(nil))
			f, p := newTestFactory(t, c, rev, land)
			headIs(f, reviewedSHA, nil)
			dirtyIs(f, "", nil)
			finish(t, f, how)
			if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "superseded" {
				t.Errorf("running reviewer not retired: %v", l)
			}
			if p.labels("r2")[labelApplied] != "" {
				t.Error("landing review retired")
			}
			storeLog(t, f, p, "r1", "TIER: low\nVERDICT: changes")
			rev.Status = "idle"
			if err := f.ApplyVerdict(rev); err != nil { // the list predates the retirement
				t.Fatal(err)
			}
			staysDone(t, "late verdict", p)
			if l := p.labels("c1"); l[labelCodeRounds] != "" {
				t.Errorf("late verdict counted a round: %v", l)
			}
			if len(p.sent("c1")) != 0 {
				t.Errorf("late verdict messaged the finished card: %v", p.sent("c1"))
			}
		})
	}
}

// A reviewer that somehow started after the card was finished does not move it either.
func TestVerdictForAFinishedCardIsConsumed(t *testing.T) {
	rev := reviewer("r1", "c1", "code")
	rev.CreatedAt = t0.Add(time.Minute)
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "landed", labelNote: "marked done by Zach"}), rev)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(rev); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("r1"); l[labelApplied] != "1" {
		t.Errorf("verdict not consumed: %v", l)
	}
	if l := p.labels("c1"); l[labelStage] != "" || len(p.sent("c1")) != 0 {
		t.Errorf("finished card moved: %v %v", l, p.sent("c1"))
	}
}

// Zach's own message to a finished card still reopens it, and its review runs once the builder answers.
func TestMessageAfterDoneStillReopensTheCard(t *testing.T) {
	c := queuedAfterFollowUp()
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
	finish(t, f, "done")
	done := merged(c, p.labels("c1"))
	f.SetAgents([]Agent{done})
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	if _, err := f.Act("send", Body{ID: "c1", Message: "One more thing: handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != "code" {
		t.Fatalf("message did not reopen the card: %v", l)
	}
	answered := merged(done, p.labels("c1"))
	answered.LastActivityAt = t0.Add(5 * time.Minute)
	f.SetAgents([]Agent{answered})
	f.Now = func() time.Time { return t0.Add(10 * time.Minute) }
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 1 || p.labels("c1")[labelStage] != "review" {
		t.Errorf("reopened card not reviewed: %v", p.labels("c1"))
	}
}

// The builder's final report can come after Done (its turn ends after the action): a review start
// decided from a list read before Done still starts nothing, though the agent was active since.
func TestStaleReviewStartAfterDoneAndAFinalReport(t *testing.T) {
	for _, how := range []string{"done", "stage"} {
		t.Run(how, func(t *testing.T) {
			c := queuedAfterFollowUp()
			f, p := newTestFactory(t, c)
			f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
			stale, _ := f.card("c1") // the Tick's card, captured before Done
			finish(t, f, how)
			reported := c // the final report lands after Done
			reported.LastActivityAt = t0.Add(time.Minute)
			f.SetAgents([]Agent{reported})
			f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
			if _, err := f.StartReview(stale, "code"); err != nil {
				t.Fatal(err)
			}
			staysDone(t, how, p)
		})
	}
}

// Zach's explicit review, re-run or move back to a review column after Done starts the review, even
// though the idle agent has done nothing since: Done was no follow-up the builder must answer.
func TestExplicitReviewAfterDoneStarts(t *testing.T) {
	for _, how := range []string{"review", "rerun", "stage"} {
		t.Run(how, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelPlanRounds: "1",
				labelStuckFrom: "review", labelSince: ago(10 * time.Minute)})
			f, p := newTestFactory(t, c)
			f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
			finish(t, f, "done")
			done := merged(c, p.labels("c1"))
			f.SetAgents([]Agent{done})
			f.Now = func() time.Time { return t0.Add(time.Minute) }
			var err error
			switch how {
			case "review":
				_, err = f.Act("review", Body{ID: "c1", Kind: "code"})
			case "rerun":
				_, err = f.Act("rerun", Body{ID: "c1"})
			case "stage":
				if _, err = f.Act("stage", Body{ID: "c1", Stage: "review"}); err == nil {
					moved := merged(done, p.labels("c1"))
					f.SetAgents([]Agent{moved})
					err = f.checkStuck(moved, t0.Add(10*time.Minute)) // the empty review column recovers
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 1 || p.labels("c1")[labelStage] != "review" {
				t.Errorf("explicit %s after Done started no review: %v", how, p.labels("c1"))
			}
		})
	}
}

// Zach's review of a Done card whose agent is still running (its final report comes after Done) is
// queued in a working column and starts once the agent stops.
func TestExplicitReviewAfterDoneWaitsForTheRunningAgent(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelPlanRounds: "1",
		labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(cleanDiff, map[string]string{"rev-parse HEAD": reviewedSHA + "\n"})
	finish(t, f, "done")
	running := merged(c, p.labels("c1"))
	running.Status = "running"
	f.SetAgents([]Agent{running})
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	res, err := f.Act("review", Body{ID: "c1", Kind: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := res.(map[string]bool); !q["queued"] {
		t.Fatalf("review of a running agent not queued: %v", res)
	}
	if l := p.labels("c1"); l[labelStage] == "landed" || l[labelAutoReview] != "code" {
		t.Fatalf("queued review left in Done: %v", l)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewKind, "code")); len(runs) != 0 {
		t.Fatalf("reviewed a running agent: %v", runs)
	}
	stopped := merged(running, p.labels("c1"))
	stopped.Status, stopped.LastActivityAt = "idle", t0.Add(3*time.Minute)
	f.SetAgents([]Agent{stopped})
	f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 1 || p.labels("c1")[labelStage] != "review" {
		t.Errorf("queued review did not start once the agent stopped: %v", p.labels("c1"))
	}
}
