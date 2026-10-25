package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseClaudeUsage(t *testing.T) {
	raw := `{
		"schemaVersion": 1,
		"activeAccountNumber": 2,
		"accounts": [
			{
				"number": 1,
				"email": "user@example.com",
				"organizationName": "Example Org",
				"organizationUuid": "org-secret-id-1",
				"active": false,
				"usageStatus": "ok",
				"usage": {
					"fiveHour": {"pct": 0.0},
					"sevenDay": {"pct": 41.0, "resetsAt": "2026-10-30T11:00:00Z"}
				}
			},
			{
				"number": 2,
				"email": "other@example.com",
				"organizationName": "Other Org",
				"organizationUuid": "org-secret-id-2",
				"active": true,
				"usageStatus": "ok",
				"usage": {
					"fiveHour": {"pct": 54.0, "resetsAt": "2026-10-25T23:40:00Z"},
					"sevenDay": {"pct": 23.0, "resetsAt": "2026-10-31T12:00:00Z"}
				},
				"usageFetchedAt": "2026-10-25T19:55:00Z"
			}
		]
	}`
	u := parseClaudeUsage([]byte(raw), t0)
	if u.Summary != "77% 7d · 46% 5h left" {
		t.Errorf("Summary = %q, want %q", u.Summary, "77% 7d · 46% 5h left")
	}
	if len(u.Windows) != 2 || u.Windows[0].Label != "7d" || u.Windows[0].RemainingPct != 77 ||
		u.Windows[1].Label != "5h" || u.Windows[1].RemainingPct != 46 {
		t.Errorf("Windows = %+v", u.Windows)
	}
	if got := withAge(u, t0).Extra; got != "+1 overflow account · as of 5m ago" {
		t.Errorf("withAge Extra = %q, want %q", got, "+1 overflow account · as of 5m ago")
	}
	b, _ := json.Marshal(u)
	for _, secret := range []string{"example.com", "Example Org", "Other Org", "org-secret-id"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("serialized AgentUsage leaked %q: %s", secret, b)
		}
	}
}

