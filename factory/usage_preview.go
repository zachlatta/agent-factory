package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// seedPreviewUsage supplies dated synthetic records to the real collector. No
// provider commands, live agents or host credentials are used by the preview.
func seedPreviewUsage(f *Factory, now time.Time) error {
	home := f.cfg.Home
	cwd := "/preview/example/usage"
	fixtures := []struct {
		id, title, provider string
		labels              map[string]string
	}{
		{"preview-usage-build", "Improve search", "claude", map[string]string{labelPrefix + labelStage: "building"}},
		{"preview-usage-review", "Review search", "claude", map[string]string{labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewKind: "code", labelPrefix + labelReviewOf: "preview-usage-build"}},
		{"preview-usage-old", "Archived sample work", "claude", nil},
		{"preview-usage-missing", "Missing sample source", "codex", nil},
		{"preview-usage-unsupported", "Unsupported sample source", "antigravity", nil},
	}
	for _, x := range fixtures {
		meta := map[string]any{"id": x.id, "title": x.title, "provider": x.provider, "cwd": cwd, "createdAt": iso(now.Add(-14 * 24 * time.Hour)), "archivedAt": iso(now.Add(-time.Hour)), "lastStatus": "closed", "labels": x.labels, "config": map[string]string{"model": "sample-model"}, "persistence": map[string]string{"sessionId": x.id}}
		b, _ := json.Marshal(meta)
		dir := filepath.Join(home, ".paseo/agents/preview")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, x.id+".json"), b, 0600); err != nil {
			return err
		}
		if x.provider != "claude" {
			continue
		}
		if err := writePreviewSession(home, cwd, x.id, "Inspect sample usage.", "Synthetic work is complete."); err != nil {
			return err
		}
		path := claudePath(home, cwd, x.id)
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		for i, days := range []int{12, 7, 5, 3, 1, 0} {
			tokens := int64(10000 * (i + 1))
			if x.labels[labelPrefix+labelRole] == "reviewer" {
				tokens /= 4
			}
			at := now.Add(-time.Duration(days)*24*time.Hour - time.Hour)
			record := map[string]any{"type": "assistant", "timestamp": iso(at), "message": map[string]any{"id": fmt.Sprintf("sample-message-%d", i), "model": "sample-model", "content": []map[string]string{{"type": "text", "text": "Recorded sample completion."}}, "usage": map[string]int64{"input_tokens": tokens, "output_tokens": tokens / 10, "cache_read_input_tokens": tokens * 2}}}
			b, _ := json.Marshal(record)
			if _, err = fh.Write(append(b, '\n')); err != nil {
				fh.Close()
				return err
			}
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
	f.timings.cards["preview-usage-build"] = &CardTiming{Events: []TimingEvent{{Stage: "plan", At: now.Add(-14 * 24 * time.Hour)}, {Stage: "building", At: now.Add(-10 * 24 * time.Hour)}, {Stage: "review", At: now.Add(-4 * 24 * time.Hour)}, {Stage: "building", At: now.Add(-2 * 24 * time.Hour)}}}
	for _, p := range []string{"claude", "codex", "antigravity", "muse"} {
		u := AgentUsage{Summary: "60% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 60, ResetsAt: iso(now.Add(2 * 24 * time.Hour))}, {Label: "5h", RemainingPct: 80, ResetsAt: iso(now.Add(2 * time.Hour))}}, UpdatedAt: iso(now), Source: "Synthetic provider reading"}
		if p == "claude" {
			u.Accounts = []AccountUsage{{Label: "Account 1", Email: "first.account@example.com", Active: true, Summary: u.Summary, Windows: u.Windows, UpdatedAt: u.UpdatedAt}, {Label: "Account 2", Email: "second.account@example.com", Summary: "40% 7d left", Windows: []UsageWindow{{Label: "7d", RemainingPct: 40, ResetsAt: iso(now.Add(24 * time.Hour))}, {Label: "5h", RemainingPct: 75, ResetsAt: iso(now.Add(time.Hour))}}, UpdatedAt: iso(now.Add(-9 * time.Hour))}}
		}
		if p == "antigravity" {
			u = unknownUsage()
		}
		if p == "muse" {
			u.UpdatedAt = iso(now.Add(-time.Hour))
			u.RefreshError = "Sample refresh failed; last reading retained."
		}
		f.usage.byProvider[p] = u
	}
	v, sources := f.collectTokenUsage(context.Background())
	f.tokenCache.view = v
	f.tokenCache.fetched = now
	f.tokenCache.seen = sources
	return nil
}
