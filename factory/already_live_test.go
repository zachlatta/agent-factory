package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAlreadyLiveRequestsBuilderCompletion(t *testing.T) {
	for _, missing := range []bool{false, true} {
		a := freshCard(false, reviewLabels(map[string]string{labelLandedVerified: "1"}))
		f, p := newTestFactory(t, a, reviewer("r1", "c1", "code"))
		f.Git = func(context.Context, string, ...string) error { return nil }
		f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "rev-parse HEAD":
				if missing {
					return "", errors.New("missing workspace")
				}
				return reviewedSHA, nil
			case "status --porcelain":
				return "", nil
			case "merge-base origin/main " + reviewedSHA:
				return reviewedSHA, nil
			case "rev-list --reverse --no-merges " + reviewedSHA + ".." + reviewedSHA:
				return "", nil
			}
			return "", errors.New("unexpected git read")
		}
		storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] != "landing" || l[labelRemainingAsk] != "1" || l[labelLiveCompletion] != reviewedSHA {
			t.Fatalf("missing=%v labels=%v", missing, l)
		}
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], "completion") || strings.Contains(msgs[0], "Land it now") {
			t.Fatalf("messages=%v", msgs)
		}
	}
}

func stubPublished(f *Factory, head, status string, statusErr error) {
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "rev-parse HEAD":
			if head == "" {
				return "", errors.New("workspace unavailable")
			}
			return head, nil
		case "status --porcelain":
			return status, statusErr
		case "merge-base origin/main " + reviewedSHA:
			return reviewedSHA, nil
		}
		return "", errors.New("unexpected git read")
	}
}

func TestAlreadyLiveAllPrelandCallers(t *testing.T) {
	for _, route := range []string{"review", "approve", "rerun", "held"} {
		for _, state := range []string{"fresh", "missing", "changed", "dirty", "status failure"} {
			t.Run(route+"/"+state, func(t *testing.T) {
				labels := reviewLabels(map[string]string{labelLandedVerified: "1"})
				if route == "approve" {
					labels = approvedLabels(map[string]string{labelLandedVerified: "1"})
				}
				if route == "rerun" || route == "held" {
					labels[labelStage] = "landing"
				}
				a := freshCard(false, labels)
				if route == "held" {
					a = heldCard(labels)
				}
				f, p := newTestFactory(t, a, reviewer("r1", "c1", "code"), dep("landed", false))
				head, status := reviewedSHA, ""
				var statusErr error
				switch state {
				case "missing":
					head = ""
				case "changed":
					head = movedSHA
				case "dirty":
					status = " M changed"
				case "status failure":
					statusErr = errors.New("status unavailable")
				}
				stubPublished(f, head, status, statusErr)
				var err error
				switch route {
				case "review":
					storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
					err = f.ApplyVerdict(f.agent("r1"))
				case "approve":
					_, err = f.Act("approve", Body{ID: "c1"})
				case "rerun":
					err = f.rerunStep(Card{ID: a.ID, Cwd: a.Cwd, Repo: a.Repo}, "landing")
				case "held":
					err = f.tickHeld(a, Card{ID: a.ID, Cwd: a.Cwd, Repo: a.Repo})
				}
				if err != nil && state != "status failure" {
					t.Fatal(err)
				}
				l := p.labels("c1")
				if state == "fresh" || state == "missing" {
					if l[labelLiveCompletion] != reviewedSHA || l[labelStage] != "landing" {
						t.Fatalf("labels=%v", l)
					}
					if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "completion report") {
						t.Fatalf("sent=%v", msgs)
					}
				} else {
					if l[labelLiveCompletion] != "" {
						t.Fatalf("accepted changed work: %v", l)
					}
					if state == "status failure" {
						if l[labelStage] != "zach" {
							t.Fatalf("labels=%v", l)
						}
					} else if l[labelStage] != "building" {
						t.Fatalf("labels=%v", l)
					}
					noLandPrompt(t, state, p)
				}
			})
		}
	}
}

func TestAlreadyLiveCompletionRevalidatesAndRoutesRemaining(t *testing.T) {
	const report = "DECISION: None.\nOVERVIEW: Published work checked.\nNEEDS FROM ZACH: Nothing.\n\n"
	for _, tc := range []struct{ name, head, reply, want string }{
		{"complete", reviewedSHA, report + "REMAINING: none", "landed"},
		{"missing report", reviewedSHA, report, "zach"},
		{"unfinished deployment", reviewedSHA, report + "REMAINING:\n- agent: verify deployment", "building"},
		{"new commits", movedSHA, report + "REMAINING: none", "building"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := freshCard(false, reviewLabels(map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelLiveCompletion: reviewedSHA, labelLandedVerified: "1", labelRemainingAsk: "1"}))
			f, p := newTestFactory(t, a)
			stubPublished(f, tc.head, "", nil)
			storeLog(t, f, p, "c1", tc.reply)
			f.Tick()
			if l := p.labels("c1"); l[labelStage] != tc.want {
				t.Fatalf("labels=%v", l)
			}
			noLandPrompt(t, tc.name, p)
		})
	}
}

