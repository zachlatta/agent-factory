package factory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

// failedAgain is card a after the attempt it was sent failed with the same error.
func failedAgain(a Agent, labels map[string]string, at time.Time) Agent {
	a = withLabels(a, labels)
	a.Status = "error"
	a.LastUserMessageAt, a.LastActivityAt = at, at.Add(time.Second)
	return a
}

func TestErrorWaitSurvivesRestartAndLabelActivity(t *testing.T) {
	a := quotaCard("building")
	f, p := newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 {
		t.Fatal("resumed before reset")
	}
	labels := p.labels(a.ID)
	r := readRecovery(withLabels(a, labels))
	want := a.LastActivityAt.Add(10*time.Minute + 18*time.Second + limitMargin)
	if !r.Reset.Equal(want) || !r.FromReset {
		t.Fatalf("reset %v want %v (%+v)", r.Reset, want, r)
	}
	a = withLabels(a, labels)
	a.LastActivityAt = t0.Add(time.Hour) // label writes are activity, not another error
	f, p = newTestFactory(t, a)
	if err := f.tickAgent(a, want.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 {
		t.Fatal("early resume after restart")
	}
	if err := f.tickAgent(a, want); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent(a.ID); len(msgs) != 1 || msgs[0] != limitResumeMessage {
		t.Fatalf("messages = %v", msgs)
	}
	// Even a poll containing the original labels must not send twice.
	if err := f.tickAgent(a, want.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 1 {
		t.Fatal("duplicate send from stale snapshot")
	}
}

// A known reset resumes once on the same model, then the error retries after 2, 10 and 30
// minutes, then the card is marked stuck in its column: never Needs Zach, never an email.
func TestErrorRetriesThenStuckWithoutEmail(t *testing.T) {
	for _, stage := range []string{"plan", "building", "landing"} {
		t.Run(stage, func(t *testing.T) {
			a := quotaCard(stage)
			a.LastActivityAt = t0.Add(-time.Hour) // the reset is already past
			f, p, m := mailFactory(t, a)
			now := t0
			tick := func() {
				t.Helper()
				f.SetAgents([]Agent{a})
				if err := f.tickAgent(a, now); err != nil {
					t.Fatal(err)
				}
			}
			tick()
			if msgs := p.sent(a.ID); len(msgs) != 1 || msgs[0] != limitResumeMessage {
				t.Fatalf("reset resume = %v", msgs)
			}
			for n, wait := range errorBackoff {
				// the attempt failed the same way
				a = failedAgain(a, p.labels(a.ID), now)
				now = now.Add(limitGrace)
				tick() // plans the next try
				if len(p.sent(a.ID)) != n+1 {
					t.Fatalf("try %d sent early: %v", n+1, p.sent(a.ID))
				}
				a = withLabels(a, p.labels(a.ID))
				if r := readRecovery(a); r.FromReset || !r.Reset.Equal(now.Add(wait)) {
					t.Fatalf("try %d: next at %v, want %v after the failure", n+1, r.Reset, wait)
				}
				now = now.Add(wait)
				tick()
				if msgs := p.sent(a.ID); len(msgs) != n+2 || msgs[n+1] != errorRetryMessage {
					t.Fatalf("try %d: messages = %v", n+1, msgs)
				}
			}
			a = failedAgain(a, p.labels(a.ID), now)
			now = now.Add(limitGrace)
			tick()
			l := p.labels(a.ID)
			if l[labelStuck] != stuckAgentError || (l[labelStage] != "" && l[labelStage] != stage) || len(p.sent(a.ID)) != 4 {
				t.Fatalf("not marked stuck in %s: %v, sent %v", stage, l, p.sent(a.ID))
			}
			a = withLabels(a, l)
			now = now.Add(time.Hour)
			tick()
			if len(p.sent(a.ID)) != 4 || len(m.sends()) != 0 {
				t.Fatalf("stuck card retried or emailed: %v %v", p.sent(a.ID), m.sends())
			}
			if n := len(p.runsWithLabel(labelArg(labelRole, "reviewer"))); n != 0 || p.labels(a.ID)[labelAutoReview] == "" && a.L(labelAutoReview) == "" {
				t.Fatalf("an errored card started its queued review (%d) or lost it", n)
			}
			f.SetAgents([]Agent{a})
			c, _ := f.card(a.ID)
			if c.Stage != stage || c.Ask == nil || actionIDs(c.Ask) != "resume,reply,close" || !strings.Contains(c.Ask.Why, a.LastError) ||
				!slices.Contains(c.Flags, stuckFlagPrefix+stuckAgentError) {
				t.Fatalf("board: stage %s flags %v ask %+v", c.Stage, c.Flags, c.Ask)
			}

			// Resume makes one attempt; its failure marks the card stuck again without rearming retries.
			if _, err := f.Act("resume", Body{ID: a.ID}); err != nil {
				t.Fatal(err)
			}
			if l := p.labels(a.ID); len(p.sent(a.ID)) != 5 || l[labelStuck] != "" || l[labelStage] != stage {
				t.Fatalf("resume: %v %v", l, p.sent(a.ID))
			}
			if _, err := f.Act("resume", Body{ID: a.ID}); err == nil {
				t.Fatal("duplicate resume accepted")
			}
			a = failedAgain(a, p.labels(a.ID), now)
			now = now.Add(limitGrace)
			tick()
			now = now.Add(time.Hour)
			a = withLabels(a, p.labels(a.ID))
			tick()
			if l := p.labels(a.ID); l[labelStuck] != stuckAgentError || len(p.sent(a.ID)) != 5 {
				t.Fatalf("manual resume rearmed retries: %v %v", l, p.sent(a.ID))
			}
		})
	}
}

func TestUnknownErrorStartsTimedRetries(t *testing.T) {
	a := quotaCard("building")
	a.LastError = "Unrecognized provider error"
	f, p := newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	l := p.labels(a.ID)
	if l[labelStage] != "" || l[labelStuck] != "" || len(p.sent(a.ID)) != 0 {
		t.Fatalf("unknown error escalated or sent at once: %v %v", l, p.sent(a.ID))
	}
	r := readRecovery(withLabels(a, l))
	if r.FromReset || !r.Reset.Equal(a.LastActivityAt.Add(errorBackoff[0])) {
		t.Fatalf("first retry at %v", r.Reset)
	}
	a = withLabels(a, l)
	if err := f.tickAgent(a, r.Reset); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent(a.ID); len(msgs) != 1 || msgs[0] != errorRetryMessage {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestErrorRecoveryLeavesProtectedCards(t *testing.T) {
	for _, stage := range []string{"zach", "landed", ""} {
		a := quotaCard(stage)
		f, p := newTestFactory(t, a)
		f.tickAgent(a, t0)
		if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelRecovery] != "" {
			t.Fatalf("touched protected stage %q", stage)
		}
	}
	a := quotaCard("building")
	a.ArchivedAt = t0
	f, p := newTestFactory(t, a)
	f.tickAgent(a, t0)
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelRecovery] != "" {
		t.Fatal("touched archived card")
	}
	for _, state := range []string{"running", "archived"} {
		a = quotaCard("landing")
		a.LastActivityAt = t0.Add(-time.Hour)
		f, p = newTestFactory(t, a)
		if state == "running" {
			p.status[a.ID] = "running"
		} else {
			p.archived[a.ID] = true
		}
		f.tickAgent(a, t0)
		if len(p.sent(a.ID)) != 0 {
			t.Fatalf("resumed live %s agent", state)
		}
	}
}

type failSend struct{ *fakePaseo }

func (p failSend) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "send" {
		p.fakePaseo.Run(ctx, args...)
		return "", errors.New("delivery uncertain")
	}
	return p.fakePaseo.Run(ctx, args...)
}

// An uncertain send is never repeated at once, even after a restart: the next try waits its turn.
func TestUncertainSendWaitsItsTurn(t *testing.T) {
	a := quotaCard("building")
	a.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, a)
	f.p = failSend{p}
	f.tickAgent(a, t0)
	a = withLabels(a, p.labels(a.ID))
	f, p2 := newTestFactory(t, a) // restart with the persisted attempt
	f.tickAgent(a, t0.Add(time.Minute))
	if len(p.sent(a.ID)) != 1 || len(p2.sent(a.ID)) != 0 {
		t.Fatal("uncertain send repeated within the grace")
	}
	f.tickAgent(a, t0.Add(limitGrace))
	a = withLabels(a, p2.labels(a.ID))
	if r := readRecovery(a); r.Phase != "waiting" || !r.Reset.After(t0.Add(limitGrace)) || a.L(labelStage) == "zach" {
		t.Fatalf("after the grace: %+v %v", r, a.Labels)
	}
}

