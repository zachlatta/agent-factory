package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Session readers for the providers other than Claude. Each turns its provider's own session file
// into transcript events, mapping the tools the page knows (shell, read, write, edit, grep) to the
// Claude names it renders and keeping the provider's name in Event.Native. Lines a reader does not
// understand are skipped, so a format change shows less, never an error.

// toolEvent is a tool call with its input mapped to the page's names where one is known.
func toolEvent(id, name string, in map[string]any) Event {
	if in == nil {
		in = map[string]any{}
	}
	for k, v := range in {
		if s, isStr := v.(string); isStr && len(s) > textCap {
			in[k] = capText(s)
		}
	}
	ev := Event{Kind: "tool", ID: id, Name: name, Input: in}
	pick := func(keys ...string) any {
		for _, k := range keys {
			if v, ok := in[k]; ok && v != nil && v != "" {
				return v
			}
		}
		return nil
	}
	mapped := map[string]any{}
	set := func(k string, v any) {
		if v != nil {
			mapped[k] = v
		}
	}
	switch name {
	case "run_command", "exec_command", "shell", "local_shell", "bash":
		cmd := pick("CommandLine", "cmd", "command")
		if parts, isList := cmd.([]any); isList {
			var ss []string
			for _, p := range parts {
				ss = append(ss, fmt.Sprint(p))
			}
			cmd = strings.Join(ss, " ")
		}
		set("command", cmd)
		set("description", pick("toolSummary", "description"))
		ev.Name = "Bash"
	case "read_file", "view_file":
		set("file_path", pick("Path", "AbsolutePath", "path", "file_path"))
		set("offset", pick("StartLine", "offset"))
		ev.Name = "Read"
	case "write_to_file", "write_file":
		set("file_path", pick("TargetFile", "Path", "path", "file_path"))
		set("content", pick("CodeContent", "content"))
		ev.Name = "Write"
	case "replace_file_content", "edit_file", "multi_replace_file_content":
		set("file_path", pick("TargetFile", "Path", "path", "file_path"))
		if chunks, ok := in["ReplacementChunks"].([]any); ok {
			var edits []any
			for _, c := range chunks {
				cm, _ := c.(map[string]any)
				edits = append(edits, map[string]any{"old_string": cm["TargetContent"], "new_string": cm["ReplacementContent"]})
			}
			set("edits", edits)
			ev.Name = "MultiEdit"
		} else {
			set("old_string", pick("TargetContent", "Target", "old_string"))
			set("new_string", pick("ReplacementContent", "Replacement", "new_string"))
			ev.Name = "Edit"
		}
	case "grep_search":
		set("pattern", pick("Query", "pattern"))
		set("path", pick("SearchPath", "path"))
		ev.Name = "Grep"
	}
	if ev.Name != name {
		ev.Native, ev.Input = name, mapped
	}
	return ev
}

// argsMap decodes a tool's JSON arguments; anything that is not a JSON object is kept as "input".
func argsMap(s string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil || m == nil {
		return map[string]any{"input": s}
	}
	return m
}

// dataImage is an image event from a data: URL ("data:image/png;base64,...").
func dataImage(url string) Image {
	head, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ";base64,")
	if !ok || !strings.HasPrefix(url, "data:") || len(data) > imageCap {
		return Image{Kind: "image", Missing: true}
	}
	return Image{Kind: "image", Media: head, Data: data}
}

var codexSystem = regexp.MustCompile(`^\s*(# AGENTS\.md instructions|<environment_context>|<user_instructions>|<system_instructions>|<turn_aborted>|<user_shell_command>)`)