func TestAlreadyLiveCompletionRequestIsOnce(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelLandedVerified: "1"}))
	f, p, m := mailFactory(t, a)
	stubPublished(f, reviewedSHA, "", nil)
	if err := f.beginLiveCompletion(a); err != nil {
		t.Fatal(err)
	}
	current := withLabels(a, p.labels("c1"))
	for i := 0; i < 3; i++ {
		if err := f.beginLiveCompletion(current); err != nil {
			t.Fatal(err)
		}
	}
	if msgs := p.sent("c1"); len(msgs) != 1 {
		t.Fatalf("sent=%v", msgs)
	}
	f.SetAgents([]Agent{current})
	for i := 0; i < 3; i++ {
		f.Tick()
	}
	if sends := m.sends(); len(sends) != 0 {
		t.Fatalf("sent approval email for already published work: %+v", sends)
	}
}

func TestPublicationUnknownPreservesFreshWork(t *testing.T) {
	for _, name := range []string{"fetch failure", "missing object", "confirmed absent"} {
		t.Run(name, func(t *testing.T) {
			a := freshCard(false, reviewLabels(map[string]string{labelLandedVerified: "1"}))
			f, _ := newTestFactory(t, a)
			headIs(f, reviewedSHA, nil)
			if name == "fetch failure" {
				f.Git = func(context.Context, string, ...string) error { return errors.New("fetch failed") }
			}
			if name == "missing object" {
				prior := f.GitOut
				f.GitOut = func(ctx context.Context, cwd string, args ...string) (string, error) {
					if args[0] == "merge-base" {
						return "", errors.New("object unavailable")
					}
					return prior(ctx, cwd, args...)
				}
			}
			disposition, hold := f.assessReview(a, a.Cwd)
			if name == "confirmed absent" {
				if disposition != reviewEligible || hold != nil {
					t.Fatalf("disposition=%v hold=%v", disposition, hold)
				}
			} else if disposition != reviewEligible || hold != nil {
				t.Fatalf("disposition=%v hold=%v", disposition, hold)
			}
		})
	}
}

func TestLiveCompletionTickFinishesWithoutFactoryMail(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelLiveCompletion: reviewedSHA, labelLandedVerified: "1", labelRemainingAsk: "1"}))
	f, p, m := mailFactory(t, a)
	stubPublished(f, reviewedSHA, "", nil)
	storeLog(t, f, p, "c1", "DECISION: None.\nOVERVIEW: Published work checked.\nNEEDS FROM ZACH: Nothing.\n\nREMAINING: none")
	for i := 0; i < 5; i++ {
		f.Tick()
		a = withLabels(a, p.labels("c1"))
		f.SetAgents([]Agent{a})
	}
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Fatalf("labels=%v", l)
	}
	if sends := m.sends(); len(sends) != 0 {
		t.Fatalf("sends=%+v", sends)
	}
}

func TestNoOwnCommitsPreservesApproval(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelBase: reviewedSHA}))
	f, p := newTestFactory(t, a, reviewer("r1", "c1", "code"))
	stubPublished(f, reviewedSHA, "", nil)
	storeLog(t, f, p, "r1", "TIER: risky\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelLiveCompletion] != "" {
		t.Fatalf("labels=%v", l)
	}
}

func TestPublishedCompletionUsesNormalEmailRules(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelLandedVerified: "1"}))
	a.Labels[gmailThreadLabel] = "test-thread"
	f, p := newTestFactory(t, a)
	if err := f.beginLiveCompletion(a); err != nil {
		t.Fatal(err)
	}
	msg := strings.Join(p.sent("c1"), "")
	if !strings.Contains(msg, strings.Join(strings.Fields(plainEmailRule), " ")) || !strings.Contains(msg, "archive the thread") {
		t.Fatal(msg)
	}
}
func TestLiveCompletionMarkerClearedOnChangedWork(t *testing.T) {
	a := freshCard(false, reviewLabels(map[string]string{labelStage: "landing", labelLiveCompletion: reviewedSHA, labelSince: ago(10 * time.Minute)}))
	f, p := newTestFactory(t, a)
	stubPublished(f, movedSHA, "", nil)
	storeLog(t, f, p, "c1", "REMAINING: none")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelLiveCompletion] != "" {
		t.Fatalf("labels=%v", l)
	}
}

func TestApproveWithFetchFailureStillStartsNormalLanding(t *testing.T) {
	a := freshCard(false, approvedLabels(nil))
	f, p := newTestFactory(t, a)
	headIs(f, reviewedSHA, nil)
	f.Git = func(context.Context, string, ...string) error { return errors.New("offline") }
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelLiveCompletion] != "" {
		t.Fatalf("labels=%v", l)
	}
	if msg := strings.Join(p.sent("c1"), ""); !strings.Contains(msg, "Land") {
		t.Fatal(msg)
	}
}
