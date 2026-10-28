package factory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFrontierAgentChoices(t *testing.T) {
	want := map[string]bool{
		"opus": true, "codex-sol": true, "gemini-argon": true,
		"sonnet": false, "codex-astra": false, "muse-wonder": false,
	}
	for _, a := range AgentChoices {
		if w, ok := want[a.ID]; !ok || a.Frontier != w {
			t.Errorf("AgentChoice %q Frontier = %v, want %v (known=%v)", a.ID, a.Frontier, w, ok)
		}
	}
}

func TestResolveCardIDAndLineage(t *testing.T) {
	old1 := card("11111111-1111-1111-1111-111111111111", map[string]string{labelStage: "zach", labelReplacedBy: "22222222-2222-2222-2222-222222222222"})
	old2 := card("22222222-2222-2222-2222-222222222222", map[string]string{labelStage: "zach", labelReplaces: old1.ID, labelReplacedBy: "33333333-3333-3333-3333-333333333333"})
	active := card("33333333-3333-3333-3333-333333333333", map[string]string{labelStage: "building", labelReplaces: old2.ID})
	unmarkedOld := card("44444444-4444-4444-4444-444444444444", map[string]string{labelStage: "zach"})
	unmarkedNew := card("55555555-5555-5555-5555-555555555555", map[string]string{labelStage: "building", labelReplaces: unmarkedOld.ID})
	cycA := card("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", map[string]string{labelStage: "building", labelReplaces: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", labelReplacedBy: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"})
	cycB := card("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", map[string]string{labelStage: "building", labelReplaces: cycA.ID, labelReplacedBy: cycA.ID})
	byID := map[string]Agent{
		old1.ID: old1, old2.ID: old2, active.ID: active,
		unmarkedOld.ID: unmarkedOld, unmarkedNew.ID: unmarkedNew, cycA.ID: cycA, cycB.ID: cycB,
	}
	for _, q := range []string{old1.ID, short(old1.ID), old2.ID, short(old2.ID), active.ID, short(active.ID)} {
		if got := resolveCardID(byID, q); got != active.ID {
			t.Errorf("resolveCardID(%q) = %q, want %q", q, got, active.ID)
		}
	}
	if got := resolveCardID(byID, unmarkedOld.ID); got != unmarkedNew.ID {
		t.Errorf("resolveCardID(unmarkedOld) = %q, want %q", got, unmarkedNew.ID)
	}
	if resolveCardID(byID, cycA.ID) == "" {
		t.Error("resolveCardID(cycA) returned empty")
	}
	for _, id := range []string{active.ID, old1.ID} {
		if lin := cardLineage(byID, id); len(lin) != 3 || lin[0].ID != old1.ID || lin[1].ID != old2.ID || lin[2].ID != active.ID {
			t.Fatalf("cardLineage(%s) = %v", id, lin)
		}
	}
	if !sameCard(byID, old1.ID, active.ID) || !sameCard(byID, short(old1.ID), short(active.ID)) {
		t.Error("sameCard failed across replacement chain")
	}
	if !replacedPredecessor(byID, old1) || !replacedPredecessor(byID, old2) || !replacedPredecessor(byID, unmarkedOld) || replacedPredecessor(byID, active) || replacedPredecessor(byID, unmarkedNew) {
		t.Error("replacedPredecessor classification mismatch")
	}
}

func TestExactIDWriteSafety(t *testing.T) {
	oldID := "11111111-1111-1111-1111-111111111111"
	newCard := card("22222222-2222-2222-2222-222222222222", map[string]string{labelStage: "building", labelReplaces: oldID})
	f, p := newTestFactory(t, newCard)
	p.unknown[oldID] = true
	if a, ok := f.agentOK(oldID); ok {
		t.Fatalf("agentOK(%q) resolved to %q; write lookup must stay exact", oldID, a.ID)
	}
	if err := f.archiveChecked(oldID, false); err == nil || p.called("archive", newCard.ID) {
		t.Fatalf("archiveChecked(oldID) err=%v archivedNew=%v", err, p.called("archive", newCard.ID))
	}
}