// readCodex reads a Codex rollout file, ~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl
// (or archived_sessions/). Developer messages are the system prompt and are left out.
func readCodex(home, id string, after int) (Transcript, bool, error) {
	path := firstFile(filepath.Join(home, ".codex/sessions/*/*/*", "rollout-*-"+id+".jsonl"),
		filepath.Join(home, ".codex/archived_sessions", "rollout-*-"+id+".jsonl"),
		filepath.Join(home, ".codex/archived_sessions/*/*/*", "rollout-*-"+id+".jsonl"))
	if path == "" {
		return Transcript{}, false, nil
	}
	res := Transcript{Events: []Event{}, Path: path}
	next, err := scanLines(path, after, func(n int, line []byte) {
		var d struct {
			Timestamp, Type string
			Payload         map[string]any
		}
		if json.Unmarshal(line, &d) != nil || d.Type != "response_item" {
			return
		}
		p := d.Payload
		add := func(ev Event) {
			ev.Ts, ev.Line = d.Timestamp, n
			res.Events = append(res.Events, ev)
		}
		switch str(p, "type") {
		case "message":
			role := str(p, "role")
			if role != "user" && role != "assistant" {
				return
			}
			blocks, _ := p["content"].([]any)
			for _, x := range blocks {
				b, _ := x.(map[string]any)
				switch str(b, "type") {
				case "input_text", "output_text", "text":
					t := str(b, "text")
					switch {
					case strings.TrimSpace(t) == "":
					case role == "user" && (codexSystem.MatchString(t) || systemText.MatchString(t)):
						add(Event{Kind: "system", Text: capText(t)})
					default:
						add(Event{Kind: role, Text: t})
					}
				case "input_image":
					im := dataImage(str(b, "image_url"))
					add(Event{Kind: "image", Media: im.Media, Data: im.Data, Miss: im.Missing, From: role})
				}
			}
		case "reasoning":
			var parts []string
			for _, key := range []string{"summary", "content"} {
				xs, _ := p[key].([]any)
				for _, x := range xs {
					xm, _ := x.(map[string]any)
					if t := str(xm, "text"); strings.TrimSpace(t) != "" {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				add(Event{Kind: "thinking", Text: strings.Join(parts, "\n\n")})
			}
		case "function_call":
			add(toolEvent(str(p, "call_id"), str(p, "name"), argsMap(str(p, "arguments"))))
		case "custom_tool_call":
			// exec is Codex's JavaScript tool runner: its script is the command
			add(toolEvent(str(p, "call_id"), str(p, "name"), map[string]any{"command": str(p, "input")}))
		case "local_shell_call":
			act, _ := p["action"].(map[string]any)
			add(toolEvent(str(p, "call_id"), "local_shell", act))
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			ev := Event{Kind: "result", ID: str(p, "call_id"), Images: []Image{}}
			switch o := p["output"].(type) {
			case string:
				ev.Text = capText(o)
			case []any:
				var texts []string
				for _, x := range o {
					b, _ := x.(map[string]any)
					switch str(b, "type") {
					case "input_text", "output_text", "text":
						texts = append(texts, str(b, "text"))
					case "input_image":
						ev.Images = append(ev.Images, dataImage(str(b, "image_url")))
					}
				}
				ev.Text = capText(strings.Join(texts, "\n"))
			}
			add(ev)
		}
	})
	res.Next = next
	return res, true, err
}

var (
	agyStepHeader = regexp.MustCompile(`^(Created At|Completed At): .*\n`)
	agyUserTags   = regexp.MustCompile(`(?s)<(ADDITIONAL_METADATA|USER_SETTINGS_CHANGE)>.*?</(ADDITIONAL_METADATA|USER_SETTINGS_CHANGE)>`)
)

// readAntigravity reads the Antigravity CLI's (Gemini) conversation log,
// ~/.gemini/antigravity-cli/brain/<conversation>/.system_generated/logs/transcript_full.jsonl.
// A PLANNER_RESPONSE step carries the model's thinking, text and tool calls; each tool call's
// result is the next GENERIC step, in order, and its full output is in steps/<n>/output.txt when the
// log has only a preview. Steps change in place (RUNNING, then DONE), so it reads the whole file.
func readAntigravity(home, id string) (Transcript, bool, error) {
	dir := filepath.Join(home, ".gemini/antigravity-cli/brain", id, ".system_generated")
	path := filepath.Join(dir, "logs/transcript_full.jsonl")
	if _, err := os.Stat(path); err != nil {
		return Transcript{}, false, nil
	}
	res := Transcript{Events: []Event{}, Path: path, Replace: true}
	var pending []string // tool call ids waiting for their GENERIC step
	_, err := scanLines(path, 0, func(n int, line []byte) {
		var d struct {
			StepIndex                       int `json:"step_index"`
			Type, Status, Content, Thinking string
			CreatedAt                       string `json:"created_at"`
			ToolCalls                       []struct {
				Name string
				Args map[string]any
			} `json:"tool_calls"`
		}
		if json.Unmarshal(line, &d) != nil {
			return
		}
		add := func(ev Event) {
			ev.Ts, ev.Line = d.CreatedAt, n
			res.Events = append(res.Events, ev)
		}
		switch d.Type {
		case "USER_INPUT":
			t := strings.TrimSpace(agyUserTags.ReplaceAllString(d.Content, ""))
			t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "<USER_REQUEST>"), "</USER_REQUEST>"))
			if t != "" {
				add(Event{Kind: "user", Text: t})
			}
			pending = nil
		case "PLANNER_RESPONSE":
			if strings.TrimSpace(d.Thinking) != "" {
				add(Event{Kind: "thinking", Text: d.Thinking})
			}
			if strings.TrimSpace(d.Content) != "" {
				add(Event{Kind: "assistant", Text: d.Content})
			}
			pending = nil
			for i, tc := range d.ToolCalls {
				tid := fmt.Sprintf("step%d.%d", d.StepIndex, i)
				pending = append(pending, tid)
				add(toolEvent(tid, tc.Name, tc.Args))
			}
		case "GENERIC":
			text := d.Content
			for agyStepHeader.MatchString(text) {
				text = agyStepHeader.ReplaceAllString(text, "")
			}
			if full, err := os.ReadFile(filepath.Join(dir, "steps", strconv.Itoa(d.StepIndex), "output.txt")); err == nil {
				text = string(full)
			}
			if len(pending) == 0 {
				if strings.TrimSpace(text) != "" {
					add(Event{Kind: "system", Text: capText(text)})
				}
				return
			}
			st := strings.ToUpper(d.Status)
			add(Event{Kind: "result", ID: pending[0], Text: capText(strings.TrimSpace(text)), Images: []Image{},
				Error: strings.Contains(st, "ERROR") || strings.Contains(st, "FAIL") || strings.Contains(st, "CANCEL")})
			pending = pending[1:]
		case "SYSTEM_MESSAGE":
			if strings.TrimSpace(d.Content) != "" {
				add(Event{Kind: "system", Text: capText(d.Content)})
			}
		}
	})
	return res, true, err
}

