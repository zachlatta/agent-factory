package factory

import (
	"strings"
	"testing"
	"time"
)

func actionIDs(a *Ask) string {
	var ids []string
	for _, x := range a.Actions {
		ids = append(ids, x.ID)
	}
	return strings.Join(ids, ",")
}

func TestAskFor(t *testing.T) {
	cases := []struct {
		name    string
		a       Agent
		title   string
		from    string
		actions string
	}{
		{"architecture plan", card("c", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build"}),
			"Approve this plan?", "builder", "approve,reply,close"},
		{"ready to land", card("c", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"}),
			"Ship it?", "builder", "approve,reply,close"},
		{"architecture found in code", card("c", map[string]string{labelStage: "zach", labelNote: "review found an architecture change: approve to land"}),
			"Ship it?", "reviewer", "approve,reply,close"},
		{"architecture preview", card("c", map[string]string{labelStage: "zach", labelNote: previewNote}),
			"Try it, then ship it?", "builder", "approve,reply,close"},
		{"rework cap", card("c", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}),
			"Reviews didn't settle: your call on the code", "builder", "recommend,reply,approve,close"},
		{"escalated", card("c", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"}),
			"A reviewer needs your decision", "builder", "reply,recommend,close"},
		{"no verdict", card("c", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "review"}),
			"The review didn't finish properly", "reviewer", "rerun,reply,close"},
		{"writing its brief", func() Agent {
			a := card("c", map[string]string{labelStage: "zach", labelNote: "rework cap reached"})
			a.Status = "running"
			return a
		}(), "Writing a summary for you…", "builder", "stop"},
		{"close suggested", card("c", map[string]string{labelStage: "zach", labelVerdict: "close", labelNote: "reviewer suggests closing"}),
			"Reviewer thinks this isn't needed", "reviewer", "close,reply"},
		{"landing unverified", card("c", map[string]string{labelStage: "zach", labelNote: "landing not verified"}),
			"Landing didn't reach main", "builder", "reply,done"},
		{"finished unread", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}},
			"Finished: read the result", "builder", "done,reply"},
		{"permission", Agent{ID: "c", Status: "idle", RequiresAttention: true, AttentionReason: "permission", Labels: map[string]string{}},
			"Waiting on you: permission", "builder", "reply,done"},
		{"rework cap on a plan", card("c", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached", labelVerdict: "plan changes"}),
			"Reviews didn't settle: your call on the plan", "builder", "recommend,reply,approve,close"},
		{"stuck", card("c", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"}),
			"Stuck in Building: nothing is queued", "builder", "rerun,reply,done,close"},
		{"moved by hand", card("c", map[string]string{labelStage: "zach"}),
			"Moved here by hand", "builder", "reply,done"},
		{"landed with work left", card("c", map[string]string{labelStage: "zach", labelNote: "landed; still to do: push 2"}),
			"Landed, with work left", "builder", "reply,done"},
		{"landed without a REMAINING line", card("c", map[string]string{labelStage: "zach", labelNote: "landed; did not say what remains"}),
			"Landed: did it finish?", "builder", "done,reply"},
		{"landed other than reviewed", card("c", map[string]string{labelStage: "zach", labelNote: "landed diff differs from the reviewed one: extra commit"}),
			"Landed something other than what was reviewed", "builder", "reply,done"},
	}
	for _, c := range cases {
		cards := BuildBoard([]Agent{c.a}, nil, t0)
		ask := cards[0].Ask
		if ask == nil {
			t.Errorf("%s: no ask", c.name)
			continue
		}
		if ask.Title != c.title || ask.From != c.from || actionIDs(ask) != c.actions || ask.Why == "" {
			t.Errorf("%s: ask = %+v (actions %s)", c.name, ask, actionIDs(ask))
		}
	}
	// a plan at the rework cap is built as it is, not landed; code is landed as it is
	plan := BuildBoard([]Agent{card("c", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached", labelVerdict: "plan changes"})}, nil, t0)[0].Ask
	if plan.Actions[2].Label != btnBuildAsIs {
		t.Errorf("plan rework cap approve label = %q", plan.Actions[2].Label)
	}
	code := BuildBoard([]Agent{card("c", map[string]string{labelStage: "zach", labelNote: "rework cap reached", labelVerdict: "code changes"})}, nil, t0)[0].Ask
	if code.Actions[2].Label != btnLandAsIs {
		t.Errorf("code rework cap approve label = %q", code.Actions[2].Label)
	}
	// only Needs Zach cards carry an ask
	if BuildBoard([]Agent{card("c", map[string]string{labelStage: "building"})}, nil, t0)[0].Ask != nil {
		t.Error("building card has an ask")
	}
}

func TestAskDetailFromBuilderAndReviewer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: noVerdictNote})
	r := reviewer("r1", "c1", "code")
	r.Labels[labelPrefix+labelApplied] = "1"
	f, p := newTestFactory(t)
	storeLog(t, f, p, "r1", "[User] review\n- test missing at a.go:3\nVERDICT: changes")
	storeLog(t, f, p, "c1", "[User] go\nI built it.")
	f.SetAgents([]Agent{c, r})
	got, _ := f.card("c1")
	if got.Ask == nil || got.Ask.Detail != "- test missing at a.go:3" {
		t.Errorf("reviewer detail = %+v", got.Ask)
	}
	// Cached detail stays unchanged while nothing changes.
	n := len(p.calls)
	f.SetAgents([]Agent{c, r})
	if len(p.calls) != n {
		t.Errorf("detail refetched without activity: %v", p.calls[n:])
	}
}

func TestArchitecturePlanAsksBuilderForSummary(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "c1", "Plan: make the requested change.\nTIER: low")
	storeLog(t, f, p, "r1", "TIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "short version for Zach") || strings.Contains(msgs[0], "Build it now") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestReplyToPlanSendsItBackToPlanning(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelTier: "architecture"}))
	if _, err := f.Act("send", Body{ID: "c1", Message: "skip the tests rename"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "plan" || l[labelAwaiting] != "" || l[labelSince] == "" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "skip the tests rename") ||
		!strings.Contains(msgs[0], "whole revised plan") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestDoneMovesCardToDoneWithoutArchiving(t *testing.T) {
	f, p := newTestFactory(t, Agent{ID: "c1", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}})
	if _, err := f.Act("done", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landed" || l[labelNote] != "marked done by Zach" {
		t.Errorf("labels = %v", l)
	}
	if p.called("archive", "c1") {
		t.Error("done must not archive")
	}
}

func TestAskDetailFromClaudeSession(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"})
	c.Provider, c.SessionID, c.Cwd = "claude", "s1", "/w/c1"
	f, _ := newTestFactory(t)
	lines := []string{
		`{"type":"user","message":{"content":"Plan approved.\nReviewer notes:\n- Scope: nothing unasked-for"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Now the tests."},{"type":"thinking","thinking":"hmm"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I added --dry-run.\n\n**Tests:** pass.\nTIER: low"}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-c1/s1.jsonl", strings.Join(lines, "\n")+"\n")
	f.SetAgents([]Agent{c})
	got, _ := f.card("c1")
	if got.Ask.Detail != "I added --dry-run.\n\n**Tests:** pass." {
		t.Errorf("detail = %q", got.Ask.Detail)
	}
}

func TestAskDetailJoinsAReplySplitOverBlocks(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"})
	c.Provider, c.SessionID, c.Cwd = "claude", "s1", "/w/c1"
	f, _ := newTestFactory(t)
	lines := []string{
		`{"type":"user","message":{"content":"go"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"u","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u","content":"ok"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Part one."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Part two.\nTIER: low"}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-c1/s1.jsonl", strings.Join(lines, "\n")+"\n")
	f.SetAgents([]Agent{c})
	got, _ := f.card("c1")
	if got.Ask.Detail != "Part one.\n\nPart two." {
		t.Errorf("detail = %q", got.Ask.Detail)
	}
}

// A landed card held in Needs Zach says why on the board, and is not emailed; nor is a card moved by hand.
func TestLandedAsks(t *testing.T) {
	for note, want := range map[string]string{
		"landed; still to do: push 2 not landed; check production": "push 2 not landed; check production",
		"landed; did not say what remains":                         "REMAINING",
		"landed diff differs from the reviewed one: extra commit":  "extra commit",
	} {
		ask := BuildBoard([]Agent{card("c", map[string]string{labelStage: "zach", labelNote: note})}, nil, t0)[0].Ask
		if ask == nil || !strings.Contains(ask.Why, want) || mailable(ask) {
			t.Errorf("%s: ask = %+v", note, ask)
		}
	}
	if mailable(BuildBoard([]Agent{card("c", map[string]string{labelStage: "zach"})}, nil, t0)[0].Ask) {
		t.Error("a card moved by hand is mailable")
	}
}

func TestAskReplyMidRunTranscriptWithoutToolOutput(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "plan-review"})
	r := reviewer("r1", "c1", "plan")
	r.Labels[labelPrefix+labelApplied] = "1"
	r.Provider, r.SessionID, r.Cwd = "claude", "s-mid", "/w/c1"
	f, _ := newTestFactory(t)
	lines := []string{
		`{"type":"user","message":{"content":"Review this plan."}}`,
		`{"type":"assistant","timestamp":"2026-10-26T04:03:09Z","message":{"content":[{"type":"thinking","thinking":"Inspecting the diff before writing verdict."},{"type":"tool_use","id":"u1","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"user","timestamp":"2026-10-26T04:03:10Z","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":"RAW_TOOL_OUTPUT_DO_NOT_MAIL"}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-c1/s-mid.jsonl", strings.Join(lines, "\n")+"\n")
	if got, err := f.storedReply(r); err != nil || got != "" {
		t.Fatalf("storedReply must stay empty when turn ends in a tool call: got %q, err %v", got, err)
	}
	f.SetAgents([]Agent{c, r})
	got, _ := f.card("c1")
	for _, want := range []string{
		"Stopped mid-run before writing a reply (last tool: Bash at 00:03:09 EDT).",
		"Last reasoning:\nInspecting the diff before writing verdict.",
	} {
		if !strings.Contains(got.Ask.Detail, want) {
			t.Errorf("detail = %q, lacks %q", got.Ask.Detail, want)
		}
	}
	if strings.Contains(got.Ask.Detail, "RAW_TOOL_OUTPUT_DO_NOT_MAIL") {
		t.Errorf("detail leaked tool output: %q", got.Ask.Detail)
	}

	// Earlier assistant text in the same turn before a trailing tool call is used for the ask, while storedReply stays empty.
	r2 := r
	r2.SessionID = "s-assistant-before-tool"
	r2.LastActivityAt = t0.Add(time.Minute)
	lines2 := []string{
		`{"type":"user","message":{"content":"Review this plan."}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hold on, checking one more file.\nTRACK: direct\nTIER: low"},{"type":"tool_use","id":"u2","name":"Read","input":{}}]}}`,
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-c1/s-assistant-before-tool.jsonl", strings.Join(lines2, "\n")+"\n")
	if gotReply, _ := f.storedReply(r2); ParseTrack(gotReply) != "" {
		t.Fatalf("storedReply leaked mid-turn TRACK line: %q", gotReply)
	}
	f.SetAgents([]Agent{c, r2})
	got, _ = f.card("c1")
	if got.Ask.Detail != "Hold on, checking one more file.\nTRACK: direct" {
		t.Errorf("detail = %q, want earlier assistant text", got.Ask.Detail)
	}

	// Missing session file explains why the message could not be read.
	r3 := r
	r3.SessionID = "s-missing"
	r3.LastActivityAt = t0.Add(2 * time.Minute)
	f.SetAgents([]Agent{c, r3})
	got, _ = f.card("c1")
	if !strings.Contains(got.Ask.Detail, "could not be read from the transcript") || !strings.Contains(got.Ask.Detail, "s-missing") {
		t.Errorf("unreadable detail = %q", got.Ask.Detail)
	}
}
