package factory

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quotaCard(stage string) Agent {
	a := card("quota", map[string]string{labelStage: stage, labelAutoReview: "code", labelSince: ago(time.Hour)})
	a.Status, a.LastError = "error", "Individual quota reached. Resets in 10m18s."
	a.LastActivityAt, a.LastUserMessageAt = t0.Add(-time.Minute), t0.Add(-time.Hour)
	return a
}

func TestLimitReset(t *testing.T) {
	for _, tc := range []struct {
		text  string
		after time.Duration
	}{
		{"Individual quota reached. Resets in 10m18s.", 10*time.Minute + 18*time.Second},
		{"Individual quota reached. Please upgrade your subscription. Resets in 9m22s.", 9*time.Minute + 22*time.Second},
		{"API error 429: Subscription quota exhausted. Your usage window resets at " + t0.Add(time.Hour).Format(time.RFC3339) + ". (rate_limit_error)", time.Hour},
	} {
		got, ok := limitReset(tc.text, t0)
		if !ok || !got.Equal(t0.Add(tc.after)) {
			t.Errorf("reset = %v %v for %q", got, ok, tc.text)
		}
	}
	for _, text := range []string{"quota", "API error 429: retry later", "Individual quota reached. Resets in tomorrow.", "resets at " + t0.Format(time.RFC3339), "Individual quota reached. Resets in -1m."} {
		if _, ok := limitReset(text, t0); ok {
			t.Errorf("accepted unknown reset: %q", text)
		}
	}
}

