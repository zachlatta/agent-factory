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
	st, err := f.waitProviderStatus(a.providerID())
	if err != nil {
		return a, err
	}
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

// providerWaitTimeout bounds how long starting a card waits for the first-ever provider
// list (see waitProviderStatus). A var so tests can shrink it.
var providerWaitTimeout = 10 * time.Second

// providers caches Paseo's provider status. Paseo caches it too and only re-checks a provider
// after `paseo provider diagnostic` (seen 2026-10-13), so an unavailable one gets a diagnostic.
// Reads serve the cache and refresh in the background: no lock is held across a `paseo` call.
type providers struct {
	status     map[string]string
	listed     time.Time
	diag       map[string]time.Time
	refreshing chan struct{} // non-nil while a background refresh runs; closed when it lands
}

// kickProviders starts a background refresh if the cached list is empty or stale and none
// is running. It returns the in-flight refresh's done channel, or nil if the cache is fresh.
func (f *Factory) kickProviders() chan struct{} {
	f.provMu.Lock()
	defer f.provMu.Unlock()
	if f.prov.refreshing != nil {
		return f.prov.refreshing
	}
	if f.prov.status != nil && f.Now().Sub(f.prov.listed) < providerListTTL {
		return nil
	}
	done := make(chan struct{})
	f.prov.refreshing = done
	go f.refreshProviders(done)
	return done
}

// refreshProviders re-reads `paseo provider ls`, then prods every unavailable provider
// with a rate-limited diagnostic and lists once more. On failure it keeps no status
// (every provider reads "unknown") and is not cached, so the next check lists again.
func (f *Factory) refreshProviders(done chan struct{}) {
	defer func() {
		f.provMu.Lock()
		f.prov.refreshing = nil
		close(done)
		f.provMu.Unlock()
	}()
	st, ok := f.fetchProviders()
	now := f.Now()
	f.provMu.Lock()
	if f.prov.diag == nil {
		f.prov.diag = map[string]time.Time{}
	}
	if !ok {
		f.prov.status = nil
		f.provMu.Unlock()
		return
	}
	f.prov.status, f.prov.listed = st, now
	var prod []string
	if !f.passive() {
		seen := map[string]bool{}
		for _, a := range AgentChoices {
			p := a.providerID()
			if seen[p] || st[p] == "available" || now.Sub(f.prov.diag[p]) < providerDiagGap {
				continue
			}
			seen[p] = true
			f.prov.diag[p] = now
			prod = append(prod, p)
		}
	}
	f.provMu.Unlock()
	for _, p := range prod {
		f.paseo("provider", "diagnostic", p)
	}
	if len(prod) > 0 {
		if st, ok := f.fetchProviders(); ok {
			f.provMu.Lock()
			f.prov.status, f.prov.listed = st, f.Now()
			f.provMu.Unlock()
		}
	}
}

func (f *Factory) fetchProviders() (map[string]string, bool) {
	out, err := f.paseo("provider", "ls", "--json")
	var rows []struct{ Provider, Status string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &rows)
	}
	if err != nil {
		f.logf("paseo provider ls: %v", err)
		return nil, false
	}
	st := map[string]string{}
	for _, r := range rows {
		st[r.Provider] = r.Status
	}
	return st, true
}

// cachedProviderStatus is the cached status for provider p and whether a list has ever landed.
func (f *Factory) cachedProviderStatus(p string) (string, bool) {
	f.provMu.Lock()
	defer f.provMu.Unlock()
	if f.prov.status == nil {
		return "unknown", false
	}
	if s := f.prov.status[p]; s != "" {
		return s, true
	}
	return "unknown", true
}

// providerStatus is Paseo's cached status for provider p ("available", "error", ...;
// "unknown" if unlisted or not yet listed). It never waits for Paseo.
func (f *Factory) providerStatus(p string) string {
	st, _ := f.cachedProviderStatus(p)
	f.kickProviders()
	return st
}

// waitProviderStatus is providerStatus for starting a card: only when no list has ever
// landed does it wait (bounded by providerWaitTimeout) instead of guessing "unknown".
func (f *Factory) waitProviderStatus(p string) (string, error) {
	if st, populated := f.cachedProviderStatus(p); populated {
		f.kickProviders()
		return st, nil
	}
	done := f.kickProviders()
	if done == nil { // a list landed between the two reads
		st, _ := f.cachedProviderStatus(p)
		return st, nil
	}
	select {
	case <-done:
	case <-time.After(providerWaitTimeout):
		return "unknown", fmt.Errorf("checking whether provider %s is available timed out after %s; try again in a moment", p, providerWaitTimeout)
	}
	st, _ := f.cachedProviderStatus(p)
	return st, nil
}

func (f *Factory) providerAvailable(p string) bool { return f.providerStatus(p) == "available" }

// handleAgents lists the agent choices, whether each is available here, remaining usage, and who reviews.
func (f *Factory) handleAgents(w http.ResponseWriter, r *http.Request) {
	type row struct {
		AgentChoice
		Available bool       `json:"available"`
		Status    string     `json:"status"`
		Usage     AgentUsage `json:"usage"`
	}
	usages := f.providerUsages()
	rows := []row{}
	for _, a := range AgentChoices {
		p := a.providerID()
		st := f.providerStatus(p)
		rows = append(rows, row{a, st == "available", st, usages[p]})
	}
	writeJSON(w, 200, map[string]any{"agents": rows, "default": defaultAgent, "reviewer": ReviewerModel})
}
