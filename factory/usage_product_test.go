package factory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageDoesNotInventResetCapacity(t *testing.T) {
	u := AgentUsage{Summary: "4% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 4, ResetsAt: iso(t0.Add(-time.Minute))}}, UpdatedAt: iso(t0.Add(-time.Hour))}
	got := withAge(u, t0)
	if got.Windows[0].RemainingPct != 4 || got.Windows[0].ResetsAt != u.Windows[0].ResetsAt {
		t.Fatalf("invented a new reading: %+v", got)
	}
}

func TestUsageFailedRefreshRetainsReading(t *testing.T) {
	f, _ := newTestFactory(t)
	f.usage.byProvider = map[string]AgentUsage{"codex": {Summary: "40% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40}}, UpdatedAt: iso(t0.Add(-time.Hour))}}
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }
	done := make(chan struct{})
	f.usage.refreshing = done
	f.refreshUsage(done, t0)
	u := f.providerUsages()["codex"]
	if len(u.Windows) != 1 || u.Windows[0].RemainingPct != 40 || u.RefreshError == "" || u.UpdatedAt != iso(t0.Add(-time.Hour)) {
		t.Fatalf("lost last reading or failure: %+v", u)
	}
}

func TestLiveCodexAllowance(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("missing authentication")
		}
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_at":1793000000},"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_at":1793500000}}}`))
	}))
	defer s.Close()
	u := fetchCodexUsage(context.Background(), s.Client(), s.URL, "test-token", "test-account", t0)
	if len(u.Windows) != 2 || u.Windows[0].RemainingPct != 70 || u.Windows[1].RemainingPct != 40 || u.UpdatedAt != iso(t0) {
		t.Fatalf("live meter: %+v", u)
	}
}

func TestUsageDayDoesNotInventPrecision(t *testing.T) {
	at := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		from, time time.Time
		day, why   string
	}{
		{at, at, "2026-01-02", ""},
		{at.Add(-time.Hour), at, "2026-01-02", ""},
		{time.Time{}, at, "", "initial cumulative reading"},
		{at.Add(-24 * time.Hour), at, "", "interval crosses UTC days"},
		{at, time.Time{}, "", "timestamp unavailable"},
	} {
		day, why := usageDay(tokenEvent{From: tc.from, At: tc.time}, at)
		if day != tc.day || why != tc.why {
			t.Errorf("day=%q why=%q", day, why)
		}
	}
}

func TestUsageDatedRowsConserveTokens(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "sample", "sample-session", t0, strings.ReplaceAll(claudeUsageLine("first", 10, 2), "2026-10-25T19:45:00Z", "2026-10-24T19:45:00Z")+claudeUsageLine("second", 20, 3))
	v, _ := f.collectTokenUsage(context.Background())
	sum := TokenCounts{}
	days := map[string]bool{}
	for _, r := range v.Rows {
		sum.add(r.Counts)
		days[r.Day] = true
		if r.Stage == "unassigned" && r.StageReason == "" {
			t.Error("unexplained stage gap")
		}
	}
	if sum != v.Totals || len(days) != 2 {
		t.Fatalf("dated rows not conserved: %+v %+v %v", sum, v.Totals, days)
	}
}

