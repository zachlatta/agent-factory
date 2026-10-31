package factory

import (
	"fmt"
	"testing"
)

func TestOutsideArchiveKeepsUnfinishedStage(t *testing.T) {
	for _, stage := range []string{"plan", "plan-review", "building", "review"} {
		t.Run(stage, func(t *testing.T) {
			a := Agent{ID: "interrupted", Status: "closed", ArchivedAt: t0, Labels: map[string]string{
				"factory.stage": stage, "factory.archive-watch": "1", "factory.auto-review": "code",
			}}
			if got := Derive(a); got != stage {
				t.Fatalf("stage = %s; want %s", got, stage)
			}
			if a.gone() {
				t.Fatal("interrupted work is finished")
			}
			f, p := newTestFactory(t, a)
			for range 2 {
				if err := f.tickAgent(a, t0); err != nil {
					t.Fatal(err)
				}
			}
			if got := p.labels(a.ID)[labelStuck]; got != "Agent archived before this step finished." {
				t.Fatalf("reason = %q", got)
			}
			if len(p.sent(a.ID)) != 0 {
				t.Fatal("automatic recovery woke archived work")
			}
			cards := BuildBoard([]Agent{a}, nil, t0)
			if len(cards) != 1 || cards[0].Stage != stage || cards[0].Ask == nil {
				t.Fatalf("missing stuck presentation: %+v", cards)
			}
		})
	}
}

func TestArchiveClassificationBoundaries(t *testing.T) {
	base := card("boundary", map[string]string{labelStage: "building", labelArchiveWatch: "1"})
	base.ArchivedAt = t0
	for _, tc := range []struct{ name, key, value string }{
		{"historical", labelArchiveWatch, ""}, {"parked", labelParked, "1"}, {"retired", labelReplacedBy, "successor"},
		{"routed", labelRouted, "successor"}, {"dropped", labelFinished, "1"}, {"landed", labelStage, "landed"},
		{"reviewer", labelRole, "reviewer"}, {"scout", labelRole, "scout"}, {"unlabelled", labelStage, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := labeled(base, map[string]string{tc.key: tc.value})
			if interruptedArchive(a) {
				t.Fatal("intentional or historical archive interrupted")
			}
		})
	}
}

func TestArchiveEnrollmentAndRestart(t *testing.T) {
	a := card("enroll", map[string]string{labelStage: "building"})
	f, p := newTestFactory(t, a)
	if _, err := f.tickArchive(a); err != nil {
		t.Fatal(err)
	}
	a = withLabels(a, p.labels(a.ID))
	a.ArchivedAt = t0
	f, p = newTestFactory(t, a)
	if handled, err := f.tickArchive(a); !handled || err != nil {
		t.Fatalf("%v %v", handled, err)
	}
	a = withLabels(a, p.labels(a.ID))
	f, p = newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 || Derive(a) != "building" {
		t.Fatal("restart advanced interrupted work")
	}
	if depOf([]Agent{a}, a.ID) != depWaiting {
		t.Fatal("dependency no longer waiting")
	}
}

