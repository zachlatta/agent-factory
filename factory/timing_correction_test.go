package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLandingTimeBelongsToCard(t *testing.T) {
	for _, mode := range []string{"no commit", "unrelated head", "old own commit", "own commit", "rebased", "squash merged", "unlanded", "missing reflog", "git failure"} {
		t.Run(mode, func(t *testing.T) {
			r := newRepos(t)
			f := realGitFactory(t)
			asked := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
			var want time.Time
			if mode == "old own commit" {
				commitFile(t, r.wt, "old", "old\n")
				asked = time.Now().Add(time.Minute)
			} else if mode == "own commit" || mode == "rebased" || mode == "unlanded" {
				commitFile(t, r.wt, "mine", "mine\n")
			} else if mode == "squash merged" {
				commitFile(t, r.wt, "mine1", "mine1\n")
				commitFile(t, r.wt, "mine2", "mine2\n")
			}
			if mode == "unrelated head" || mode == "rebased" {
				r.otherPushes(t, "other")
			}
			if mode == "squash merged" {
				gitRun(t, r.wt, "push", "-q", "origin", "HEAD:refs/heads/card")
				gitRun(t, r.other, "fetch", "-q", "origin", "card")
				gitRun(t, r.other, "merge", "-q", "--squash", "origin/card")
				gitRun(t, r.other, "commit", "-q", "-m", "squash card")
				gitRun(t, r.other, "push", "-q", "origin", "HEAD:main")
				gitRun(t, r.wt, "fetch", "-q", "origin", "main")
				want = f.Now().UTC()
			} else if mode != "unlanded" {
				r.land(t)
			}
			if mode == "own commit" || mode == "rebased" {
				want = f.Now().UTC()
			}
			if mode == "missing reflog" {
				gitRun(t, r.wt, "reflog", "expire", "--expire=all", "HEAD")
			}
			if mode == "git failure" {
				f.GitOut = func(context.Context, string, ...string) (string, error) { return "", errors.New("unavailable") }
			}
			f.timings.cards["c"] = &CardTiming{RequestedAt: asked}
			f.recordLanded(Agent{ID: "c"}, r.wt)
			if got := timing(t, f, "c").LandedAt; !got.Equal(want) {
				t.Fatalf("landing = %v, want %v", got, want)
			}
		})
	}
}

func TestReplyWithoutLandingAndNoReplyStatus(t *testing.T) {
	for _, reply := range []bool{false, true} {
		f, m := replyCard(t, nil)
		f.timings.update("e1", func(ct *CardTiming) { ct.LandedAt = time.Time{} })
		if reply {
			m.threads["th1"] = []threadMsg{msg("answer", t0.Add(-15*time.Minute), true)}
		}
		f.checkReplies(t0)
		ct := timing(t, f, "e1")
		if !ct.ReplyChecked || ct.NoReply == reply || ct.RepliedAt.IsZero() == reply {
			t.Fatalf("reply=%v: %+v", reply, ct)
		}
	}
}

func TestLandingTimeDoesNotUseNewerInheritedCommit(t *testing.T) {
	r := newRepos(t)
	when := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	t.Setenv("GIT_COMMITTER_DATE", when.Format(time.RFC3339))
	commitFile(t, r.wt, "mine", "mine\n")
	r.land(t)
	t.Setenv("GIT_COMMITTER_DATE", when.Add(10*time.Minute).Format(time.RFC3339))
	r.otherPushes(t, "other")
	gitRun(t, r.wt, "fetch", "-q", "origin", "main")
	gitRun(t, r.wt, "merge", "-q", "--ff-only", "origin/main")
	f := realGitFactory(t)
	f.timings.cards["c"] = &CardTiming{RequestedAt: when.Add(-time.Minute)}
	f.recordLanded(Agent{ID: "c"}, r.wt)
	if got := timing(t, f, "c").LandedAt; !got.Equal(f.Now().UTC()) {
		t.Fatalf("landing = %v, want verification time %v", got, f.Now())
	}
}

func TestRebaseCannotMakeOldWorkBelongToNewRequest(t *testing.T) {
	r := newRepos(t)
	asked := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	t.Setenv("GIT_COMMITTER_DATE", asked.Add(-time.Minute).Format(time.RFC3339))
	commitFile(t, r.wt, "old", "old\n")
	t.Setenv("GIT_COMMITTER_DATE", asked.Add(time.Minute).Format(time.RFC3339))
	r.otherPushes(t, "other")
	r.land(t)
	f := realGitFactory(t)
	f.timings.cards["c"] = &CardTiming{RequestedAt: asked}
	f.recordLanded(Agent{ID: "c"}, r.wt)
	if got := timing(t, f, "c").LandedAt; !got.IsZero() {
		t.Fatalf("old rebased work got landing %v", got)
	}
}

