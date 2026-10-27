package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPostLandingReplyRouting(t *testing.T) {
	for _, via := range []string{"board", "email"} {
		for _, note := range []string{"landing not verified", "landed; did not say what remains", "landed; still to do: finish"} {
			t.Run(via+note, func(t *testing.T) {
				c := card("c1", map[string]string{labelStage: "zach", labelNote: note, labelPlanRounds: "1", labelAutoReview: "code"})
				f, p := newTestFactory(t, c)
				if _, err := f.Act("send", Body{ID: c.ID, Message: "continue", Via: via}); err != nil {
					t.Fatal(err)
				}
				l := p.labels(c.ID)
				landing := !strings.HasPrefix(note, "landed; still to do: ")
				if landing {
					if l[labelStage] != "landing" || l[labelAutoReview] != "" {
						t.Fatalf("labels: %v", l)
					}
					if !strings.Contains(strings.Join(p.sent(c.ID), ""), remainingRule) {
						t.Fatal("missing rule")
					}
				} else if l[labelStage] != "building" || l[labelAutoReview] != "code" {
					t.Fatalf("labels: %v", l)
				}
			})
		}
	}
}

func TestPostLandingEmptyReview(t *testing.T) {
	for _, tc := range []struct {
		name, commits, reply string
		verified             bool
		gitErr               bool
		stage                string
	}{
		{"empty done", "", "REMAINING: none", true, false, "landed"},
		{"empty missing", "", "Finished", true, false, "landing"},
		{"empty items", "", "REMAINING: Zach: approve", true, false, "zach"},
		{"new commits", "new\n", "REMAINING: none", true, false, "review"},
		{"git failure", "", "REMAINING: none", true, true, "review"},
		{"unverified", "", "REMAINING: none", false, false, "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelPlanRounds: "1",
				labelNote: "landed; still to do: finish", labelRemainingChecked: "1"})
			if tc.verified {
				c.Labels[labelPrefix+labelLandedVerified] = "1"
			}
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			writeSession(t, f.cfg.Home, c.Cwd, c.SessionID, sessionLine("continue", false), postLandingAnswer(tc.reply))
			f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "origin/main..HEAD") {
					if tc.gitErr {
						return "", fmt.Errorf("git failed")
					}
					return tc.commits, nil
				}
				return "", nil
			}
			f.Tick()
			if l := p.labels(c.ID); l[labelStage] != tc.stage {
				t.Fatalf("labels: %v", l)
			}
			if tc.stage == "landing" {
				l := p.labels(c.ID)
				if l[labelRemainingAsk] != "1" || l[labelRemainingChecked] != "" || l[labelAutoReview] != "" {
					t.Fatalf("completion not queued cleanly: %v", l)
				}
			}
			if tc.stage != "review" && p.runs != 0 {
				t.Fatal("empty review started")
			}
		})
	}
}

func TestRemainingClarificationLifecycle(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	storeLog(t, f, p, c.ID, "Finished")
	f.Tick()
	if l := p.labels(c.ID); l[labelStage] != "landing" || l[labelRemainingAsk] != "1" {
		t.Fatalf("first omission: %v", l)
	}
	sent := len(p.sent(c.ID))
	// Refresh labels and time as the agent answers each prompt.
	advance := func() {
		for k, v := range p.labels(c.ID) {
			c.Labels[labelPrefix+k] = v
		}
		now := f.Now().Add(2 * time.Minute)
		c.LastActivityAt = now.Add(-time.Minute)
		f.Now = func() time.Time { return now }
		f.SetAgents([]Agent{c})
	}
	for k, v := range p.labels(c.ID) {
		c.Labels[labelPrefix+k] = v
	}
	f.SetAgents([]Agent{c})
	f.Tick()
	if len(p.sent(c.ID)) != sent {
		t.Fatal("repeated question without answer")
	}
	advance()
	f.Tick()
	if l := p.labels(c.ID); l[labelStage] != "zach" {
		t.Fatalf("second omission: %v", l)
	}
	c.Labels[labelPrefix+labelStage] = "landing"
	storeLog(t, f, p, c.ID, "REMAINING: none")
	advance()
	c.Labels[labelPrefix+labelStage] = "landing"
	f.SetAgents([]Agent{c})
	f.Tick()
	if l := p.labels(c.ID); l[labelRemainingAsk] != "" || l[labelStage] != "landed" {
		t.Fatalf("valid result: %v", l)
	}
	// A later landing gets its own clarification allowance.
	advance()
	c.Labels[labelPrefix+labelStage] = "landing"
	f.SetAgents([]Agent{c})
	storeLog(t, f, p, c.ID, "Finished again")
	f.Tick()
	if l := p.labels(c.ID); l[labelStage] != "landing" || l[labelRemainingAsk] != "1" {
		t.Fatalf("second landing: %v", l)
	}
}

func postLandingAnswer(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}})
	return b
}

func TestNewLandingPromptsResetClarification(t *testing.T) {
	for _, route := range []string{"approve", "rerun", "code review", "dependency"} {
		t.Run(route, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "zach", labelPlanRounds: "1", labelTier: "low", labelStuckFrom: "landing", labelRemainingAsk: "1", labelRemainingChecked: "1"})
			agents := []Agent{c, reviewer("r1", c.ID, "code")}
			if route == "dependency" {
				c = heldCard(map[string]string{labelRemainingAsk: "1", labelRemainingChecked: "1"})
				agents = []Agent{c, dep("landed", false)}
			}
			f, p := newTestFactory(t, agents...)
			var err error
			switch route {
			case "approve", "rerun":
				_, err = f.Act(route, Body{ID: c.ID})
			case "code review":
				storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
				err = f.ApplyVerdict(f.agent("r1"))
			case "dependency":
				err = f.tickHeld(c, Card{ID: c.ID, Repo: c.Repo, Cwd: c.Cwd})
			}
			if err != nil {
				t.Fatal(err)
			}
			l := p.labels(c.ID)
			if len(p.sent(c.ID)) != 1 {
				t.Fatalf("no landing prompt: %v", p.sent(c.ID))
			}
			for _, key := range []string{labelRemainingAsk, labelRemainingChecked} {
				if val, ok := l[key]; !ok || val != "" {
					t.Fatalf("%s not reset: %v", key, l)
				}
			}
		})
	}
}