// readMuse reads a Muse Code session, ~/.local/share/muse/sessions/YYYY/MM/DD/<id>/session.jsonl
// (sub-agents have their own files under subagent/ and are left out).
//
// Provisional: the record names below come from short sessions only, not a long coding run. Records
// with any other kind, or fields that are missing, are skipped.
func readMuse(home, id string, after int) (Transcript, bool, error) {
	path := firstFile(filepath.Join(home, ".local/share/muse/sessions/*/*/*", id, "session.jsonl"))
	if path == "" {
		return Transcript{}, false, nil
	}
	res := Transcript{Events: []Event{}, Path: path}
	next, err := scanLines(path, after, func(n int, line []byte) {
		var d struct {
			RecordedAt  int64  `json:"recorded_at"`
			PayloadType string `json:"payload_type"`
			Payload     struct {
				Event map[string]any
			}
		}
		if json.Unmarshal(line, &d) != nil || d.PayloadType != "runtime.session" || d.Payload.Event == nil {
			return
		}
		ts := ""
		if d.RecordedAt > 0 {
			ts = time.UnixMicro(d.RecordedAt).UTC().Format(time.RFC3339Nano)
		}
		add := func(ev Event) {
			ev.Ts, ev.Line = ts, n
			res.Events = append(res.Events, ev)
		}
		e := d.Payload.Event
		text := func(k string) string { return str(e, k) }
		switch text("kind") {
		case "user_prompt_display":
			if t := text("prompt"); strings.TrimSpace(t) != "" {
				add(Event{Kind: "user", Text: t})
			}
		case "assistant_message_committed":
			if t := text("text"); strings.TrimSpace(t) != "" {
				add(Event{Kind: "assistant", Text: t})
			}
		case "reasoning_summary_committed":
			if t := text("text"); strings.TrimSpace(t) != "" {
				add(Event{Kind: "thinking", Text: t})
			}
		case "assistant_tool_calls_committed":
			calls, _ := e["tool_calls"].([]any)
			for _, x := range calls {
				c, _ := x.(map[string]any)
				if str(c, "name") == "" {
					continue
				}
				in, isMap := c["args"].(map[string]any)
				if !isMap {
					in = argsMap(str(c, "args"))
				}
				add(toolEvent(str(c, "call_id"), str(c, "name"), in))
			}
		case "tool_result_batch_committed":
			rs, _ := e["results"].([]any)
			for _, x := range rs {
				r, _ := x.(map[string]any)
				if str(r, "tool_call_id") == "" {
					continue
				}
				ev := Event{Kind: "result", ID: str(r, "tool_call_id"), Text: capText(str(r, "text")), Images: []Image{}}
				ev.Error, _ = r["is_error"].(bool)
				add(ev)
			}
		}
	})
	res.Next = next
	return res, true, err
}