func TestCarriedLabelsAndEmailChain(t *testing.T) {
	old := card("11111111-1111-1111-1111-111111111111", map[string]string{
		labelStage: "zach", labelNote: "architecture: approve the plan to build", labelAwaiting: "plan", labelTold: "architecture: approve the plan to build",
		labelZachThread: "th-zach-1", labelAnswered: "msg-1 msg-2", labelSource: "email", labelOrigin: "idea", labelTitled: "1",
		labelTier: "architecture", labelTrack: "build", labelBase: "base-commit-1", labelPlanRounds: "2", labelCodeRounds: "1",
		labelZachPlan: "approved", labelDecided: "1", labelPlanFix: "add ordering line", labelReviewed: "rev-commit-1", labelCodeOK: "rev-commit-1",
		labelLandedVerified: "1",
		labelRecovery:       "e30", labelReviewerRetries: "1",
		labelWaitsOn: "depcard", labelWaitsDone: "1", labelHeld: "depcard", labelHeldApproved: "2026-10-25T19:30:00Z",
		labelDelegated: "2026-10-25T19:00:00Z", labelArbitrated: "plan", labelWorkLeftRounds: "2", labelModelHandoff: "1", labelReplacedBy: "22222222-2222-2222-2222-222222222222",
	})
	old.Labels[gmailThreadLabel] = "th-gmail-1"
	copied := copyCarriedLabels(old)
	if copied[labelPrefix+labelHeldApproved] == "" {
		t.Error("copyCarriedLabels dropped Zach's held approval")
	}
	for _, k := range carriedFactoryLabels {
		if k != labelTold && copied[labelPrefix+k] == "" {
			t.Errorf("copyCarriedLabels missing factory.%s", k)
		}
	}
	if copied[gmailThreadLabel] != "th-gmail-1" {
		t.Errorf("copyCarriedLabels gmail-thread = %q", copied[gmailThreadLabel])
	}
	inOld := card("old-imm", map[string]string{labelAnswered: "m1", labelReplacedBy: "new-imm"})
	inNew := card("new-imm", map[string]string{labelReplaces: "old-imm", labelAnswered: "m2"})
	outImm := applyLineageLabels([]Agent{inOld, inNew})
	if inNew.L(labelAnswered) != "m2" || outImm[1].L(labelAnswered) != "m1 m2" {
		t.Fatalf("applyLineageLabels mutated input (%q) or failed merge (%q)", inNew.L(labelAnswered), outImm[1].L(labelAnswered))
	}
	// When the replacement agent clears labels after handoff (either set to "" or deleted/omitted),
	// applyLineageLabels must not bring them back from old.
	for _, deleteKeys := range []bool{false, true} {
		cleared := card("22222222-2222-2222-2222-222222222222", map[string]string{labelStage: "landing", labelReplaces: old.ID})
		for k, v := range copied {
			cleared.Labels[k] = v
		}
		for _, k := range []string{labelHeld, labelHeldApproved, labelPlanFix, labelWaitsOn, labelWaitsDone, labelReviewed, labelLandedVerified, labelDecided} {
			if deleteKeys {
				delete(cleared.Labels, labelPrefix+k)
			} else {
				cleared.Labels[labelPrefix+k] = ""
			}
		}
		gotCleared := applyLineageLabels([]Agent{old, cleared})[1]
		for _, k := range []string{labelHeld, labelHeldApproved, labelPlanFix, labelWaitsOn, labelWaitsDone, labelReviewed, labelLandedVerified, labelDecided} {
			if v := gotCleared.L(k); v != "" {
				t.Fatalf("deleteKeys=%v: cleared label %s came back from old as %q", deleteKeys, k, v)
			}
		}
	}
	repl := card("22222222-2222-2222-2222-222222222222", map[string]string{labelStage: "zach", labelNote: "architecture: approve the plan to build", labelAwaiting: "plan", labelReplaces: old.ID})
	f, p := newTestFactory(t, old, repl)
	f.cfg.ZachEmail = "zach@example.com"
	mailer := &fakeMail{}
	f.Mail = mailer
	storeLog(t, f, p, old.ID, "[User] Zach's request:\nBuild feature\n\nWork in this repo\nDECISION: Approve plan?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Approve.")
	storeLog(t, f, p, repl.ID, "[User] Handoff\nDECISION: Approve plan?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Approve.")
	f.SetAgents([]Agent{old, repl})
	for _, th := range []string{"th-zach-1", "th-gmail-1"} {
		if w, err := f.waitingCard(th); err != nil || w.ID != repl.ID {
			t.Fatalf("waitingCard(%q) = %s, %v; want %s", th, w.ID, err, repl.ID)
		}
	}
	c, ok := f.card(repl.ID)
	if !ok || f.mailZach(f.agent(repl.ID), c) != nil || len(mailer.calls) != 0 {
		t.Fatalf("card=%v calls=%d, want 0 (already told on predecessor)", ok, len(mailer.calls))
	}
	if c.Tier != "architecture" || c.Rounds.Plan != 2 || c.Rounds.Code != 1 {
		t.Errorf("Card carried gates = %+v", c)
	}
	ra := f.agent(repl.ID)
	for k, want := range map[string]string{labelBase: "base-commit-1", labelZachPlan: "approved", labelZachThread: "th-zach-1", labelSource: "email", labelOrigin: "idea", labelTitled: "1", labelWorkLeftRounds: "2"} {
		if got := ra.L(k); got != want {
			t.Errorf("repl inherited L(%s) = %q, want %q", k, got, want)
		}
	}
	if dupRes, err := f.answerByEmail(Body{Thread: "th-zach-1", MessageID: "msg-2", Text: "approve"}); err != nil || dupRes["action"] != "duplicate" {
		t.Fatalf("answerByEmail dup = %v, %v", dupRes, err)
	}
	if okRes, err := f.answerByEmail(Body{Thread: "th-zach-1", MessageID: "msg-3", Text: "approve"}); err != nil || okRes["card"] != repl.ID || okRes["action"] != "approve" || len(p.sent(repl.ID)) == 0 {
		t.Fatalf("answerByEmail new = %v, %v, sent=%d", okRes, err, len(p.sent(repl.ID)))
	}
}