func TestTimingCorrectionPreservesRetiredHistory(t *testing.T) {
	for _, mode := range []string{"retired", "early", "no own commit", "own commit", "git failure", "expired reflog"} {
		t.Run(mode, func(t *testing.T) {
			r := newRepos(t)
			f := realGitFactory(t)
			at := t0.Add(-time.Hour)
			ct := &CardTiming{RequestedAt: at.Add(-time.Hour), LandedAt: at, Source: "email", ReplyChecked: true,
				Events: []TimingEvent{{Stage: "landed", At: at.Add(time.Minute)}}}
			cwd := r.wt
			if mode == "retired" || mode == "early" {
				cwd = filepath.Join(r.wt, "gone")
			}
			if mode == "early" {
				ct.LandedAt = ct.RequestedAt.Add(-time.Minute)
			}
			if mode == "git failure" {
				f.GitOut = func(context.Context, string, ...string) (string, error) { return "", errors.New("unavailable") }
			}
			if mode == "expired reflog" {
				gitRun(t, r.wt, "reflog", "expire", "--expire=all", "HEAD")
			}
			if mode == "own commit" {
				commitFile(t, r.wt, "mine", "mine\n")
				r.land(t)
			}
			f.timings.cards["past"] = ct
			f.SetAgents([]Agent{{ID: "past", Cwd: cwd}})
			f.reconcileTimings()
			got := timing(t, f, "past")
			clear := mode == "early" || mode == "no own commit"
			if got.LandedAt.IsZero() != clear || !got.NoReply {
				t.Fatalf("%s: %+v", mode, got)
			}
			reloaded := newTimingStore(f.timings.path, f.Now, f.cfg.Location)
			saved, ok := reloaded.get("past")
			if !ok || !saved.LandedAt.Equal(got.LandedAt) || saved.NoReply != got.NoReply {
				t.Fatal("correction did not survive restart")
			}
		})
	}
}

func TestUnansweredAndEarlyLandingsExcluded(t *testing.T) {
	for _, mode := range []string{"no reply", "pending reply", "early landing"} {
		ct := CardTiming{Source: "email", RequestedAt: t0.Add(-time.Hour), LandedAt: t0.Add(-time.Minute),
			Events: []TimingEvent{{Stage: "landed", At: t0}}}
		if mode == "no reply" {
			ct.NoReply = true
			ct.ReplyChecked = true
		}
		if mode == "early landing" {
			ct.Source = "board"
			ct.LandedAt = ct.RequestedAt.Add(-time.Minute)
		}
		if m := metrics(map[string]CardTiming{"c": ct}, t0, 7); m.Cards != 0 || (mode != "early landing" && m.Excluded.NoReply != 1) || (mode == "early landing" && m.Excluded.NotLanded != 1) {
			t.Fatalf("%s counted: %+v", mode, m)
		}
	}
}

func TestNoReplyOnTimingAPIs(t *testing.T) {
	f, _ := newTestFactory(t, fresh("c"))
	f.timings.update("c", func(ct *CardTiming) { ct.NoReply = true })
	f.SetAgents(f.agents())
	for _, route := range []string{"/api/board", "/api/timings"} {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", route, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"noReply":true`) {
			t.Fatalf("%s: %s", route, rec.Body.String())
		}
	}
}

func TestTimingCorrectionReadOnly(t *testing.T) {
	f, _ := newTestFactory(t)
	f.timings.cards["old"] = &CardTiming{RequestedAt: t0, LandedAt: t0.Add(-time.Hour)}
	f.cfg.ReadOnly, f.timings.readOnly = true, true
	f.reconcileTimings()
	if _, err := os.Stat(f.timings.path); !os.IsNotExist(err) {
		t.Fatal("read-only reconciliation wrote state")
	}
}

func TestTimingCorrectionBoundsRetriesAfterRestart(t *testing.T) {
	f, _ := newTestFactory(t)
	calls := 0
	f.GitOut = func(context.Context, string, ...string) (string, error) { calls++; return "", errors.New("gone") }
	var agents []Agent
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		f.timings.cards[id] = &CardTiming{RequestedAt: t0.Add(-time.Hour), LandedAt: t0, Events: []TimingEvent{{Stage: "landed", At: t0}}}
		agents = append(agents, Agent{ID: id, Cwd: filepath.Join(f.cfg.Home, "gone")})
	}
	f.SetAgents(agents)
	f.timings.dirty = true
	f.reconcileTimings()
	if calls != 3 {
		t.Fatalf("first tick inspected %d worktrees", calls)
	}
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	f.reconcileTimings()
	if calls != 6 {
		t.Fatalf("restart must retry at most three worktrees: %d inspections", calls)
	}
	f.reconcileTimings()
	if calls != 9 {
		t.Fatal("failed upgrades must remain retryable")
	}
}