func TestLimitWaitSurvivesRestartAndLabelActivity(t *testing.T) {
	a := quotaCard("building")
	f, p := newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 {
		t.Fatal("resumed before reset")
	}
	labels := p.labels(a.ID)
	r := readLimit(withLabels(a, labels))
	want := a.LastActivityAt.Add(10*time.Minute + 18*time.Second + limitMargin)
	if !r.Reset.Equal(want) {
		t.Fatalf("reset %v want %v", r.Reset, want)
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
	if len(p.sent(a.ID)) != 1 {
		t.Fatalf("messages = %v", p.sent(a.ID))
	}
	// Even a poll containing the original labels must not send twice.
	if err := f.tickAgent(a, want.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 1 {
		t.Fatal("duplicate send from stale snapshot")
	}
}

func TestLimitRepeatedErrorAndManualResume(t *testing.T) {
	for _, stage := range []string{"plan", "building", "landing"} {
		t.Run(stage, func(t *testing.T) {
			a := quotaCard(stage)
			a.LastActivityAt = t0.Add(-time.Hour)
			f, p := newTestFactory(t, a)
			if err := f.tickAgent(a, t0); err != nil {
				t.Fatal(err)
			}
			a = withLabels(a, p.labels(a.ID))
			a.LastUserMessageAt = t0
			a.LastActivityAt = t0.Add(time.Minute)
			if err := f.tickAgent(a, t0.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			a = withLabels(a, p.labels(a.ID))
			f.SetAgents([]Agent{a})
			if a.L(labelStage) != "zach" {
				t.Fatalf("not escalated: %v", p.labels(a.ID))
			}
			ask := BuildBoard([]Agent{a}, nil, t0)[0].Ask
			if ask == nil || actionIDs(ask) != "resume,reply,close" || ask.Yes != "" || !strings.Contains(ask.Why, a.LastError) {
				t.Fatalf("ask = %+v", ask)
			}
			if _, err := f.Act("resume", Body{ID: a.ID}); err != nil {
				t.Fatal(err)
			}
			l := p.labels(a.ID)
			resumed := withLabels(a, l)
			if resumed.L(labelStage) != stage || resumed.L(labelAutoReview) != "code" {
				t.Fatalf("stage or queued review lost: %v", resumed.Labels)
			}
			if len(p.sent(a.ID)) != 2 {
				t.Fatal("manual resume not sent")
			}
			a = withLabels(a, l)
			a.LastUserMessageAt = t0.Add(2 * time.Minute)
			if err := f.tickAgent(a, t0.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if p.labels(a.ID)[labelStage] != "zach" || len(p.sent(a.ID)) != 2 {
				t.Fatal("manual retry rearmed automatic recovery")
			}
		})
	}
}

func TestLimitUnknownAndProtectedCards(t *testing.T) {
	a := quotaCard("building")
	a.LastError = "Unrecognized provider error"
	f, p := newTestFactory(t, a)
	f.tickAgent(a, t0)
	if p.labels(a.ID)[labelStage] != "zach" || len(p.sent(a.ID)) != 0 {
		t.Fatal("unknown error did not escalate")
	}
	for _, stage := range []string{"zach", "landed", ""} {
		a := quotaCard(stage)
		f, p := newTestFactory(t, a)
		f.tickAgent(a, t0)
		if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelLimit] != "" {
			t.Fatalf("touched protected stage %q", stage)
		}
	}
	a = quotaCard("building")
	a.ArchivedAt = t0
	f, p = newTestFactory(t, a)
	f.tickAgent(a, t0)
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelLimit] != "" {
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

type failLimitSend struct{ *fakePaseo }

func (p failLimitSend) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "send" {
		p.fakePaseo.Run(ctx, args...)
		return "", errors.New("delivery uncertain")
	}
	return p.fakePaseo.Run(ctx, args...)
}

func TestLimitUncertainSendNeverRetries(t *testing.T) {
	a := quotaCard("building")
	a.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, a)
	f.p = failLimitSend{p}
	f.tickAgent(a, t0)
	a = withLabels(a, p.labels(a.ID))
	f, p2 := newTestFactory(t, a) // restart with persisted attempt
	f.tickAgent(a, t0.Add(3*time.Minute))
	if len(p.sent(a.ID)) != 1 || len(p2.sent(a.ID)) != 0 || a.L(labelStage) != "zach" {
		t.Fatal("uncertain send repeated or not escalated")
	}
}

func TestLimitAttemptSurvivesCrashBeforeSend(t *testing.T) {
	a := quotaCard("landing")
	f, p := newTestFactory(t, a)
	r := limitRecovery{Stage: "landing", Phase: "attempted", Attempt: t0, UserAt: a.LastUserMessageAt}
	if err := f.saveLimit(a, r, nil); err != nil {
		t.Fatal(err)
	}
	a = withLabels(a, p.labels(a.ID))
	f, p = newTestFactory(t, a)
	if err := f.tickAgent(a, t0.Add(limitGrace)); err != nil {
		t.Fatal(err)
	}
	if len(p.sent(a.ID)) != 0 || p.labels(a.ID)[labelStage] != "zach" {
		t.Fatal("repeated an uncertain attempt")
	}
}

func TestLimitReplyRestoresLandingAndDisallowsDoubleResume(t *testing.T) {
	a := quotaCard("landing")
	a.LastError = "Unknown error"
	f, p := newTestFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	a = withLabels(a, p.labels(a.ID))
	f.SetAgents([]Agent{a})
	if _, err := f.Act("send", Body{ID: a.ID, Message: "Check what already landed first"}); err != nil {
		t.Fatal(err)
	}
	if p.labels(a.ID)[labelStage] != "landing" || len(p.sent(a.ID)) != 1 {
		t.Fatal("reply lost landing stage")
	}
	if _, err := f.Act("resume", Body{ID: a.ID}); err == nil {
		t.Fatal("duplicate resume accepted")
	}
}

func TestLimitResumeHTTPAndErrorReviewerRerun(t *testing.T) {
	a := quotaCard("building")
	a.LastError = "Unknown error"
	f, p := newTestFactory(t, a)
	f.tickAgent(a, t0)
	f.SetAgents([]Agent{withLabels(a, p.labels(a.ID))})
	req := httptest.NewRequest("POST", "/api/resume", strings.NewReader(`{"id":"quota"}`))
	req.Header.Set("X-Factory", "1")
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, req)
	if w.Code != 200 || len(p.sent(a.ID)) != 1 {
		t.Fatalf("resume HTTP = %d %s", w.Code, w.Body)
	}
	c := card("parent", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "review"})
	r := reviewer("failed-review", c.ID, "code")
	r.Status = "error"
	f, p = newTestFactory(t, c, r)
	if _, err := f.Act("rerun", Body{ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if !p.called("archive", r.ID) || p.labels(r.ID)[labelApplied] != "1" || len(p.runsWithLabel(labelArg(labelReviewOf, c.ID))) != 1 {
		t.Fatal("error reviewer was not replaced")
	}
}

type failLimitWrite struct{ *fakePaseo }

func (p failLimitWrite) Run(ctx context.Context, args ...string) (string, error) {
	if args[0] == "agent" {
		return "", errors.New("label write failed")
	}
	return p.fakePaseo.Run(ctx, args...)
}

func TestLimitPersistenceFailureAndInspectFailureDoNotSend(t *testing.T) {
	for _, failure := range []string{"persist", "inspect"} {
		a := quotaCard("building")
		a.LastActivityAt = t0.Add(-time.Hour)
		f, p := newTestFactory(t, a)
		if failure == "persist" {
			f.p = failLimitWrite{p}
		} else {
			p.inspectErr = errors.New("inspect failed")
		}
		if err := f.tickAgent(a, t0); err == nil {
			t.Fatalf("%s: failure ignored", failure)
		}
		if len(p.sent(a.ID)) != 0 {
			t.Fatalf("%s: unsafe send", failure)
		}
	}
}

func TestLimitCompletedTurnAllowsNextEpisode(t *testing.T) {
	a := quotaCard("building")
	a.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, a)
	f.tickAgent(a, t0)
	a = withLabels(a, p.labels(a.ID))
	a.Status = "idle"
	a.LastUserMessageAt = t0
	a.LastActivityAt = t0.Add(time.Second)
	// Exercise the recovery hook without advancing the successful work to review.
	if _, err := f.tickLimit(a, t0.Add(time.Minute)); err != nil {
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

func TestLimitEscalationEmailsWithoutStartingQueuedReview(t *testing.T) {
	a := quotaCard("building")
	a.LastError = "Unknown provider failure"
	f, p, m := mailFactory(t, a)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	a = withLabels(a, p.labels(a.ID))
	f.SetAgents([]Agent{a})
	if err := f.tickAgent(a, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(m.sends()) != 1 || len(p.runsWithLabel(labelArg(labelRole, "reviewer"))) != 0 {
		t.Fatal("error wait skipped notification or started a review")
	}
}

func TestErrorReviewerOffersRerun(t *testing.T) {
	c := card("parent", map[string]string{labelStage: "review", labelSince: ago(time.Hour)})
	c.LastActivityAt = t0.Add(-time.Hour)
	r := reviewer("failed-review", "parent", "code")
	r.Status = "error"
	f, p := newTestFactory(t, c, r)
	f.Tick()
	if reviewPending([]Agent{r}, c.ID, t0) || p.labels(c.ID)[labelStage] != "zach" {
		t.Fatal("error reviewer blocks parent")
	}
	ask := BuildBoard([]Agent{withLabels(c, p.labels(c.ID)), r}, nil, t0)[0].Ask
	if ask == nil || actionIDs(ask) != "rerun,reply,close" {
		t.Fatalf("ask = %+v", ask)
	}
}

func TestErroredIdeaReleasesCapacity(t *testing.T) {
	a := quotaCard("building")
	f, p := newTestFactory(t, a)
	f.cfg.IdeaWIP = 1
	if err := f.ideas.save([]Idea{{ID: "started", Status: "started", Card: a.ID, StartedAt: t0.Add(-time.Hour), StartedBy: "auto"}, {ID: "waiting", Status: "waiting", Request: "Improve checks", Repo: "example/desk"}}); err != nil {
		t.Fatal(err)
	}
	f.cfg.IdeaDaily = 1
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelOrigin, "idea"))) != 0 {
		t.Fatal("daily cap bypassed")
	}
	f.cfg.IdeaDaily = 2
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelOrigin, "idea"))) != 1 {
		t.Fatal("errored idea consumed capacity")
	}
}