func TestInterruptedRecoveryAndDrop(t *testing.T) {
	for _, action := range []string{"resume", "send", "rerun", "close", "archive", "stage"} {
		t.Run(action, func(t *testing.T) {
			a := card("recover", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			p.archived[a.ID] = true
			if _, err := f.Act(action, Body{ID: a.ID, Message: "Continue the work", Stage: "landed"}); err != nil {
				t.Fatal(err)
			}
			a = withLabels(f.agent(a.ID), p.labels(a.ID))
			if action == "resume" || action == "send" || action == "rerun" {
				if a.archived() || stuckReason(a) != "" || a.L(labelAutoReview) != "code" {
					t.Fatalf("not recovered: %+v", a)
				}
			} else {
				if Derive(a) != "landed" || interruptedArchive(a) {
					t.Fatalf("not finished: %+v", a)
				}
				f, _ = newTestFactory(t, a)
				if err := f.tickAgent(a, t0); err != nil {
					t.Fatal(err)
				}
				if Derive(f.agent(a.ID)) != "landed" {
					t.Fatal("drop reopened")
				}
			}
		})
	}
}

func TestDeliberateFinishPersistsBeforeArchive(t *testing.T) {
	for _, action := range []string{"archive", "stage"} {
		for _, failure := range []string{"labels", "archive", "none"} {
			t.Run(action+"/"+failure, func(t *testing.T) {
				a := card("finish", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
				f, p := newTestFactory(t, a)
				if failure == "labels" {
					f.p = failLabelWrite{p}
				}
				if failure == "archive" {
					p.inspectErr = fmt.Errorf("archive inspect failed")
				}
				_, err := f.Act(action, Body{ID: a.ID, Stage: "landed"})
				if (err != nil) != (failure != "none") {
					t.Fatalf("error = %v", err)
				}
				if failure == "labels" {
					if p.called("archive", a.ID) {
						t.Fatal("archived before completion persisted")
					}
					return
				}
				a = withLabels(a, p.labels(a.ID))
				a.ArchivedAt = t0
				if interruptedArchive(a) || Derive(a) != "landed" {
					t.Fatal("deliberate finish reopened")
				}
				if failure == "archive" {
					f, _ = newTestFactory(t, a)
					if err := f.tickAgent(a, t0); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestFailedArchiveRecoveryStaysStuck(t *testing.T) {
	for _, action := range []string{"resume", "send", "rerun"} {
		t.Run(action, func(t *testing.T) {
			a := card("failed", map[string]string{labelStage: "plan", labelArchiveWatch: "1", labelStuck: stuckArchived})
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			f.p = failSend{p}
			if _, err := f.Act(action, Body{ID: a.ID, Message: "Continue"}); err == nil {
				t.Fatal("send succeeded")
			}
			a = withLabels(a, p.labels(a.ID))
			if !interruptedArchive(a) || stuckReason(a) != stuckArchived {
				t.Fatal("lost interruption on failed recovery")
			}
		})
	}
}

func TestInterruptedWorkflowAndTimingRemainActive(t *testing.T) {
	a := card("active", map[string]string{labelStage: "building", labelArchiveWatch: "1"})
	a.ArchivedAt = t0
	f, _ := newTestFactory(t, a)
	if workflowHealth([]Agent{a}).Active != 1 {
		t.Fatal("lost active work")
	}
	ct := timing(t, f, a.ID)
	if !ct.LandedAt.IsZero() {
		t.Fatal("interruption recorded as landing")
	}
}

func TestStageSuccessorIsWatchedAtCreation(t *testing.T) {
	a, f, p := farCard(t)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelReplaces, a.ID))
	if len(runs) != 1 {
		t.Fatalf("successor runs: %v", runs)
	}
	found := false
	for _, arg := range runs[0] {
		if arg == labelArg(labelArchiveWatch, "1") {
			found = true
		}
	}
	if !found {
		t.Fatal("successor has an unprotected first tick")
	}
}

func TestArchivePromptExplainsInterruption(t *testing.T) {
	a := card("prompt", map[string]string{labelStage: "building", labelArchiveWatch: "1"})
	a.ArchivedAt = t0
	ask := askFor(Card{ID: a.ID, Stage: "building"}, a)
	ask.Overview = &Overview{Overview: "The work is ready."}
	refineAsk(ask)
	if ask.Overview != nil {
		t.Fatal("old report hides the interruption reason")
	}
}

func TestLateVerdictCannotAdvanceInterruptedWork(t *testing.T) {
	a := card("interrupted-review", map[string]string{labelStage: "review", labelArchiveWatch: "1"})
	a.ArchivedAt = t0
	r := reviewer("late-review", a.ID, "code")
	f, p := newTestFactory(t, a, r)
	p.archived[a.ID] = true
	storeLog(t, f, p, r.ID, "[Assistant] Approved.\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(r); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelStage] != "" {
		t.Fatal("late verdict advanced interruption")
	}
}
