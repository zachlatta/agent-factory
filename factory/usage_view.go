package factory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UsageView is a snapshot of retained local records, not a subscription bill.
// Rows are disjoint: totals sum the rows, cache/reasoning columns are subsets.
type UsageView struct {
	UpdatedAt  string          `json:"updatedAt,omitempty"`
	Refreshing bool            `json:"refreshing"`
	Rows       []UsageTokenRow `json:"rows"`
	Totals     TokenCounts     `json:"totals"`
	Coverage   UsageCoverage   `json:"coverage"`
	Plans      []UsagePlan     `json:"plans"`
	Stages     [][3]string     `json:"stages"`
}

type UsagePlan struct {
	Provider  string     `json:"provider"`
	Plan      string     `json:"plan"`
	Allowance AgentUsage `json:"allowance"`
}

type UsageCoverage struct {
	Agents             int  `json:"agents"`
	Readable           int  `json:"readableSessions"`
	Missing            int  `json:"missingSessions"`
	Unsupported        int  `json:"unsupportedSessions"`
	Errors             int  `json:"errors"`
	Duplicates         int  `json:"duplicateSessions"`
	TimingWithoutAgent int  `json:"timingWithoutAgent"`
	Disappeared        int  `json:"disappearedSources"`
	Limited            bool `json:"limited"`
	Skipped            int  `json:"limitedSessions"`
}

type UsageTokenRow struct {
	Agent         string      `json:"agent"`
	Title         string      `json:"title"`
	Card          string      `json:"card,omitempty"`
	CardTitle     string      `json:"cardTitle,omitempty"`
	Provider      string      `json:"provider"`
	Plan          string      `json:"plan"`
	Model         string      `json:"model"`
	Role          string      `json:"role"`
	Stage         string      `json:"stage"`
	Span          int         `json:"span"` // one-based TimingStore event index; repeated build spans identify rework
	Round         int         `json:"round"`
	Status        string      `json:"status"`
	SharedSession bool        `json:"sharedSession"`
	Counts        TokenCounts `json:"counts"`
}

type tokenUsageCache struct {
	view    UsageView
	fetched time.Time
	done    chan struct{}
	seen    map[string]bool // memory only: disappearance detection restarts with the process
}