func TestAttemptSurvivesCrashBeforeSend(t *testing.T) {
	a := quotaCard("landing")
	f, p := newTestFactory(t, a)
	r := recovery{Stage: "landing", Phase: "attempted", Attempt: t0, UserAt: a.LastUserMessageAt, Error: errorFingerprint(a), ResetUsed: true}
	if err := f.saveRecovery(a, r, nil); err != nil {
		t.Fatal(err)
	}
	a = withLabels(a, p.labels(a.ID))
	f, p = newTestFactory(t, a)
	if err := f.tickAgent(a, t0.Add(limitGrace)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelStage] == "zach" {
		t.Fatal("repeated an uncertain attempt at once")
	}
}

func TestReplyOnErrorStuckCardKeepsItsStage(t *testing.T) {
	a := quotaCard("landing")
	a.Labels[labelPrefix+labelStuck] = stuckAgentError
	b, _ := json.Marshal(recovery{Stage: "landing", Phase: "exhausted", Error: errorFingerprint(a)})
	a.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	f, p := newTestFactory(t, a)
	if _, err := f.Act("send", Body{ID: a.ID, Message: "Check what already landed first"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels(a.ID); l[labelStage] != "landing" || l[labelStuck] != "" || len(p.sent(a.ID)) != 1 {
		t.Fatalf("reply: %v %v", l, p.sent(a.ID))
	}
	if _, err := f.Act("resume", Body{ID: a.ID}); err == nil {
		t.Fatal("resume accepted while the reply is in flight")
	}
}

func TestResumeHTTP(t *testing.T) {
	a := quotaCard("building")
	a.Labels[labelPrefix+labelStuck] = stuckAgentError
	b, _ := json.Marshal(recovery{Stage: "building", Phase: "exhausted"})
	a.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	f, p := newTestFactory(t, a)
	req := httptest.NewRequest("POST", "/api/resume", strings.NewReader(`{"id":"quota"}`))
	req.Header.Set("X-Factory", "1")
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != 200 || len(p.sent(a.ID)) != 1 {
		t.Fatalf("resume HTTP = %d %s", w.Code, w.Body)
	}
}

func TestCompletedTurnAllowsNextEpisode(t *testing.T) {
	a := quotaCard("building")
	a.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, a)
	f.tickAgent(a, t0)
	a = withLabels(a, p.labels(a.ID))
	a.Status = "idle"
	a.LastUserMessageAt = t0
	a.LastActivityAt = t0.Add(time.Second)
	// Exercise the recovery hook without advancing the successful work to review.
	if _, err := f.tickError(a, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stale := quotaCard("building")
	if err := f.tickAgent(stale, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 1 {
		t.Fatal("stale error reopened completed episode")
	}
	a = withLabels(a, p.labels(a.ID))
	if r := readRecovery(a); r.ResetUsed || r.Timed != 0 {
		t.Fatalf("completed episode kept its tries: %+v", r)
	}
	a.Status = "error"
	a.LastActivityAt = t0.Add(time.Hour)
	a.LastUserMessageAt = t0.Add(time.Hour)
	if err := f.tickAgent(a, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 2 {
		t.Fatal("successful turn did not close recovery episode")
	}
}

func TestOldLimitLabelIsReadThenCleared(t *testing.T) {
	a := quotaCard("building")
	b, _ := json.Marshal(map[string]any{"Revision": 3, "Stage": "building", "Phase": "waiting",
		"Error": errorFingerprint(a), "Reset": t0.Add(time.Minute), "UserAt": a.LastUserMessageAt})
	a.Labels[labelPrefix+legacyLimitLabel] = base64.RawURLEncoding.EncodeToString(b)
	f, p := newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 {
		t.Fatal("old wait not kept")
	}
	if err := f.tickAgent(a, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	l := p.labels(a.ID)
	if len(p.sent(a.ID)) != 1 || l[legacyLimitLabel] != "" || l[labelRecovery] == "" {
		t.Fatalf("old label: sent %v, labels %v", p.sent(a.ID), l)
	}
}

func TestErrorReviewerIsReplaced(t *testing.T) {
	c := card("parent", map[string]string{labelStage: "review", labelSince: ago(time.Hour)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("failed-review", "parent", "code")
	r.Status = "error"
	f, p, m := mailFactory(t, c, r)
	f.Tick()
	if reviewPending([]Agent{r}, c.ID, t0) {
		t.Fatal("error reviewer counts as pending")
	}
	l := p.labels(c.ID)
	if l[labelStage] != "review" || l[labelReviewerRetries] != "1" || !p.called("archive", r.ID) || p.labels(r.ID)[labelApplied] != "1" ||
		len(p.runsWithLabel(labelArg(labelReviewOf, c.ID))) != 1 || len(m.sends()) != 0 {
		t.Fatalf("error reviewer not replaced: %v %v", l, p.calls)
	}
}

func TestBlockedReviewerIsReplaced(t *testing.T) {
	for _, why := range []string{"permission", "stalled"} {
		c := card("parent", map[string]string{labelStage: "plan-review", labelSince: ago(time.Hour)})
		c.LastActivityAt = t0.Add(-time.Hour)
		r := reviewer("blocked-review", "parent", "plan")
		r.Status, r.LastActivityAt = "running", t0.Add(-(stallMinutes+1)*time.Minute)
		if why == "permission" {
			r.Pending, r.LastActivityAt = "Bash: rm -rf x", t0.Add(-stuckAfter)
		}
		f, p := newTestFactory(t, c, r)
		f.Tick()
		if l := p.labels(c.ID); l[labelReviewerRetries] != "1" || !p.called("archive", r.ID) ||
			len(p.runsWithLabel(labelArg(labelReviewOf, c.ID))) != 1 {
			t.Errorf("%s reviewer not replaced: %v %v", why, l, p.calls)
		}
	}
}

func recoveryLandingCard(repo string) Agent {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelReviewed: "h1",
		labelBase: "b0", labelTier: "low", labelPlanRounds: "1"})
	c.Repo = repo
	return c
}

func TestLandingNotVerifiedReturnsToBuildingThenStuck(t *testing.T) {
	c := recoveryLandingCard("example/desk")
	c.Cwd = t.TempDir()
	f, p, m := mailFactory(t, c)
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "h2\n"})
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "merge-base" {
			return errors.New("exit status 1: rebase conflict in a.go")
		}
		return nil
	}
	storeLog(t, f, p, "c1", "Landing failed: conflict.")
	for n := 1; n <= landReturnCap; n++ {
		f.Tick()
		l := p.labels("c1")
		msgs := p.sent("c1")
		if l[labelStage] != "building" || l[labelAutoReview] != "code" || len(msgs) != n {
			t.Fatalf("return %d: %v %v", n, l, msgs)
		}
		for _, want := range []string{"rebase conflict in a.go", "Rebase on origin/main", "secret scan hit", "git rebase -i"} {
			if !strings.Contains(msgs[n-1], want) {
				t.Errorf("message lacks %q: %s", want, msgs[n-1])
			}
		}
		if r := readRecovery(withLabels(c, l)); r.Lands != n {
			t.Errorf("lands = %d", r.Lands)
		}
		// code review passed again and the land prompt went out: it failed again
		c = withLabels(c, l)
		c.Labels[labelPrefix+labelStage] = "landing"
		c.Labels[labelPrefix+labelAutoReview] = ""
		c.Labels[labelPrefix+labelSince] = ago(10 * time.Minute)
		f.SetAgents([]Agent{c})
	}
	f.Tick()
	if l := p.labels("c1"); l[labelStuck] != stuckLanding || len(p.sent("c1")) != landReturnCap {
		t.Fatalf("not stuck in landing: %v", l)
	}
	if len(m.sends()) != 0 {
		t.Errorf("emailed Zach: %v", m.sends())
	}
}

func TestLandedDiffDiffersOnAPublicRepo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hits    []scan.Hit
		scanErr error
		base    string
		toZach  bool
	}{
		{name: "clean scan", base: "b0"},
		{name: "scan hit", base: "b0", hits: []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, toZach: true},
		{name: "scan error", base: "b0", scanErr: errors.New("gitleaks missing"), toZach: true},
		{name: "no base", toZach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := recoveryLandingCard(publicRepo)
			c.Cwd = t.TempDir()
			if tc.base == "" {
				delete(c.Labels, labelPrefix+labelBase)
			}
			f, p, m := mailFactory(t, c)
			ranges := fakeScan(f, tc.hits, tc.scanErr)
			f.GitOut = gitStub(withAnswers(landedExtraGit, "rev-parse HEAD", "n3\n"))
			storeLog(t, f, p, "c1", "Landed.\nREMAINING: none")
			f.Tick()
			l := p.labels("c1")
			if tc.base != "" && (len(*ranges) != 1 || (*ranges)[0] != "b0..HEAD") {
				t.Errorf("scanned %v, want the landed range", *ranges)
			}
			if tc.toZach {
				if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], landDiffNotePrefix) || len(p.sent("c1")) != 0 {
					t.Fatalf("labels %v sent %v", l, p.sent("c1"))
				}
				f.SetAgents([]Agent{withLabels(c, l)})
				f.Tick()
				if len(m.sends()) != 1 {
					t.Errorf("Zach not emailed: %v", m.sends())
				}
				return
			}
			if l[labelStage] != "building" || len(p.sent("c1")) != 1 || len(m.sends()) != 0 {
				t.Fatalf("clean scan: labels %v sent %v mails %v", l, p.sent("c1"), m.sends())
			}
			// the land prompt after the next code review says what landed may already be on main
			if msg := f.landMsgFor(withLabels(c, l)); !strings.HasPrefix(msg, landedDiffLandNote) || !strings.Contains(msg, "do not push again") {
				t.Errorf("land prompt = %q", msg)
			}
		})
	}
}

