package factory

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sessionAgent(a Agent, session string) Agent {
	a.Provider, a.Model, a.SessionID = "codex", "model-a", session
	return a
}

func TestTimingSessionLinksFollowStepsAndSessions(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	f, p := newTestFactory(t, a)
	first := timing(t, f, a.ID)
	if first.Repo != a.Repo || len(first.Sessions) != 1 {
		t.Fatalf("initial metadata = %+v", first)
	}
	l := first.Sessions[0]
	if l.AgentID != a.ID || l.SessionID != "first" || l.Provider != "codex" || l.Model != "model-a" ||
		l.Device != a.Host || l.Role != "fellow" || l.Stage != "plan" || !l.ObservedAt.Equal(t0) ||
		!l.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("session link = %+v", l)
	}
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	a = moved(a, "building", iso(f.Now()))
	f.SetAgents([]Agent{a})
	f.Now = func() time.Time { return t0.Add(2 * time.Minute) }
	a.SessionID, a.Model = "second", "model-b"
	f.SetAgents([]Agent{a})
	ct := timing(t, f, a.ID)
	if len(ct.Sessions) != 3 || ct.Sessions[1].Stage != "building" ||
		ct.Sessions[1].SessionID != "first" || ct.Sessions[2].SessionID != "second" || ct.Sessions[2].Model != "model-b" {
		t.Fatalf("session history = %+v", ct.Sessions)
	}
	if len(p.calls) != 0 {
		t.Fatalf("observation called Paseo: %v", p.calls)
	}
	// A caller cannot mutate the store through a returned snapshot.
	ct.Sessions[0].Model = "changed"
	if timing(t, f, a.ID).Sessions[0].Model != "model-a" {
		t.Fatal("session links were not copied")
	}
}

func TestTimingSessionLinksNestedReviewersAndArchivedChildren(t *testing.T) {
	a := sessionAgent(fresh("c1"), "builder")
	a = moved(a, "plan-review", iso(t0))
	r := sessionAgent(reviewer("r1", a.ID, "plan"), "reviewer")
	b := sessionAgent(card("child", nil), "child")
	b.Labels["paseo.parent-agent-id"] = r.ID
	c := sessionAgent(card("nested", nil), "nested")
	c.Labels["paseo.parent-agent-id"] = b.ID
	unrelated := sessionAgent(card("unrelated", nil), "unrelated")
	f, _ := newTestFactory(t, a, r, b, c, unrelated)
	ct := timing(t, f, a.ID)
	if len(ct.Sessions) != 4 {
		t.Fatalf("links = %+v", ct.Sessions)
	}
	for _, l := range ct.Sessions {
		if l.Stage != "plan-review" {
			t.Errorf("wrong step: %+v", l)
		}
		switch l.AgentID {
		case r.ID:
			if l.ParentID != a.ID || l.Role != "reviewer" || l.ReviewKind != "plan" {
				t.Errorf("review link = %+v", l)
			}
		case b.ID:
			if l.ParentID != r.ID || l.Role != "subagent" {
				t.Errorf("child link = %+v", l)
			}
		case c.ID:
			if l.ParentID != b.ID || l.Role != "subagent" {
				t.Errorf("nested link = %+v", l)
			}
		}
	}
	// Old reviewers and their children must not acquire the next build's step.
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	r.ArchivedAt, b.ArchivedAt, c.ArchivedAt = f.Now(), f.Now(), f.Now()
	a = moved(a, "building", iso(f.Now()))
	f.SetAgents([]Agent{a, r, b, c})
	ct = timing(t, f, a.ID)
	if len(ct.Sessions) != 5 || ct.Sessions[4].AgentID != a.ID || ct.Sessions[4].Stage != "building" {
		t.Fatalf("archived children relabelled: %+v", ct.Sessions)
	}
}

func TestTimingSessionUnchangedPollDoesNotWrite(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	a.Status = "running"
	f, _ := newTestFactory(t, a)
	path := filepath.Join(f.cfg.StateDir, "timings.json")
	stamp := t0.Add(-time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		f.Now = func() time.Time { return t0.Add(time.Duration(i+1) * 15 * time.Second) }
		a.LastActivityAt = f.Now()
		f.SetAgents([]Agent{a})
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("unchanged poll wrote file: %v %v", info, err)
	}
	if len(timing(t, f, a.ID).Sessions) != 1 {
		t.Fatal("unchanged polls added links")
	}
	// Reject stale observations even when they contain a different native session.
	seq := f.nextRead()
	f.setAgents([]Agent{a}, f.nextRead(), false)
	a.SessionID = "stale"
	if f.setAgents([]Agent{a}, seq, false) || len(timing(t, f, a.ID).Sessions) != 1 {
		t.Fatal("stale observation changed links")
	}
}

func TestTimingSessionLinksSurviveRestartAndMissingAgents(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	f, p := newTestFactory(t, a)
	want := timing(t, f, a.ID)
	g := New(Config{Host: "host1", Home: f.cfg.Home, StateDir: f.cfg.StateDir}, p)
	g.Now = func() time.Time { return t0.Add(time.Minute) }
	g.SetAgents([]Agent{a})
	if got := timing(t, g, a.ID); !reflect.DeepEqual(got.Sessions, want.Sessions) {
		t.Fatalf("restart changed links: %+v", got.Sessions)
	}
	g.SetAgents(nil)
	if got := timing(t, g, a.ID); !reflect.DeepEqual(got.Sessions, want.Sessions) {
		t.Fatal("disappearing agent lost history")
	}
}