func TestReplacementResolutionConsumers(t *testing.T) {
	oldID, newID := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	old := card(oldID, map[string]string{labelStage: "building", labelOrigin: "idea", labelTier: "low", labelPlanRounds: "1", labelReplacedBy: newID})
	old.ArchivedAt = t0.Add(-24 * time.Hour)
	repl := card(newID, map[string]string{labelStage: "building", labelOrigin: "idea", labelTier: "low", labelReplaces: oldID})
	repl.Provider, repl.Model = "codex", "gpt-6.1-sol"
	sub := Agent{
		ID: "sub-old-1", Host: "host1", Title: "helper sub-agent", Status: "running", Provider: "claude", Model: "claude-opus-5-5",
		CreatedAt: t0.Add(-15 * time.Minute), LastActivityAt: t0.Add(-time.Minute), Labels: map[string]string{"paseo.parent-agent-id": oldID},
	}
	rev := reviewer("rev-old-1", oldID, "code")
	waiter := card("33333333-3333-3333-3333-333333333333", map[string]string{labelStage: "landing", labelWaitsOn: short(oldID), labelHeld: short(oldID)})
	f, p := newTestFactory(t)
	// This test exercises replacement routing, with a readable harmless diff.
	// Unreadable policy data must continue to ask rather than auto-land.
	f.GitOut = gitStub(map[string]string{
		"diff --no-renames --name-only origin/main...HEAD": "notes.txt\n",
		"show origin/main:.factory.json":                   "{}",
	})

	fakeScan(f, nil, nil) // replacement routing uses a synthetic clean scan, never host credentials

	writeAgentDisk := func(a Agent) {
		dir := filepath.Join(f.cfg.Home, ".paseo/agents", a.Provider)
		os.MkdirAll(dir, 0o755)
		raw := map[string]any{
			"id": a.ID, "name": a.Title, "cwd": a.Cwd, "workspaceId": a.WorkspaceID, "provider": a.Provider, "model": a.Model,
			"lastStatus": a.Status, "createdAt": a.CreatedAt.Format(time.RFC3339), "lastActivityAt": a.LastActivityAt.Format(time.RFC3339),
			"labels": a.Labels, "persistence": map[string]any{"sessionId": a.SessionID},
		}
		if !a.ArchivedAt.IsZero() {
			raw["archivedAt"] = a.ArchivedAt.Format(time.RFC3339)
		}
		b, _ := json.Marshal(raw)
		os.WriteFile(filepath.Join(dir, a.ID+".json"), b, 0o644)
	}
	sharedCwd := filepath.Join(t.TempDir(), "wt")
	os.MkdirAll(filepath.Join(sharedCwd, ".factory-evidence"), 0o755)
	png1x1 := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82")
	os.WriteFile(filepath.Join(sharedCwd, ".factory-evidence/after.png"), png1x1, 0o644)
	old.Cwd, repl.Cwd = sharedCwd, sharedCwd

	claudeLine := func(typ string, content any) string {
		b, _ := json.Marshal(map[string]any{"type": typ, "message": map[string]any{"content": content}})
		return string(b) + "\n"
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-test/"+testSession(oldID)+".jsonl",
		claudeLine("user", "Zach's request:\nOriginal task text\n\nWork in this repo (example/desk).")+
			claudeLine("user", "Zach's follow-up email (Sun, 25 Oct 2026 19:10:00 +0000):\nFirst follow-up\n(end of Zach's email)")+
			claudeLine("assistant", []any{map[string]any{"type": "text", "text": "Old assistant reply"}}))
	f.SetAgents([]Agent{old})
	if err := f.ideas.save([]Idea{{ID: "idea-1", Title: "Test idea", Request: "Original task text", Repo: "example/desk", Status: "started", Card: oldID, StartedBy: "auto", StartedAt: t0.Add(-30 * time.Minute), CreatedAt: t0.Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(f.cfg.Home, ".codex/sessions/2026/10/25")
	os.MkdirAll(codexDir, 0o755)
	codexFile := filepath.Join(codexDir, "rollout-2026-10-25-"+testSession(newID)+".jsonl")
	writeCodexMsg := func(role, itemType, text, ts string) string {
		b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": itemType, "text": text}}}})
		return string(b) + "\n"
	}
	os.WriteFile(codexFile, []byte(
		writeCodexMsg("user", "input_text", "Handoff prompt\n\nZach's follow-up email (Sun, 25 Oct 2026 19:10:00 +0000):\nFirst follow-up\n(end of Zach's email)", "2026-10-25T19:30:00Z")+
			writeCodexMsg("assistant", "output_text", "DECISION: Preview ready\nOVERVIEW: Built.\nNEEDS FROM ZACH: Try it.\n\nAFTER: after.png", "2026-10-25T19:35:00Z")), 0o644)
	for _, a := range []Agent{old, repl, sub, rev, waiter} {
		writeAgentDisk(a)
	}
	loaded, err := LoadAgents(f.cfg.Home, "host1", 12*time.Hour, t0)
	if err != nil || !slices.ContainsFunc(loaded, func(a Agent) bool { return a.ID == oldID }) {
		t.Fatalf("LoadAgents err=%v, retainedOld=%v", err, slices.ContainsFunc(loaded, func(a Agent) bool { return a.ID == oldID }))
	}
	f.SetAgents(loaded)
	if slices.ContainsFunc(f.cards, func(c Card) bool { return c.ID == oldID }) {
		t.Fatalf("BuildBoard showed replaced predecessor %s as a separate card", oldID)
	}
	c, ok := f.card(oldID)
	if !ok || c.ID != newID || !slices.Equal(c.Replaces, []string{oldID}) || c.Idea == nil || c.Idea.ID != "idea-1" || len(c.Children) != 2 {
		t.Fatalf("f.card(oldID) = %+v, %v", c, ok)
	}
	if st := depState(f.agents(), waiter, waiter.L(labelWaitsOn)); st != depWaiting {
		t.Fatalf("depState(waiter -> oldID while newID building) = %v, want depWaiting", st)
	}
	if !reviewPending(f.agents(), newID, t0) {
		t.Error("reviewPending(newID) = false for reviewer targeting oldID")
	}
	storeLog(t, f, p, rev.ID, "Looks good.\nALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(rev); err != nil || p.labels(newID)[labelStage] != "landing" || len(p.sent(oldID)) > 0 {
		t.Fatalf("ApplyVerdict err=%v labels=%v sentOld=%d", err, p.labels(newID), len(p.sent(oldID)))
	}
	tm, ok := f.timings.get(newID)
	if !ok || len(tm.Sessions) < 2 {
		t.Fatalf("CardTiming(newID) = %+v, %v; want migrated with >=2 sessions", tm, ok)
	}
	if _, stillOld := f.timings.get(oldID); stillOld {
		t.Error("oldID timing entry still exists after migration to newID")
	}
	byID := f.byID()
	for _, ag := range []Agent{old, repl, sub, rev} {
		if got := usageOwner(ag, byID); got != newID {
			t.Errorf("usageOwner(%s) = %q, want %q", ag.ID, got, newID)
		}
	}
	if req := f.cardRequest(f.agent(newID)); req.Base != "Original task text" || len(req.FollowUps) != 1 || req.FollowUps[0].Text != "First follow-up" {
		t.Fatalf("cardRequest initial = %+v", req)
	}
	fh, err := os.OpenFile(codexFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(writeCodexMsg("user", "input_text", "Zach's follow-up email (Sun, 25 Oct 2026 19:40:00 +0000):\nSecond follow-up on Codex\n(end of Zach's email)", "2026-10-25T19:40:00Z"))
	fh.WriteString(writeCodexMsg("assistant", "output_text", "DECISION: Preview ready\nOVERVIEW: Built.\nNEEDS FROM ZACH: Try it.\n\nAFTER: after.png", "2026-10-25T19:45:00Z"))
	fh.Close()
	if req2 := f.cardRequest(f.agent(newID)); len(req2.FollowUps) != 2 || req2.FollowUps[1].Text != "Second follow-up on Codex" {
		t.Fatalf("cardRequest after Codex growth = %+v, want 2 follow-ups", req2)
	}
	if peers := f.Peers(oldID, ""); peers.For == nil || peers.For.ID != newID {
		t.Errorf("Peers(oldID).For = %+v, want %s", peers.For, newID)
	}
	if err := f.steers.put("dup-card", Steer{Keep: oldID, From: "dup1234", Title: "Dup card", Repo: "example/desk", Request: CardRequest{Base: "Extra dup request"}}); err != nil {
		t.Fatal(err)
	}
	f.tickSteers()
	if len(p.sent(newID)) == 0 {
		t.Error("tickSteers did not deliver steer targeting oldID to newID")
	}
	srv := f.Handler()
	for _, path := range []string{"/api/transcript/" + oldID + "?tail=1", "/api/transcript/" + newID + "?tail=1"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var tr Transcript
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tr) != nil {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		var texts []string
		for _, e := range tr.Events {
			texts = append(texts, e.Kind+":"+e.Text)
		}
		if joined := strings.Join(texts, "\n"); !strings.Contains(joined, "Old assistant reply") || !strings.Contains(joined, "Switched from") || !strings.Contains(joined, "DECISION: Preview ready") {
			t.Errorf("GET %s events missing stitched history:\n%s", path, joined)
		}
	}
	replZach := repl
	replZach.Labels = map[string]string{labelPrefix + labelStage: "zach", labelPrefix + labelNote: readyNote(1), labelPrefix + labelReplaces: oldID}
	f.SetAgents([]Agent{old, replZach})
	for _, path := range []string{"/api/evidence/" + oldID + "/after", "/api/evidence/" + newID + "/after"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d (%s), want 200", path, rec.Code, rec.Body.String())
		}
	}
	replLanded := repl
	replLanded.Labels = map[string]string{labelPrefix + labelStage: "landed", labelPrefix + labelReplaces: oldID}
	f.SetAgents([]Agent{old, replLanded, waiter})
	if st := depState(f.agents(), waiter, waiter.L(labelWaitsOn)); st != depLanded {
		t.Errorf("depState(waiter -> oldID after newID landed) = %v, want depLanded", st)
	}
	wsFile := filepath.Join(f.cfg.Home, ".paseo/projects/workspaces.json")
	os.MkdirAll(filepath.Dir(wsFile), 0o755)
	wsJSON, _ := json.Marshal([]map[string]any{{"workspaceId": "ws-1", "kind": "worktree", "cwd": sharedCwd, "worktreeRoot": sharedCwd, "mainRepoRoot": sharedCwd, "branch": "feat", "isPaseoOwnedWorktree": true}})
	os.WriteFile(wsFile, wsJSON, 0o644)
	kindOf := func(inv RetirementInventory, id string) string {
		for _, w := range inv.Workspaces {
			for _, ag := range w.Agents {
				if ag.ID == id {
					return ag.Kind
				}
			}
		}
		return ""
	}
	if got := kindOf(InventoryRetirement(context.Background(), f.cfg.Home, t0), oldID); got != "replaced-with-protected-target" {
		t.Errorf("retirement kind while replacement building = %q, want replaced-with-protected-target", got)
	}
	writeAgentDisk(replLanded)
	if got := kindOf(InventoryRetirement(context.Background(), f.cfg.Home, t0), oldID); got != "replaced-card" {
		t.Errorf("retirement kind after replacement landed = %q, want replaced-card", got)
	}
}