func TestLandedDiffRecoveryReviewsPublishedHead(t *testing.T) {
	c := card("c1", map[string]string{
		labelStage: "building", labelAutoReview: "code", labelPlanRounds: "1",
		labelSince: ago(10 * time.Minute), labelLandedVerified: "1", labelReviewed: "h1",
	})
	b, _ := json.Marshal(recovery{Lands: 1, LandedDiff: true})
	c.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{
		"rev-list origin/main..HEAD": "", "rev-parse HEAD": "h2\n", "status --porcelain": "",
	})
	storeLog(t, f, p, c.ID, "The published change needs no further edits.\nREMAINING: none")
	f.Tick()
	if l := p.labels(c.ID); l[labelStage] != "review" || l[labelReviewed] != "h2" || p.runs != 1 {
		t.Fatalf("published recovery skipped review: labels %v runs %d", l, p.runs)
	}
}

func TestReLandAfterRecoveryStillScans(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1", labelCodeRounds: "1",
		labelReviewed: reviewedSHA})
	c.Repo = publicRepo
	b, _ := json.Marshal(recovery{Lands: 1})
	c.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
	ranges := fakeScan(f, []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, nil)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); len(*ranges) != 1 || l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan") || len(p.sent("c1")) != 0 {
		t.Errorf("scan skipped on the re-land: %v %v", *ranges, l)
	}
}

