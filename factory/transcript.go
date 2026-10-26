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
	Native string         `json:"native,omitempty"` // the provider's own tool name, when Name is mapped
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
	Replace  bool    `json:"replace,omitempty"` // Events is the whole transcript, not lines after `after`
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

// ReadTranscript reads an agent's own session file, which has every message, thinking block, full
// tool input and output, and image; `paseo logs` truncates those and keeps nothing once an agent is
// archived. Each provider has its reader (transcript_providers.go for all but Claude). after is a
// line offset for incremental polling; a reader whose file changes in place sets Replace and sends
// everything each time. With no file, or nothing readable in it yet, it returns Fallback.
func ReadTranscript(home string, src TranscriptSource, after int) (Transcript, error) {
	var (
		res Transcript
		err error
		ok  bool
	)
	id := nativeSessionID(src.SessionID)
	switch {
	case id == "":
	case src.Provider == "claude":
		res, ok, err = readClaude(home, src.Cwd, id, after)
	case src.Provider == "codex":
		res, ok, err = readCodex(home, id, after)
	case src.Provider == "antigravity":
		res, ok, err = readAntigravity(home, id)
	case src.Provider == "muse":
		res, ok, err = readMuse(home, id, after)
	}
	if err != nil {
		return res, err
	}
	if !ok || (len(res.Events) == 0 && (after == 0 || res.Replace)) {
		return Transcript{Fallback: true, Replace: true, Provider: src.Provider, Events: []Event{}}, nil
	}
	res.Provider = src.Provider
	return res, nil
}

var plainID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// nativeSessionID is the provider's own session id from Paseo's persistence.sessionId: plain for
// Claude and Codex, `plugin:{"version":1,"data":{"conversationId"|"sessionId":...}}` for plugin
// providers. It is "" unless the id is a plain token, since it becomes part of a file path.
func nativeSessionID(raw string) string {
	if js, ok := strings.CutPrefix(raw, "plugin:"); ok {
		var p struct {
			Data struct{ ConversationID, SessionID string }
		}
		if json.Unmarshal([]byte(js), &p) != nil {
			return ""
		}
		raw = p.Data.ConversationID
		if raw == "" {
			raw = p.Data.SessionID
		}
	}
	if !plainID.MatchString(raw) {
		return ""
	}
	return raw
}

// firstFile is the first existing path of patterns (globs allowed), or "".
func firstFile(patterns ...string) string {
	for _, p := range patterns {
		hits, _ := filepath.Glob(p)
		if len(hits) > 0 {
			return hits[0]
		}
	}
	return ""
}

// scanLines calls fn for each line of path after line number after, and returns the last line number.
func scanLines(path string, after int, fn func(n int, line []byte)) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return after, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	n, next := 0, after
	for sc.Scan() {
		n++
		if n <= after {
			continue
		}
		next = n
		fn(n, sc.Bytes())
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return next, err
	}
	return next, nil
}

// readClaude reads ~/.claude/projects/<cwd>/<session>.jsonl.
func readClaude(home, cwd, id string, after int) (Transcript, bool, error) {
	path := claudePath(home, cwd, id)
	if path == "" {
		return Transcript{}, false, nil
	}
	res := Transcript{Events: []Event{}, Path: path}
	next, err := scanLines(path, after, func(n int, line []byte) {
		res.Events = append(res.Events, claudeLineEvents(n, line)...)
	})
	res.Next = next
	return res, true, err
}

// claudePath is the Claude session file for id, or "" when there is none.
func claudePath(home, cwd, id string) string {
	dir := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	return firstFile(filepath.Join(home, ".claude/projects", dir, id+".jsonl"),
		filepath.Join(home, ".claude/projects/*", id+".jsonl"))
}

// claudeLineEvents are the events of line n of a Claude session file.
func claudeLineEvents(n int, line []byte) []Event {
	var d struct {
		Type, Timestamp     string
		IsSidechain, IsMeta bool
		Message             struct{ Content json.RawMessage }
	}
	if json.Unmarshal(line, &d) != nil || (d.Type != "user" && d.Type != "assistant") || d.IsSidechain {
		return nil
	}
	var blocks []map[string]any
	var s string
	if json.Unmarshal(d.Message.Content, &s) == nil {
		blocks = []map[string]any{{"type": "text", "text": s}}
	} else {
		json.Unmarshal(d.Message.Content, &blocks)
	}
	var out []Event
	for _, b := range blocks {
		ev, keep := event(d.Type, d.IsMeta, b)
		if keep {
			ev.Ts, ev.Line = d.Timestamp, n
			out = append(out, ev)
		}
	}
	return out
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
