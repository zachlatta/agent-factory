package factory

import (
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
