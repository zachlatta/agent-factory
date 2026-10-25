package factory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
		}
	}
}

func TestProviderStatusRefreshAndDiagnosticRateLimit(t *testing.T) {
	f, p := newTestFactory(t)
	p.providers["antigravity"] = "error"
	count := func(verb string) int {
		n := 0
		for _, c := range p.calls {
			if c[0] == "provider" && c[1] == verb {
				n++
			}
		}
		return n
	}
	if f.providerAvailable("antigravity") {
		t.Fatal("antigravity available while Paseo says error")
	}
	if count("diagnostic") != 1 || count("ls") != 2 {
		t.Errorf("first check: ls %d, diagnostic %d; want 2 and 1", count("ls"), count("diagnostic"))
	}
	f.providerAvailable("antigravity") // cached: no new calls
	if count("ls") != 2 {
		t.Errorf("cached check listed again: ls %d", count("ls"))
	}

	now := t0.Add(6 * time.Minute) // list is stale, diagnostic is not due yet
	f.Now = func() time.Time { return now }
	f.providerAvailable("antigravity")
	if count("ls") != 3 || count("diagnostic") != 1 {
		t.Errorf("after 6 min: ls %d, diagnostic %d; want 3 and 1", count("ls"), count("diagnostic"))
	}

	now = t0.Add(31 * time.Minute) // diagnostic due again, and this time it fixes the provider
	p.fixed["antigravity"] = true
	if !f.providerAvailable("antigravity") {
		t.Error("antigravity still unavailable after a diagnostic fixed it")
	}
	if count("diagnostic") != 2 {
		t.Errorf("diagnostic %d, want 2", count("diagnostic"))
	}
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
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Default  string `json:"default"`
		Reviewer string `json:"reviewer"`
		Agents   []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
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
	// Muse and Gemini only when Zach picks them; automatic routing is a later card.
	// An agent-submitted idea may suggest an agent, but only Zach's Start uses it.
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
