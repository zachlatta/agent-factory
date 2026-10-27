package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PreviewHandler serves the real board and API with synthetic inputs. root must be an
// empty directory owned by the caller. No pipeline loop runs and mutations are refused.
// The caller removes root after closing the server.
func PreviewHandler(root string) (http.Handler, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("preview requires an empty directory")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	home, state := filepath.Join(root, "home"), filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	repo := filepath.Join(home, "dev", "example", "board")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		return nil, err
	}
	f := New(Config{Host: "preview", Home: home, StateDir: state, ReadOnly: true,
		DefaultRepo: "example/board", Guarded: []string{}}, previewPaseo{})
	// Freeze ages for reproducible captures, and prevent every optional process hook.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return now }
	f.GitOut = func(context.Context, string, ...string) (string, error) {
		return "", fmt.Errorf("git disabled in preview")
	}
	f.Git = func(context.Context, string, ...string) error { return fmt.Errorf("git disabled in preview") }
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }
	f.prov.status = map[string]string{"claude": "available", "codex": "available", "antigravity": "available", "muse": "available"}
	f.prov.listed = now
	f.usage.byProvider = map[string]AgentUsage{}
	f.usage.fetched = now
	if err := f.ideas.save([]Idea{{ID: "idea-preview", Title: "Make the board easier to read", Repo: "example/board",
		Summary: "Check an idea before starting it in the selected repo.", Why: "Keep the next action clear on a phone.",
		Request: "Build a readable board layout.", DoneWhen: "Desktop and phone layouts are checked.",
		By: "preview", Tier: "low", Status: "waiting", CreatedAt: now.Add(-time.Hour)}}); err != nil {
		return nil, err
	}
	var agents []Agent
	for _, stage := range Stages {
		if stage[0] == "idea" {
			continue
		}
		id := "preview-" + stage[0]
		a := Agent{ID: id, Host: "preview", Provider: "claude", SessionID: id,
			Cwd: "/preview/example/board", Repo: "example/board", Title: stage[1] + ": check the board layout",
			Status: "idle", CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
			Labels: map[string]string{labelPrefix + labelStage: stage[0], labelPrefix + labelTier: "architecture",
				labelPrefix + labelSource: "board", labelPrefix + labelSeen: strconv.FormatInt(now.Unix(), 10)}}
		if stage[0] == "zach" {
			a.Labels[labelPrefix+labelAwaiting] = "plan"
		}
		// A real session file exercises the normal transcript and Ask parsing paths.
		dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(a.Cwd))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		line, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{
			"content": []map[string]string{{"type": "text", "text": previewReply}}}})
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(line, '\n'), 0600); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	f.hostMetrics.read = func(string) hostReading {
		return hostReading{Metrics: hostMetrics{CPUCount: 4, Errors: map[string]string{}}}
	}
	f.fleet = nil
	f.SetAgents(agents)
	real := f.Handler()
	// Keep host/process inspection routes out of the preview, including future routes
	// until they are explicitly reviewed as safe. Allowed APIs use production handlers.
	allowed := map[string]bool{"/": true, "/index.html": true, "/api/board": true,
		"/api/agents": true, "/api/lanes": true, "/api/deploy": true, "/api/hosts": true, "/host-metrics.js": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safe := allowed[r.URL.Path]
		for _, prefix := range []string{"/t/", "/api/logs/", "/api/transcript/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				id := strings.TrimPrefix(r.URL.Path, prefix)
				_, safe = f.agentOK(id)
				if prefix == "/api/logs/" {
					_, safe = f.card(id)
				}
				break
			}
		}
		if r.Method != http.MethodGet || !safe {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "preview: navigation only; mutations and host inspection are disabled"})
			return
		}
		real.ServeHTTP(w, r)
	}), nil
}

const previewReply = `DECISION: Approve the board layout plan?
OVERVIEW: Make the board easy to read on desktop and phone screens. Check long text, choices, and card details before landing.
NEEDS FROM ZACH: Choose an option so the agent can build it.

The preview uses synthetic cards. Open a card to inspect its details, or switch stage tabs on a phone.

QUESTION: Which layout should the agent build?
- Keep the clear layout (recommended)
- Ask for a revised layout

TIER: architecture`

// previewPaseo never starts a process. Session reads normally satisfy logs; the
// fallback stays synthetic so changes to transcript discovery cannot reach Paseo.
type previewPaseo struct{}

func (previewPaseo) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "logs" {
		return previewReply, nil
	}
	return "", fmt.Errorf("Paseo disabled in preview")
}
