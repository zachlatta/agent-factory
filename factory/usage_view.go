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
	"sync"
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
	Agents             int   `json:"agents"`
	Readable           int   `json:"readableSessions"`
	Missing            int   `json:"missingSessions"`
	Unsupported        int   `json:"unsupportedSessions"`
	Errors             int   `json:"errors"`
	Duplicates         int   `json:"duplicateSessions"`
	TimingWithoutAgent int   `json:"timingWithoutAgent"`
	Disappeared        int   `json:"disappearedSources"`
	Limited            bool  `json:"limited"`
	Skipped            int   `json:"limitedSessions"`
	BytesRead          int64 `json:"bytesRead"`    // session bytes read by this refresh
	PendingBytes       int64 `json:"pendingBytes"` // bytes of found sessions not decoded yet
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
	scans   int             // refreshes started, under tokenMu

	// Between refreshes, memory only: each session file's decoder state and each agent file's
	// metadata, so a refresh reads only what changed and goes on to records the last one left.
	scan  sync.Mutex
	files map[string]*tokenFile
	metas map[string]usageMeta
	// budget, chunk and metaLimit override tokenScanBudget, tokenFileLimit and usageMetaLimit (tests).
	budget, chunk int64
	metaLimit     int
}

// tokenScanBudget is the session bytes one refresh may read; tokenFileLimit is the most from one file.
// Agent files are parsed at most usageMetaLimit per refresh; unchanged ones come from the cache.
const (
	usageSliceCooldown       = 15 * time.Second
	tokenScanBudget    int64 = 128 << 20
	usageMetaLimit           = 2000
)

// usageMeta is one agent file's accounting metadata as of its size and modification time.
type usageMeta struct {
	size     int64
	mod      time.Time
	agent    Agent
	internal bool
}

