package factory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	usageTTL     = 5 * time.Minute
	usageTimeout = 5 * time.Second
)

// UsageWindow is one rate-limit window (such as 7d or 5h) with its remaining percentage.
type UsageWindow struct {
	Label        string `json:"label"`
	RemainingPct int    `json:"remainingPct"`
	ResetsAt     string `json:"resetsAt,omitempty"`
}

// AgentUsage is the remaining subscription allowance for an agent choice's provider.
type AgentUsage struct {
	Summary   string        `json:"summary"`
	Windows   []UsageWindow `json:"windows,omitempty"`
	Extra     string        `json:"extra,omitempty"`
	UpdatedAt string        `json:"updatedAt,omitempty"`
}

type usageCache struct {
	byProvider map[string]AgentUsage
	fetched    time.Time
}

func unknownUsage() AgentUsage { return AgentUsage{Summary: "usage unknown"} }

func remPct(used float64) int {
	return max(0, min(100, int(math.Round(100-used))))
}

func normISO(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func formatSummary(ws []UsageWindow) string {
	if len(ws) == 0 {
		return "usage unknown"
	}
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = fmt.Sprintf("%d%% %s", w.RemainingPct, w.Label)
	}
	return strings.Join(parts, " · ") + " left"
}

func withAge(u AgentUsage, now time.Time) AgentUsage {
	if len(u.Windows) > 0 {
		ws := make([]UsageWindow, len(u.Windows))
		copy(ws, u.Windows)
		reset := false
		for i, w := range ws {
			if rt, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil && !now.Before(rt) {
				ws[i].RemainingPct = 100
				ws[i].ResetsAt = ""
				reset = true
			}
		}
		if reset {
			u.Windows = ws
			u.Summary = formatSummary(ws)
		}
	}
	if u.UpdatedAt == "" {
		return u
	}
	ts, err := time.Parse(time.RFC3339Nano, u.UpdatedAt)
	if err != nil {
		return u
	}
	age := "as of " + formatAge(now.Sub(ts))
	if u.Extra != "" {
		u.Extra += " · " + age
	} else {
		u.Extra = age
	}
	return u
}

// providerUsages returns remaining usage per provider, refreshing the cache when it is empty or
// older than usageTTL, and computing each reading's "as of ..." age against now.
func (f *Factory) providerUsages() map[string]AgentUsage {
	now := f.Now()
	f.usageMu.Lock()
	defer f.usageMu.Unlock()
	if f.usage.byProvider == nil || now.Sub(f.usage.fetched) >= usageTTL {
		f.refreshUsage(now)
	}
	out := make(map[string]AgentUsage, len(f.usage.byProvider))
	for k, v := range f.usage.byProvider {
		out[k] = withAge(v, now)
	}
	return out
}

