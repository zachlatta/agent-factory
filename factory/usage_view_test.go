package factory

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func usageAgentFile(t *testing.T, f *Factory, id, provider, session string, labels map[string]string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": id, "title": "Synthetic agent", "provider": provider, "cwd": "/work/demo", "archivedAt": "2025-01-01T00:00:00Z", "labels": labels, "config": map[string]string{"model": "fallback"}, "persistence": map[string]string{"sessionId": session}})
	writeFile(t, agentFile(f.cfg.Home, id), string(b))
}

func TestUsageCollectionAndCoverage(t *testing.T) {
	f, _ := newTestFactory(t)
	usageAgentFile(t, f, "a-card", "claude", "sess-a", map[string]string{"factory.stage": "landed"})
	usageAgentFile(t, f, "b-alias", "claude", "sess-a", nil)
	usageAgentFile(t, f, "c-missing", "codex", "gone", nil)
	usageAgentFile(t, f, "d-unsupported", "antigravity", "gem-a", nil)
	usageAgentFile(t, f, "e-child", "claude", "sess-e", map[string]string{"paseo.parent-agent-id": "a-card"})
	usageAgentFile(t, f, "f-review", "claude", "sess-f", map[string]string{"factory.role": "reviewer", "factory.review-kind": "code", "factory.review-of": "a-card"})
	for _, s := range []string{"sess-a", "sess-e", "sess-f"} {
		writeFile(t, filepath.Join(f.cfg.Home, ".claude/projects/demo", s+".jsonl"), `{"type":"assistant","timestamp":"2026-10-25T19:45:00Z","message":{"id":"message-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n")
	}
	f.timings.cards["a-card"] = &CardTiming{Events: []TimingEvent{{Stage: "review", At: t0.Add(-30 * time.Minute)}}}
	f.timings.cards["absent"] = &CardTiming{Events: []TimingEvent{{Stage: "building", At: t0.Add(-time.Hour)}}}
	r, sources := f.collectTokenUsage(context.Background())
	if r.Totals.Total != 45 || r.Coverage.Readable != 3 || r.Coverage.Missing != 1 || r.Coverage.Unsupported != 1 || r.Coverage.Duplicates != 1 || r.Coverage.TimingWithoutAgent != 1 {
		t.Fatalf("snapshot = %+v", r)
	}
	roles := map[string]bool{}
	for _, row := range r.Rows {
		roles[row.Role] = true
		if row.SharedSession {
			if row.Role != "unassigned" || row.Card != "" || row.Stage != "unassigned" {
				t.Fatal(row)
			}
			continue
		}
		if row.Counts.Total > 0 && (row.Card != "a-card" || row.Stage != "review" || row.Model != "model-a") {
			t.Fatalf("row = %+v", row)
		}
	}
	if !roles["unassigned"] || !roles["sub-agent"] || !roles["code reviewer"] {
		t.Fatal(roles)
	}
	if !sources["session:claude:sess-a"] {
		t.Fatal("missing source")
	}
	if err := os.Remove(filepath.Join(f.cfg.Home, ".claude/projects/demo/sess-a.jsonl")); err != nil {
		t.Fatal(err)
	}
	r, after := f.collectTokenUsage(context.Background())
	if r.Coverage.Missing != 2 || after["session:claude:sess-a"] {
		t.Fatalf("deletion = %+v", r.Coverage)
	}
	usageAgentFile(t, f, "g-nested", "claude", "none", map[string]string{"paseo.parent-agent-id": "e-child"})
	r, _ = f.collectTokenUsage(context.Background())
	for _, row := range r.Rows {
		if row.Agent == "g-nested" && row.Card != "a-card" {
			t.Fatal(row)
		}
	}
}

func TestUsageAPIBackgroundRefreshAndPrivacy(t *testing.T) {
	f, p := newTestFactory(t)
	usageAgentFile(t, f, "a-card", "claude", "sess-a", nil)
	path := filepath.Join(f.cfg.Home, ".claude/projects/demo/sess-a.jsonl")
	writeFile(t, path, `{"type":"assistant","timestamp":"2026-10-25T19:45:00Z","cwd":"private-path-marker","message":{"id":"private-session-marker","content":"private-prompt-marker","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n")
	get := func() UsageView {
		t.Helper()
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		for _, bad := range []string{f.cfg.Home, "private-path-marker", "private-session-marker", "private-prompt-marker"} {
			if strings.Contains(rec.Body.String(), bad) {
				t.Fatal("private source escaped")
			}
		}
		var r UsageView
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := get()
	if first.UpdatedAt != "" || !first.Refreshing {
		t.Fatalf("first = %+v", first)
	}
	f.tokenMu.Lock()
	done := f.tokenCache.done
	f.tokenMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh stalled")
		}
	}
	r := get()
	if r.Totals.Total != 15 || r.UpdatedAt == "" || len(r.Plans) != 4 {
		t.Fatalf("ready = %+v", r)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if get().Totals.Total != 15 {
		t.Fatal("fresh snapshot was rebuilt")
	}
	// Expire just the token cache; provider cache has an independent lifetime.
	f.tokenMu.Lock()
	f.tokenCache.fetched = time.Time{}
	f.tokenMu.Unlock()
	get()
	f.tokenMu.Lock()
	done = f.tokenCache.done
	f.tokenMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh stalled")
		}
	}
	r = get()
	if r.Coverage.Disappeared != 1 || r.Totals.Total != 0 || r.Coverage.Missing != 1 {
		t.Fatalf("loss = %+v", r)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 0 {
		t.Fatal("usage collection called paseo")
	}
}

func TestUsageGroupsModelsAndStages(t *testing.T) {
	f, _ := newTestFactory(t)
	usageAgentFile(t, f, "card-a", "claude", "sess-a", map[string]string{"factory.stage": "building"})
	writeFile(t, filepath.Join(f.cfg.Home, ".claude/projects/demo/sess-a.jsonl"), `{"type":"assistant","timestamp":"2026-10-25T19:10:00Z","message":{"id":"msg-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"assistant","timestamp":"2026-10-25T19:50:00Z","message":{"id":"msg-b","model":"model-b","usage":{"input_tokens":20,"output_tokens":10}}}`+"\n")
	f.timings.cards["card-a"] = &CardTiming{Events: []TimingEvent{{Stage: "plan", At: t0.Add(-time.Hour)}, {Stage: "building", At: t0.Add(-30 * time.Minute)}}}
	childPath := filepath.Join(f.cfg.Home, ".claude/projects/demo/sess-a/subagents/agent-a.jsonl")
	writeFile(t, childPath, `{"type":"assistant","isSidechain":true,"timestamp":"2026-10-25T19:50:00Z","message":{"id":"msg-b","model":"model-b","usage":{"input_tokens":7,"output_tokens":3}}}`+"\n")
	r, _ := f.collectTokenUsage(context.Background())
	if len(r.Rows) != 2 || r.Totals.Total != 55 || r.Coverage.Readable != 2 || r.Rows[0].Stage != "plan" || r.Rows[0].Model != "model-a" || r.Rows[1].Stage != "building" || r.Rows[1].Counts.Total != 40 || r.Rows[1].Card != "card-a" || r.Rows[1].Agent != "card-a" || r.Rows[1].Model != "model-b" || r.Rows[1].Plan != "Claude Max" || r.Rows[1].Role != "fellow" {
		t.Fatalf("groups = %+v", r)
	}
	if err := os.Remove(filepath.Join(f.cfg.Home, ".claude/projects/demo/sess-a.jsonl")); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 10 || r.Coverage.Missing != 1 || r.Coverage.Readable != 1 {
		t.Fatal(r)
	}
	writeFile(t, childPath, "{broken}\n")
	if r, _ := f.collectTokenUsage(context.Background()); r.Coverage.Errors != 1 || r.Totals.Total != 0 {
		t.Fatal(r)
	}
	usageAgentFile(t, f, "codex-a", "codex", "sess-c", nil)
	writeFile(t, filepath.Join(f.cfg.Home, ".codex/sessions/2026/01/01/rollout-demo-sess-c.jsonl"), `{"type":"event_msg","timestamp":"2026-10-25T19:50:00Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":2}}}}`+"\n")
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 7 || r.Coverage.Readable != 1 {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ = f.collectTokenUsage(ctx)
	if !r.Coverage.Limited || len(r.Rows) != 0 {
		t.Fatal(r)
	}
	child := Agent{ID: "child", Labels: map[string]string{"paseo.parent-agent-id": "child"}}
	if usageOwner(child, map[string]Agent{"child": child}) != "" {
		t.Fatal("cycle invented owner")
	}
}

func TestUsageOwnerStopsAtQuietAnchors(t *testing.T) {
	for _, anchor := range []Agent{
		{ID: "anchor", Labels: map[string]string{labelPrefix + labelRole: quietAnchorRole, parentAgentLabel: quietBootstrapParent}},
		{ID: "anchor", Labels: map[string]string{"role": InboxAnchorRole}},
	} {
		t.Run(anchor.L(labelRole)+anchor.Labels["role"], func(t *testing.T) {
			root := card("root", map[string]string{labelStage: "building"})
			root.Labels[parentAgentLabel] = anchor.ID
			rev := reviewer("review", "root", "code")
			rev.Labels[parentAgentLabel] = anchor.ID
			child := Agent{ID: "child", Labels: map[string]string{parentAgentLabel: root.ID}}
			nested := Agent{ID: "nested", Labels: map[string]string{parentAgentLabel: rev.ID}}
			agents := map[string]Agent{}
			for _, a := range []Agent{anchor, root, rev, child, nested} {
				agents[a.ID] = a
			}
			for _, a := range []Agent{root, rev, child, nested} {
				if got := usageOwner(a, agents); got != root.ID {
					t.Errorf("usageOwner(%s) = %q, want %q", a.ID, got, root.ID)
				}
			}
			if got := usageOwner(anchor, agents); got != "" {
				t.Errorf("anchor has invented card owner %q", got)
			}
		})
	}
}

func TestUsageCollectionKeepsQuietCardsSeparate(t *testing.T) {
	f, _ := newTestFactory(t)
	labels := map[string]map[string]string{
		"anchor": {labelPrefix + labelRole: quietAnchorRole, parentAgentLabel: quietBootstrapParent},
		"first":  {labelPrefix + labelStage: "building", parentAgentLabel: "anchor"},
		"second": {labelPrefix + labelStage: "review", parentAgentLabel: "anchor"},
		"review": {labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewKind: "code", labelPrefix + labelReviewOf: "first", parentAgentLabel: "anchor"},
		"child":  {parentAgentLabel: "first"},
		"nested": {parentAgentLabel: "review"},
	}
	want := map[string]string{"anchor": "", "first": "first", "second": "second", "review": "first", "child": "first", "nested": "first"}
	for id, l := range labels {
		session := "usage-" + id
		usageAgentFile(t, f, id, "claude", session, l)
		writeFile(t, filepath.Join(f.cfg.Home, ".claude/projects/demo", session+".jsonl"), `{"type":"assistant","timestamp":"2026-10-25T19:45:00Z","message":{"id":"message-a","model":"model-a","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n")
	}
	view, _ := f.collectTokenUsage(context.Background())
	if view.Totals.Total != 90 || len(view.Rows) != len(want) {
		t.Fatalf("lost token rows: %+v", view)
	}
	for _, row := range view.Rows {
		if row.Card != want[row.Agent] || row.Counts.Total != 15 {
			t.Errorf("tokens filed under wrong card: %+v; want %q", row, want[row.Agent])
		}
	}
}
