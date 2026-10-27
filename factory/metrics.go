package factory

import (
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GET /api/metrics?days=N (default 7, at most 30): card turnaround, per-step durations, and
// per-turn agent durations from local transcripts joined to CardTiming.Sessions. It counts cards
// that reached Done in the window and landed or are inbox cards with a confirmed reply.
// Unanswered cards, cards that did not land and partial ones are counted
// in excluded. Optional query parameters (repo, source, tier, role, provider, model, step) filter
// the results.

const (
	metricsDays    = 7
	metricsMaxDays = 30
)

type Metrics struct {
	Days              int               `json:"days"`
	From              string            `json:"from"`
	To                string            `json:"to"`
	Filters           MetricsFilter     `json:"filters,omitempty"`
	Cards             int               `json:"cards"`
	Excluded          MetricsSkipped    `json:"excluded"`
	Coverage          MetricsCoverage   `json:"coverage"`
	Turnaround        MetricGroup       `json:"turnaround"`
	BySource          []MetricGroup     `json:"bySource"`
	ByTier            []MetricGroup     `json:"byTier"`
	BySourceTier      []MetricGroup     `json:"bySourceTier"`
	ByRepo            []MetricGroup     `json:"byRepo"`
	BySourceRepo      []MetricGroup     `json:"bySourceRepo"`
	ByAgent           []MetricGroup     `json:"byAgent"`
	Steps             []StepMetric      `json:"steps"`
	StepsByRepo       []StepMetric      `json:"stepsByRepo"`
	AgentTurns        []AgentTurnMetric `json:"agentTurns"`
	AgentTurnsByRole  []AgentTurnMetric `json:"agentTurnsByRole"`
	AgentTurnsByModel []AgentTurnMetric `json:"agentTurnsByModel"`
	AgentTurnsByStep  []AgentTurnMetric `json:"agentTurnsByStep"`
	AgentTurnsByRepo  []AgentTurnMetric `json:"agentTurnsByRepo"`
}

type MetricsFilter struct {
	Repo     string `json:"repo,omitempty"`
	Source   string `json:"source,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Role     string `json:"role,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Step     string `json:"step,omitempty"`
}

type MetricsSkipped struct {
	NoReply   int `json:"noReply"`   // inbox cards without a confirmed reply
	NotLanded int `json:"notLanded"` // no landing or confirmed inbox reply
	Partial   int `json:"partial"`   // first seen mid-flight: history incomplete
}

type MetricsCoverage struct {
	CardsWithRepo        int `json:"cardsWithRepo"`
	CardsWithoutRepo     int `json:"cardsWithoutRepo"`
	CardsWithSessions    int `json:"cardsWithSessions"`
	CardsWithoutSessions int `json:"cardsWithoutSessions"`
	LinkedSessions       int `json:"linkedSessions"`
	MatchedSessions      int `json:"matchedSessions"`
	UnmatchedSessions    int `json:"unmatchedSessions"`
	Turns                int `json:"turns"`
	MissingBoundaries    int `json:"missingBoundaries"`
}

type MetricGroup struct {
	Repo     string   `json:"repo,omitempty"`
	Source   string   `json:"source,omitempty"`
	Tier     string   `json:"tier,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Model    string   `json:"model,omitempty"`
	N        int      `json:"n"`
	LandedN  int      `json:"landedN"`  // cards contributing a toLanded sample
	EndToEnd Quantile `json:"endToEnd"` // seconds: asked to replied, else landed
	ToLanded Quantile `json:"toLanded"` // seconds: asked to landed
	ToDone   Quantile `json:"toDone"`   // seconds: asked to done
	ZachWait Quantile `json:"zachWait"` // seconds: time in Needs Zach up to done
}

type StepMetric struct {
	Step           string   `json:"step"`
	Repo           string   `json:"repo,omitempty"`
	Source         string   `json:"source,omitempty"`
	Cards          int      `json:"cards"`
	Visits         int      `json:"visits"`
	RepeatedVisits int      `json:"repeatedVisits"`
	VisitSecs      Quantile `json:"visitSecs"`
	CardSecs       Quantile `json:"cardSecs"`
}

type AgentTurnMetric struct {
	Repo            string   `json:"repo,omitempty"`
	Source          string   `json:"source,omitempty"`
	Role            string   `json:"role,omitempty"`
	Provider        string   `json:"provider,omitempty"`
	Model           string   `json:"model,omitempty"`
	Step            string   `json:"step,omitempty"`
	Cards           int      `json:"cards"`
	Sessions        int      `json:"sessions"`
	Turns           int      `json:"turns"`
	TurnSecs        Quantile `json:"turnSecs"`
	SessionTurnSecs Quantile `json:"sessionTurnSecs"`
}

type Quantile struct {
	Average int `json:"average"`
	Median  int `json:"median"`
	P90     int `json:"p90"`
}

type metricRow struct {
	id, repo, source, tier, provider, model string
	e2e, toLanded, toDone, zach             int
	hasLanding                              bool
}

type stepVisitRow struct {
	cardID, repo, source, step string
	secs                       int
}

type turnRow struct {
	cardID, sessionKey, repo, source, role, provider, model, step string
	secs                                                          int
}

type turnSpan struct {
	start, end time.Time
	complete   bool
}

type cachedTranscriptTurns struct {
	path  string
	mtime int64
	size  int64
	turns []turnSpan
}

// transcriptTurnCache caches extracted turn timestamps by file mtime and size. Transcript text is never retained.
type transcriptTurnCache struct {
	mu      sync.Mutex
	entries map[string]cachedTranscriptTurns
}

func (c *transcriptTurnCache) turns(home, provider, sessionID string) ([]turnSpan, bool) {
	id := nativeSessionID(sessionID)
	if home == "" || provider == "" || id == "" {
		return nil, false
	}
	key := home + "\x00" + provider + ":" + id
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && entry.path != "" {
		if st, err := os.Stat(entry.path); err == nil && st.ModTime().UnixNano() == entry.mtime && st.Size() == entry.size {
			out := slices.Clone(entry.turns)
			c.mu.Unlock()
			return out, true
		}
	}
	c.mu.Unlock()

	tr, err := ReadTranscript(home, TranscriptSource{Provider: provider, SessionID: id}, 0)
	if err != nil || tr.Fallback || tr.Path == "" {
		return nil, false
	}
	turns := extractTurnSpans(tr.Events)
	if st, err := os.Stat(tr.Path); err == nil {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]cachedTranscriptTurns{}
		}
		c.entries[key] = cachedTranscriptTurns{
			path:  tr.Path,
			mtime: st.ModTime().UnixNano(),
			size:  st.Size(),
			turns: slices.Clone(turns),
		}
		c.mu.Unlock()
	}
	return turns, true
}

func extractTurnSpans(events []Event) []turnSpan {
	var turns []turnSpan
	cur := -1
	inPrompt := false
	promptLine := 0
	for _, ev := range events {
		if ev.Kind == "system" {
			continue
		}
		isUser := ev.Kind == "user" || (ev.Kind == "image" && ev.From == "user")
		if isUser {
			if inPrompt && ev.Line > 0 && ev.Line == promptLine {
				if turns[cur].start.IsZero() {
					turns[cur].start = parseTime(ev.Ts)
				}
				continue
			}
			turns = append(turns, turnSpan{start: parseTime(ev.Ts)})
			cur = len(turns) - 1
			inPrompt = true
			promptLine = ev.Line
			continue
		}
		inPrompt = false
		if cur < 0 {
			turns = append(turns, turnSpan{})
			cur = len(turns) - 1
		}
		if ev.Kind == "assistant" {
			if at := parseTime(ev.Ts); !at.IsZero() {
				turns[cur].end = at
			}
		}
	}
	for i := range turns {
		t := &turns[i]
		t.complete = !t.start.IsZero() && !t.end.IsZero() && !t.end.Before(t.start)
	}
	return turns
}

// metrics summarises the timings of cards that reached Done in the days before now.
func metrics(all map[string]CardTiming, now time.Time, days int) Metrics {
	return computeMetrics(all, "", nil, now, days, MetricsFilter{})
}

func computeMetrics(all map[string]CardTiming, home string, cache *transcriptTurnCache, now time.Time, days int, filter MetricsFilter) Metrics {
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	m := Metrics{
		Days:              days,
		From:              iso(from),
		To:                iso(now),
		Filters:           filter,
		BySource:          []MetricGroup{},
		ByTier:            []MetricGroup{},
		BySourceTier:      []MetricGroup{},
		ByRepo:            []MetricGroup{},
		BySourceRepo:      []MetricGroup{},
		ByAgent:           []MetricGroup{},
		Steps:             []StepMetric{},
		StepsByRepo:       []StepMetric{},
		AgentTurns:        []AgentTurnMetric{},
		AgentTurnsByRole:  []AgentTurnMetric{},
		AgentTurnsByModel: []AgentTurnMetric{},
		AgentTurnsByStep:  []AgentTurnMetric{},
		AgentTurnsByRepo:  []AgentTurnMetric{},
	}
	if cache == nil {
		cache = &transcriptTurnCache{}
	}

	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var rows []metricRow
	var visits []stepVisitRow
	var turns []turnRow

	needSubFilter := filter.Role != "" || filter.Provider != "" || filter.Model != "" || filter.Step != ""
	needSessionMatch := filter.Role != "" || filter.Provider != "" || filter.Model != ""

	for _, id := range ids {
		t := all[id]
		done := t.doneAt()
		if done.IsZero() || done.Before(from) || done.After(now) {
			continue
		}
		repo := t.Repo
		if repo == "" {
			repo = "unknown"
		}
		tier := t.Tier
		if tier == "" {
			tier = "none"
		}
		if (filter.Repo != "" && repo != filter.Repo) ||
			(filter.Source != "" && t.Source != filter.Source) ||
			(filter.Tier != "" && tier != filter.Tier) {
			continue
		}
		hasLanding := t.LandedAt.After(t.RequestedAt)
		switch {
		case t.Partial || t.RequestedAt.IsZero():
			m.Excluded.Partial++
			continue
		case t.NoReply || (t.inbox() && t.RepliedAt.IsZero()):
			m.Excluded.NoReply++
			continue
		case !hasLanding && !(t.inbox() && !t.RepliedAt.IsZero()):
			m.Excluded.NotLanded++
			continue
		}

		var cardVisits []stepVisitRow
		matchedStepVisit := false
		for i, e := range t.Events {
			if e.Stage == "landed" {
				continue
			}
			end := done
			if i+1 < len(t.Events) {
				end = t.Events[i+1].At
			}
			if filter.Step != "" && e.Stage != filter.Step {
				continue
			}
			matchedStepVisit = true
			cardVisits = append(cardVisits, stepVisitRow{
				cardID: id,
				repo:   repo,
				source: t.Source,
				step:   e.Stage,
				secs:   max(0, secs(end.Sub(e.At))),
			})
		}

		// Group session links on this card by (provider, nativeSessionID) so a multi-step session is read once.
		type sessionGroup struct {
			key   string
			links []TimingSession
		}
		var groups []sessionGroup
		bySession := map[string]int{}
		unmatchedLinks := 0
		for _, s := range t.Sessions {
			role := s.Role
			if role == "" {
				role = "fellow"
			}
			if (filter.Role != "" && role != filter.Role) || (filter.Provider != "" && s.Provider != filter.Provider) {
				continue
			}
			sid := nativeSessionID(s.SessionID)
			if s.Provider == "" || sid == "" {
				unmatchedLinks++
				continue
			}
			k := s.Provider + ":" + sid
			if idx, ok := bySession[k]; ok {
				groups[idx].links = append(groups[idx].links, s)
			} else {
				bySession[k] = len(groups)
				groups = append(groups, sessionGroup{key: k, links: []TimingSession{s}})
			}
		}

		var cardTurns []turnRow
		cardMatchedSessions := 0
		cardUnmatchedSessions := unmatchedLinks
		cardMissingBoundaries := 0
		matchedSessionFilter := false

		for _, g := range groups {
			last := g.links[len(g.links)-1]
			role := last.Role
			if role == "" {
				role = "fellow"
			}
			spans, ok := cache.turns(home, last.Provider, last.SessionID)
			if !ok {
				if filter.Model == "" && filter.Step == "" {
					cardUnmatchedSessions++
				} else if sessionLinksMatch(g.links, filter) {
					cardUnmatchedSessions++
					matchedSessionFilter = true
				}
				continue
			}
			sessionHadMatch := filter.Model == "" && filter.Step == ""
			if len(spans) == 0 && sessionHadMatch {
				cardMissingBoundaries++
			}
			for _, sp := range spans {
				step, model := turnStepAndModel(sp, g.links, t.Events)
				if (filter.Model != "" && model != filter.Model) || (filter.Step != "" && step != filter.Step) {
					continue
				}
				sessionHadMatch = true
				matchedSessionFilter = true
				if !sp.complete {
					cardMissingBoundaries++
					continue
				}
				cardTurns = append(cardTurns, turnRow{
					cardID:     id,
					sessionKey: id + "\x00" + g.key,
					repo:       repo,
					source:     t.Source,
					role:       role,
					provider:   last.Provider,
					model:      model,
					step:       step,
					secs:       max(0, secs(sp.end.Sub(sp.start))),
				})
			}
			if sessionHadMatch {
				cardMatchedSessions++
			}
		}

		if needSubFilter {
			if needSessionMatch && !matchedSessionFilter {
				continue
			}
			if !needSessionMatch && !matchedStepVisit && !matchedSessionFilter {
				continue
			}
		}

		if t.Repo != "" {
			m.Coverage.CardsWithRepo++
		} else {
			m.Coverage.CardsWithoutRepo++
		}
		if len(t.Sessions) > 0 {
			m.Coverage.CardsWithSessions++
		} else {
			m.Coverage.CardsWithoutSessions++
		}
		m.Coverage.LinkedSessions += cardMatchedSessions + cardUnmatchedSessions
		m.Coverage.MatchedSessions += cardMatchedSessions
		m.Coverage.UnmatchedSessions += cardUnmatchedSessions
		m.Coverage.Turns += len(cardTurns)
		m.Coverage.MissingBoundaries += cardMissingBoundaries

		fellowProv, fellowModel := cardFellowAgent(t.Sessions)
		rows = append(rows, metricRow{
			id:         id,
			repo:       repo,
			source:     t.Source,
			tier:       tier,
			provider:   fellowProv,
			model:      fellowModel,
			e2e:        max(0, secs(t.end().Sub(t.RequestedAt))),
			toLanded:   max(0, secs(t.LandedAt.Sub(t.RequestedAt))),
			hasLanding: hasLanding,
			toDone:     max(0, secs(done.Sub(t.RequestedAt))),
			zach:       secs(t.zach(done)),
		})
		visits = append(visits, cardVisits...)
		turns = append(turns, cardTurns...)
	}

	m.Cards = len(rows)
	if allGroups := groupRows(rows, func(metricRow) MetricGroup { return MetricGroup{} }); len(allGroups) == 1 {
		m.Turnaround = allGroups[0]
	}
	m.BySource = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Source: r.source} })
	m.ByTier = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Tier: r.tier} })
	m.BySourceTier = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Source: r.source, Tier: r.tier} })
	m.ByRepo = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Repo: r.repo} })
	m.BySourceRepo = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Repo: r.repo, Source: r.source} })
	m.ByAgent = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Provider: r.provider, Model: r.model} })

	m.Steps = groupSteps(visits, func(v stepVisitRow) StepMetric { return StepMetric{Step: v.step} })
	m.StepsByRepo = groupSteps(visits, func(v stepVisitRow) StepMetric { return StepMetric{Repo: v.repo, Step: v.step} })

	m.AgentTurns = groupTurns(turns, func(r turnRow) AgentTurnMetric {
		return AgentTurnMetric{Role: r.role, Provider: r.provider, Model: r.model, Step: r.step}
	})
	m.AgentTurnsByRole = groupTurns(turns, func(r turnRow) AgentTurnMetric { return AgentTurnMetric{Role: r.role} })
	m.AgentTurnsByModel = groupTurns(turns, func(r turnRow) AgentTurnMetric {
		return AgentTurnMetric{Provider: r.provider, Model: r.model}
	})
	m.AgentTurnsByStep = groupTurns(turns, func(r turnRow) AgentTurnMetric { return AgentTurnMetric{Step: r.step} })
	m.AgentTurnsByRepo = groupTurns(turns, func(r turnRow) AgentTurnMetric {
		return AgentTurnMetric{Repo: r.repo, Role: r.role, Provider: r.provider, Model: r.model}
	})
	return m
}

func sessionLinksMatch(links []TimingSession, filter MetricsFilter) bool {
	for _, l := range links {
		model := l.Model
		if model == "" {
			model = "unknown"
		}
		if (filter.Model == "" || model == filter.Model) && (filter.Step == "" || l.Stage == filter.Step) {
			return true
		}
	}
	return false
}

func cardFellowAgent(sessions []TimingSession) (provider, model string) {
	provider, model = "unknown", "unknown"
	for _, s := range sessions {
		if s.Role != "" && s.Role != "fellow" {
			continue
		}
		if s.Provider != "" {
			provider = s.Provider
		}
		if s.Model != "" {
			model = s.Model
		}
	}
	return provider, model
}

func turnStepAndModel(turn turnSpan, links []TimingSession, events []TimingEvent) (step, model string) {
	model = "unknown"
	for _, l := range links {
		if l.Model != "" {
			model = l.Model
			break
		}
	}
	ref := turn.end
	if ref.IsZero() {
		ref = turn.start
	}
	for _, l := range links {
		if l.Model != "" && (l.ObservedAt.IsZero() || ref.IsZero() || !l.ObservedAt.After(ref)) {
			model = l.Model
		}
	}
	for _, l := range links {
		switch l.ReviewKind {
		case "plan":
			return "plan-review", model
		case "code":
			return "review", model
		}
		if l.Role == "reviewer" && l.Stage != "" && l.Stage != "landed" {
			return l.Stage, model
		}
	}
	type span struct {
		stage      string
		start, end time.Time
	}
	var spans []span
	for i, e := range events {
		if e.Stage == "landed" {
			continue
		}
		var end time.Time
		if i+1 < len(events) {
			end = events[i+1].At
		}
		spans = append(spans, span{stage: e.Stage, start: e.At, end: end})
	}
	if len(spans) > 0 {
		step = spans[0].stage
		if !turn.start.IsZero() {
			for i, sp := range spans {
				if !turn.start.Before(sp.start) && (sp.end.IsZero() || turn.start.Before(sp.end)) {
					step = sp.stage
					if (step == "plan-review" || step == "review") && i+1 < len(spans) {
						step = spans[i+1].stage
					}
					break
				}
			}
		}
		return step, model
	}
	for i := len(links) - 1; i >= 0; i-- {
		if links[i].Stage != "" && links[i].Stage != "landed" {
			return links[i].Stage, model
		}
	}
	return "unknown", model
}

// groupRows is one MetricGroup per distinct key.
func groupRows(rows []metricRow, key func(metricRow) MetricGroup) []MetricGroup {
	type acc struct{ e2e, toLanded, toDone, zach []int }
	groups := map[MetricGroup]*acc{}
	for _, r := range rows {
		k := key(r)
		if groups[k] == nil {
			groups[k] = &acc{}
		}
		groups[k].e2e = append(groups[k].e2e, r.e2e)
		if r.hasLanding {
			groups[k].toLanded = append(groups[k].toLanded, r.toLanded)
		}
		groups[k].toDone = append(groups[k].toDone, r.toDone)
		groups[k].zach = append(groups[k].zach, r.zach)
	}
	out := make([]MetricGroup, 0, len(groups))
	for k, a := range groups {
		k.N = len(a.e2e)
		k.LandedN = len(a.toLanded)
		k.EndToEnd = quantiles(a.e2e)
		k.ToLanded = quantiles(a.toLanded)
		k.ToDone = quantiles(a.toDone)
		k.ZachWait = quantiles(a.zach)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Repo != b.Repo:
			return a.Repo < b.Repo
		case a.Source != b.Source:
			return a.Source < b.Source
		case a.Tier != b.Tier:
			return a.Tier < b.Tier
		case a.Provider != b.Provider:
			return a.Provider < b.Provider
		default:
			return a.Model < b.Model
		}
	})
	return out
}

func groupSteps(visits []stepVisitRow, key func(stepVisitRow) StepMetric) []StepMetric {
	type acc struct {
		visitSecs []int
		byCard    map[string]int
	}
	groups := map[StepMetric]*acc{}
	for _, v := range visits {
		k := key(v)
		if groups[k] == nil {
			groups[k] = &acc{byCard: map[string]int{}}
		}
		groups[k].visitSecs = append(groups[k].visitSecs, v.secs)
		groups[k].byCard[v.cardID] += v.secs
	}
	out := make([]StepMetric, 0, len(groups))
	for k, a := range groups {
		cardSecs := make([]int, 0, len(a.byCard))
		for _, s := range a.byCard {
			cardSecs = append(cardSecs, s)
		}
		k.Cards = len(a.byCard)
		k.Visits = len(a.visitSecs)
		k.RepeatedVisits = k.Visits - k.Cards
		k.VisitSecs = quantiles(a.visitSecs)
		k.CardSecs = quantiles(cardSecs)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Step < b.Step
	})
	return out
}

func groupTurns(turns []turnRow, key func(turnRow) AgentTurnMetric) []AgentTurnMetric {
	type acc struct {
		turnSecs  []int
		cards     map[string]bool
		bySession map[string]int
	}
	groups := map[AgentTurnMetric]*acc{}
	for _, r := range turns {
		k := key(r)
		if groups[k] == nil {
			groups[k] = &acc{cards: map[string]bool{}, bySession: map[string]int{}}
		}
		groups[k].turnSecs = append(groups[k].turnSecs, r.secs)
		groups[k].cards[r.cardID] = true
		groups[k].bySession[r.sessionKey] += r.secs
	}
	out := make([]AgentTurnMetric, 0, len(groups))
	for k, a := range groups {
		sessionSecs := make([]int, 0, len(a.bySession))
		for _, s := range a.bySession {
			sessionSecs = append(sessionSecs, s)
		}
		k.Cards = len(a.cards)
		k.Sessions = len(a.bySession)
		k.Turns = len(a.turnSecs)
		k.TurnSecs = quantiles(a.turnSecs)
		k.SessionTurnSecs = quantiles(sessionSecs)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Repo != b.Repo:
			return a.Repo < b.Repo
		case a.Source != b.Source:
			return a.Source < b.Source
		case a.Role != b.Role:
			return a.Role < b.Role
		case a.Provider != b.Provider:
			return a.Provider < b.Provider
		case a.Model != b.Model:
			return a.Model < b.Model
		default:
			return a.Step < b.Step
		}
	})
	return out
}

// quantiles is the arithmetic mean (rounded to the nearest second), nearest-rank median, and 90th percentile of v.
func quantiles(v []int) Quantile {
	if len(v) == 0 {
		return Quantile{}
	}
	s := slices.Clone(v)
	slices.Sort(s)
	var sum int64
	for _, x := range s {
		sum += int64(x)
	}
	avg := int(math.Round(float64(sum) / float64(len(s))))
	rank := func(p float64) int { return s[max(0, int(math.Ceil(p*float64(len(s))))-1)] }
	return Quantile{Average: avg, Median: rank(0.5), P90: rank(0.9)}
}

func (f *Factory) handleMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := metricsDays
	if s := q.Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be a whole number of days, 1 to 30"})
			return
		}
		days = min(n, metricsMaxDays)
	}
	filter := MetricsFilter{
		Repo:     strings.TrimSpace(q.Get("repo")),
		Source:   strings.TrimSpace(q.Get("source")),
		Tier:     strings.TrimSpace(q.Get("tier")),
		Role:     strings.TrimSpace(q.Get("role")),
		Provider: strings.TrimSpace(q.Get("provider")),
		Model:    strings.TrimSpace(q.Get("model")),
		Step:     strings.TrimSpace(q.Get("step")),
	}
	writeJSON(w, http.StatusOK, computeMetrics(f.timings.all(), f.cfg.Home, &f.turnCache, f.Now(), days, filter))
}
