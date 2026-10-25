package factory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ReviewerModel runs every reviewer and scout, whatever agent builds the card: Opus stays the judge.
const ReviewerModel = "claude/claude-opus-5-5"

// AgentChoice is one agent Zach can pick to plan and build a card. The planner and the builder
// are the same Paseo agent, so the choice covers the whole card, rework rounds included.
type AgentChoice struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"` // paseo run --provider (provider/model)
	Thinking string `json:"thinking"` // paseo run --thinking; "" = the model's default
	Mode     string `json:"mode"`     // the provider's mode with no permission prompts
	Plan     string `json:"plan"`     // the subscription it draws on
	Note     string `json:"note,omitempty"`
}

// defaultAgent is used when a card or idea names no agent.
const defaultAgent = "opus"

// AgentChoices is every agent a card can use. Mode ids were checked live against Paseo.
var AgentChoices = []AgentChoice{
	{ID: "opus", Label: "Opus 5.5", Provider: ReviewerModel, Thinking: "medium", Mode: "bypassPermissions",
		Plan: "Claude Max"},
	{ID: "codex-astra", Label: "GPT-6 Astra", Provider: "codex/gpt-6-astra", Thinking: "high", Mode: "full-access",
		Plan: "ChatGPT Pro", Note: "sends this card's work to OpenAI"},
	{ID: "codex-sol", Label: "GPT-6.1 Sol", Provider: "codex/gpt-6.1-sol", Thinking: "medium", Mode: "full-access",
		Plan: "ChatGPT Pro", Note: "sends this card's work to OpenAI"},
	{ID: "gemini-argon", Label: "Gemini 4 Argon", Provider: "antigravity/gemini-4-argon-high", Mode: "full-access",
		Plan: "Google AI Ultra", Note: "sends this card's work to Google"},
	{ID: "muse-wonder", Label: "Muse Wonder", Provider: "muse/muse-wonder-full-1.0", Mode: "allowAll",
		Plan: "Muse Code Power", Note: "sends this card's work to Meta"},
}

func agentByID(id string) (AgentChoice, bool) {
	for _, a := range AgentChoices {
		if a.ID == id {
			return a, true
		}
	}
	return AgentChoice{}, false
}

// runArgs are the `paseo run` flags that start this agent.
func (a AgentChoice) runArgs() []string {
	args := []string{"run", "--provider", a.Provider}
	if a.Thinking != "" {
		args = append(args, "--thinking", a.Thinking)
	}
	return append(args, "--mode", a.Mode)
}

func (a AgentChoice) providerID() string { return strings.SplitN(a.Provider, "/", 2)[0] }

// reviewerRunArgs start a reviewer or scout.
func reviewerRunArgs() []string {
	a, _ := agentByID(defaultAgent)
	return a.runArgs()
}

// checkAgentID accepts "" (the default) or a known agent id.
func checkAgentID(id string) error {
	if id == "" {
		return nil
	}
	if _, ok := agentByID(id); !ok {
		return fmt.Errorf("unknown agent %q", id)
	}
	return nil
}

// resolveAgent returns the agent to start for id ("" = default). It fails if the id is unknown or
// its Paseo provider is not available on this host.
func (f *Factory) resolveAgent(id string) (AgentChoice, error) {
	if id == "" {
		id = defaultAgent
	}
	a, ok := agentByID(id)
	if !ok {
		return a, fmt.Errorf("unknown agent %q", id)
	}
	st := f.providerStatus(a.providerID())
	// "unknown" means `paseo provider ls` failed: the default agent starts anyway, as it did before
	// there was a provider check, so a Paseo hiccup cannot stop board, idea or email cards.
	if st != "available" && !(st == "unknown" && a.ID == defaultAgent) {
		return a, fmt.Errorf("%s is not available on %s (Paseo provider %s: %s)", a.Label, f.cfg.Host,
			a.providerID(), st)
	}
	return a, nil
}

const (
	providerListTTL = 5 * time.Minute  // how long a `paseo provider ls` answer is trusted
	providerDiagGap = 30 * time.Minute // at most one `paseo provider diagnostic` per provider this often
)

// providers caches Paseo's provider status. Paseo caches it too and only re-checks a provider
// after `paseo provider diagnostic` (seen 2026-10-13), so an unavailable one gets a diagnostic.
type providers struct {
	status map[string]string
	listed time.Time
	diag   map[string]time.Time
}

// refreshProviders re-reads `paseo provider ls` if the cached answer is stale. f.provMu held.
func (f *Factory) refreshProviders() {
	now := f.Now()
	if f.prov.status != nil && now.Sub(f.prov.listed) < providerListTTL {
		return
	}
	if !f.listProviders(now) || f.cfg.ReadOnly {
		return
	}
	diagnosed := false
	for _, a := range AgentChoices {
		p := a.providerID()
		if f.prov.status[p] == "available" || now.Sub(f.prov.diag[p]) < providerDiagGap {
			continue
		}
		f.prov.diag[p] = now
		f.paseo("provider", "diagnostic", p)
		diagnosed = true
	}
	if diagnosed {
		f.listProviders(now)
	}
}

// listProviders reads `paseo provider ls`. On failure it keeps no status (every provider reads
// "unknown") and is not cached, so the next check lists again.
func (f *Factory) listProviders(now time.Time) bool {
	if f.prov.diag == nil {
		f.prov.diag = map[string]time.Time{}
	}
	out, err := f.paseo("provider", "ls", "--json")
	var rows []struct{ Provider, Status string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &rows)
	}
	if err != nil {
		f.logf("paseo provider ls: %v", err)
		f.prov.status = nil
		return false
	}
	st := map[string]string{}
	for _, r := range rows {
		st[r.Provider] = r.Status
	}
	f.prov.status, f.prov.listed = st, now
	return true
}

// providerStatus is Paseo's status for provider p ("available", "error", ...; "unknown" if unlisted).
func (f *Factory) providerStatus(p string) string {
	f.provMu.Lock()
	defer f.provMu.Unlock()
	f.refreshProviders()
	if s := f.prov.status[p]; s != "" {
		return s
	}
	return "unknown"
}

func (f *Factory) providerAvailable(p string) bool { return f.providerStatus(p) == "available" }

// handleAgents lists the agent choices, whether each is available here, and who reviews.
func (f *Factory) handleAgents(w http.ResponseWriter, r *http.Request) {
	type row struct {
		AgentChoice
		Available bool   `json:"available"`
		Status    string `json:"status"`
	}
	rows := []row{}
	for _, a := range AgentChoices {
		st := f.providerStatus(a.providerID())
		rows = append(rows, row{a, st == "available", st})
	}
	writeJSON(w, 200, map[string]any{"agents": rows, "default": defaultAgent, "reviewer": ReviewerModel})
}