func TestDirectCardNotOnMainIsToldOnceThenStuck(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", "Landed the work item.\nTRACK: direct")
	f.GitOut = gitStub(directOK)
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "merge-base" {
			return errors.New("exit status 1")
		}
		return nil
	}
	f.Tick()
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "land your work item again") {
		t.Fatalf("sent = %v", msgs)
	}
	c = withLabels(c, p.labels("c1"))
	c.Labels[labelPrefix+labelSince] = ago(10 * time.Minute)
	f.SetAgents([]Agent{c})
	f.Tick()
	if l := p.labels("c1"); l[labelStuck] != stuckLanding || l[labelStage] == "zach" || len(p.sent("c1")) != 1 || len(m.sends()) != 0 {
		t.Fatalf("labels %v sent %v", l, p.sent("c1"))
	}
}

// A handoff to a replacement agent keeps the recovery counts, so it cannot reset a cap. The stuck
// marker is not carried: the replacement is the next recovery step.
func TestHandoffCarriesRecoveryCounts(t *testing.T) {
	old := card("old", map[string]string{labelStage: "landing", labelReviewerRetries: "2", labelStuck: stuckLanding})
	b, _ := json.Marshal(recovery{Lands: landReturnCap, Timed: len(errorBackoff)})
	old.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	carried := copyCarriedLabels(old)
	if carried[labelPrefix+labelReviewerRetries] != "2" || carried[labelPrefix+labelRecovery] == "" {
		t.Fatalf("carried = %v", carried)
	}
	if _, ok := carried[labelPrefix+labelStuck]; ok {
		t.Fatalf("stuck marker carried: %v", carried)
	}
	next := card("new", nil)
	for k, v := range carried {
		next.Labels[k] = v
	}
	if r := readRecovery(next); r.Lands != landReturnCap {
		t.Fatalf("replacement lost the landing returns: %+v", r)
	}
	next.Labels[labelPrefix+labelStage] = "landing"
	next.Labels[labelPrefix+labelSince] = ago(10 * time.Minute)
	next.Cwd = t.TempDir()
	f, p := newTestFactory(t, next)
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "merge-base" {
			return errors.New("exit status 1")
		}
		return nil
	}
	storeLog(t, f, p, "new", "Landing failed.")
	f.Tick()
	if l := p.labels("new"); l[labelStuck] != stuckLanding || l[labelStage] != "" || len(p.sent("new")) != 0 {
		t.Fatalf("a handoff reset the landing cap: %v %v", l, p.sent("new"))
	}
}

