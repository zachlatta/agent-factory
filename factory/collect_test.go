package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgents(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/.paseo/projects/projects.json",
		`[{"projectId":"p1","projectKey":"remote:github.com/example/desk","displayName":"desk"}]`)
	writeFile(t, home+"/.paseo/projects/workspaces.json",
		`[{"workspaceId":"w1","projectId":"p1","branch":"feat","kind":"worktree"}]`)
	writeFile(t, home+"/.paseo/agents/a/open.json", `{"id":"open","title":"Open card","provider":"claude",
		"cwd":"/x","workspaceId":"w1","createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z",
		"lastUserMessageAt":"2026-10-25T19:30:00.000Z","lastStatus":"running","labels":{"factory.stage":"building"},"config":{"model":"claude-opus-5-5"},
		"requiresAttention":false,"persistence":{"sessionId":"s1"}}`)
	writeFile(t, home+"/.paseo/agents/a/old.json", `{"id":"old","archivedAt":"2026-10-20T00:00:00.000Z",
		"lastActivityAt":"2026-10-20T00:00:00.000Z","lastStatus":"idle"}`)
	writeFile(t, home+"/.paseo/agents/a/internal.json", `{"id":"internal","internal":true,"lastStatus":"idle"}`)
	writeFile(t, home+"/.paseo/agents/a/broken.json", `{not json`)
	// a worktree with no Paseo project: repo comes from the git config
	writeFile(t, home+"/wt/.git/config", "[remote \"origin\"]\n\turl = git@github.com:hackclub/merch.git\n")
	writeFile(t, home+"/wt/sub/x.go", "package x")
	writeFile(t, home+"/.paseo/agents/b/loose.json", `{"id":"loose","cwd":"`+home+`/wt/sub","lastStatus":"idle",
		"lastActivityAt":"2026-10-25T19:00:00.000Z"}`)

	agents, err := LoadAgents(home, "host1", 12*time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Agent{}
	for _, a := range agents {
		by[a.ID] = a
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents: %v", len(agents), by)
	}
	a := by["open"]
	if a.Repo != "example/desk" || a.Branch != "feat" || a.Model != "claude-opus-5-5" || a.Status != "running" ||
		a.Labels[labelPrefix+labelStage] != "building" || a.SessionID != "s1" || a.Host != "host1" ||
		!a.LastUserMessageAt.Equal(time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC)) {
		t.Errorf("open = %+v", a)
	}
	if by["loose"].Repo != "hackclub/merch" {
		t.Errorf("loose repo = %q", by["loose"].Repo)
	}
}

func TestLoadAgentsKeepsInterruptedPastWindow(t *testing.T) {
	for _, stage := range []string{"plan", "plan-review", "building", "review"} {
		for _, age := range []time.Duration{13 * time.Hour, 3 * 24 * time.Hour, 15 * 24 * time.Hour} {
			t.Run(fmt.Sprintf("%s/%s", stage, age), func(t *testing.T) {
				home := t.TempDir()
				for _, kind := range []string{"interrupted", "finished", "historical"} {
					labels := map[string]string{"factory.stage": stage, "factory.archive-watch": "1"}
					if kind == "finished" {
						labels["factory.finished"] = "1"
					}
					if kind == "historical" {
						delete(labels, "factory.archive-watch")
					}
					b, err := json.Marshal(map[string]any{"id": kind, "lastStatus": "closed", "archivedAt": iso(t0.Add(-age)), "labels": labels})
					if err != nil {
						t.Fatal(err)
					}
					writeFile(t, filepath.Join(home, ".paseo/agents/a", kind+".json"), string(b))
				}
				agents, err := LoadAgents(home, "host1", 12*time.Hour, t0)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if age < parkedKeep {
					want = 1
				}
				if len(agents) != want {
					t.Fatalf("loaded %v; want %d agents", agents, want)
				}
				if want == 1 && (agents[0].ID != "interrupted" || stuckReason(agents[0]) != stuckArchived) {
					t.Fatalf("lost interrupted work: %+v", agents)
				}
			})
		}
	}
}