func TestUsageEvidenceArchivedAndBounded(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "sample-old", "old-session", t0.Add(-48*time.Hour), claudeUsageLine("counter", 12, 2))
	writeFile(t, tokenPath(f.cfg.Home, Agent{Provider: "claude", SessionID: "old-session"}), `{"type":"user","message":{"content":"Synthetic evidence request"}}`+"\n"+claudeUsageLine("counter", 12, 2))
	v, _ := f.collectTokenUsage(context.Background())
	f.tokenCache.view = v
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/evidence/sample-old?tail=1", nil))
	if strings.Contains(rec.Body.String(), `"path":`) {
		t.Fatal("evidence exposed a source path as metadata")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"events"`) {
		t.Fatalf("archived evidence %d: %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/evidence/not-in-inventory", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown evidence: %d", rec.Code)
	}
}

func TestUsageFailureDoesNotReplaceLiveWithOlderFallback(t *testing.T) {
	f, _ := newTestFactory(t)
	f.usage.byProvider = map[string]AgentUsage{"codex": {Summary: "40% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40}}, UpdatedAt: iso(t0)}}
	f.Usage = func(context.Context, string) AgentUsage {
		return AgentUsage{Summary: "80% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 80}}, UpdatedAt: iso(t0.Add(-time.Hour)), RefreshError: "Live meter unavailable"}
	}
	done := make(chan struct{})
	f.usage.refreshing = done
	f.refreshUsage(done, t0)
	u := f.providerUsages()["codex"]
	if u.Windows[0].RemainingPct != 40 || u.RefreshError == "" {
		t.Fatalf("replaced newer live reading: %+v", u)
	}
}

func TestLiveCodexFailureDoesNotExposeResponse(t *testing.T) {
	for _, body := range []string{`{"error":"private-marker"}`, `not-json-private-marker`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); w.Write([]byte(body)) }))
		u := fetchCodexUsage(context.Background(), s.Client(), s.URL, "test-token", "test-account", t0)
		s.Close()
		if len(u.Windows) != 0 || strings.Contains(u.Summary, "private-marker") {
			t.Fatalf("failed meter: %+v", u)
		}
	}
}

func TestUsageRepeatedBuildAndReasons(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "card", "sample-session", t0, claudeUsageLine("counter", 10, 2))
	usageAgentFile(t, f, "card", "claude", "sample-session", map[string]string{labelPrefix + labelStage: "building"})
	f.timings.cards["card"] = &CardTiming{Events: []TimingEvent{{Stage: "building", At: t0.Add(-time.Hour)}, {Stage: "review", At: t0.Add(-30 * time.Minute)}, {Stage: "building", At: t0.Add(-20 * time.Minute)}}}
	v, _ := f.collectTokenUsage(context.Background())
	for _, r := range v.Rows {
		if r.Stage != "building" || !r.Rework || r.StageReason != "" {
			t.Fatalf("repeated build attribution: %+v", r)
		}
	}
}

func TestExpiredExhaustionCanBePickedForHandoff(t *testing.T) {
	u := AgentUsage{Windows: []UsageWindow{{Label: "5h", RemainingPct: 0, ResetsAt: iso(t0.Add(-time.Minute))}}}
	if score := usageScore(u, t0); score.tier != 1 {
		t.Fatalf("expired exhaustion must be unknown: %+v", score)
	}
	choice, ok := pickHandoff("claude", map[string]string{"codex": "available"}, map[string]AgentUsage{"codex": u}, t0)
	if !ok || choice.providerID() != "codex" {
		t.Fatalf("expired provider permanently excluded: %+v %v", choice, ok)
	}
	u.Windows = append(u.Windows, UsageWindow{Label: "7d", RemainingPct: 0, ResetsAt: iso(t0.Add(time.Hour))})
	if _, ok := pickHandoff("claude", map[string]string{"codex": "available"}, map[string]AgentUsage{"codex": u}, t0); ok {
		t.Fatal("known unexpired exhaustion must still block a handoff")
	}
}

func TestUnknownAccountRetainsLastReading(t *testing.T) {
	f, _ := newTestFactory(t)
	previous := AccountUsage{Label: "Account 1", Email: "person@example.com", Active: true, Summary: "40% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40}}, UpdatedAt: iso(t0.Add(-time.Hour))}
	f.usage.byProvider = map[string]AgentUsage{"claude": {Accounts: []AccountUsage{previous}}}
	f.Usage = func(context.Context, string) AgentUsage {
		return AgentUsage{Accounts: []AccountUsage{{Label: previous.Label, Email: previous.Email, Active: true, Summary: "usage unknown"}}}
	}
	done := make(chan struct{})
	f.usage.refreshing = done
	f.refreshUsage(done, t0)
	u := f.providerUsages()["claude"]
	if len(u.Accounts[0].Windows) != 1 || u.Accounts[0].Windows[0].RemainingPct != 40 || u.Accounts[0].RefreshError == "" || u.Accounts[0].UpdatedAt != previous.UpdatedAt || u.RefreshError != "" || usageScore(u, t0).tier != 1 {
		t.Fatalf("lost last known account reading: %+v", u)
	}
}

func TestUsageEvidenceDoesNotWaitForScan(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "sample-evidence", "sample-session", t0, `{"type":"user","message":{"content":"Synthetic evidence"}}`+"\n"+claudeUsageLine("counter", 10, 2))
	v, _ := f.collectTokenUsage(context.Background())
	f.tokenCache.view = v
	f.tokenCache.scan.Lock()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/evidence/sample-evidence", nil))
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("evidence %d", code)
		}
	case <-time.After(time.Second):
		t.Error("evidence waited for the collection lock")
	}
	f.tokenCache.scan.Unlock()
}

func TestIdeaHoldIgnoresUnusableRetainedReadings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reset   time.Time
		failure string
		hold    bool
	}{
		{"elapsed reset", t0.Add(-time.Minute), "", false},
		{"failed refresh", t0.Add(time.Hour), "meter unavailable", false},
		{"known current low allowance", t0.Add(time.Hour), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t)
			defer waitForAgentRefreshes(t, f)
			if _, err := f.waitProviderStatus("claude"); err != nil {
				t.Fatal(err)
			}
			usages := map[string]AgentUsage{}
			for _, a := range AgentChoices {
				usages[a.providerID()] = AgentUsage{Summary: "5% 7d left", RefreshError: tc.failure, Windows: []UsageWindow{{Label: "7d", RemainingPct: 5, ResetsAt: iso(tc.reset)}}}
			}
			seedUsage(f, usages)
			if hold := f.ideaHold(); (hold != "") != tc.hold {
				t.Fatalf("hold %q, want held=%v", hold, tc.hold)
			}
			if _, err := f.addIdea(Body{Request: "Synthetic idea"}); err != nil {
				t.Fatal(err)
			}
			if err := f.tickIdeas(); err != nil {
				t.Fatal(err)
			}
			runs := len(p.runsWithLabel(labelArg(labelOrigin, "idea")))
			if tc.hold && runs != 0 || !tc.hold && runs != 1 {
				t.Fatalf("idea starts=%d, held=%v", runs, tc.hold)
			}
		})
	}
}

func TestAccountRefreshKeepsCurrentActiveIdentity(t *testing.T) {
	f, _ := newTestFactory(t)
	first := AccountUsage{Label: "Account 1", Email: "first@example.com", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40}}, UpdatedAt: iso(t0.Add(-time.Hour))}
	second := AccountUsage{Label: "Account 2", Email: "second@example.com", Active: true, Windows: []UsageWindow{{Label: "7d", RemainingPct: 70}}, UpdatedAt: iso(t0)}
	f.usage.byProvider = map[string]AgentUsage{"claude": {Windows: second.Windows, UpdatedAt: second.UpdatedAt, Accounts: []AccountUsage{first, second}}}
	f.Usage = func(context.Context, string) AgentUsage {
		return AgentUsage{Accounts: []AccountUsage{{Label: first.Label, Email: first.Email, Active: true}, {Label: second.Label, Email: second.Email, Windows: second.Windows, UpdatedAt: second.UpdatedAt}}}
	}
	done := make(chan struct{})
	f.usage.refreshing = done
	f.refreshUsage(done, t0)
	u := f.providerUsages()["claude"]
	if !u.Accounts[0].Active || u.Accounts[1].Active || u.Windows[0].RemainingPct != 40 {
		t.Fatalf("restored a different account's identity or counters: %+v", u)
	}
}

func TestFailedExhaustedReadingDoesNotPermanentlyBlockWork(t *testing.T) {
	f, _ := newTestFactory(t)
	defer waitForAgentRefreshes(t, f)
	if _, err := f.waitProviderStatus("claude"); err != nil {
		t.Fatal(err)
	}
	u := AgentUsage{RefreshError: "meter unavailable", Windows: []UsageWindow{{Label: "7d", RemainingPct: 0}}}
	usages := map[string]AgentUsage{}
	for _, a := range AgentChoices {
		usages[a.providerID()] = u
	}
	seedUsage(f, usages)
	if hold := f.ideaHold(); hold != "" {
		t.Fatalf("failed retained exhaustion held ideas: %s", hold)
	}
	if _, ok := pickHandoff("claude", map[string]string{"codex": "available"}, usages, t0); !ok {
		t.Fatal("failed reading permanently excluded handoff provider")
	}
}

func TestUnsupportedCountersCanStillHaveTranscriptEvidence(t *testing.T) {
	f, _ := newTestFactory(t)
	usageAgentFile(t, f, "sample-gemini", "antigravity", "sample-session", nil)
	path := filepath.Join(f.cfg.Home, ".gemini/antigravity-cli/brain/sample-session/.system_generated/logs/transcript_full.jsonl")
	writeFile(t, path, `{"type":"USER_INPUT","content":"Synthetic evidence request","created_at":"2026-10-25T19:45:00Z"}`+"\n")
	v, _ := f.collectTokenUsage(context.Background())
	f.tokenCache.view = v
	if len(v.Rows) != 1 || v.Rows[0].Status != "unsupported" || !v.Rows[0].Evidence || v.Totals.Total != 0 {
		t.Fatalf("unsupported counters lost evidence: %+v", v)
	}
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage/evidence/sample-gemini", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Synthetic evidence request") || strings.Contains(rec.Body.String(), `"path":`) {
		t.Fatalf("evidence response: %d %s", rec.Code, rec.Body)
	}
}

func TestCurrentLowWeekStillHoldsAfterShortWindowReset(t *testing.T) {
	f, p := newTestFactory(t)
	defer waitForAgentRefreshes(t, f)
	if _, err := f.waitProviderStatus("claude"); err != nil {
		t.Fatal(err)
	}
	u := AgentUsage{Windows: []UsageWindow{{Label: "7d", RemainingPct: 5, ResetsAt: iso(t0.Add(48 * time.Hour))}, {Label: "5h", RemainingPct: 0, ResetsAt: iso(t0.Add(-time.Minute))}}}
	usages := map[string]AgentUsage{}
	for _, a := range AgentChoices {
		usages[a.providerID()] = u
	}
	seedUsage(f, usages)
	if hold := f.ideaHold(); hold == "" {
		t.Fatal("current weekly floor bypassed by short-window reset")
	}
	if score := usageScore(u, t0); score.tier == 1 || score.w7 != 5 {
		t.Fatalf("lost current weekly reading: %+v", score)
	}
	if _, err := f.addIdea(Body{Request: "Synthetic idea"}); err != nil {
		t.Fatal(err)
	}
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 0 {
		t.Fatalf("started %d ideas below the current weekly floor", n)
	}
}
