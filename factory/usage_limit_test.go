package factory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestErroredIdeaReleasesCapacity(t *testing.T) {
	a := quotaCard("building")
	f, p := newTestFactory(t, a)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":1,"daily":1}}}`)
	if err := f.ideas.save([]Idea{{ID: "started", Status: "started", Repo: "example/desk", Card: a.ID, StartedAt: t0.Add(-time.Hour), StartedBy: "auto"}, {ID: "waiting", Status: "waiting", Request: "Improve checks", Repo: "example/desk"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelOrigin, "idea"))) != 0 {
		t.Fatal("daily cap bypassed")
	}
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":1,"daily":2}}}`)
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelOrigin, "idea"))) != 1 {
		t.Fatal("errored idea consumed capacity")
	}
}

func TestErroredCardWhoseWorkLandedGoesToDone(t *testing.T) {
	const doneReport = "DECISION: None. It is landed and live.\nOVERVIEW: The fix is on main.\nNEEDS FROM ZACH: nothing\n\nLanded.\nREMAINING: none"
	r := newRepos(t)
	reviewed := commitFile(t, r.wt, "feature.go", "package feature\n")
	r.otherPushes(t, "other.txt")
	r.land(t)
	wantLandedAt := parseTime(gitRun(t, r.wt, "show", "-s", "--format=%cI", "HEAD"))

	a := card("c1", map[string]string{
		labelStage:        "landing",
		labelReviewed:     reviewed,
		labelAutoReview:   "code",
		labelRemainingAsk: "1",
		labelStuck:        stuckAgentError,
		labelSince:        ago(10 * time.Minute),
	})
	a.Cwd = r.wt
	a.Status = "error"
	a.LastError = "Individual quota reached, please upgrade.\nResets in 9m22s."
	a.LastActivityAt = t0.Add(-time.Minute)
	a.LastUserMessageAt = t0.Add(-5 * time.Minute)

	f, p := newTestFactory(t, a)
	f.Git, f.GitOut = runGit, runGitOut
	f.timings.cards[a.ID] = &CardTiming{RequestedAt: wantLandedAt.Add(-time.Minute)}
	storeLog(t, f, p, a.ID, "[User] Land it.\n"+doneReport+"\n[User] "+limitResumeMessage)

	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	l := p.labels(a.ID)
	wantNote := "landed; Individual quota reached; please upgrade. Resets in 9m22s."
	if v, ok := l[labelStuck]; !ok || v != "" || l[labelStage] != "landed" || l[labelNote] != wantNote || l[labelLandedVerified] != "1" {
		t.Fatalf("labels = %v", l)
	}
	for _, k := range []string{labelAutoReview, labelRemainingAsk, labelStuck} {
		if v, ok := l[k]; !ok || v != "" {
			t.Fatalf("label %s = %q (ok=%v), want empty write; labels = %v", k, v, ok, l)
		}
	}
	if len(p.sent(a.ID)) != 0 || p.runs != 0 {
		t.Fatalf("sent %v runs %d", p.sent(a.ID), p.runs)
	}
	if rec := readRecovery(withLabels(a, l)); rec.Phase != "complete" || !rec.UserAt.Equal(a.LastUserMessageAt) {
		t.Fatalf("recovery = %+v", rec)
	}
	if got := timing(t, f, a.ID).LandedAt; !got.Equal(wantLandedAt) {
		t.Fatalf("LandedAt = %v, want %v", got, wantLandedAt)
	}

	// A stale snapshot from before the label reload does not send a retry or write labels again.
	callsBefore := len(p.calls)
	if err := f.tickAgent(a, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != callsBefore || len(p.sent(a.ID)) != 0 {
		t.Fatalf("stale snapshot acted: calls %d -> %d, sent %v", callsBefore, len(p.calls), p.sent(a.ID))
	}
}

func TestErroredCardNotDoneWhenChecksFail(t *testing.T) {
	const doneReport = "DECISION: None. It is landed and live.\nOVERVIEW: The fix is on main.\nNEEDS FROM ZACH: nothing\n\nLanded.\nREMAINING: none"
	landedOK := withAnswers(landedGit, "rev-parse HEAD", "n2\n")

	for _, tc := range []struct {
		name    string
		labels  map[string]string
		repo    string
		log     string
		gitOut  map[string]string
		gitErr  string
		predLog string
	}{
		{
			name:   "not on origin/main",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    doneReport,
			gitOut: landedOK,
			gitErr: "merge-base",
		},
		{
			name:   "no new commit since base",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1", labelBase: "n2"},
			log:    doneReport,
			gitOut: landedOK,
		},
		{
			name:   "diff differs from reviewed",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    doneReport,
			gitOut: withAnswers(landedExtraGit, "rev-parse HEAD", "n3\n"),
		},
		{
			name:   "reviewed commit missing on origin/main",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    doneReport,
			gitOut: withAnswers(landedOK, "rev-list --no-merges m..origin/main", "o1\n"),
		},
		{
			name:   "guarded repo without reviewed commit",
			labels: map[string]string{labelStage: "landing"},
			repo:   publicRepo,
			log:    doneReport,
			gitOut: landedOK,
		},
		{
			name:   "verdict is code changes",
			labels: map[string]string{labelStage: "building", labelReviewed: "h1", labelVerdict: "code changes"},
			log:    doneReport,
			gitOut: landedOK,
		},
		{
			name:   "remaining items listed",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    "DECISION: None.\nOVERVIEW: Landed.\nNEEDS FROM ZACH: nothing\n\nREMAINING:\n- agent: check prod",
			gitOut: landedOK,
		},
		{
			name:   "missing remaining line",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    "DECISION: None.\nOVERVIEW: Landed.\nNEEDS FROM ZACH: nothing\n\nLanded.",
			gitOut: landedOK,
		},
		{
			name:   "needs from zach asks for action",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    "DECISION: Try it.\nOVERVIEW: Landed.\nNEEDS FROM ZACH: Please check the page.\n\nREMAINING: none",
			gitOut: landedOK,
		},
		{
			name:   "question block present",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    "DECISION: None.\nOVERVIEW: Landed.\nNEEDS FROM ZACH: nothing\n\nQUESTION: Keep it?\n- Yes (recommended)\n- No\n\nREMAINING: none",
			gitOut: landedOK,
		},
		{
			name:   "newer reply after retry lacks remaining none",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    doneReport + "\n[User] " + limitResumeMessage + "\nFound a problem, fixing it now.",
			gitOut: landedOK,
		},
		{
			name:   "newer tool call after retry",
			labels: map[string]string{labelStage: "landing", labelReviewed: "h1"},
			log:    doneReport + "\n[User] " + errorRetryMessage + "\n[Bash] git status",
			gitOut: landedOK,
		},
		{
			name:    "predecessor reply is not used for replacement agent",
			labels:  map[string]string{labelStage: "landing", labelReviewed: "h1", labelReplaces: "prev"},
			log:     "[User] Handoff prompt",
			predLog: doneReport,
			gitOut:  landedOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := card("c1", tc.labels)
			a.Cwd = t.TempDir()
			if tc.repo != "" {
				a.Repo = tc.repo
			}
			a.Status, a.LastError = "error", "Individual quota reached. Resets in 10m."
			a.LastActivityAt, a.LastUserMessageAt = t0.Add(-time.Minute), t0.Add(-time.Hour)
			agents := []Agent{a}
			if tc.predLog != "" {
				prev := card("prev", map[string]string{labelStage: "landing", labelReplacedBy: a.ID})
				prev.ArchivedAt = t0.Add(-2 * time.Minute)
				agents = append(agents, prev)
			}
			f, p := newTestFactory(t, agents...)
			f.GitOut = gitStub(tc.gitOut)
			f.Git = func(_ context.Context, _ string, args ...string) error {
				if tc.gitErr != "" && args[0] == tc.gitErr {
					return errors.New("exit status 1")
				}
				return nil
			}
			storeLog(t, f, p, a.ID, tc.log)
			if tc.predLog != "" {
				storeLog(t, f, p, "prev", tc.predLog)
			}
			if err := f.tickAgent(a, t0); err != nil {
				t.Fatal(err)
			}
			if l := p.labels(a.ID); l[labelStage] == "landed" {
				t.Fatalf("unexpectedly marked landed: %v", l)
			}
		})
	}
}