func TestLateSameThreadReplyRemainsExplicitlyUnanswered(t *testing.T) {
	f, m := replyCard(t, []threadMsg{msg("later", t0.Add(-time.Minute), true)})
	f.checkReplies(t0)
	f.checkReplies(t0.Add(time.Hour))
	ct := timing(t, f, "e1")
	if !ct.NoReply || !ct.RepliedAt.IsZero() || gets(m) != 1 {
		t.Fatalf("late reply misattributed: %+v", ct)
	}
}

func TestOtherCardFactoryNoticeIsNotAReply(t *testing.T) {
	at := t0.Add(-12 * time.Minute)
	f, _ := replyCard(t, []threadMsg{msg("notice", at, true)})
	f.timings.cards["later"] = &CardTiming{Thread: "th1", Mails: []FactoryMail{{ID: "notice", At: at}}}
	f.checkReplies(t0)
	if ct := timing(t, f, "e1"); !ct.NoReply || !ct.RepliedAt.IsZero() {
		t.Fatalf("factory notice counted as reply: %+v", ct)
	}
}

func TestRepliedErrandsCountWithoutLandingSamples(t *testing.T) {
	for _, source := range []string{"email", "voice"} {
		t.Run(source, func(t *testing.T) {
			asked := t0.Add(-time.Hour)
			err := CardTiming{Repo: "owner/project", Source: source, RequestedAt: asked, RepliedAt: asked.Add(20 * time.Minute),
				Events: []TimingEvent{{Stage: "plan", At: asked}, {Stage: "landed", At: asked.Add(50 * time.Minute)}}}
			land := CardTiming{Source: "board", RequestedAt: asked, LandedAt: asked.Add(40 * time.Minute), Events: err.Events}
			all := map[string]CardTiming{"reply": err, "commit": land}
			f, _ := newTestFactory(t)
			for id, ct := range all {
				copy := ct
				f.timings.cards[id] = &copy
			}
			for _, query := range []string{"", "?source=" + source, "?repo=owner/project", "?step=plan"} {
				rec := httptest.NewRecorder()
				f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics"+query, nil))
				var got Metrics
				if e := json.Unmarshal(rec.Body.Bytes(), &got); e != nil {
					t.Fatal(e)
				}
				wantN, wantE2E, wantLanded, wantLandedN := 2, 30*60, 40*60, 1
				if query == "?source="+source || query == "?repo=owner/project" {
					wantN, wantE2E, wantLanded, wantLandedN = 1, 20*60, 0, 0
				}
				if rec.Code != 200 || got.Cards != wantN || got.Turnaround.N != wantN || got.Turnaround.EndToEnd.Average != wantE2E || got.Turnaround.ToLanded.Average != wantLanded || got.Turnaround.LandedN != wantLandedN || got.Turnaround.ToDone.Average != 50*60 || got.Excluded.NotLanded != 0 {
					t.Fatalf("%s: %+v", query, got)
				}
				if len(got.Steps) != 1 || got.Steps[0].Cards != wantN || got.Steps[0].CardSecs.Average != 50*60 {
					t.Fatalf("lost step times: %+v", got.Steps)
				}
				for _, g := range got.BySource {
					if g.Source == source && (g.N != 1 || g.LandedN != 0 || g.EndToEnd.Average != 20*60) {
						t.Fatalf("source group: %+v", g)
					}
				}
			}
		})
	}
}

func TestTimingReconciliationDoesNotRewriteUnchangedFile(t *testing.T) {
	f, _ := newTestFactory(t)
	f.timings.cards["c"] = &CardTiming{Source: "board", RequestedAt: t0.Add(-time.Hour), LandedAt: t0, LandingChecked: true}
	f.timings.dirty = true
	if err := f.timings.save(); err != nil {
		t.Fatal(err)
	}
	f.reconcileTimings()
	old := t0.Add(-24 * time.Hour)
	if err := os.Chtimes(f.timings.path, old, old); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.reconcileTimings()
	}
	st, err := os.Stat(f.timings.path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(old) {
		t.Fatal("unchanged reconciliation rewrote timings")
	}
}