func (f *Factory) handleUsage(w http.ResponseWriter, r *http.Request) {
	f.tokenMu.Lock()
	// One scan at a time, started only by a request: a complete snapshot lasts usageTTL, a limited
	// one only usageSliceCooldown, so an open usage page counts the next bounded slice soon.
	period := usageTTL
	if f.tokenCache.view.Coverage.Limited {
		period = usageSliceCooldown
	}
	if f.tokenCache.done == nil && (f.tokenCache.fetched.IsZero() || f.Now().Sub(f.tokenCache.fetched) >= period) {
		f.tokenCache.done = make(chan struct{})
		f.tokenCache.scans++
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
				return resolveCardID(agents, a.ID)
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
	c := &f.tokenCache
	c.scan.Lock()
	defer c.scan.Unlock()
	if c.files == nil {
		c.files = map[string]*tokenFile{}
	}
	if c.metas == nil {
		c.metas = map[string]usageMeta{}
	}
	listed := map[string]bool{}
	parsed, metaLimit := 0, usageMetaLimit
	if c.metaLimit > 0 {
		metaLimit = c.metaLimit
	}
	for _, path := range paths {
		if ctx.Err() != nil {
			v.Coverage.Limited = true
			break
		}
		listed[path] = true
		st, err := os.Stat(path)
		if err != nil {
			v.Coverage.Errors++
			continue
		}
		m, ok := c.metas[path]
		if !ok || m.size != st.Size() || !m.mod.Equal(st.ModTime()) {
			if parsed >= metaLimit {
				// left for the next refresh, which parses the files this one did not
				v.Coverage.Limited = true
				continue
			}
			parsed++
			if m, ok = readUsageMeta(path, st); !ok {
				delete(c.metas, path)
				v.Coverage.Errors++
				continue
			}
			c.metas[path] = m
		}
		if m.internal {
			continue
		}
		agents[m.agent.ID] = m.agent
		sources["agent:"+m.agent.ID] = true
	}
	for path := range c.metas {
		if !listed[path] {
			delete(c.metas, path)
		}
	}
	allTiming := f.timings.all()
	for id := range allTiming {
		if _, ok := agents[id]; !ok {
			v.Coverage.TimingWithoutAgent++
		}
	}
	ids := make([]string, 0, len(agents))
	sessionOwners, shared, ambiguous := map[string]string{}, map[string]bool{}, map[string]bool{}
	unfinished := map[string]bool{}
	metadataComplete := !v.Coverage.Limited && v.Coverage.Errors == 0
	for id := range agents {
		ids = append(ids, id)
		a := agents[id]
		if nativeSessionID(a.SessionID) == "" {
			continue
		}
		key := a.Provider + ":" + nativeSessionID(a.SessionID)
		finished := a.Status == "closed" || (a.archived() && a.Status != "running")
		unfinished[key] = unfinished[key] || !finished
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
	budget, chunk := tokenScanBudget, tokenFileLimit
	if c.budget > 0 {
		budget = c.budget
	}
	if c.chunk > 0 {
		chunk = c.chunk
	}
	var used int64
	touched := map[string]bool{}
	readPath := func(path, key, provider string, finished bool) tokenRead {
		if path == "" {
			return tokenRead{status: "missing"}
		}
		touched[path] = true
		tf := c.files[path]
		if tf == nil {
			tf = &tokenFile{}
			c.files[path] = tf
		}
		r, n := tf.read(ctx, path, provider, budget-used, chunk, finished)
		used += n
		switch r.status {
		case "missing":
			delete(c.files, path)
			return r
		case "limited":
			v.Coverage.PendingBytes += tf.pending()
		}
		if tf.info != nil {
			sources["session:"+key] = true
		}
		return r
	}
	complete := !v.Coverage.Limited // every agent file was read: unreferenced sessions can go
	for _, id := range ids {
		if ctx.Err() != nil {
			v.Coverage.Limited = true
			complete = false
			break
		}
		a := agents[id]
		v.Coverage.Agents++
		owner := usageOwner(a, agents)
		row := UsageTokenRow{Agent: id, Title: a.Title, Card: owner, CardTitle: agents[owner].Title, Provider: a.Provider, Plan: usagePlan(a.Provider), Model: a.Model, Role: usageRole(a), Stage: "unassigned"}
		key := a.Provider + ":" + nativeSessionID(a.SessionID)
		finished := metadataComplete && !unfinished[key]
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
			read = readPath(path, key, a.Provider, finished)
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
							complete = false
							break
						}
						if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
							reads = append(reads, readPath(filepath.Join(dir, "subagents", entry.Name()), key+"/"+entry.Name(), a.Provider, finished))
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
	if complete {
		// sessions no agent points at any more; a cut-short refresh keeps them for the next
		for path := range c.files {
			if !touched[path] {
				delete(c.files, path)
			}
		}
	}
	for row, n := range rows {
		row.Counts = n
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
	v.Coverage.BytesRead = used
	v.UpdatedAt = iso(f.Now())
	return v, sources
}

// readUsageMeta parses the accounting metadata of one agent file; ok is false for a bad file.
func readUsageMeta(path string, st os.FileInfo) (usageMeta, bool) {
	file, err := os.Open(path)
	if err != nil {
		return usageMeta{}, false
	}
	defer file.Close()
	var d struct {
		ID, Title, Provider, Cwd, CreatedAt, ArchivedAt, LastStatus string
		Internal                                                    bool
		Labels                                                      map[string]string
		Config                                                      struct{ Model string }
		Persistence                                                 struct{ SessionID string }
	}
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&d); err != nil || d.ID == "" {
		return usageMeta{}, false
	}
	return usageMeta{size: st.Size(), mod: st.ModTime(), internal: d.Internal,
		agent: Agent{ID: d.ID, Title: d.Title, Provider: d.Provider, Model: d.Config.Model, Cwd: d.Cwd, CreatedAt: parseTime(d.CreatedAt), ArchivedAt: parseTime(d.ArchivedAt), Status: d.LastStatus, SessionID: d.Persistence.SessionID, Labels: d.Labels}}, true
}
