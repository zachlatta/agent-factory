package factory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flag returns the value after name in a paseo argument list ("" if absent).
func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestNewCardRunsTheChosenAgent(t *testing.T) {
	for _, tc := range []struct {
		agent, provider, thinking, mode string
	}{
		{"", "claude/claude-opus-5-5", "medium", "bypassPermissions"},
		{"opus", "claude/claude-opus-5-5", "medium", "bypassPermissions"},
		{"codex-astra", "codex/gpt-6-astra", "high", "full-access"},
		{"codex-sol", "codex/gpt-6.1-sol", "medium", "full-access"},
		{"sonnet", "claude/claude-sonnet-5-5", "medium", "bypassPermissions"},
		{"gemini-argon", "antigravity/gemini-4-argon-high", "", "full-access"},
		{"muse-wonder", "muse/muse-wonder-full-1.0", "", "allowAll"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			f, p := newTestFactory(t)
			if _, err := f.Act("new", Body{Request: "do a thing", Agent: tc.agent}); err != nil {
				t.Fatal(err)
			}
			runs := p.runsWithLabel(labelArg(labelStage, "plan"))
			if len(runs) != 1 {
				t.Fatalf("runs = %v", p.calls)
			}
			r := runs[0]
			if got := flag(r, "--provider"); got != tc.provider {
				t.Errorf("--provider = %q, want %q", got, tc.provider)
			}
			if got := flag(r, "--thinking"); got != tc.thinking {
				t.Errorf("--thinking = %q, want %q", got, tc.thinking)
			}
			if got := flag(r, "--mode"); got != tc.mode {
				t.Errorf("--mode = %q, want %q", got, tc.mode)
			}
			want := tc.agent
			if want == "" {
				want = "opus"
			}
			if !slices.Contains(r, labelArg(labelAgent, want)) {
				t.Errorf("run has no %s label: %v", labelArg(labelAgent, want), r)
			}
		})
	}
}

func TestNewCardRejectsUnknownOrUnavailableAgent(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "x", Agent: "fable"}); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("fable: err = %v, want unknown agent", err)
	}
	p.providers["muse"] = "error"
	if _, err := f.Act("new", Body{Request: "x", Agent: "muse-wonder"}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("muse down: err = %v, want not available", err)
	}
	if n := len(p.runsWithLabel(labelArg(labelStage, "plan"))); n != 0 {
		t.Errorf("started %d cards, want 0", n)
	}
}

