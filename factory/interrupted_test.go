package factory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

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
			a := card("failed", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
			a.Cwd = t.TempDir()
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
	a := card("interrupted-review", map[string]string{labelStage: gateDesign, labelArchiveWatch: "1"})
	a.ArchivedAt = t0
	r := reviewer("late-review", a.ID, gateDesign)
	f, p := newTestFactory(t, a, r)
	p.archived[a.ID] = true
	storeLog(t, f, p, r.ID, "[Assistant] Approved.\nTIER: low\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(r); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelStage] != "" {
		t.Fatal("late verdict advanced interruption")
	}
}

func TestFinishedArchiveRetriesOnlyWhileLive(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(fmt.Sprint(archived), func(t *testing.T) {
			a := card("finished", map[string]string{labelStage: "building", labelFinished: "1"})
			if archived {
				a.ArchivedAt = t0
			}
			f, p := newTestFactory(t, a)
			p.archived[a.ID] = archived
			for range 2 {
				if err := f.tickAgent(a, t0); err != nil {
					t.Fatal(err)
				}
				a = f.agent(a.ID)
			}
			if archived {
				if n := p.countCalls("inspect", ""); n != 0 {
					t.Fatalf("finished archive made %d inspect calls", n)
				}
			} else if !p.called("archive", a.ID) {
				t.Fatal("unfinished archive was not retried")
			}
		})
	}
}

func TestArchivedPickerIsNotInterrupted(t *testing.T) {
	a := card("picker", map[string]string{labelStage: stageRoute, labelRouting: "1", labelArchiveWatch: "1"})
	a.ArchivedAt = t0
	if archiveStage(a) || interruptedArchive(a) {
		t.Fatal("picker enrolled in stage recovery")
	}
}

