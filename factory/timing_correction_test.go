package factory

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLandingTimeBelongsToCard(t *testing.T) {
	for _, mode := range []string{"no commit", "unrelated head", "old own commit", "own commit", "rebased", "unlanded", "missing reflog", "git failure"} {
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
			}
			if mode == "unrelated head" || mode == "rebased" {
				r.otherPushes(t, "other")
			}
			if mode != "unlanded" {
				r.land(t)
			}
			if mode == "own commit" || mode == "rebased" {
				want = parseTime(gitRun(t, r.wt, "show", "-s", "--format=%cI", "HEAD"))
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
	if got := timing(t, f, "c").LandedAt; !got.Equal(when) {
		t.Fatalf("landing = %v, want own commit %v", got, when)
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

func TestTimingCorrectionBoundedAndNotRepeatedAfterRestart(t *testing.T) {
	f, _ := newTestFactory(t)
	calls := 0
	f.GitOut = func(context.Context, string, ...string) (string, error) { calls++; return "", errors.New("gone") }
	var agents []Agent
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		f.timings.cards[id] = &CardTiming{RequestedAt: t0.Add(-time.Hour), LandedAt: t0, Events: []TimingEvent{{Stage: "landed", At: t0}}}
		agents = append(agents, Agent{ID: id, Cwd: filepath.Join(f.cfg.Home, "gone")})
	}
	f.SetAgents(agents)
	f.reconcileTimings()
	if calls != 3 {
		t.Fatalf("first tick inspected %d worktrees", calls)
	}
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	f.reconcileTimings()
	if calls != 5 {
		t.Fatalf("restart repeated work: %d inspections", calls)
	}
	f.reconcileTimings()
	if calls != 5 {
		t.Fatal("finished upgrade repeated Git reads")
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