func TestReviewersAndScoutsAlwaysUseOpus(t *testing.T) {
	for _, agent := range []string{"codex-astra", "gemini-argon", "muse-wonder"} {
		c := card("c1", map[string]string{labelStage: "building", labelAgent: agent})
		a, _ := agentByID(agent)
		c.Provider = a.providerID()
		f, p := newTestFactory(t, c)
		got, _ := f.card("c1")
		if _, err := f.StartReview(got, "code"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Act("scout", Body{Repo: "example/desk"}); err != nil {
			t.Fatal(err)
		}
		for _, r := range p.calls {
			if r[0] != "run" {
				continue
			}
			if got := flag(r, "--provider"); got != ReviewerModel {
				t.Errorf("%s card: run %q uses %q, want %q", agent, flag(r, "--title"), got, ReviewerModel)
			}
			if got := flag(r, "--thinking"); got != "medium" {
				t.Errorf("reviewer/scout thinking = %q, want medium", got)
			}
		}
	}
}

func TestProviderStatusRefreshAndDiagnosticRateLimit(t *testing.T) {
	f, p := newTestFactory(t)
	p.providers["antigravity"] = "error"
	count := func(verb string) int { return p.countCalls("provider", verb) }
	// Reads serve the cache and refresh in the background: the first read (empty cache)
	// reports unknown, and Paseo's answer shows up once the refresh lands.
	if f.providerAvailable("antigravity") {
		t.Fatal("antigravity available while Paseo says error")
	}
	waitFor(t, "first provider refresh", func() bool {
		return count("ls") == 2 && count("diagnostic") == 1 && f.providerStatus("antigravity") == "error"
	})
	f.providerAvailable("antigravity") // cached: no new calls
	if count("ls") != 2 {
		t.Errorf("cached check listed again: ls %d", count("ls"))
	}

	// The fake clock is atomic: a background refresh may still be reading it.
	var clock atomic.Value
	clock.Store(t0.Add(6 * time.Minute)) // list is stale, diagnostic is not due yet
	f.Now = func() time.Time { return clock.Load().(time.Time) }
	f.providerAvailable("antigravity")
	waitFor(t, "stale provider refresh", func() bool { return count("ls") == 3 })
	if count("diagnostic") != 1 {
		t.Errorf("after 6 min: ls %d, diagnostic %d; want 3 and 1", count("ls"), count("diagnostic"))
	}

	clock.Store(t0.Add(31 * time.Minute)) // diagnostic due again, and this time it fixes the provider
	p.mu.Lock()
	p.fixed["antigravity"] = true
	p.mu.Unlock()
	f.providerAvailable("antigravity") // kicks the refresh; the fix lands in the background
	waitFor(t, "diagnostic fix visible", func() bool { return f.providerAvailable("antigravity") })
	if count("diagnostic") != 2 {
		t.Errorf("diagnostic %d, want 2", count("diagnostic"))
	}
}

// While a background provider refresh is stuck in a slow `paseo` call, cached reads
// still answer at once (no lock is held across the call).
func TestProviderStatusReadDoesNotWaitForRefresh(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "x"}); err != nil { // populates the provider cache
		t.Fatal(err)
	}
	now := t0.Add(6 * time.Minute) // stale: the next read kicks a refresh
	f.Now = func() time.Time { return now }
	p.lsBlock = make(chan struct{})
	before := p.lsCalls.Load()
	done := make(chan string, 1)
	go func() { done <- f.providerStatus("claude") }()
	select {
	case st := <-done:
		if st != "available" {
			t.Errorf("stale read = %q, want the cached %q", st, "available")
		}
	case <-time.After(2 * time.Second):
		close(p.lsBlock)
		t.Fatal("providerStatus waited for the background refresh")
	}
	waitFor(t, "refresh in flight", func() bool { return p.lsCalls.Load() > before })
	close(p.lsBlock)
}

// Card creation waits for the first-ever provider list, but with a bound: a stuck
// `paseo` call must turn into a clear error, not a hang.
func TestResolveAgentWaitIsBounded(t *testing.T) {
	f, p := newTestFactory(t)
	p.lsBlock = make(chan struct{})
	old := providerWaitTimeout
	providerWaitTimeout = 100 * time.Millisecond
	defer func() { providerWaitTimeout = old }()
	start := time.Now()
	_, err := f.resolveAgent("codex-astra")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("resolveAgent error = %v, want a timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("resolveAgent wait was not bounded: %v", time.Since(start))
	}
	close(p.lsBlock)
}

func TestIdeaCarriesItsAgent(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("idea", Body{Request: "x", Title: "bad", Agent: "nope"}); err == nil {
		t.Error("idea with unknown agent accepted")
	}
	i, err := f.addIdea(Body{Request: "do it", Title: "with codex", Agent: "codex-astra"})
	if err != nil {
		t.Fatal(err)
	}
	f.SetAgents(nil)
	if c, _ := f.card(i.ID); c.Idea == nil || c.Idea.Agent != "codex-astra" {
		t.Errorf("waiting idea card = %+v, want agent codex-astra", c.Idea)
	}
	if _, err := f.Act("approve", Body{ID: i.ID}); err != nil {
		t.Fatal(err)
	}
	r := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(r) != 1 || flag(r[0], "--provider") != "codex/gpt-6-astra" {
		t.Fatalf("idea run = %v", r)
	}

	// Zach's pick on Start overrides the idea's own agent
	j, _ := f.addIdea(Body{Request: "do it", Title: "zach picks", Agent: "codex-astra"})
	if _, err := f.Act("approve", Body{ID: j.ID, Agent: "opus"}); err != nil {
		t.Fatal(err)
	}
	r = p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(r) != 2 || flag(r[1], "--provider") != ReviewerModel {
		t.Errorf("override run = %v", r[len(r)-1])
	}
}

