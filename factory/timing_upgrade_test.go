package factory

import (
	"context"
	"testing"
	"time"
)

func TestFinalLandingUsesVerificationClock(t *testing.T) {
	r := newRepos(t)
	commitAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	t.Setenv("GIT_COMMITTER_DATE", commitAt.Format(time.RFC3339))
	commitFile(t, r.wt, "first", "first\n")
	r.land(t)
	f := realGitFactory(t)
	first := commitAt.Add(20 * time.Minute)
	final := first.Add(20 * time.Minute)
	f.Now = func() time.Time { return first.In(time.FixedZone("local", -4*60*60)) }
	f.timings.cards["c"] = &CardTiming{RequestedAt: commitAt.Add(-time.Minute), Events: []TimingEvent{{Stage: "plan", At: commitAt.Add(-time.Minute)}}}
	f.recordLanded(Agent{ID: "c"}, r.wt)
	if got := timing(t, f, "c").LandedAt; !got.Equal(first) || got.Location() != time.UTC {
		t.Fatalf("first landing = %v", got)
	}
	f.timings.update("c", func(ct *CardTiming) {
		ct.Events = append(ct.Events, TimingEvent{Stage: "landed", At: first}, TimingEvent{Stage: "review", At: first.Add(time.Minute)})
	})
	t.Setenv("GIT_COMMITTER_DATE", first.Add(2*time.Minute).Format(time.RFC3339))
	commitFile(t, r.wt, "second", "second\n")
	r.land(t)
	f.Now = func() time.Time { return final.In(time.FixedZone("local", -4*60*60)) }
	f.recordLanded(Agent{ID: "c"}, r.wt)
	f.timings.update("c", func(ct *CardTiming) { ct.Events = append(ct.Events, TimingEvent{Stage: "landed", At: final}) })
	f.reconcileTimings()
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	f.Now = func() time.Time { return final.Add(time.Hour) }
	f.reconcileTimings()
	got := timing(t, f, "c")
	if !got.LandedAt.Equal(final) || len(got.Events) != 4 || !got.Events[1].At.Equal(first) {
		t.Fatalf("final landing or retained events = %+v", got)
	}
}

func TestMigrationUsesFinalRecordedLanding(t *testing.T) {
	r := newRepos(t)
	commitAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	t.Setenv("GIT_COMMITTER_DATE", commitAt.Format(time.RFC3339))
	commitFile(t, r.wt, "mine", "mine\n")
	r.land(t)
	f := realGitFactory(t)
	final := commitAt.Add(40 * time.Minute)
	f.Now = func() time.Time { return final.Add(time.Hour) }
	f.timings.cards["c"] = &CardTiming{RequestedAt: commitAt.Add(-time.Minute), LandedAt: commitAt, LandingChecked: true, Events: []TimingEvent{{Stage: "landed", At: commitAt.Add(10 * time.Minute)}, {Stage: "review", At: commitAt.Add(20 * time.Minute)}, {Stage: "landed", At: final.In(time.FixedZone("local", -4*60*60))}}}
	f.SetAgents(nil)
	f.GitOut = func(context.Context, string, ...string) (string, error) {
		t.Fatal("verified history must not require a surviving agent or worktree")
		return "", nil
	}
	f.reconcileTimings()
	got := timing(t, f, "c")
	if !got.LandedAt.Equal(final) || got.LandedAt.Location() != time.UTC || len(got.Events) != 3 {
		t.Fatalf("migration = %+v", got)
	}
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	calls := 0
	f.GitOut = func(context.Context, string, ...string) (string, error) {
		calls++
		t.Error("repeated completed migration")
		return "", nil
	}
	f.reconcileTimings()
	if calls != 0 || !timing(t, f, "c").LandedAt.Equal(final) {
		t.Fatal("migration not restart safe")
	}
}

func TestMigrationRetriesMissingAgentAfterRestart(t *testing.T) {
	r := newRepos(t)
	commitAt := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	t.Setenv("GIT_COMMITTER_DATE", commitAt.Format(time.RFC3339))
	commitFile(t, r.wt, "mine", "mine\n")
	r.land(t)
	f := realGitFactory(t)
	final := commitAt.Add(40 * time.Minute)
	f.Now = func() time.Time { return final.Add(time.Hour) }
	f.timings.cards["c"] = &CardTiming{RequestedAt: commitAt.Add(-time.Minute), LandedAt: commitAt,
		Events: []TimingEvent{{Stage: "landed", At: final}}}
	f.timings.dirty = true
	f.reconcileTimings()
	got := timing(t, f, "c")
	if got.Version != 0 || got.LandingChecked || !got.LandedAt.Equal(commitAt) {
		t.Fatalf("missing evidence marked complete: %+v", got)
	}
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	f.SetAgents([]Agent{{ID: "c", Cwd: r.wt}})
	f.reconcileTimings()
	got = timing(t, f, "c")
	if got.Version != timingVersion || !got.LandingChecked || !got.LandedAt.Equal(final) {
		t.Fatalf("migration was not retried: %+v", got)
	}
}