func TestErroredCardGitCheckRunsOncePerError(t *testing.T) {
	const doneReport = "DECISION: None.\nOVERVIEW: Landed.\nNEEDS FROM ZACH: nothing\n\nREMAINING: none"
	a := card("c1", map[string]string{labelStage: "landing", labelReviewed: "h1", labelSince: ago(10 * time.Minute)})
	a.Cwd = t.TempDir()
	a.Status, a.LastError = "error", "Individual quota reached. Resets in 30m."
	a.LastActivityAt, a.LastUserMessageAt = t0.Add(-time.Minute), t0.Add(-time.Hour)

	f, p := newTestFactory(t, a)
	storeLog(t, f, p, a.ID, doneReport)
	fetches := 0
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "fetch" {
			fetches++
		}
		if args[0] == "merge-base" {
			return errors.New("not ancestor")
		}
		return nil
	}

	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	a = withLabels(a, p.labels(a.ID))
	f.SetAgents([]Agent{a})
	if err := f.tickAgent(a, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("repeated fetch on same error: %d", fetches)
	}

	// Survives a restart via the persisted recovery label.
	f2, p2 := newTestFactory(t, a)
	storeLog(t, f2, p2, a.ID, doneReport)
	f2.Git = f.Git
	if err := f2.tickAgent(a, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("repeated fetch after restart: %d", fetches)
	}

	// A new activity timestamp checks again once.
	a.LastActivityAt = t0.Add(3 * time.Minute)
	f2.SetAgents([]Agent{a})
	if err := f2.tickAgent(a, t0.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fetches != 2 {
		t.Fatalf("fetches after new activity = %d, want 2", fetches)
	}

	// An exhausted (stuck) card checks once and caches in memory without extra label writes.
	b, _ := json.Marshal(recovery{Stage: "landing", Phase: "exhausted", Error: errorFingerprint(a)})
	a.Labels[labelPrefix+labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	a.Labels[labelPrefix+labelStuck] = stuckAgentError
	a.LastActivityAt = t0.Add(5 * time.Minute)
	f3, p3 := newTestFactory(t, a)
	storeLog(t, f3, p3, a.ID, doneReport)
	f3.Git = f.Git
	if err := f3.tickAgent(a, t0.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f3.tickAgent(a, t0.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fetches != 3 {
		t.Fatalf("fetches on exhausted card = %d, want 3", fetches)
	}
}