func TestStuckMarkerClearsWhenTheCardRunsAgain(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelStuck: stuckStopped})
	c.Status = "running"
	f, p := newTestFactory(t, c)
	f.Tick()
	if v, ok := p.labels("c1")[labelStuck]; !ok || v != "" {
		t.Fatalf("labels = %v", p.labels("c1"))
	}
}

// Cards left in Needs Zach by waits that are now recovered automatically move on their next pass.
func TestOldWaitsMoveToAutomaticRecovery(t *testing.T) {
	exhausted, _ := json.Marshal(map[string]any{"Stage": "landing", "Phase": "blocked"})
	for _, tc := range []struct {
		name   string
		labels map[string]string
		repo   string
		stage  string // the stage label written ("" = none)
		stuck  string
		sends  int
	}{
		{"agent error", map[string]string{labelNote: legacyLimitNote, legacyLimitLabel: base64.RawURLEncoding.EncodeToString(exhausted)}, "", "landing", stuckAgentError, 0},
		{"stuck builder", map[string]string{labelNote: "stuck", labelStuckFrom: "building"}, "", "building", stuckStopped, 0},
		{"stuck reviewer", map[string]string{labelNote: "stuck", labelStuckFrom: "review"}, "", "review", stuckReview, 0},
		{"no verdict", map[string]string{labelNote: legacyNoVerdictNote, labelStuckFrom: "plan-review"}, "", "plan-review", stuckReview, 0},
		{"landing not verified", map[string]string{labelNote: legacyNotVerified}, "", "building", "", 1},
		{"direct not on main", map[string]string{labelNote: legacyNotVerified, labelTrack: "direct"}, "", "plan", stuckLanding, 0},
		{"landed diff, private", map[string]string{labelNote: landDiffNotePrefix + ": extra commit"}, "", "building", "", 1},
		{"landed diff, public", map[string]string{labelNote: landDiffNotePrefix + ": extra commit"}, publicRepo, "", "", 0},
		{"history unavailable stays", map[string]string{labelNote: histWait, labelStuckFrom: "review"}, "", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", tc.labels)
			c.Labels[labelPrefix+labelStage] = "zach"
			if tc.repo != "" {
				c.Repo = tc.repo
			}
			f, p, m := mailFactory(t, c)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != tc.stage || l[labelStuck] != tc.stuck || len(p.sent("c1")) != tc.sends {
				t.Fatalf("labels %v sent %v", l, p.sent("c1"))
			}
			if moved := tc.stage != ""; moved && len(m.sends()) != 0 {
				t.Errorf("moved wait was emailed: %v", m.sends())
			}
		})
	}
}