func (f *Factory) refreshUsage(now time.Time) {
	seen := map[string]bool{}
	var provs []string
	for _, a := range AgentChoices {
		p := a.providerID()
		if !seen[p] {
			seen[p] = true
			provs = append(provs, p)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageTimeout)
	defer cancel()
	res := make(map[string]AgentUsage, len(provs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range provs {
		wg.Add(1)
		go func(prov string) {
			defer wg.Done()
			u := f.Usage(ctx, prov)
			if u.Summary == "" {
				u = unknownUsage()
			}
			mu.Lock()
			res[prov] = u
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	f.usage.byProvider = res
	f.usage.fetched = now
}

func (f *Factory) readProviderUsage(ctx context.Context, provider string) AgentUsage {
	switch provider {
	case "claude":
		out, err := exec.CommandContext(ctx, "cswap", "list", "--json").Output()
		if err != nil {
			return unknownUsage()
		}
		return parseClaudeUsage(out, f.Now())
	case "codex":
		return readCodexUsage(f.cfg.Home)
	case "muse":
		out, err := exec.CommandContext(ctx, "muse", "usage", "--json").Output()
		if err != nil {
			return unknownUsage()
		}
		return parseMuseUsage(out, f.Now())
	default:
		return unknownUsage()
	}
}

func parseClaudeUsage(raw []byte, now time.Time) AgentUsage {
	type win struct {
		Pct      float64 `json:"pct"`
		ResetsAt string  `json:"resetsAt"`
	}
	type acct struct {
		Active         bool   `json:"active"`
		UsageStatus    string `json:"usageStatus"`
		UsageFetchedAt string `json:"usageFetchedAt"`
		Usage          struct {
			FiveHour *win `json:"fiveHour"`
			SevenDay *win `json:"sevenDay"`
		} `json:"usage"`
	}
	var payload struct {
		Accounts []acct `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Accounts) == 0 {
		return unknownUsage()
	}
	var pick *acct
	overflow := 0
	for i := range payload.Accounts {
		a := &payload.Accounts[i]
		if a.Active {
			pick = a
			continue
		}
		if a.UsageStatus == "ok" &&
			(a.Usage.SevenDay == nil || a.Usage.SevenDay.Pct < 100) &&
			(a.Usage.FiveHour == nil || a.Usage.FiveHour.Pct < 100) {
			overflow++
		}
	}
	if pick == nil {
		pick = &payload.Accounts[0]
	}
	var ws []UsageWindow
	if w := pick.Usage.SevenDay; w != nil {
		ws = append(ws, UsageWindow{Label: "7d", RemainingPct: remPct(w.Pct), ResetsAt: normISO(w.ResetsAt)})
	}
	if w := pick.Usage.FiveHour; w != nil {
		ws = append(ws, UsageWindow{Label: "5h", RemainingPct: remPct(w.Pct), ResetsAt: normISO(w.ResetsAt)})
	}
	if len(ws) == 0 {
		return unknownUsage()
	}
	extra := ""
	if overflow == 1 {
		extra = "+1 overflow account"
	} else if overflow > 1 {
		extra = fmt.Sprintf("+%d overflow accounts", overflow)
	}
	updated := normISO(pick.UsageFetchedAt)
	if updated == "" {
		updated = now.UTC().Format(time.RFC3339)
	}
	return AgentUsage{
		Summary:   formatSummary(ws),
		Windows:   ws,
		Extra:     extra,
		UpdatedAt: updated,
	}
}

func readCodexUsage(home string) AgentUsage {
	paths, _ := filepath.Glob(filepath.Join(home, ".codex/sessions/*/*/*/*.jsonl"))
	if len(paths) == 0 {
		return unknownUsage()
	}
	type fileMtime struct {
		path string
		mod  time.Time
	}
	files := make([]fileMtime, 0, len(paths))
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			files = append(files, fileMtime{p, st.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files {
		if u, ok := readCodexSessionFile(f.path); ok {
			return u
		}
	}
	return unknownUsage()
}

func readCodexSessionFile(path string) (AgentUsage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return AgentUsage{}, false
	}
	defer f.Close()

	type rlWin struct {
		UsedPercent   float64 `json:"used_percent"`
		WindowMinutes int     `json:"window_minutes"`
		ResetsAt      int64   `json:"resets_at"`
	}
	type rlLine struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			RateLimits *struct {
				Primary   *rlWin `json:"primary"`
				Secondary *rlWin `json:"secondary"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}

	var last *rlLine
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(`"rate_limits"`)) {
			var d rlLine
			if json.Unmarshal(line, &d) == nil && d.Payload.RateLimits != nil && d.Payload.RateLimits.Primary != nil {
				last = &d
			}
		}
		if err != nil {
			if err != io.EOF {
				return AgentUsage{}, false
			}
			break
		}
	}
	if last == nil {
		return AgentUsage{}, false
	}
	winLabel := func(mins int, fallback string) string {
		switch mins {
		case 10080:
			return "7d"
		case 300:
			return "5h"
		default:
			if mins > 0 && mins%60 == 0 {
				return fmt.Sprintf("%dh", mins/60)
			}
			return fallback
		}
	}
	addWin := func(w *rlWin, def string) UsageWindow {
		rst := ""
		if w.ResetsAt > 0 {
			rst = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		return UsageWindow{Label: winLabel(w.WindowMinutes, def), RemainingPct: remPct(w.UsedPercent), ResetsAt: rst}
	}
	ws := []UsageWindow{addWin(last.Payload.RateLimits.Primary, "7d")}
	if s := last.Payload.RateLimits.Secondary; s != nil {
		ws = append(ws, addWin(s, "5h"))
	}
	return AgentUsage{
		Summary:   formatSummary(ws),
		Windows:   ws,
		Extra:     "shared pool",
		UpdatedAt: normISO(last.Timestamp),
	}, true
}

func formatAge(d time.Duration) string {
	if d < 90*time.Second {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(math.Round(d.Minutes())))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(math.Round(d.Hours())))
	}
	return fmt.Sprintf("%dd ago", int(math.Round(d.Hours()/24)))
}

func parseMuseUsage(raw []byte, now time.Time) AgentUsage {
	type win struct {
		ResetsAtMs  int64   `json:"resetsAtMs"`
		UsedPercent float64 `json:"usedPercent"`
	}
	var payload struct {
		Allowance *struct {
			AsOfMs int64 `json:"asOfMs"`
			Weekly *win  `json:"weekly"`
			Window *win  `json:"window"`
		} `json:"allowance"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Allowance == nil {
		return unknownUsage()
	}
	msISO := func(ms int64) string {
		if ms <= 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	var ws []UsageWindow
	if w := payload.Allowance.Weekly; w != nil {
		ws = append(ws, UsageWindow{Label: "7d", RemainingPct: remPct(w.UsedPercent), ResetsAt: msISO(w.ResetsAtMs)})
	}
	if w := payload.Allowance.Window; w != nil {
		ws = append(ws, UsageWindow{Label: "5h", RemainingPct: remPct(w.UsedPercent), ResetsAt: msISO(w.ResetsAtMs)})
	}
	if len(ws) == 0 {
		return unknownUsage()
	}
	updated := msISO(payload.Allowance.AsOfMs)
	if updated == "" {
		updated = now.UTC().Format(time.RFC3339)
	}
	return AgentUsage{
		Summary:   formatSummary(ws),
		Windows:   ws,
		UpdatedAt: updated,
	}
}
