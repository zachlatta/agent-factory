package factory

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const tokenFileLimit int64 = 32 << 20

// TokenCounts normalizes input to include cache reads/writes. Cache and reasoning
// are subsets: Total is Input + Output, never the sum of every displayed column.
type TokenCounts struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

func (c *TokenCounts) add(d TokenCounts) {
	c.Input += d.Input
	c.Output += d.Output
	c.CacheRead += d.CacheRead
	c.CacheWrite += d.CacheWrite
	c.Reasoning += d.Reasoning
	c.Total += d.Total
}

type tokenEvent struct {
	At, From time.Time // From is unknown for an initial cumulative snapshot or counter reset.
	Model    string
	Counts   TokenCounts
}

type tokenRead struct {
	events []tokenEvent
	status string
}

type nativeTokens struct {
	Input         *int64 `json:"input_tokens"`
	Output        int64  `json:"output_tokens"`
	Cached        int64  `json:"cached_input_tokens"`
	CacheRead     int64  `json:"cache_read_input_tokens"`
	CacheWrite    int64  `json:"cache_creation_input_tokens"`
	Write         int64  `json:"cache_write_input_tokens"`
	MuseCached    int64  `json:"cached_tokens"`
	MuseRead      int64  `json:"cache_read_tokens"`
	MuseWrite     int64  `json:"cache_write_tokens"`
	Reasoning     int64  `json:"reasoning_output_tokens"`
	MuseReasoning int64  `json:"reasoning_tokens"`
	Details       struct {
		Thinking int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (n nativeTokens) counts(provider string) (TokenCounts, bool) {
	if n.Input == nil || *n.Input < 0 {
		return TokenCounts{}, false
	}
	c := TokenCounts{Input: *n.Input, Output: n.Output, CacheRead: n.Cached, CacheWrite: n.Write, Reasoning: n.Reasoning}
	switch provider {
	case "claude":
		c.CacheRead, c.CacheWrite, c.Reasoning = n.CacheRead, n.CacheWrite, n.Details.Thinking
		c.Input += c.CacheRead + c.CacheWrite
	case "muse":
		c.CacheRead, c.CacheWrite, c.Reasoning = max(n.MuseCached, n.MuseRead), n.MuseWrite, n.MuseReasoning
	}
	c.Total = c.Input + c.Output
	return c, c.Total >= 0 && c.Input >= 0 && c.Output >= 0 && c.CacheRead >= 0 && c.CacheWrite >= 0 && c.Reasoning >= 0 && c.CacheRead <= c.Input && c.CacheWrite <= c.Input && c.Reasoning <= c.Output
}

func tokenPath(home string, a Agent) string {
	id := nativeSessionID(a.SessionID)
	if id == "" {
		return ""
	}
	switch a.Provider {
	case "claude":
		return claudePath(home, a.Cwd, id)
	case "codex":
		return firstFile(filepath.Join(home, ".codex/sessions/*/*/*", "rollout-*-"+id+".jsonl"), filepath.Join(home, ".codex/archived_sessions", "rollout-*-"+id+".jsonl"), filepath.Join(home, ".codex/archived_sessions/*/*/*", "rollout-*-"+id+".jsonl"))
	case "muse":
		return firstFile(filepath.Join(home, ".local/share/muse/sessions/*/*/*", id, "session.jsonl"))
	}
	return ""
}

// Only provider usage records are decoded. Transcript text is never retained.
func readTokenFile(ctx context.Context, path, provider string) tokenRead {
	r := tokenRead{status: "missing"}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			r.status = "error"
		}
		return r
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		r.status = "error"
		return r
	}
	if st.Size() > tokenFileLimit {
		r.status = "limited"
		return r
	}
	r.status = "unsupported"
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), 4<<20)
	seen := map[string]int{}
	cumulative := map[TokenCounts]bool{}
	var prev TokenCounts
	var previous time.Time
	model := ""
	for scan.Scan() {
		if ctx.Err() != nil {
			r.status = "limited"
			return r
		}
		var d struct {
			Type, Timestamp string
			Recorded        int64  `json:"recorded_at"`
			PayloadType     string `json:"payload_type"`
			Message         struct {
				ID, Model string
				Usage     nativeTokens
			}
			Payload struct {
				Type, Model string
				Info        *struct {
					Total nativeTokens `json:"total_token_usage"`
				}
				Event struct {
					Kind, Model string
					Usage       nativeTokens
				}
			}
		}
		if json.Unmarshal(scan.Bytes(), &d) != nil {
			r.status = "error"
			continue
		}
		var c TokenCounts
		var ok bool
		at := parseTime(d.Timestamp)
		from := at
		key := ""
		switch provider {
		case "claude":
			if d.Type != "assistant" {
				continue
			}
			c, ok = d.Message.Usage.counts(provider)
			model = d.Message.Model
			key = d.Message.ID
			if key == "" {
				ok = false
			}
		case "codex":
			if d.Type == "turn_context" {
				model = d.Payload.Model
				continue
			}
			if d.Type != "event_msg" || d.Payload.Type != "token_count" || d.Payload.Info == nil {
				continue
			}
			c, ok = d.Payload.Info.Total.counts(provider)
			if !ok {
				break
			}
			if r.status == "unsupported" {
				r.status = "readable"
			}
			if cumulative[c] {
				continue
			}
			cumulative[c] = true
			from = previous
			if c.Input < prev.Input || c.Output < prev.Output {
				prev = TokenCounts{}
				from = time.Time{}
			}
			total := c
			if c.CacheRead < prev.CacheRead || c.CacheWrite < prev.CacheWrite || c.Reasoning < prev.Reasoning {
				r.status = "error"
			}
			c = TokenCounts{Input: c.Input - prev.Input, Output: c.Output - prev.Output, CacheRead: c.CacheRead - prev.CacheRead, CacheWrite: c.CacheWrite - prev.CacheWrite, Reasoning: c.Reasoning - prev.Reasoning, Total: c.Total - prev.Total}
			c.CacheRead, c.CacheWrite, c.Reasoning = max(0, c.CacheRead), max(0, c.CacheWrite), max(0, c.Reasoning)
			prev, previous = total, at
			if c == (TokenCounts{}) {
				continue
			}
		case "muse":
			if d.PayloadType != "runtime.session" || d.Payload.Event.Kind != "model_completed" {
				continue
			}
			c, ok = d.Payload.Event.Usage.counts(provider)
			model = d.Payload.Event.Model
			if d.Recorded > 0 {
				at = time.UnixMicro(d.Recorded).UTC()
			}
			from = at
			if !at.IsZero() {
				key = at.Format(time.RFC3339Nano) + model
			}
		}
		if !ok {
			r.status = "error"
			continue
		}
		e := tokenEvent{At: at, From: from, Model: model, Counts: c}
		if i, exists := seen[key]; key != "" && exists {
			// Claude repeats a message while its output grows. Keep one final reading.
			if e.At.Before(r.events[i].At) {
				continue
			}
			e.From = r.events[i].From
			r.events[i] = e
		} else {
			if key != "" {
				seen[key] = len(r.events)
			}
			r.events = append(r.events, e)
		}
	}
	if scan.Err() != nil {
		r.status = "error"
	}
	if ctx.Err() != nil {
		r.status = "limited"
	}
	if r.status == "unsupported" && len(r.events) > 0 {
		r.status = "readable"
	}
	sort.SliceStable(r.events, func(i, j int) bool { return r.events[i].At.Before(r.events[j].At) })
	return r
}