func TestReplyToAStuckCardClearsTheMark(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelStuck: stuckStopped, labelPlanRounds: "1"})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("send", Body{ID: "c1", Message: "try the other approach"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStuck] != "" || l[labelStage] != "building" || l[labelAutoReview] != "code" || len(p.sent("c1")) != 1 {
		t.Fatalf("labels %v sent %v", l, p.sent("c1"))
	}
}

func TestStuckPickerIsAskedForItsRepoLine(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelRouting: "1", labelSince: ago(10 * time.Minute)})
	c.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c)
	f.Tick()
	if msgs := p.sent("c1"); len(msgs) != 1 || msgs[0] != pickerNudgeMsg {
		t.Fatalf("sent = %v", msgs)
	}
}

func TestStuckAsksAreNeverMailed(t *testing.T) {
	for _, why := range []string{stuckAgentError, stuckReview, stuckStopped, stuckLanding} {
		ask := stuckAsk(card("c", map[string]string{labelStuck: why}))
		if mailable(ask) {
			t.Errorf("%s: stuck ask is mailable", why)
		}
	}
}

type failLabelWrite struct{ *fakePaseo }

func (p failLabelWrite) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "agent" {
		return "", errors.New("label write failed")
	}
	return p.fakePaseo.Run(ctx, args...)
}

