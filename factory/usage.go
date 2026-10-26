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
	Summary   string         `json:"summary"`
	Windows   []UsageWindow  `json:"windows,omitempty"`
	Extra     string         `json:"extra,omitempty"`
	UpdatedAt string         `json:"updatedAt,omitempty"`
	Accounts  []AccountUsage `json:"accounts,omitempty"`
}

// AccountUsage is one account's remaining allowance, for providers whose
// allowance is split across accounts. The label is the account's local
// number; the account email is carried for display, but no organization
// name or identifier is.
type AccountUsage struct {
	Label     string        `json:"label"`
	Email     string        `json:"email,omitempty"`
	Active    bool          `json:"active,omitempty"`
	Summary   string        `json:"summary"`
	Windows   []UsageWindow `json:"windows,omitempty"`
	Extra     string        `json:"extra,omitempty"`
	UpdatedAt string        `json:"updatedAt,omitempty"`
}

type usageCache struct {
	byProvider map[string]AgentUsage
	fetched    time.Time
	refreshing chan struct{} // non-nil while a background refresh runs; closed when it lands
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

// resetWindows copies ws and flips any window whose reset has passed back
// to 100% remaining, reporting whether anything flipped.
func resetWindows(ws []UsageWindow, now time.Time) ([]UsageWindow, bool) {
	out := make([]UsageWindow, len(ws))
	copy(out, ws)
	reset := false
	for i, w := range out {
		if rt, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil && !now.Before(rt) {
			out[i].RemainingPct = 100
			out[i].ResetsAt = ""
			reset = true
		}
	}
	return out, reset
}

func ageExtra(extra, updatedAt string, now time.Time) string {
	ts, err := time.Parse(time.RFC3339Nano, updatedAt)
	if updatedAt == "" || err != nil {
		return extra
	}
	age := "as of " + formatAge(now.Sub(ts))
	if extra != "" {
		return extra + " · " + age
	}
	return age
}

func withAge(u AgentUsage, now time.Time) AgentUsage {
	if len(u.Windows) > 0 {
		if ws, reset := resetWindows(u.Windows, now); reset {
			u.Windows = ws
			u.Summary = formatSummary(ws)
		}
	}
	if len(u.Accounts) > 0 {
		accts := make([]AccountUsage, len(u.Accounts))
		copy(accts, u.Accounts)
		for i := range accts {
			if len(accts[i].Windows) > 0 {
				if ws, reset := resetWindows(accts[i].Windows, now); reset {
					accts[i].Windows = ws
					accts[i].Summary = formatSummary(ws)
				}
			}
			accts[i].Extra = ageExtra(accts[i].Extra, accts[i].UpdatedAt, now)
		}
		u.Accounts = accts
	}
	u.Extra = ageExtra(u.Extra, u.UpdatedAt, now)
	return u
}

// providerUsages returns remaining usage per provider from the cache, computing each
// reading's "as of ..." age against now. It never waits for the usage commands (they take
// seconds and this feeds the board and the new-card dialog): an empty or stale cache is
// served as-is — "usage unknown" before the first fill — and refreshed in the background.
func (f *Factory) providerUsages() map[string]AgentUsage {
	now := f.Now()
	f.usageMu.Lock()
	stale := f.usage.byProvider == nil || now.Sub(f.usage.fetched) >= usageTTL
	if stale && f.usage.refreshing == nil {
		done := make(chan struct{})
		f.usage.refreshing = done
		go f.refreshUsage(done, now)
	}
	out := make(map[string]AgentUsage, len(f.usage.byProvider))
	for k, v := range f.usage.byProvider {
		out[k] = v
	}
	f.usageMu.Unlock()
	for _, a := range AgentChoices { // before the first fill: unknown, as a failed fetch reads
		if _, ok := out[a.providerID()]; !ok {
			out[a.providerID()] = unknownUsage()
		}
	}
	for k, v := range out {
		out[k] = withAge(v, now)
	}
	return out
}

// refreshUsage fetches every provider's usage (slow subprocesses; no lock held) and
// stores the result, closing done to mark the refresh landed.
func (f *Factory) refreshUsage(done chan struct{}, now time.Time) {
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
	f.usageMu.Lock()
	f.usage.byProvider = res
	f.usage.fetched = now
	f.usage.refreshing = nil
	close(done)
	f.usageMu.Unlock()
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
	case "antigravity":
		// A read-only slash command in print mode: no agent turn, no quota spent, no conversation.
		cmd := exec.CommandContext(ctx, "agy", "-p", "/usage", "--output-format", "json")
		cmd.Dir = os.TempDir()
		out, err := cmd.Output()
		if err != nil {
			return unknownUsage()
		}
		return parseAntigravityUsage(out, f.Now())
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
		Number         int    `json:"number"`
		Email          string `json:"email"`
		Active         bool   `json:"active"`
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
	// Every account is listed, in account-number order. An account with no
	// number keeps its place by list position.
	type numbered struct {
		num int
		au  AccountUsage
	}
	rows := make([]numbered, len(payload.Accounts))
	pick := 0
	pickedActive := false
	for i, a := range payload.Accounts {
		if a.Active && !pickedActive {
			pick, pickedActive = i, true
		}
		num := a.Number
		if num <= 0 {
			num = i + 1
		}
		var ws []UsageWindow
		if w := a.Usage.SevenDay; w != nil {
			ws = append(ws, UsageWindow{Label: "7d", RemainingPct: remPct(w.Pct), ResetsAt: normISO(w.ResetsAt)})
		}
		if w := a.Usage.FiveHour; w != nil {
			ws = append(ws, UsageWindow{Label: "5h", RemainingPct: remPct(w.Pct), ResetsAt: normISO(w.ResetsAt)})
		}
		updated := normISO(a.UsageFetchedAt)
		if updated == "" && len(ws) > 0 {
			updated = now.UTC().Format(time.RFC3339)
		}
		rows[i] = numbered{num, AccountUsage{
			Label:     fmt.Sprintf("Claude #%d", num),
			Email:     a.Email,
			Active:    a.Active,
			Summary:   formatSummary(ws),
			Windows:   ws,
			UpdatedAt: updated,
		}}
	}
	// The top-level reading stays the active account's (the first account
	// when none is flagged active), as before accounts were listed.
	top := rows[pick].au
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].num < rows[j].num })
	accounts := make([]AccountUsage, len(rows))
	for i, r := range rows {
		accounts[i] = r.au
	}
	return AgentUsage{
		Summary:   top.Summary,
		Windows:   top.Windows,
		UpdatedAt: top.UpdatedAt,
		Accounts:  accounts,
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
		Reason    string `json:"reason"`
		Allowance *struct {
			AsOfMs int64 `json:"asOfMs"`
			Weekly *win  `json:"weekly"`
			Window *win  `json:"window"`
		} `json:"allowance"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return unknownUsage()
	}
	if payload.Allowance == nil {
		// Before the first use in a new window, Muse reports reason "server_sent_no_usage" for an active subscription.
		if payload.Reason == "server_sent_no_usage" {
			ws := []UsageWindow{{Label: "7d", RemainingPct: 100}, {Label: "5h", RemainingPct: 100}}
			return AgentUsage{
				Summary:   formatSummary(ws),
				Windows:   ws,
				UpdatedAt: now.UTC().Format(time.RFC3339),
			}
		}
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

// parseAntigravityUsage reads `agy -p /usage --output-format json`. Quota is pooled per model
// group; Gemini models (Argon included) share the "Gemini Models" group's weekly and 5h limits.
func parseAntigravityUsage(raw []byte, now time.Time) AgentUsage {
	type bucket struct {
		ID                string   `json:"id"`
		Window            string   `json:"window"`
		RemainingFraction *float64 `json:"remaining_fraction"`
		ResetTime         string   `json:"reset_time"`
	}
	type group struct {
		Name    string   `json:"name"`
		Buckets []bucket `json:"buckets"`
	}
	var payload struct {
		Status  string `json:"status"`
		Command struct {
			Data struct {
				Groups []group `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Status != "SUCCESS" {
		return unknownUsage()
	}
	var gem *group
	for i := range payload.Command.Data.Groups {
		g := &payload.Command.Data.Groups[i]
		for _, b := range g.Buckets {
			if strings.HasPrefix(b.ID, "gemini-") {
				gem = g
			}
		}
		if gem == nil && g.Name == "Gemini Models" {
			gem = g
		}
		if gem != nil {
			break
		}
	}
	if gem == nil {
		return unknownUsage()
	}
	labels := map[string]string{"weekly": "7d", "5h": "5h"}
	var ws []UsageWindow
	for _, label := range []string{"7d", "5h"} {
		for _, b := range gem.Buckets {
			if b.RemainingFraction == nil || labels[b.Window] != label {
				continue
			}
			ws = append(ws, UsageWindow{Label: label, RemainingPct: remPct((1 - *b.RemainingFraction) * 100), ResetsAt: normISO(b.ResetTime)})
			break
		}
	}
	if len(ws) == 0 {
		return unknownUsage()
	}
	return AgentUsage{
		Summary:   formatSummary(ws),
		Windows:   ws,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
}
