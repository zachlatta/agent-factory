package factory

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"
)

// GET /api/metrics?days=N (default 7, at most 30): how long cards took, from the timings
// (timing.go). It counts cards that reached Done in the window and landed: end to end is from
// when Zach asked to the reply (else the landing commit), and the Needs Zach wait is their time in
// that column. Cards that did not land, and partial ones, are counted in excluded.

const (
	metricsDays    = 7
	metricsMaxDays = 30
)

type Metrics struct {
	Days         int            `json:"days"`
	From         string         `json:"from"`
	To           string         `json:"to"`
	Cards        int            `json:"cards"`
	Excluded     MetricsSkipped `json:"excluded"`
	BySource     []MetricGroup  `json:"bySource"`
	ByTier       []MetricGroup  `json:"byTier"`
	BySourceTier []MetricGroup  `json:"bySourceTier"`
}

type MetricsSkipped struct {
	NotLanded int `json:"notLanded"` // dropped, closed or moved to Done without a landing
	Partial   int `json:"partial"`   // first seen mid-flight: history incomplete
}

type MetricGroup struct {
	Source   string   `json:"source,omitempty"`
	Tier     string   `json:"tier,omitempty"`
	N        int      `json:"n"`
	EndToEnd Quantile `json:"endToEnd"` // seconds
	ZachWait Quantile `json:"zachWait"` // seconds
}

type Quantile struct {
	Median int `json:"median"`
	P90    int `json:"p90"`
}

type metricRow struct {
	source, tier string
	e2e, zach    int
}

// metrics summarises the timings of cards that reached Done in the days before now.
func metrics(all map[string]CardTiming, now time.Time, days int) Metrics {
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	m := Metrics{Days: days, From: iso(from), To: iso(now), BySource: []MetricGroup{}, ByTier: []MetricGroup{},
		BySourceTier: []MetricGroup{}}
	var rows []metricRow
	for _, t := range all {
		done := t.doneAt()
		if done.IsZero() || done.Before(from) || done.After(now) {
			continue
		}
		switch {
		case t.Partial || t.RequestedAt.IsZero():
			m.Excluded.Partial++
			continue
		case t.LandedAt.IsZero():
			m.Excluded.NotLanded++
			continue
		}
		tier := t.Tier
		if tier == "" {
			tier = "none"
		}
		rows = append(rows, metricRow{source: t.Source, tier: tier, e2e: max(0, secs(t.end().Sub(t.RequestedAt))),
			zach: secs(t.zach(done))})
	}
	m.Cards = len(rows)
	m.BySource = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Source: r.source} })
	m.ByTier = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Tier: r.tier} })
	m.BySourceTier = groupRows(rows, func(r metricRow) MetricGroup { return MetricGroup{Source: r.source, Tier: r.tier} })
	return m
}

// groupRows is one MetricGroup per distinct key, sorted by source then tier.
func groupRows(rows []metricRow, key func(metricRow) MetricGroup) []MetricGroup {
	type acc struct{ e2e, zach []int }
	groups := map[MetricGroup]*acc{}
	for _, r := range rows {
		k := key(r)
		if groups[k] == nil {
			groups[k] = &acc{}
		}
		groups[k].e2e = append(groups[k].e2e, r.e2e)
		groups[k].zach = append(groups[k].zach, r.zach)
	}
	out := []MetricGroup{}
	for k, a := range groups {
		k.N = len(a.e2e)
		k.EndToEnd = quantiles(a.e2e)
		k.ZachWait = quantiles(a.zach)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Tier < out[j].Tier
	})
	return out
}

// quantiles is the nearest-rank median and 90th percentile of v.
func quantiles(v []int) Quantile {
	if len(v) == 0 {
		return Quantile{}
	}
	s := slices.Clone(v)
	slices.Sort(s)
	rank := func(p float64) int { return s[max(0, int(math.Ceil(p*float64(len(s))))-1)] }
	return Quantile{Median: rank(0.5), P90: rank(0.9)}
}

func (f *Factory) handleMetrics(w http.ResponseWriter, r *http.Request) {
	days := metricsDays
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be a whole number of days, 1 to 30"})
			return
		}
		days = min(n, metricsMaxDays)
	}
	writeJSON(w, http.StatusOK, metrics(f.timings.all(), f.Now(), days))
}