func TestReadCodexUsage(t *testing.T) {
	home := t.TempDir()
	if got := readCodexUsage(home); got.Summary != "usage unknown" {
		t.Fatalf("empty home: got %+v, want usage unknown", got)
	}

	dir := filepath.Join(home, ".codex/sessions/2026/10/25")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Older session file: includes a >64KB line followed by a rate_limits event 12m before t0.
	bigLine := fmt.Sprintf(`{"timestamp":"2026-10-25T19:47:00Z","type":"response_item","payload":{"text":%q}}`, strings.Repeat("x", 80_000))
	rateLine := `{"timestamp":"2026-10-25T19:48:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":4.0,"window_minutes":10080,"resets_at":1793513843},"secondary":null}}}`
	older := filepath.Join(dir, "rollout-older.jsonl")
	if err := os.WriteFile(older, []byte(bigLine+"\n"+rateLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := t0.Add(-12 * time.Minute)
	os.Chtimes(older, oldTime, oldTime)

	// Newest session file: just started, no rate_limits line yet.
	newer := filepath.Join(dir, "rollout-newer.jsonl")
	if err := os.WriteFile(newer, []byte(`{"timestamp":"2026-10-25T19:59:00Z","type":"session_meta"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := t0.Add(-time.Minute)
	os.Chtimes(newer, newTime, newTime)

	u := readCodexUsage(home)
	if u.Summary != "96% 7d left" {
		t.Errorf("Summary = %q, want %q", u.Summary, "96% 7d left")
	}
	if len(u.Windows) != 1 || u.Windows[0].Label != "7d" || u.Windows[0].RemainingPct != 96 {
		t.Errorf("Windows = %+v", u.Windows)
	}
	if u.UpdatedAt != "2026-10-25T19:48:00Z" {
		t.Errorf("UpdatedAt = %q, want 2026-10-25T19:48:00Z", u.UpdatedAt)
	}
	if got := withAge(u, t0).Extra; got != "shared pool · as of 12m ago" {
		t.Errorf("at t0 Extra = %q, want %q", got, "shared pool · as of 12m ago")
	}
	if got := withAge(u, t0.Add(48*time.Minute)).Extra; got != "shared pool · as of 1h ago" {
		t.Errorf("at t0+48m Extra = %q, want %q", got, "shared pool · as of 1h ago")
	}
	if afterReset := withAge(u, time.Unix(1793513843, 0).Add(time.Minute)); afterReset.Summary != "100% 7d left" || afterReset.Windows[0].RemainingPct != 100 {
		t.Errorf("after reset = %+v, want 100%% 7d left", afterReset)
	}
}

func TestParseMuseUsage(t *testing.T) {
	asOf := t0.Add(-2 * time.Minute).UnixMilli()
	raw := fmt.Sprintf(`{"allowance":{"asOfMs":%d,"tier":"Muse Code Power Usage","weekly":{"resetsAtMs":1792972800000,"usedPercent":15},"window":{"resetsAtMs":1792983497000,"usedPercent":40}},"schemaVersion":1}`, asOf)
	u := parseMuseUsage([]byte(raw), t0)
	if u.Summary != "85% 7d · 60% 5h left" {
		t.Errorf("Summary = %q, want %q", u.Summary, "85% 7d · 60% 5h left")
	}
	if len(u.Windows) != 2 || u.Windows[0].RemainingPct != 85 || u.Windows[1].RemainingPct != 60 {
		t.Errorf("Windows = %+v", u.Windows)
	}
	if got := withAge(u, t0).Extra; got != "as of 2m ago" {
		t.Errorf("withAge Extra = %q, want %q", got, "as of 2m ago")
	}
}

func TestUsageFallbacksAndGemini(t *testing.T) {
	if u := parseClaudeUsage([]byte("not json"), t0); u.Summary != "usage unknown" {
		t.Errorf("bad claude json = %+v", u)
	}
	if u := parseMuseUsage([]byte("{}"), t0); u.Summary != "usage unknown" {
		t.Errorf("empty muse json = %+v", u)
	}
	f, _ := newTestFactory(t)
	f.Usage = f.readProviderUsage
	if u := f.Usage(context.Background(), "antigravity"); u.Summary != "usage unknown" {
		t.Errorf("antigravity = %+v, want usage unknown", u)
	}
}

func TestUsageCacheAndAgentsAPI(t *testing.T) {
	f, _ := newTestFactory(t)
	now := t0
	f.Now = func() time.Time { return now }
	var calls atomic.Int32
	var pct atomic.Int32
	pct.Store(80)
	f.Usage = func(_ context.Context, provider string) AgentUsage {
		calls.Add(1)
		p := int(pct.Load())
		return AgentUsage{
			Summary:   fmt.Sprintf("%s %d%% 7d left", provider, p),
			Windows:   []UsageWindow{{Label: "7d", RemainingPct: p}},
			UpdatedAt: now.UTC().Format(time.RFC3339),
		}
	}

	callAPI := func() []struct {
		ID    string     `json:"id"`
		Usage AgentUsage `json:"usage"`
	} {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var out struct {
			Agents []struct {
				ID    string     `json:"id"`
				Usage AgentUsage `json:"usage"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Agents
	}

	agents := callAPI()
	if len(agents) != len(AgentChoices) {
		t.Fatalf("len(agents) = %d, want %d", len(agents), len(AgentChoices))
	}
	for _, a := range agents {
		if !strings.HasSuffix(a.Usage.Summary, "80% 7d left") || a.Usage.Extra != "as of just now" {
			t.Errorf("%s usage = %+v", a.ID, a.Usage)
		}
	}
	firstCalls := calls.Load()
	if firstCalls != 4 { // 4 distinct providers across the 5 AgentChoices
		t.Errorf("first calls = %d, want 4", firstCalls)
	}

	// Within 5m TTL: cached (no new calls), and age updates dynamically from UpdatedAt.
	now = t0.Add(3 * time.Minute)
	agents = callAPI()
	if got := calls.Load(); got != firstCalls {
		t.Errorf("cached calls = %d, want %d", got, firstCalls)
	}
	if got := agents[0].Usage.Extra; got != "as of 3m ago" {
		t.Errorf("cached age at +3m = %q, want %q", got, "as of 3m ago")
	}

	// Past 5m TTL: refreshes synchronously before returning so stale hours-old values are never shown.
	now = t0.Add(2 * time.Hour)
	pct.Store(65)
	agents = callAPI()
	if got := calls.Load(); got != firstCalls+4 {
		t.Errorf("after stale refresh calls = %d, want %d", got, firstCalls+4)
	}
	if !strings.HasSuffix(agents[0].Usage.Summary, "65% 7d left") || agents[0].Usage.Extra != "as of just now" {
		t.Errorf("after stale refresh usage = %+v", agents[0].Usage)
	}
}
