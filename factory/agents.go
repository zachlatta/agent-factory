package factory

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// ReviewerModel runs every reviewer and scout, whatever agent builds the card: Opus stays the judge.
const ReviewerModel = "claude/claude-opus-5-5"

// AgentChoice is one agent Zach can pick to plan and build a card. The planner and the builder
// are the same Paseo agent, so the choice covers the whole card, rework rounds included.
type AgentChoice struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Provider        string   `json:"provider"`                  // paseo run --provider (provider/model)
	ThinkingOptions []string `json:"thinkingOptions,omitempty"` // supported Paseo levels; nil means no control
	Thinking        string   `json:"thinking"`                  // paseo run --thinking; "" = the model's default
	Mode            string   `json:"mode"`                      // the provider's mode with no permission prompts
	Frontier        bool     `json:"frontier,omitempty"`        // currently known as a Frontier model
	Plan            string   `json:"plan"`                      // the subscription it draws on
	Note            string   `json:"note,omitempty"`
}

// defaultAgent is used when a card or idea names no agent.
const defaultAgent = "opus"

// AgentChoices is every agent a card can use. Mode ids were checked live against Paseo.
var AgentChoices = []AgentChoice{
	{ID: "opus", Label: "Opus 5.5", Provider: ReviewerModel, Thinking: "medium", ThinkingOptions: []string{"low", "medium", "high", "xhigh", "max", "ultracode"}, Mode: "bypassPermissions",
		Frontier: true, Plan: "Claude Max"},
	{ID: "sonnet", Label: "Sonnet 5.5", Provider: "claude/claude-sonnet-5-5", Thinking: "medium", ThinkingOptions: []string{"low", "medium", "high", "xhigh", "max", "ultracode"}, Mode: "bypassPermissions",
		Plan: "Claude Max"},
	{ID: "codex-sol", Label: "GPT-6.1 Sol", Provider: "codex/gpt-6.1-sol", Thinking: "medium", ThinkingOptions: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Mode: "full-access",
		Frontier: true, Plan: "ChatGPT Pro", Note: "sends this card's work to OpenAI"},
	{ID: "codex-astra", Label: "GPT-6 Astra", Provider: "codex/gpt-6-astra", Thinking: "high", ThinkingOptions: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, Mode: "full-access",
		Plan: "ChatGPT Pro", Note: "sends this card's work to OpenAI"},
	{ID: "gemini-argon", Label: "Gemini 4 Argon", Provider: "antigravity/gemini-4-argon-high", Mode: "full-access",
		Frontier: true, Plan: "Google AI Ultra", Note: "sends this card's work to Google"},
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

// withThinking validates an optional override against the model's supported levels.
// An empty override keeps the Factory default, including for older callers.
func (a AgentChoice) withThinking(level string) (AgentChoice, error) {
	if level == "" {
		return a, nil
	}
	if !slices.Contains(a.ThinkingOptions, level) {
		return a, fmt.Errorf("unsupported thinking level %q for %s", level, a.Label)
	}
	a.Thinking = level
	return a, nil
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

// resolveAgent returns the agent to start for id ("" = usage-balanced auto-suggestion). It fails if
// the id is unknown or its Paseo provider is not available on this host.
func (f *Factory) resolveAgent(id string) (AgentChoice, error) {
	if id == "" {
		if _, err := f.waitProviderStatus("claude"); err != nil {
			return AgentChoice{}, err
		}
		id = f.suggestedAgent()
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

type planScore struct {
	tier int     // 3 = healthy known, 2 = 5h window in red (<=20%), 1 = usage unknown, 0 = window exhausted (<=0%)
	pace float64 // % remaining minus % of 7d window remaining until reset
	w7   int
	w5   int
}

func usageScore(u AgentUsage, now time.Time) planScore {
	windows := decisionUsageWindows(u, now)
	if len(windows) == 0 {
		return planScore{tier: 1}
	}
	var w7 UsageWindow
	w5Pct := 0
	hasWeek, has5h := false, false
	for _, w := range windows {
		if w.RemainingPct <= 0 {
			return planScore{tier: 0}
		}
		if w.Label == "7d" {
			w7 = w
			hasWeek = true
		}
		if w.Label == "5h" {
			w5Pct = w.RemainingPct
			has5h = true
		}
	}
	// A short window cannot establish the weekly pace.
	if !hasWeek {
		return planScore{tier: 1, w5: w5Pct}
	}
	if !has5h {
		w5Pct = w7.RemainingPct
	}
	// A missing or unparsable reset time counts as a full 7-day window remaining (100%),
	// so it never outranks a plan with the same % left and a known reset time.
	weekLeftPct := 100.0
	if w7.ResetsAt != "" {
		if rt, err := time.Parse(time.RFC3339Nano, w7.ResetsAt); err == nil {
			if rem := rt.Sub(now); rem > 0 {
				weekLeftPct = min(100.0, float64(rem)/float64(7*24*time.Hour)*100.0)
			}
		}
	}
	tier := 3
	eff := w7.RemainingPct
	if has5h && w5Pct <= 20 {
		tier = 2
		eff = min(eff, w5Pct)
	}
	return planScore{
		tier: tier,
		pace: float64(eff) - weekLeftPct,
		w7:   w7.RemainingPct,
		w5:   w5Pct,
	}
}

// rankAgentChoices orders AgentChoices by availability and remaining plan quota pace,
// returning the ordered slice and the auto-suggested default agent id.
func rankAgentChoices(statuses map[string]string, usages map[string]AgentUsage, now time.Time) ([]AgentChoice, string) {
	type item struct {
		choice AgentChoice
		idx    int
		avail  bool
		score  planScore
	}
	items := make([]item, len(AgentChoices))
	for i, a := range AgentChoices {
		p := a.providerID()
		items[i] = item{
			choice: a,
			idx:    i,
			avail:  statuses[p] == "available",
			score:  usageScore(usages[p], now),
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.avail != b.avail {
			return a.avail
		}
		if !a.avail {
			return a.idx < b.idx
		}
		if a.score.tier != b.score.tier {
			return a.score.tier > b.score.tier
		}
		if math.Abs(a.score.pace-b.score.pace) > 1e-9 {
			return a.score.pace > b.score.pace
		}
		if a.score.w7 != b.score.w7 {
			return a.score.w7 > b.score.w7
		}
		if a.score.w5 != b.score.w5 {
			return a.score.w5 > b.score.w5
		}
		return a.idx < b.idx
	})
	ordered := make([]AgentChoice, len(items))
	for i, it := range items {
		ordered[i] = it.choice
	}
	def := defaultAgent
	if len(items) > 0 && items[0].avail {
		def = items[0].choice.ID
	}
	return ordered, def
}

func (f *Factory) suggestedAgent() string {
	usages := f.providerUsages()
	statuses := make(map[string]string, len(AgentChoices))
	for _, a := range AgentChoices {
		p := a.providerID()
		if _, ok := statuses[p]; !ok {
			statuses[p], _ = f.cachedProviderStatus(p)
		}
	}
	_, def := rankAgentChoices(statuses, usages, f.Now())
	return def
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
	statuses := make(map[string]string, len(AgentChoices))
	for _, a := range AgentChoices {
		p := a.providerID()
		if _, ok := statuses[p]; !ok {
			statuses[p] = f.providerStatus(p)
		}
	}
	ordered, def := rankAgentChoices(statuses, usages, f.Now())
	rows := make([]row, 0, len(ordered))
	for _, a := range ordered {
		p := a.providerID()
		st := statuses[p]
		rows = append(rows, row{a, st == "available", st, usages[p]})
	}
	writeJSON(w, 200, map[string]any{"agents": rows, "default": def, "reviewer": ReviewerModel})
}
