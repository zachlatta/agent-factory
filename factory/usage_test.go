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
			},
			{
				"number": 3,
				"email": "third@example.com",
				"organizationName": "Third Org",
				"organizationUuid": "org-secret-id-3",
				"active": false,
				"usageStatus": "ok",
				"usage": {
					"fiveHour": {"pct": 100.0, "resetsAt": "2026-10-26T01:00:00Z"},
					"sevenDay": {"pct": 100.0, "resetsAt": "2026-11-01T12:00:00Z"}
				},
				"usageFetchedAt": "2026-10-25T19:58:00Z"
			},
			{
				"email": "fourth@example.com",
				"organizationName": "Fourth Org",
				"organizationUuid": "org-secret-id-4",
				"active": false,
				"usageStatus": "stale",
				"usage": {}
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
	if got := withAge(u, t0).Extra; got != "as of 5m ago" {
		t.Errorf("withAge Extra = %q, want %q", got, "as of 5m ago")
	}
	if len(u.Accounts) != 4 {
		t.Fatalf("Accounts = %+v, want 4 accounts", u.Accounts)
	}
	wantLabels := []string{"Claude #1", "Claude #2", "Claude #3", "Claude #4"}
	wantEmails := []string{"user@example.com", "other@example.com", "third@example.com", "fourth@example.com"}
	wantLeft := [][]int{{59, 100}, {77, 46}, {0, 0}, nil}
	for i, a := range u.Accounts {
		if a.Label != wantLabels[i] {
			t.Errorf("Accounts[%d].Label = %q, want %q", i, a.Label, wantLabels[i])
		}
		if a.Email != wantEmails[i] {
			t.Errorf("Accounts[%d].Email = %q, want %q", i, a.Email, wantEmails[i])
		}
		if a.Active != (i == 1) {
			t.Errorf("Accounts[%d].Active = %v, want %v", i, a.Active, i == 1)
		}
		if len(a.Windows) != len(wantLeft[i]) {
			t.Errorf("Accounts[%d].Windows = %+v", i, a.Windows)
			continue
		}
		for j, p := range wantLeft[i] {
			if a.Windows[j].RemainingPct != p {
				t.Errorf("Accounts[%d].Windows[%d] = %+v, want %d%% left", i, j, a.Windows[j], p)
			}
		}
	}
	if got := u.Accounts[3].Summary; got != "usage unknown" {
		t.Errorf("Accounts[3].Summary = %q, want usage unknown", got)
	}
	// Past an account's own reset, withAge flips only that window to 100%
	// and leaves the cached reading untouched on repeated calls.
	aged := withAge(u, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	if got := aged.Accounts[0].Windows[0].RemainingPct; got != 100 {
		t.Errorf("aged Accounts[0] 7d = %d%%, want 100 after its reset", got)
	}
	if got := u.Accounts[0].Windows[0].RemainingPct; got != 59 {
		t.Errorf("withAge mutated the cached account: 7d = %d%%, want 59", got)
	}
	if again := withAge(u, t0); again.Accounts[0].Windows[0].RemainingPct != 59 {
		t.Errorf("second withAge = %+v, want the un-aged reading", again.Accounts[0])
	}
	b, _ := json.Marshal(u)
	// The account email is shown; organization names and ids are not.
	for _, secret := range []string{"Example Org", "Other Org", "Third Org", "Fourth Org", "org-secret-id"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("serialized AgentUsage leaked %q: %s", secret, b)
		}
	}
}

