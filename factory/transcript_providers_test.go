package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func kinds(evs []Event) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func TestReadCodexTranscript(t *testing.T) {
	home := t.TempDir()
	lines := []string{
		`{"timestamp":"t0","type":"session_meta","payload":{"id":"sess-c"}}`,
		`{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex"}]}}`,
		`{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /w\n\nbe good"}]}}`,
		`{"timestamp":"t2","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"},{"type":"input_image","image_url":"data:image/jpeg;base64,QUJD"}]}}`,
		`{"timestamp":"t3","type":"event_msg","payload":{"type":"token_count"}}`,
		`{"timestamp":"t3","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"think it over"}],"encrypted_content":"x"}}`,
		`{"timestamp":"t3","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"x"}}`,
		`{"timestamp":"t4","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"On it"}]}}`,
		`{"timestamp":"t5","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.exec_command({cmd:\"ls\"})"}}`,
		`{"timestamp":"t5","type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}`,
		`{"timestamp":"t6","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"a.go"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}}`,
		`{"timestamp":"t6","type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"ok"}}`,
	}
	writeFile(t, home+"/.codex/sessions/2026/01/02/rollout-2026-01-02T00-00-00-sess-c.jsonl", strings.Join(lines, "\n")+"\n")

	src := TranscriptSource{Provider: "codex", SessionID: "sess-c"}
	res, err := ReadTranscript(home, src, 0)
	if err != nil || res.Fallback {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if got := kinds(res.Events); got != "system,user,image,thinking,assistant,tool,tool,result,result" {
		t.Errorf("kinds = %s", got)
	}
	if e := res.Events[2]; e.Media != "image/jpeg" || e.Data != "QUJD" || e.From != "user" {
		t.Errorf("image = %+v", e)
	}
	if e := res.Events[5]; e.Name != "exec" || e.ID != "c1" || !strings.Contains(e.Input["command"].(string), "exec_command") {
		t.Errorf("exec = %+v", e)
	}
	if e := res.Events[6]; e.Name != "Bash" || e.Native != "exec_command" || e.Input["command"] != "go test ./..." {
		t.Errorf("exec_command = %+v", e)
	}
	if e := res.Events[7]; e.ID != "c1" || e.Text != "a.go" || len(e.Images) != 1 || e.Images[0].Data != "AAA" {
		t.Errorf("result = %+v", e)
	}
	if res.Next != len(lines) || res.Replace {
		t.Errorf("next = %d replace = %v", res.Next, res.Replace)
	}
	res2, _ := ReadTranscript(home, src, len(lines))
	if len(res2.Events) != 0 || res2.Fallback {
		t.Errorf("incremental = %+v", res2)
	}
}

func TestReadAntigravityTranscript(t *testing.T) {
	home := t.TempDir()
	conv := "conv-1"
	brain := home + "/.gemini/antigravity-cli/brain/" + conv + "/.system_generated"
	lines := []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"t0","content":"<USER_REQUEST>\nFix the bug\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"t1","thinking":"look first","content":"Reading.","tool_calls":[{"name":"run_command","args":{"CommandLine":"ls","toolSummary":"List files"}},{"name":"read_file","args":{"Path":"/w/a.go","StartLine":5}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE","created_at":"t2","content":"Created At: x\nCompleted At: y\nThe tool's output was truncated."}`,
		`{"step_index":3,"type":"GENERIC","status":"DONE","created_at":"t2","content":"Created At: x\nCompleted At: y\nShowing lines 5-6\n5: package a"}`,
		`{"step_index":4,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"t3","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"/w/b.go","CodeContent":"package b"}},{"name":"edit_file","args":{"Path":"/w/a.go","Target":"x","Replacement":"y"}}]}`,
		`{"step_index":5,"type":"GENERIC","status":"ERROR","created_at":"t4","content":"permission denied"}`,
		`{"step_index":6,"type":"GENERIC","status":"RUNNING","created_at":"t4","content":"Created At: x\nTool is running as a background task"}`,
		`{"step_index":7,"type":"SYSTEM_MESSAGE","status":"DONE","created_at":"t5","content":"<SYSTEM_MESSAGE>task done</SYSTEM_MESSAGE>"}`,
		`{"step_index":8,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"t6","content":"Plan.\nTIER: low"}`,
	}
	writeFile(t, brain+"/logs/transcript_full.jsonl", strings.Join(lines, "\n")+"\n")
	writeFile(t, brain+"/steps/2/output.txt", "the whole output")

	src := TranscriptSource{Provider: "antigravity", SessionID: `plugin:{"version":1,"data":{"conversationId":"conv-1"}}`}
	res, err := ReadTranscript(home, src, 0)
	if err != nil || res.Fallback {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if got := kinds(res.Events); got != "user,thinking,assistant,tool,tool,result,result,tool,tool,result,result,system,assistant" {
		t.Errorf("kinds = %s", got)
	}
	if !res.Replace {
		t.Error("antigravity steps change in place: the whole transcript is sent each time")
	}
	if res.Events[0].Text != "Fix the bug" {
		t.Errorf("user = %q", res.Events[0].Text)
	}
	run, read := res.Events[3], res.Events[4]
	if run.Name != "Bash" || run.Native != "run_command" || run.Input["command"] != "ls" || run.Input["description"] != "List files" {
		t.Errorf("run = %+v", run)
	}
	if read.Name != "Read" || read.Input["file_path"] != "/w/a.go" || read.Input["offset"] != float64(5) {
		t.Errorf("read = %+v", read)
	}
	r1, r2 := res.Events[5], res.Events[6]
	if r1.ID != run.ID || r1.Text != "the whole output" || r2.ID != read.ID || r2.Text != "Showing lines 5-6\n5: package a" {
		t.Errorf("results = %+v / %+v", r1, r2)
	}
	w, ed := res.Events[7], res.Events[8]
	if w.Name != "Write" || w.Input["content"] != "package b" || ed.Name != "Edit" || ed.Input["old_string"] != "x" || ed.Input["new_string"] != "y" || ed.Input["file_path"] != "/w/a.go" {
		t.Errorf("write/edit = %+v / %+v", w, ed)
	}
	if e := res.Events[9]; e.ID != w.ID || !e.Error {
		t.Errorf("error result = %+v", e)
	}
	if e := res.Events[10]; e.ID != ed.ID || e.Error || e.Text != "Tool is running as a background task" {
		t.Errorf("running result = %+v", e)
	}
	// a step that changes in place (RUNNING -> DONE) shows its new output on the next read
	lines[6] = `{"step_index":6,"type":"GENERIC","status":"DONE","created_at":"t4","content":"done now"}`
	writeFile(t, brain+"/logs/transcript_full.jsonl", strings.Join(lines, "\n")+"\n")
	res2, _ := ReadTranscript(home, src, res.Next)
	if len(res2.Events) != len(res.Events) || res2.Events[10].Text != "done now" {
		t.Errorf("reread = %+v", res2.Events)
	}
}

// The Muse reader is provisional: its record names come from a short session only.
func TestReadMuseTranscript(t *testing.T) {
	home := t.TempDir()
	ev := func(ts int, e string) string {
		return `{"recorded_at":` + strconv.Itoa(ts) + `,"payload_type":"runtime.session","payload":{"event":` + e + `}}`
	}
	lines := []string{
		`{"payload_type":"runtime.session.metadata","payload":{}}`,
		ev(1018, `{"kind":"started","prompt":"<system_instructions>x</system_instructions>Fix it"}`),
		ev(1019, `{"kind":"user_prompt_display","prompt":"Fix it"}`),
		ev(1020, `{"kind":"reasoning_summary_committed","text":"hmm"}`),
		ev(1021, `{"kind":"assistant_message_committed","text":"Looking."}`),
		ev(1022, `{"kind":"assistant_tool_calls_committed","tool_calls":[{"call_id":"k1","name":"shell","args":"{\"command\":\"ls\"}"},{"call_id":"k2","name":"odd","args":"not json"}]}`),
		ev(1023, `{"kind":"tool_result_batch_committed","results":[{"tool_call_id":"k1","text":"a.go"},{"tool_call_id":"k2","text":"boom","is_error":true}]}`),
		ev(1024, `{"kind":"brand_new_kind","text":"ignored"}`),
		ev(1025, `{"kind":"assistant_message_committed","text":"Done.\nTIER: low"}`),
	}
	writeFile(t, home+"/.local/share/muse/sessions/2026/01/02/sess-m/session.jsonl", strings.Join(lines, "\n")+"\n")
	writeFile(t, home+"/.local/share/muse/sessions/2026/01/02/sess-m/subagent/x/session.jsonl", lines[4]+"\n")

	src := TranscriptSource{Provider: "muse", SessionID: `plugin:{"version":1,"data":{"sessionId":"sess-m","model":"m"}}`}
	res, err := ReadTranscript(home, src, 0)
	if err != nil || res.Fallback {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if got := kinds(res.Events); got != "user,thinking,assistant,tool,tool,result,result,assistant" {
		t.Errorf("kinds = %s", got)
	}
	if e := res.Events[3]; e.Name != "Bash" || e.ID != "k1" || e.Input["command"] != "ls" {
		t.Errorf("shell = %+v", e)
	}
	if e := res.Events[4]; e.Name != "odd" || e.Input["input"] != "not json" {
		t.Errorf("odd = %+v", e)
	}
	if e := res.Events[6]; e.ID != "k2" || !e.Error {
		t.Errorf("error result = %+v", e)
	}
	if res.Events[0].Ts == "" {
		t.Error("no timestamp")
	}
}

func TestReadTranscriptFallbacks(t *testing.T) {
	home := t.TempDir()
	for _, src := range []TranscriptSource{
		{Provider: "gemini-cli", SessionID: "s"},                                         // unknown provider
		{Provider: "codex", SessionID: "missing"},                                        // no file
		{Provider: "codex", SessionID: "../../etc/x"},                                    // not a plain id
		{Provider: "antigravity", SessionID: `plugin:{"data":{"conversationId":"a/b"}}`}, // not a plain id
		{Provider: "muse", SessionID: "plugin:not json"},
	} {
		res, err := ReadTranscript(home, src, 0)
		if err != nil || !res.Fallback || !res.Replace {
			t.Errorf("%+v: %+v, %v", src, res, err)
		}
	}
	// A file with nothing readable in it yet reports unavailable native history.
	writeFile(t, home+"/.codex/sessions/2026/01/02/rollout-x-sess-e.jsonl", `{"type":"session_meta"}`+"\n")
	if res, _ := ReadTranscript(home, TranscriptSource{Provider: "codex", SessionID: "sess-e"}, 0); !res.Fallback {
		t.Errorf("empty file: %+v", res)
	}
}

// antigravityFile writes an Argon (Antigravity) conversation log for agent a.
func antigravityFile(t *testing.T, home, conv string, lines ...string) string {
	t.Helper()
	writeFile(t, home+"/.gemini/antigravity-cli/brain/"+conv+"/.system_generated/logs/transcript_full.jsonl",
		strings.Join(lines, "\n")+"\n")
	return `plugin:{"version":1,"data":{"conversationId":"` + conv + `"}}`
}

func TestAskDetailFromAntigravitySession(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"})
	f, _ := newTestFactory(t)
	c.Provider, c.Cwd = "antigravity", "/w/c1"
	c.SessionID = antigravityFile(t, f.cfg.Home, "conv-a",
		`{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\nPlan approved.\n</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","content":"Testing.","tool_calls":[{"name":"run_command","args":{"CommandLine":"go test"}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE","content":"ok"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","thinking":"done","content":"I added --dry-run.\nTIER: low"}`)
	f.SetAgents([]Agent{c})
	got, _ := f.card("c1")
	if got.Ask.Detail != "I added --dry-run." {
		t.Errorf("detail = %q", got.Ask.Detail)
	}
}

func TestTickReadsTrackFromACodexReply(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	c.Provider, c.SessionID = "codex", "sess-t"
	writeFile(t, f.cfg.Home+"/.codex/sessions/2026/01/02/rollout-x-sess-t.jsonl", strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"You are the Fellow...\nTRACK: direct\nAnything else"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Plan: fix the board.\nTIER: risky"}]}}`,
	}, "\n")+"\n")
	// The prompt's TRACK line must not be interpreted as the agent's reply.
	f.SetAgents([]Agent{c})
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "plan-review" {
		t.Errorf("labels = %v, calls = %v", l, p.calls)
	}
}

