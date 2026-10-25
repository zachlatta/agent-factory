package factory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	textCap  = 60000     // chars per tool input/output; the rest is cut with a note
	imageCap = 3_000_000 // base64 chars per inline image
)

type TranscriptSource struct{ Provider, SessionID, Cwd string }

type Image struct {
	Kind    string `json:"kind"`
	Media   string `json:"media,omitempty"`
	Data    string `json:"data,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	From    string `json:"from,omitempty"`
	Ts      string `json:"ts,omitempty"`
	Line    int    `json:"line,omitempty"`
}

type Event struct {
	Kind   string         `json:"kind"` // user, assistant, system, thinking, tool, result, image
	Text   string         `json:"text,omitempty"`
	ID     string         `json:"id,omitempty"`
	Name   string         `json:"name,omitempty"`
	Input  map[string]any `json:"input,omitempty"`
	Images []Image        `json:"images,omitempty"`
	Error  bool           `json:"error,omitempty"`
	Media  string         `json:"media,omitempty"`
	Data   string         `json:"data,omitempty"`
	Miss   bool           `json:"missing,omitempty"`
	From   string         `json:"from,omitempty"`
	Ts     string         `json:"ts,omitempty"`
	Line   int            `json:"line,omitempty"`
}

type Transcript struct {
	Events   []Event `json:"events"`
	Next     int     `json:"next"`
	Fallback bool    `json:"fallback,omitempty"`
	Provider string  `json:"provider,omitempty"`
	Path     string  `json:"path,omitempty"`
}

var systemText = regexp.MustCompile(`^\s*<(command-|local-command|system-reminder|task-notification|bash-|user-memory)`)

func capText(s string) string {
	if len(s) > textCap {
		return s[:textCap] + fmt.Sprintf("\n\n[... %d more characters not shown]", len(s)-textCap)
	}
	return s
}

// ReadTranscript reads a Claude agent's full session file (~/.claude/projects/<cwd>/<session>.jsonl),
// which has every message, thinking block, full tool input and output, and image; `paseo logs`
// truncates those. Other providers return Fallback. after is a line offset for incremental polling.
func ReadTranscript(home string, src TranscriptSource, after int) (Transcript, error) {
	if src.Provider != "claude" || src.SessionID == "" {
		return Transcript{Fallback: true, Provider: src.Provider, Events: []Event{}}, nil
	}
	dir := strings.NewReplacer("/", "-", ".", "-").Replace(src.Cwd)
	path := filepath.Join(home, ".claude/projects", dir, src.SessionID+".jsonl")
	if _, err := os.Stat(path); err != nil {
		hits, _ := filepath.Glob(filepath.Join(home, ".claude/projects/*", src.SessionID+".jsonl"))
		if len(hits) == 0 {
			return Transcript{Fallback: true, Provider: "claude", Events: []Event{}}, nil
		}
		path = hits[0]
	}
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer f.Close()
	res := Transcript{Events: []Event{}, Next: after, Path: path, Provider: "claude"}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	n := 0
	for sc.Scan() {
		n++
		if n <= after {
			continue
		}
		res.Next = n
		var d struct {
			Type, Timestamp     string
			IsSidechain, IsMeta bool
			Message             struct{ Content json.RawMessage }
		}
		if json.Unmarshal(sc.Bytes(), &d) != nil || (d.Type != "user" && d.Type != "assistant") || d.IsSidechain {
			continue
		}
		var blocks []map[string]any
		var s string
		if json.Unmarshal(d.Message.Content, &s) == nil {
			blocks = []map[string]any{{"type": "text", "text": s}}
		} else {
			json.Unmarshal(d.Message.Content, &blocks)
		}
		for _, b := range blocks {
			ev, keep := event(d.Type, d.IsMeta, b)
			if keep {
				ev.Ts, ev.Line = d.Timestamp, n
				res.Events = append(res.Events, ev)
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return res, err
	}
	return res, nil
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func image(b map[string]any) Image {
	src, _ := b["source"].(map[string]any)
	data := str(src, "data")
	if str(src, "type") != "base64" || len(data) > imageCap {
		return Image{Kind: "image", Missing: true}
	}
	media := str(src, "media_type")
	if media == "" {
		media = "image/png"
	}
	return Image{Kind: "image", Media: media, Data: data}
}

func event(typ string, meta bool, b map[string]any) (Event, bool) {
	switch str(b, "type") {
	case "text":
		t := str(b, "text")
		if strings.TrimSpace(t) == "" || (typ == "user" && meta) {
			return Event{}, false
		}
		if typ == "user" && systemText.MatchString(t) {
			return Event{Kind: "system", Text: capText(t)}, true
		}
		return Event{Kind: typ, Text: t}, true
	case "thinking":
		if t := str(b, "thinking"); strings.TrimSpace(t) != "" {
			return Event{Kind: "thinking", Text: t}, true
		}
	case "image":
		im := image(b)
		return Event{Kind: "image", Media: im.Media, Data: im.Data, Miss: im.Missing, From: typ}, true
	case "tool_use":
		in, _ := b["input"].(map[string]any)
		if in == nil {
			in = map[string]any{}
		}
		for k, v := range in {
			if s, isStr := v.(string); isStr && len(s) > textCap {
				in[k] = capText(s)
			}
		}
		return Event{Kind: "tool", ID: str(b, "id"), Name: str(b, "name"), Input: in}, true
	case "tool_result":
		ev := Event{Kind: "result", ID: str(b, "tool_use_id"), Images: []Image{}}
		ev.Error, _ = b["is_error"].(bool)
		switch c := b["content"].(type) {
		case string:
			ev.Text = capText(c)
		case []any:
			var texts []string
			for _, x := range c {
				xb, _ := x.(map[string]any)
				switch str(xb, "type") {
				case "text":
					texts = append(texts, str(xb, "text"))
				case "image":
					ev.Images = append(ev.Images, image(xb))
				}
			}
			ev.Text = capText(strings.Join(texts, "\n"))
		}
		return ev, true
	}
	return Event{}, false
}

var logTag = regexp.MustCompile(`^\[(User|Thought|[A-Z][A-Za-z_ -]{1,30})\]\s?(.*)$`)

// parseLogs turns `paseo logs` text (codex and other providers) into transcript events.
func parseLogs(text string) []Event {
	events := []Event{}
	cur := -1
	for _, line := range strings.Split(text, "\n") {
		if m := logTag.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "User":
				events = append(events, Event{Kind: "user", Text: m[2]})
			case "Thought":
				events = append(events, Event{Kind: "thinking", Text: m[2]})
			default:
				events = append(events, Event{Kind: "tool", Name: m[1], Input: map[string]any{"summary": m[2]}})
				cur = -1
				continue
			}
			cur = len(events) - 1
			continue
		}
		if cur >= 0 {
			events[cur].Text += "\n" + line
		} else {
			events = append(events, Event{Kind: "assistant", Text: line})
			cur = len(events) - 1
		}
	}
	return events
}