func TestParseClaudeUsageSingleAndNoActive(t *testing.T) {
	single := `{"accounts":[{"number":5,"active":false,"usageStatus":"ok",
		"usage":{"sevenDay":{"pct":10.0}},"usageFetchedAt":"2026-10-25T19:59:00Z"}]}`
	u := parseClaudeUsage([]byte(single), t0)
	if u.Summary != "90% 7d left" || len(u.Accounts) != 1 ||
		u.Accounts[0].Label != "Claude #5" || u.Accounts[0].Active || u.Accounts[0].Email != "" {
		t.Errorf("single account = %+v", u)
	}
	if b, _ := json.Marshal(u.Accounts[0]); strings.Contains(string(b), `"email"`) {
		t.Errorf("account without an email serialized one: %s", b)
	}

	noActive := `{"accounts":[
		{"number":2,"active":false,"usageStatus":"ok","usage":{"sevenDay":{"pct":20.0}}},
		{"number":1,"active":false,"usageStatus":"ok","usage":{"sevenDay":{"pct":30.0}}}]}`
	u = parseClaudeUsage([]byte(noActive), t0)
	if u.Summary != "80% 7d left" {
		t.Errorf("no active: Summary = %q, want the first account's 80%% 7d left", u.Summary)
	}
	if len(u.Accounts) != 2 || u.Accounts[0].Label != "Claude #1" || u.Accounts[1].Label != "Claude #2" {
		t.Errorf("no active: Accounts = %+v, want number order", u.Accounts)
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
}

const agyUsageJSON = `{"conversation_id":"","status":"SUCCESS","response":"","num_turns":0,"command":{"name":"usage","data":{"groups":[` +
	`{"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","window":"weekly","remaining_fraction":0.5,"reset_time":"2026-11-01T23:35:13Z"},{"id":"3p-5h","window":"5h","remaining_fraction":0.1,"reset_time":"2026-10-26T04:35:13Z"}]},` +
	`{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.9392,"reset_time":"2026-10-31T01:48:04Z"},{"id":"gemini-5h","window":"5h","remaining_fraction":0.7717,"reset_time":"2026-10-26T03:01:40.5Z"}]}]}}}`

func TestParseAntigravityUsage(t *testing.T) {
	u := parseAntigravityUsage([]byte(agyUsageJSON), t0)
	if u.Summary != "94% 7d · 77% 5h left" {
		t.Errorf("Summary = %q, want %q", u.Summary, "94% 7d · 77% 5h left")
	}
	want := []UsageWindow{
		{Label: "7d", RemainingPct: 94, ResetsAt: "2026-10-31T01:48:04Z"},
		{Label: "5h", RemainingPct: 77, ResetsAt: "2026-10-26T03:01:40Z"},
	}
	if fmt.Sprint(u.Windows) != fmt.Sprint(want) {
		t.Errorf("Windows = %+v, want %+v", u.Windows, want)
	}
	if u.UpdatedAt != t0.UTC().Format(time.RFC3339) {
		t.Errorf("UpdatedAt = %q", u.UpdatedAt)
	}

	for name, raw := range map[string]string{
		"bad json":  "not json",
		"failed":    strings.Replace(agyUsageJSON, `"SUCCESS"`, `"ERROR"`, 1),
		"no gemini": strings.NewReplacer(`"Gemini Models"`, `"Other"`, `"gemini-`, `"other-`).Replace(agyUsageJSON),
		"no groups": `{"status":"SUCCESS","command":{"data":{"groups":[]}}}`,
	} {
		if u := parseAntigravityUsage([]byte(raw), t0); u.Summary != "usage unknown" {
			t.Errorf("%s = %+v, want usage unknown", name, u)
		}
	}
}

func TestReadAntigravityUsage(t *testing.T) {
	f, _ := newTestFactory(t)
	f.Usage = f.readProviderUsage

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "agy.log")
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("PATH", bin)
	if u := f.Usage(context.Background(), "antigravity"); u.Summary != "usage unknown" {
		t.Errorf("no agy on PATH = %+v, want usage unknown", u)
	}

	script := "#!/bin/sh\necho \"$*\" > " + log + "\npwd >> " + log + "\nprintf '%s\\n' '" + agyUsageJSON + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if u := f.Usage(context.Background(), "antigravity"); u.Summary != "94% 7d · 77% 5h left" {
		t.Errorf("antigravity = %+v", u)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "-p /usage --output-format json\n" + os.TempDir() + "\n"
	if string(got) != want {
		t.Errorf("agy call = %q, want %q", got, want)
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
		u := AgentUsage{
			Summary:   fmt.Sprintf("%s %d%% 7d left", provider, p),
			Windows:   []UsageWindow{{Label: "7d", RemainingPct: p}},
			UpdatedAt: now.UTC().Format(time.RFC3339),
		}
		if provider == "claude" {
			u.Accounts = []AccountUsage{
				{Label: "Claude #1", Summary: u.Summary, Windows: u.Windows, UpdatedAt: u.UpdatedAt},
				{Label: "Claude #2", Active: true, Summary: u.Summary, Windows: u.Windows, UpdatedAt: u.UpdatedAt},
			}
		}
		return u
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

	// First read: the cache is empty, so the API answers with "usage unknown" at once and
	// refreshes in the background; the fetched values show up on a following read.
	agents := callAPI()
	if len(agents) != len(AgentChoices) {
		t.Fatalf("len(agents) = %d, want %d", len(agents), len(AgentChoices))
	}
	waitFor(t, "usage refresh", func() bool { return calls.Load() == 4 }) // 4 distinct providers across the 5 AgentChoices
	waitFor(t, "usage visible", func() bool {
		agents = callAPI()
		return strings.HasSuffix(agents[0].Usage.Summary, "80% 7d left")
	})
	for _, a := range agents {
		if !strings.HasSuffix(a.Usage.Summary, "80% 7d left") || a.Usage.Extra != "as of just now" {
			t.Errorf("%s usage = %+v", a.ID, a.Usage)
		}
		if a.ID == "opus" {
			if len(a.Usage.Accounts) != 2 || a.Usage.Accounts[1].Label != "Claude #2" || !a.Usage.Accounts[1].Active {
				t.Errorf("opus accounts = %+v, want both Claude accounts", a.Usage.Accounts)
			}
		} else if len(a.Usage.Accounts) != 0 {
			t.Errorf("%s accounts = %+v, want none", a.ID, a.Usage.Accounts)
		}
	}
	firstCalls := calls.Load()

	// Within 5m TTL: cached (no new calls), and age updates dynamically from UpdatedAt.
	now = t0.Add(3 * time.Minute)
	agents = callAPI()
	if got := calls.Load(); got != firstCalls {
		t.Errorf("cached calls = %d, want %d", got, firstCalls)
	}
	if got := agents[0].Usage.Extra; got != "as of 3m ago" {
		t.Errorf("cached age at +3m = %q, want %q", got, "as of 3m ago")
	}

	// Past 5m TTL: the read returns the stale values and kicks a background refresh;
	// the new values replace them once it lands.
	now = t0.Add(2 * time.Hour)
	pct.Store(65)
	agents = callAPI()
	if !strings.HasSuffix(agents[0].Usage.Summary, "80% 7d left") {
		t.Errorf("stale read = %+v, want the previous 80%% values", agents[0].Usage)
	}
	waitFor(t, "stale refresh visible", func() bool {
		agents = callAPI()
		return strings.HasSuffix(agents[0].Usage.Summary, "65% 7d left")
	})
	if got := calls.Load(); got != firstCalls+4 {
		t.Errorf("after stale refresh calls = %d, want %d", got, firstCalls+4)
	}
	if agents[0].Usage.Extra != "as of just now" {
		t.Errorf("after stale refresh usage = %+v", agents[0].Usage)
	}
}

// A slow usage command must not delay /api/agents (it gates the board and the new-card dialog).
func TestAgentsAPIDoesNotWaitForUsage(t *testing.T) {
	f, _ := newTestFactory(t)
	release := make(chan struct{})
	f.Usage = func(_ context.Context, provider string) AgentUsage {
		<-release
		return AgentUsage{Summary: provider + " 50% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 50}}}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "usage unknown") {
			t.Errorf("first response = %d %s, want 200 with usage unknown", rec.Code, rec.Body)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("/api/agents waited for the usage commands")
	}
	close(release)
	waitFor(t, "usage after release", func() bool {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		return strings.Contains(rec.Body.String(), "50% 7d left")
	})
}