func (f *Factory) handleUsage(w http.ResponseWriter, r *http.Request) {
	f.tokenMu.Lock()
	if f.tokenCache.done == nil && (f.tokenCache.fetched.IsZero() || f.Now().Sub(f.tokenCache.fetched) >= usageTTL) {
		f.tokenCache.done = make(chan struct{})
		go f.refreshTokenUsage()
	}
	v := f.tokenCache.view
	v.Refreshing = f.tokenCache.done != nil
	f.tokenMu.Unlock()
	if v.Rows == nil {
		v.Rows = []UsageTokenRow{}
	}
	v.Stages = Stages
	allowances := f.providerUsages()
	seen := map[string]bool{}
	for _, a := range AgentChoices {
		p := a.providerID()
		if !seen[p] {
			v.Plans = append(v.Plans, UsagePlan{Provider: p, Plan: a.Plan, Allowance: allowances[p]})
			seen[p] = true
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (f *Factory) refreshTokenUsage() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, sources := f.collectTokenUsage(ctx)
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	if f.tokenCache.seen == nil {
		f.tokenCache.seen = map[string]bool{}
	}
	for source := range sources {
		f.tokenCache.seen[source] = true
	}
	for source := range f.tokenCache.seen {
		if !v.Coverage.Limited && !sources[source] {
			v.Coverage.Disappeared++
		}
	}
	f.tokenCache.view = v
	f.tokenCache.fetched = f.Now()
	close(f.tokenCache.done)
	f.tokenCache.done = nil
}

func usagePlan(provider string) string {
	for _, a := range AgentChoices {
		if a.providerID() == provider {
			return a.Plan
		}
	}
	return "unknown"
}

// usageOwner follows reviewers and nested sub-agents to their card without
// inventing an owner for an orphan or cycling through malformed parent labels.
// Quiet anchors are grouping boundaries, so each card retains its own token usage.
func usageOwner(a Agent, agents map[string]Agent) string {
	if boardAnchor(a) {
		return ""
	}
	seen := map[string]bool{}
	for !seen[a.ID] {
		seen[a.ID] = true
		parent := a.L(labelReviewOf)
		if parent == "" {
			parent = a.Labels["paseo.parent-agent-id"]
		}
		if parent == "" || boardAnchor(agents[parent]) {
			if timed(a) {
				return a.ID
			}
			return ""
		}
		next, ok := agents[parent]
		if !ok {
			return parent
		}
		a = next
	}
	return ""
}

// The usage inventory deliberately does not call LoadAgents: that drops older
// archives and resolves repos with git. Only metadata needed for accounting is read.
func (f *Factory) collectTokenUsage(ctx context.Context) (UsageView, map[string]bool) {
	now := f.Now()
	v := UsageView{Rows: []UsageTokenRow{}}
	sources := map[string]bool{}
	agents := map[string]Agent{}
	paths, _ := filepath.Glob(filepath.Join(f.cfg.Home, ".paseo/agents/*/*.json"))
	if _, err := os.Stat(filepath.Join(f.cfg.Home, ".paseo/agents")); err != nil {
		v.Coverage.Errors++
	}
	var used int64
	for i, path := range paths {
		if ctx.Err() != nil || i >= 2000 {
			v.Coverage.Limited = true
			break
		}
		file, err := os.Open(path)
		if err != nil {
			v.Coverage.Errors++
			continue
		}
		var d struct {
			ID, Title, Provider, Cwd, CreatedAt string
			Internal                            bool
			Labels                              map[string]string
			Config                              struct{ Model string }
			Persistence                         struct{ SessionID string }
		}
		err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&d)
		file.Close()
		if err != nil || d.ID == "" {
			v.Coverage.Errors++
			continue
		}
		if d.Internal {
			continue
		}
		agents[d.ID] = Agent{ID: d.ID, Title: d.Title, Provider: d.Provider, Model: d.Config.Model, Cwd: d.Cwd, CreatedAt: parseTime(d.CreatedAt), SessionID: d.Persistence.SessionID, Labels: d.Labels}
		sources["agent:"+d.ID] = true
	}
	allTiming := f.timings.all()
	for id := range allTiming {
		if _, ok := agents[id]; !ok {
			v.Coverage.TimingWithoutAgent++
		}
	}
	ids := make([]string, 0, len(agents))
	sessionOwners, shared, ambiguous := map[string]string{}, map[string]bool{}, map[string]bool{}
	for id := range agents {
		ids = append(ids, id)
		a := agents[id]
		if nativeSessionID(a.SessionID) == "" {
			continue
		}
		key := a.Provider + ":" + nativeSessionID(a.SessionID)
		owner := usageOwner(a, agents) + ":" + usageRole(a)
		if previous, ok := sessionOwners[key]; ok {
			shared[key] = true
			if previous != owner {
				ambiguous[key] = true
			}
		} else {
			sessionOwners[key] = owner
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := agents[ids[i]], agents[ids[j]]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	seen := map[string]bool{}
	rows := map[UsageTokenRow]TokenCounts{}
	readPath := func(path, key, provider string) tokenRead {
		if path == "" {
			return tokenRead{status: "missing"}
		}
		st, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return tokenRead{status: "missing"}
			}
			return tokenRead{status: "error"}
		}
		sources["session:"+key] = true
		if st.Size() > tokenFileLimit || used+st.Size() > 128<<20 {
			return tokenRead{status: "limited"}
		}
		used += st.Size()
		return readTokenFile(ctx, path, provider)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			v.Coverage.Limited = true
			break
		}
		a := agents[id]
		v.Coverage.Agents++
		owner := usageOwner(a, agents)
		row := UsageTokenRow{Agent: id, Title: a.Title, Card: owner, CardTitle: agents[owner].Title, Provider: a.Provider, Plan: usagePlan(a.Provider), Model: a.Model, Role: usageRole(a), Stage: "unassigned"}
		key := a.Provider + ":" + nativeSessionID(a.SessionID)
		row.SharedSession = shared[key]
		if ambiguous[key] {
			owner = ""
			row.Card = ""
			row.CardTitle = ""
			row.Role = "unassigned"
		}
		read := tokenRead{status: "missing"}
		var reads []tokenRead
		switch {
		case a.Provider != "claude" && a.Provider != "codex" && a.Provider != "muse":
			read.status = "unsupported"
		case nativeSessionID(a.SessionID) == "":
		case seen[key]:
			read.status = "duplicate"
		default:
			seen[key] = true
			path := tokenPath(f.cfg.Home, a)
			read = readPath(path, key, a.Provider)
			if a.Provider == "claude" {
				dir := strings.TrimSuffix(path, ".jsonl")
				if dir == "" {
					dir = firstFile(filepath.Join(f.cfg.Home, ".claude/projects/*", nativeSessionID(a.SessionID)))
				}
				if dir != "" {
					entries, err := os.ReadDir(filepath.Join(dir, "subagents"))
					if err != nil && !os.IsNotExist(err) {
						v.Coverage.Errors++
					}
					for _, entry := range entries {
						if ctx.Err() != nil {
							v.Coverage.Limited = true
							break
						}
						if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
							reads = append(reads, readPath(filepath.Join(dir, "subagents", entry.Name()), key+"/"+entry.Name(), a.Provider))
						}
					}
				}
			}
		}
		reads = append(reads, read)
		for _, read := range reads {
			switch read.status {
			case "readable":
				v.Coverage.Readable++
			case "missing":
				v.Coverage.Missing++
			case "unsupported":
				v.Coverage.Unsupported++
			case "duplicate":
				v.Coverage.Duplicates++
			case "limited":
				v.Coverage.Skipped++
				v.Coverage.Limited = true
			default:
				v.Coverage.Errors++
			}
			row.Status = read.status
			if len(read.events) == 0 {
				if _, exists := rows[row]; !exists {
					rows[row] = TokenCounts{}
				}
			}
			for _, e := range read.events {
				r := row
				if e.Model != "" {
					r.Model = e.Model
				}
				r.Stage, r.Span, r.Round = tokenStage(e, allTiming[owner], now)
				c := rows[r]
				c.add(e.Counts)
				rows[r] = c
				v.Totals.add(e.Counts)
			}
		}
	}
	for row, c := range rows {
		row.Counts = c
		v.Rows = append(v.Rows, row)
	}
	sort.Slice(v.Rows, func(i, j int) bool {
		a, b := v.Rows[i], v.Rows[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Span != b.Span {
			return a.Span < b.Span
		}
		return a.Model < b.Model
	})
	v.UpdatedAt = iso(f.Now())
	return v, sources
}