func TestBoardCardShowsAgent(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "plan", labelAgent: "codex-astra"}))
	c, _ := f.card("c1")
	if c.Agent != "codex-astra" {
		t.Errorf("card agent = %q", c.Agent)
	}
}

func TestAgentsAPI(t *testing.T) {
	f, p := newTestFactory(t)
	p.providers["muse"] = "error"
	var out struct {
		Default  string `json:"default"`
		Reviewer string `json:"reviewer"`
		Agents   []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"agents"`
	}
	// Availability refreshes in the background: poll until Paseo's answer is served.
	waitFor(t, "agents availability", func() bool {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, a := range out.Agents {
			if a.Status == "unknown" {
				return false
			}
		}
		return len(out.Agents) == len(AgentChoices)
	})
	if out.Default != "opus" || out.Reviewer != ReviewerModel || len(out.Agents) != len(AgentChoices) {
		t.Errorf("got %+v", out)
	}
	for _, a := range out.Agents {
		if want := a.ID != "muse-wonder"; a.Available != want {
			t.Errorf("%s available = %v (status %q), want %v", a.ID, a.Available, a.Status, want)
		}
	}
}

func TestProviderListFailureKeepsOpusAndRetries(t *testing.T) {
	f, p := newTestFactory(t)
	p.lsErr = fmt.Errorf("daemon restarting")
	if _, err := f.Act("new", Body{Request: "x"}); err != nil {
		t.Errorf("default agent blocked while provider ls fails: %v", err)
	}
	if _, err := f.Act("new", Body{Request: "x", Agent: "codex-astra"}); err == nil {
		t.Error("codex-astra started with provider status unknown")
	}
	for _, c := range p.calls {
		if c[0] == "provider" && c[1] == "diagnostic" {
			t.Errorf("diagnostic run while provider ls fails: %v", c)
		}
	}
	p.lsErr = nil // recovers at once: a failed list is not cached
	if _, err := f.Act("new", Body{Request: "x", Agent: "codex-astra"}); err != nil {
		t.Errorf("codex-astra after provider ls recovered: %v", err)
	}
}

func TestAutoStartedIdeaUsesDefaultAgent(t *testing.T) {
	// An agent-submitted idea may suggest an agent, but auto-start uses the factory's
	// usage-balanced suggestion (which falls back to Opus when usage is unknown).
	f, p := newTestFactory(t)
	if _, err := f.addIdea(Body{Request: "do it", Title: "scout wants muse", Agent: "muse-wonder", By: "scout"}); err != nil {
		t.Fatal(err)
	}
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	r := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(r) != 1 {
		t.Fatalf("auto-start runs = %v", p.calls)
	}
	if got := flag(r[0], "--provider"); got != ReviewerModel {
		t.Errorf("auto-started idea runs %q, want the default %q", got, ReviewerModel)
	}
}

// seedUsage first waits for any background usage refresh to land, so a late refresh
// cannot overwrite the seeded readings.
func seedUsage(f *Factory, byProv map[string]AgentUsage) {
	f.usageMu.Lock()
	done := f.usage.refreshing
	f.usageMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			panic("seedUsage: timed out waiting for a usage refresh")
		}
	}
	f.usageMu.Lock()
	f.usage.byProvider = byProv
	f.usage.fetched = f.Now()
	f.usageMu.Unlock()
}

func TestAgentsAPIRanksByPaceAndSuggestsTopPlan(t *testing.T) {
	f, p := newTestFactory(t)
	inDays := func(d float64) string {
		return t0.Add(time.Duration(d * float64(24*time.Hour))).UTC().Format(time.RFC3339)
	}
	// Claude: 50% left resetting in 1d (weekLeft ≈ 14.3%, pace ≈ +35.7).
	// Codex: 60% left resetting in 6d (weekLeft ≈ 85.7%, pace ≈ -25.7).
	// Gemini: 95% 7d left resetting in 1d, but 5h is in the red at 15% (low-5h tier).
	// Muse: fresh unused window (100% left, no ResetsAt -> weekLeft = 100%, pace = 0).
	seedUsage(f, map[string]AgentUsage{
		"claude": {
			Summary: "50% 7d · 80% 5h left",
			Windows: []UsageWindow{{Label: "7d", RemainingPct: 50, ResetsAt: inDays(1)}, {Label: "5h", RemainingPct: 80}},
		},
		"codex": {
			Summary: "60% 7d left",
			Windows: []UsageWindow{{Label: "7d", RemainingPct: 60, ResetsAt: inDays(6)}},
		},
		"antigravity": {
			Summary: "95% 7d · 15% 5h left",
			Windows: []UsageWindow{{Label: "7d", RemainingPct: 95, ResetsAt: inDays(1)}, {Label: "5h", RemainingPct: 15}},
		},
		"muse": parseMuseUsage([]byte(`{"allowance":null,"reason":"server_sent_no_usage"}`), t0),
	})

	var out struct {
		Default string `json:"default"`
		Agents  []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
		} `json:"agents"`
	}
	waitFor(t, "agents ranked", func() bool {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Agents[0].Available
	})
	got := make([]string, len(out.Agents))
	for i, a := range out.Agents {
		got[i] = a.ID
	}
	// Expected order:
	// 1. opus (+35.7 pace: 50% left with 1d to go beats 60% with 6d to go and Muse's 0 pace)
	// 2. sonnet (same Claude quota as Opus)
	// 3. muse-wonder (0 pace: 100% left, full week assumed)
	// 4. codex-sol (-25.7 pace, preferred on ChatGPT Pro)
	// 5. codex-astra (-25.7 pace)
	// 6. gemini-argon (5h window <= 20% in the red)
	want := []string{"opus", "sonnet", "muse-wonder", "codex-sol", "codex-astra", "gemini-argon"}
	if !slices.Equal(got, want) || out.Default != "opus" {
		t.Errorf("agents = %v (default %q), want %v (default opus)", got, out.Default, want)
	}

	// A plan with no ResetsAt (or unparsable ResetsAt) counts as having a full week left (100%),
	// so it never outranks a plan with the same % left and a known reset time.
	seedUsage(f, map[string]AgentUsage{
		"claude":      {Summary: "0% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 0, ResetsAt: inDays(1)}}},
		"codex":       {Summary: "70% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 70}}},
		"antigravity": {Summary: "70% 7d · 80% 5h left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 70, ResetsAt: inDays(3)}, {Label: "5h", RemainingPct: 80}}},
		"muse":        {Summary: "70% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 70, ResetsAt: "not-a-timestamp"}}},
	})
	p.mu.Lock()
	p.providers["muse"] = "error"
	p.mu.Unlock()
	f.provMu.Lock()
	f.prov.status["muse"] = "error"
	f.provMu.Unlock()

	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for _, a := range out.Agents {
		got = append(got, a.ID)
	}
	// gemini-argon (70% with 3d left -> pace +27.1) > codex-astra, codex-sol (70% with no ResetsAt -> pace -30)
	// > opus (exhausted 0%) > muse-wonder (unavailable).
	want = []string{"gemini-argon", "codex-sol", "codex-astra", "opus", "sonnet", "muse-wonder"}
	if !slices.Equal(got, want) || out.Default != "gemini-argon" {
		t.Errorf("agents = %v (default %q), want %v (default gemini-argon)", got, out.Default, want)
	}
}

func TestNewCardAndAutoIdeaBalanceUsageAndKeepEmailOnOpus(t *testing.T) {
	f, p := newTestFactory(t)
	in3d := t0.Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedUsage(f, map[string]AgentUsage{
		"claude":      {Summary: "35% 7d · 70% 5h left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 35, ResetsAt: in3d}, {Label: "5h", RemainingPct: 70}}},
		"codex":       {Summary: "50% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 50, ResetsAt: in3d}}},
		"antigravity": {Summary: "90% 7d · 85% 5h left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 90, ResetsAt: in3d}, {Label: "5h", RemainingPct: 85}}},
		"muse":        {Summary: "40% 7d · 80% 5h left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40, ResetsAt: in3d}, {Label: "5h", RemainingPct: 80}}},
	})

	// 1. Board card with no agent picked uses the usage-balanced suggestion (gemini-argon).
	if _, err := f.Act("new", Body{Request: "board work"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelStage, "plan"))
	if len(runs) != 1 || flag(runs[0], "--provider") != "antigravity/gemini-4-argon-high" || !slices.Contains(runs[0], labelArg(labelAgent, "gemini-argon")) {
		t.Fatalf("board card run = %v, want gemini-argon", runs)
	}

	// 2. Auto-started idea (tickIdeas) ignores the scout's suggested agent and uses the usage-balanced suggestion.
	if _, err := f.addIdea(Body{Request: "idea work", Title: "auto balanced idea", Agent: "opus", By: "scout"}); err != nil {
		t.Fatal(err)
	}
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	ideaRuns := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(ideaRuns) != 1 || flag(ideaRuns[0], "--provider") != "antigravity/gemini-4-argon-high" {
		t.Fatalf("auto-started idea run = %v, want gemini-argon", ideaRuns)
	}

	// 3. Direct-repo card from email (Thread set, Agent empty) stays on Opus 5.5.
	if _, err := f.Act("new", Body{Request: "email work", Thread: "thread-abc"}); err != nil {
		t.Fatal(err)
	}
	runs = p.runsWithLabel(labelArg(labelStage, "plan"))
	if got := flag(runs[len(runs)-1], "--provider"); got != ReviewerModel {
		t.Errorf("email card provider = %q, want %q", got, ReviewerModel)
	}

	// 4. Direct-repo card from email with an explicit agent honors the explicit choice.
	if _, err := f.Act("new", Body{Request: "email work on codex", Thread: "thread-abc", Agent: "codex-astra"}); err != nil {
		t.Fatal(err)
	}
	runs = p.runsWithLabel(labelArg(labelStage, "plan"))
	if got := flag(runs[len(runs)-1], "--provider"); got != "codex/gpt-6-astra" {
		t.Errorf("explicit email card provider = %q, want codex/gpt-6-astra", got)
	}

	// 5. Auto-repo card from email (Repo: "auto", Thread set, Agent empty) routes onto Opus 5.5.
	withRepos(t, f, "example/desk")
	res, err := f.Act("new", Body{Request: "routed email work", Repo: "auto", Thread: "thread-routed"})
	if err != nil {
		t.Fatal(err)
	}
	pickerID := res.(map[string]string)["id"]
	storeLog(t, f, p, pickerID, "[Assistant]\nREPO: example/desk\n")
	if err := f.routeFromReply(picker(pickerID, nil)); err != nil {
		t.Fatal(err)
	}
	runs = p.runsWithLabel(gmailThreadLabel + "=thread-routed")
	if len(runs) != 1 || flag(runs[0], "--provider") != ReviewerModel || !slices.Contains(runs[0], labelArg(labelAgent, "opus")) {
		t.Errorf("auto-routed email card = %v, want opus (%s)", runs, ReviewerModel)
	}
}