// tokenStage uses the exact events behind StageSpan, with half-open spans. A
// cumulative interval crossing a boundary stays unassigned instead of being prorated.
func tokenStage(e tokenEvent, t CardTiming, now time.Time) (string, int, int) {
	if e.At.IsZero() || e.From.IsZero() || e.At.After(now) || e.From.After(e.At) {
		return "unassigned", 0, 0
	}
	for i, span := range t.Events {
		end := now.Add(time.Nanosecond)
		if i+1 < len(t.Events) {
			end = t.Events[i+1].At
		}
		if span.Stage == "landed" || e.From.Before(span.At) || !e.At.Before(end) {
			continue
		}
		round := 0
		for _, r := range t.Rounds {
			if !e.From.Before(r.Start) && !r.End.IsZero() && e.At.Before(r.End) && ((span.Stage == "plan-review" && r.Kind == "plan") || (span.Stage == "review" && r.Kind == "code")) {
				round = r.N
			}
		}
		return span.Stage, i + 1, round
	}
	return "unassigned", 0, 0
}

func usageRole(a Agent) string {
	k := agentKind(a)
	if k == "card" {
		return "fellow"
	}
	if k == "reviewer" {
		return strings.TrimSpace(a.L(labelReviewKind) + " reviewer")
	}
	if k == "other" && a.Labels["paseo.parent-agent-id"] != "" {
		return "sub-agent"
	}
	return k
}