func TestArchivedDirectCardFinishesInsteadOfStuck(t *testing.T) {
	t.Run("verified on main when worktree exists", func(t *testing.T) {
		a := card("direct-ok", map[string]string{labelStage: "building", labelSource: "board", labelArchiveWatch: "1"})
		a.Cwd = t.TempDir()
		a.ArchivedAt = t0
		f, p := newTestFactory(t, a)
		f.GitOut = gitStub(directOK)
		storeLog(t, f, p, a.ID, "[User] Move call to 4 PM\n[Assistant] TITLE: Moved call to 4 PM\nDone.\nTRACK: direct")
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		l := p.labels(a.ID)
		if l[labelTrack] != "direct" || l[labelStage] != "landed" || l[labelNote] != "done in one pass" || l[labelStuck] != "" {
			t.Fatalf("labels = %v", l)
		}
		if names := p.names(a.ID); len(names) != 1 || names[0] != "Moved call to 4 PM" {
			t.Fatalf("names = %v", names)
		}
		if len(p.sent(a.ID)) != 0 {
			t.Fatalf("sent = %v", p.sent(a.ID))
		}
	})

	t.Run("already stuck with removed worktree finishes with unverified note", func(t *testing.T) {
		a := card("direct-gone", map[string]string{
			labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived,
		})
		a.Cwd = filepath.Join(t.TempDir(), "removed-worktree")
		a.ArchivedAt = t0
		r := reviewer("rev1", a.ID, gateRequest)
		f, p := newTestFactory(t, a, r)
		storeLog(t, f, p, a.ID, "[User] Move the call to 4 PM\n[Assistant] Queued for approval.\nTRACK: direct")
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		l := p.labels(a.ID)
		if v, ok := l[labelStuck]; !ok || v != "" || l[labelTrack] != "direct" || l[labelStage] != "landed" || l[labelNote] != directWorktreeGoneNote {
			t.Fatalf("labels = %v", l)
		}
		if !strings.Contains(strings.Join(f.log, "\n"), directWorktreeGoneNote) {
			t.Fatalf("missing log entry: %v", f.log)
		}
		if p.labels(r.ID)[labelVerdict] != "superseded" {
			t.Fatalf("reviewer labels = %v", p.labels(r.ID))
		}
		if len(p.sent(a.ID)) != 0 {
			t.Fatalf("sent on removed worktree: %v", p.sent(a.ID))
		}
	})

	t.Run("worktree exists but not on main wakes direct card", func(t *testing.T) {
		a := card("direct-unpushed", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
		a.Cwd = t.TempDir()
		a.ArchivedAt = t0
		f, p := newTestFactory(t, a)
		p.archived[a.ID] = true
		f.GitOut = gitStub(directOK)
		f.Git = func(_ context.Context, _ string, args ...string) error {
			if args[0] == "merge-base" {
				return errors.New("exit status 1")
			}
			return nil
		}
		storeLog(t, f, p, a.ID, "[User] Move call\n[Assistant] Done.\nTRACK: direct")
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		l := p.labels(a.ID)
		if v, ok := l[labelStuck]; !ok || v != "" || l[labelTrack] != "direct" || l[labelNote] != "landing its result again" {
			t.Fatalf("labels = %v", l)
		}
		if len(p.sent(a.ID)) != 1 || f.agent(a.ID).archived() {
			t.Fatalf("sent = %v archived = %v", p.sent(a.ID), f.agent(a.ID).archived())
		}
	})

	t.Run("guarded repo waits in Needs Zach", func(t *testing.T) {
		a := card("direct-guarded", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
		a.Repo = "zachlatta/agent-factory"
		a.Cwd = filepath.Join(t.TempDir(), "removed-worktree")
		a.ArchivedAt = t0
		f, p := newTestFactory(t, a)
		storeLog(t, f, p, a.ID, "[User] Check something\n[Assistant] Done.\nTRACK: direct")
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		l := p.labels(a.ID)
		if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "public repo: finished without review") || l[labelStuck] != "" {
			t.Fatalf("labels = %v", l)
		}
	})

	t.Run("plan with TIER or plan-rounds stays stuck", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			labels map[string]string
			log    string
		}{
			{"has tier", map[string]string{labelStage: "building", labelArchiveWatch: "1"}, "[User] Build it\n[Assistant] Plan.\nRESULT: code\nTRACK: direct"},
			{"has plan-rounds", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelRoundsRequest: "1"}, "[User] Build it\n[Assistant] Done.\nTRACK: direct"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := card("not-direct", tc.labels)
				a.ArchivedAt = t0
				f, p := newTestFactory(t, a)
				storeLog(t, f, p, a.ID, tc.log)
				if err := f.tickAgent(a, t0); err != nil {
					t.Fatal(err)
				}
				if got := p.labels(a.ID)[labelStuck]; got != stuckArchived {
					t.Fatalf("stuck = %q; labels = %v", got, p.labels(a.ID))
				}
				// Once marked stuck and checked, later ticks do not re-read the session file.
				a = withLabels(a, p.labels(a.ID))
				storeLog(t, f, p, a.ID, "[User] Build it\n[Assistant] Done.\nTRACK: direct")
				if err := f.tickAgent(a, t0); err != nil {
					t.Fatal(err)
				}
				if got := p.labels(a.ID)[labelStuck]; got != stuckArchived {
					t.Fatalf("re-read session after caching stuck check: %v", p.labels(a.ID))
				}
			})
		}
	})
}

type missingCwdSend struct{ *fakePaseo }

func (p missingCwdSend) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "send" {
		return "", errors.New("paseo send --no-wait: Error: Failed to send message: Working directory does not exist")
	}
	return p.fakePaseo.Run(ctx, args...)
}