// No retry is sent when its attempt cannot be recorded first, or the agent cannot be checked.
func TestRecoveryPersistenceFailureAndInspectFailureDoNotSend(t *testing.T) {
	for _, kind := range []string{"reset", "timed"} {
		for _, failure := range []string{"persist", "inspect"} {
			for _, due := range []bool{false, true} {
				a := quotaCard("building")
				a.LastActivityAt = t0.Add(-time.Hour)
				if kind == "timed" {
					a.LastError = "Unrecognized provider error"
				}
				if due {
					// already waiting and due: the failure hits the attempt itself
					b, _ := json.Marshal(recovery{Stage: "building", Phase: "waiting", Error: errorFingerprint(a), Reset: t0.Add(-time.Second),
						FromReset: kind == "reset", UserAt: a.LastUserMessageAt})
					a.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
				}
				f, p := newTestFactory(t, a)
				if failure == "persist" {
					f.p = failLabelWrite{p}
				} else {
					p.inspectErr = errors.New("inspect failed")
				}
				err := f.tickAgent(a, t0)
				if len(p.sent(a.ID)) != 0 {
					t.Fatalf("%s %s due=%v: unsafe send", kind, failure, due)
				}
				if (failure == "persist" || due) && err == nil {
					t.Fatalf("%s %s due=%v: failure ignored", kind, failure, due)
				}
			}
		}
	}
}