func TestPeersRequestFromCodexSession(t *testing.T) {
	a := card("rq1", map[string]string{labelStage: "plan", labelSource: "board"})
	a.Provider, a.SessionID, a.Cwd = "codex", "sess-q", "/w/rq1"
	f, _ := newTestFactory(t, a)
	prompt := fmt.Sprintf(newCardPrompt, "Make agents aware\nof each other", "example/desk", "", "", "", "")
	line, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": prompt}}}})
	writeFile(t, f.cfg.Home+"/.codex/sessions/2026/01/02/rollout-x-sess-q.jsonl",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /w"}]}}`+"\n"+string(line)+"\n")
	p := f.Peers("", "example/desk")
	if len(p.InFlight) != 1 || p.InFlight[0].Request != "Make agents aware of each other" {
		t.Errorf("request = %+v", p.InFlight)
	}
}

func TestMuseFollowUpResetsAskReply(t *testing.T) {
	c := card("c1", map[string]string{})
	c.Provider = "muse"
	c.SessionID = `plugin:{"version":1,"data":{"sessionId":"synthetic-muse"}}`
	f, _ := newTestFactory(t, c)
	path := f.cfg.Home + "/.local/share/muse/sessions/2026/01/02/synthetic-muse/session.jsonl"
	first := `{"payload_type":"runtime.session","payload":{"event":{"kind":"assistant_message_committed","text":"Previous build reply"}}}`
	follow := `{"recorded_at":2000,"payload_type":"runtime.command_intake.received","payload":{"record":{"command":{"kind":"turn_submit","prompt":"Check completion"}}}}`
	writeFile(t, path, first+"\n"+follow+"\n")
	reply, err := f.askReply(c)
	if strings.Contains(reply, "Previous build reply") || reply != "" {
		t.Fatalf("stale reply: %q, %v", reply, err)
	}
	tr, err := ReadTranscript(f.cfg.Home, TranscriptSource{Provider: "muse", SessionID: c.SessionID}, 1)
	if err != nil || len(tr.Events) != 1 || tr.Events[0].Kind != "user" || tr.Events[0].Text != "Check completion" || tr.Events[0].Ts == "" || tr.Events[0].Line != 2 {
		t.Fatalf("follow-up: %+v %v", tr, err)
	}
	answer := `{"payload_type":"runtime.session","payload":{"event":{"kind":"assistant_message_committed","text":"REMAINING: none"}}}`
	writeFile(t, path, first+"\n"+follow+"\n"+answer+"\n")
	reply, err = f.askReply(c)
	if err != nil || reply != "REMAINING: none" {
		t.Fatalf("new reply: %q, %v", reply, err)
	}
}