func TestOutsideArchiveKeepsUnfinishedStage(t *testing.T) {
	for _, stage := range []string{"building", gateRequest, gateDesign, gateSignoff} {
		t.Run(stage, func(t *testing.T) {
			a := Agent{ID: "interrupted", Status: "closed", ArchivedAt: t0, Labels: map[string]string{
				"factory.stage": stage, "factory.archive-watch": "1", "factory.auto-review": gateRequest,
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

func TestInterruptedRecoveryAndDrop(t *testing.T) {
	for _, action := range []string{"resume", "send", "rerun", "close", "archive", "stage"} {
		t.Run(action, func(t *testing.T) {
			a := card("recover", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
			a.Cwd = t.TempDir()
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			p.archived[a.ID] = true
			if _, err := f.Act(action, Body{ID: a.ID, Message: "Continue the work", Stage: "landed"}); err != nil {
				t.Fatal(err)
			}
			a = withLabels(f.agent(a.ID), p.labels(a.ID))
			if action == "resume" || action == "send" || action == "rerun" {
				if a.archived() || stuckReason(a) != "" || a.L(labelAutoReview) != gateRequest {
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

func TestInterruptedReplyKeepsReplyInstructions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      map[string]string
		want, stage string
	}{
		{"landing report", map[string]string{labelNote: "landed; did not say what remains"}, remainingRule, "landing"},
		{"plan", map[string]string{labelStage: "building"}, "Message from Zach via the factory board:", "building"},
		{"picker", map[string]string{labelRouting: "1", labelStage: stageRoute}, "Pick the repo again and end with the REPO: line.", stageRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := card("instructions", map[string]string{labelStage: "building", labelArchiveWatch: "1"})
			a = withLabels(a, tc.labels)
			a.Cwd = t.TempDir()
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			if _, err := f.Act("send", Body{ID: a.ID, Message: "Continue"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(p.sent(a.ID), ""), tc.want) {
				t.Fatal("missing reply instructions")
			}
			if p.labels(a.ID)[labelStage] != tc.stage {
				t.Fatalf("wrong step: %v", p.labels(a.ID))
			}
		})
	}
}

func TestArchivedCardWithRemovedWorktreeRefusesWakeActions(t *testing.T) {
	a := card("gone-cwd", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived})
	a.Cwd = filepath.Join(t.TempDir(), "missing-worktree")
	a.ArchivedAt = t0
	f, p := newTestFactory(t, a)
	p.archived[a.ID] = true
	if err := f.replies.save(map[string]pendingReply{"thr1": {Card: a.ID, Text: "clarify"}}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"choose resume", func() error { _, err := f.choose(Body{ID: a.ID, Choice: "resume"}); return err }},
		{"choose rerun", func() error { _, err := f.choose(Body{ID: a.ID, Choice: "rerun"}); return err }},
		{"choose change", func() error { _, err := f.choose(Body{ID: a.ID, Choice: "change", Message: "try again"}); return err }},
		{"act resume", func() error { _, err := f.Act("resume", Body{ID: a.ID}); return err }},
		{"act rerun", func() error { _, err := f.Act("rerun", Body{ID: a.ID}); return err }},
		{"act send", func() error { _, err := f.Act("send", Body{ID: a.ID, Message: "try again"}); return err }},
		{"act approve", func() error { _, err := f.Act("approve", Body{ID: a.ID}); return err }},
		{"act review", func() error { _, err := f.Act("review", Body{ID: a.ID, Kind: gateRequest}); return err }},
		{"act stage building", func() error { _, err := f.Act("stage", Body{ID: a.ID, Stage: "building"}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, errWorktreeGone) {
				t.Fatalf("err = %v; want %v", err, errWorktreeGone)
			}
			if len(p.sent(a.ID)) != 0 || len(p.labelUpdates(a.ID)) != 0 {
				t.Fatalf("modified card on refused action: sent=%v labels=%v", p.sent(a.ID), p.labelUpdates(a.ID))
			}
			saved, err := f.replies.read()
			if err != nil || saved["thr1"].Card != a.ID {
				t.Fatalf("refused choice cleared saved clarification: %v %v", saved, err)
			}
		})
	}

	if _, err := f.choose(Body{ID: a.ID, Choice: "drop"}); err != nil {
		t.Fatalf("drop failed on missing worktree: %v", err)
	}
	if got := Derive(withLabels(a, p.labels(a.ID))); got != "landed" {
		t.Fatalf("stage after drop = %q; want landed", got)
	}
	if saved, err := f.replies.read(); err != nil || len(saved) != 0 {
		t.Fatalf("successful drop left saved clarification: %v %v", saved, err)
	}

	f.p = missingCwdSend{p}
	if err := f.send(a.ID, "retry"); !errors.Is(err, errWorktreeGone) {
		t.Fatalf("send error = %v; want %v", err, errWorktreeGone)
	}
}

func TestInterruptedReplyUsesNormalSend(t *testing.T) {
	for _, via := range []string{"board", "email"} {
		t.Run(via, func(t *testing.T) {
			a := card("reply", map[string]string{labelStage: gateDesign, labelArchiveWatch: "1", labelStuck: stuckArchived,
				labelRoundsDesign: "4", labelReviewerRetries: "2"})
			a.Cwd = t.TempDir()
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			if _, err := f.Act("send", Body{ID: a.ID, Message: "Update the work", Via: via}); err != nil {
				t.Fatal(err)
			}
			messages := strings.Join(p.sent(a.ID), "\n")
			want := "Message from Zach via the factory board:"
			if via == "email" {
				want = "Zach answered by email while the card waited on him:"
			}
			if !strings.Contains(messages, want) {
				t.Fatalf("missing reply format: %s", messages)
			}
			l := p.labels(a.ID)
			if l[labelRoundsDesign] != "" || l[labelReviewerRetries] != "" {
				t.Fatalf("review generation not reset: %v", l)
			}
		})
	}
}