func TestTimingSessionIDAppearsLater(t *testing.T) {
	a := fresh("c1")
	f, _ := newTestFactory(t, a)
	if len(timing(t, f, a.ID).Sessions) != 0 {
		t.Fatal("recorded an empty session identity")
	}
	a = sessionAgent(a, "ready")
	f.SetAgents([]Agent{a})
	if ct := timing(t, f, a.ID); len(ct.Sessions) != 1 || ct.Sessions[0].SessionID != "ready" {
		t.Fatalf("late session link = %+v", ct.Sessions)
	}
}

func TestTimingSessionArchivedIdentityCanArriveLate(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	f, _ := newTestFactory(t, a)
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	a.SessionID, a.ArchivedAt = "final", f.Now()
	f.SetAgents([]Agent{a})
	ct := timing(t, f, a.ID)
	if len(ct.Sessions) != 2 || ct.Sessions[1].SessionID != "final" || ct.Sessions[1].Stage != "plan" {
		t.Fatalf("late archived identity = %+v", ct.Sessions)
	}
}

func TestTimingSessionNativeProviders(t *testing.T) {
	for _, tc := range []struct{ provider, raw, want string }{
		{"claude", "claude-session", "claude-session"},
		{"codex", "codex-session", "codex-session"},
		{"antigravity", `plugin:{"version":1,"data":{"conversationId":"gemini-session"}}`, "gemini-session"},
		{"muse", `plugin:{"version":1,"data":{"sessionId":"muse-session"}}`, "muse-session"},
		{"codex", "../invalid", ""},
	} {
		t.Run(tc.provider+tc.want, func(t *testing.T) {
			a := fresh("c1")
			a.Provider, a.SessionID = tc.provider, tc.raw
			f, _ := newTestFactory(t, a)
			links := timing(t, f, a.ID).Sessions
			if tc.want == "" {
				if len(links) != 0 {
					t.Fatalf("invalid native identity linked: %+v", links)
				}
			} else if len(links) != 1 || links[0].SessionID != tc.want || links[0].Provider != tc.provider {
				t.Fatalf("native identity = %+v", links)
			}
		})
	}
}

func TestTimingSessionMetadataReadOnlyAndDryRun(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	f, p := newTestFactory(t, a)
	path := filepath.Join(f.cfg.StateDir, "timings.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A canary may read the live state but cannot persist new identities.
	g := New(Config{Host: "host1", Home: f.cfg.Home, StateDir: f.cfg.StateDir, ReadOnly: true}, p)
	g.Now = func() time.Time { return t0.Add(time.Minute) }
	a.SessionID = "second"
	g.SetAgents([]Agent{a})
	if got := timing(t, g, a.ID); len(got.Sessions) != 2 {
		t.Fatalf("read-only observation missing: %+v", got.Sessions)
	}
	// A dry run can write only its state copy.
	dir, err := DryRunState(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := New(Config{Host: "host1", Home: f.cfg.Home, StateDir: dir, DryRun: true}, p)
	h.Now = g.Now
	h.SetAgents([]Agent{a})
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("live state changed: %v", err)
	}
}

func TestTimingExportUsesFactoryTimesAndOmitsPrivateFields(t *testing.T) {
	a := sessionAgent(fresh("c1"), "first")
	a.Cwd, a.Title = "do-not-export-folder", "do-not-export-title"
	f, p := newTestFactory(t, a)
	f.timings.update(a.ID, func(ct *CardTiming) {
		ct.Thread = "do-not-export-thread"
		ct.Mails = []FactoryMail{{ID: "do-not-export-message", At: t0}}
		ct.LandedAt, ct.RepliedAt = t0.Add(time.Minute), t0.Add(2*time.Minute)
		ct.Events = append(ct.Events, TimingEvent{Stage: "landed", At: t0.Add(3 * time.Minute)})
		ct.Rounds = []TimingRound{{Kind: "plan", N: 1, Start: t0, End: t0.Add(time.Second), Verdict: "approve"}}
	})
	before := len(p.calls)
	get := func() []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/timings", nil))
		if rec.Code != 200 {
			t.Fatalf("export status = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	raw := get()
	if strings.Contains(string(raw), "do-not-export") || strings.Contains(string(raw), "replyTries") {
		t.Fatalf("export contains internal fields: %s", raw)
	}
	var out TimingExport
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Cards) != 1 {
		t.Fatalf("export = %s", raw)
	}
	ct := timing(t, f, a.ID)
	x := out.Cards[0]
	if x.ID != a.ID || x.Repo != a.Repo || x.Source != "board" || !x.RequestedAt.Equal(ct.RequestedAt) ||
		!x.LandedAt.Equal(ct.LandedAt) || !x.RepliedAt.Equal(ct.RepliedAt) || !x.DoneAt.Equal(ct.doneAt()) ||
		!reflect.DeepEqual(x.Events, ct.Events) || !reflect.DeepEqual(x.Rounds, ct.Rounds) ||
		!reflect.DeepEqual(x.Sessions, ct.Sessions) {
		t.Fatalf("export did not preserve facts: %+v", x)
	}
	if !bytes.Equal(raw, get()) || len(p.calls) != before {
		t.Fatal("export is unstable or called Paseo")
	}
}

func TestTimingExportOldRecordsAndEmptyStore(t *testing.T) {
	f, _ := newTestFactory(t)
	if got := f.timings.export(); len(got.Cards) != 0 || got.Cards == nil {
		t.Fatalf("empty export = %+v", got)
	}
	f.timings.cards["old"] = &CardTiming{Partial: true, Source: "email", Events: []TimingEvent{{Stage: "review", At: t0}}}
	f.timings.cards["a"] = &CardTiming{Source: "board"}
	out := f.timings.export()
	if len(out.Cards) != 2 || out.Cards[0].ID != "a" || !out.Cards[1].Partial || out.Cards[1].Repo != "" ||
		len(out.Cards[1].Sessions) != 0 {
		t.Fatalf("old records = %+v", out)
	}
}
