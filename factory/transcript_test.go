package factory

import (
	"strings"
	"testing"
)

func TestReadTranscript(t *testing.T) {
	home := t.TempDir()
	cwd := "/srv/u/.paseo/worktrees/x"
	lines := []string{
		`{"type":"user","timestamp":"t1","message":{"content":"Fix the bug"}}`,
		`{"type":"user","isMeta":true,"message":{"content":[{"type":"text","text":"meta"}]}}`,
		`{"type":"user","message":{"content":"<system-reminder>be good</system-reminder>"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"On it"},{"type":"tool_use","id":"u1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"text","text":"a.go"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"sidechain"}]}}`,
		`not json`,
	}
	writeFile(t, home+"/.claude/projects/-srv-u--paseo-worktrees-x/sess.jsonl", strings.Join(lines, "\n")+"\n")

	res, err := ReadTranscript(home, TranscriptSource{Provider: "claude", SessionID: "sess", Cwd: cwd}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range res.Events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user,system,thinking,assistant,tool,result" {
		t.Errorf("kinds = %s", got)
	}
	if res.Next != 7 {
		t.Errorf("next = %d", res.Next)
	}
	r := res.Events[5]
	if r.Text != "a.go" || len(r.Images) != 1 || r.Images[0].Data != "AAA" {
		t.Errorf("result = %+v", r)
	}
	// incremental: nothing after the last line
	res2, _ := ReadTranscript(home, TranscriptSource{Provider: "claude", SessionID: "sess", Cwd: cwd}, 7)
	if len(res2.Events) != 0 || res2.Next != 7 {
		t.Errorf("incremental = %+v", res2)
	}
	// An agent with no session file reports that native history is unavailable.
	res3, _ := ReadTranscript(home, TranscriptSource{Provider: "codex"}, 0)
	if !res3.Fallback {
		t.Error("codex with no session should report unavailable history")
	}
}
