package factory

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// runningReviewer is a reviewer of card of, running, last active idle ago, with pending permission perm.
func runningReviewer(id, of string, idle time.Duration, perm string) Agent {
	r := reviewer(id, of, "code")
	r.Status, r.LastActivityAt, r.Pending = "running", t0.Add(-idle), perm
	return r
}

func TestBuildBoardFlagsBlockedChildren(t *testing.T) {
	parent := card("c1", map[string]string{labelStage: "review"})
	sub := Agent{ID: "s1", Status: "running", LastActivityAt: t0.Add(-time.Minute), Pending: "Bash: rm /tmp/x",
		Labels: map[string]string{"paseo.parent-agent-id": "c1"}}
	for _, tc := range []struct {
		name    string
		kids    []Agent
		flag    string
		blocked string
	}{
		{"reviewer on a permission prompt", []Agent{runningReviewer("r1", "c1", time.Minute, "Bash: cd /w; rm /tmp/rv.log")},
			"reviewer waiting on permission", "waiting on permission"},
		{"reviewer stalled", []Agent{runningReviewer("r1", "c1", 25*time.Minute, "")}, "reviewer stalled 25 min", "stalled 25 min"},
		{"reviewer working", []Agent{runningReviewer("r1", "c1", time.Minute, "")}, "", ""},
		{"reviewer finished long ago", []Agent{func() Agent { r := reviewer("r1", "c1", "code"); r.LastActivityAt = t0.Add(-time.Hour); return r }()}, "", ""},
		{"archived reviewer with a stale prompt", []Agent{func() Agent {
			r := runningReviewer("r1", "c1", time.Hour, "Bash: rm x")
			r.ArchivedAt = t0
			return r
		}()}, "", ""},
		{"sub-agent on a permission prompt", []Agent{sub}, "subagent waiting on permission", "waiting on permission"},
	} {
		c := BuildBoard(append([]Agent{parent}, tc.kids...), nil, t0)[0]
		if got := strings.Join(c.Flags, "|"); got != tc.flag {
			t.Errorf("%s: flags = %q, want %q", tc.name, got, tc.flag)
		}
		if len(c.Children) != 1 || c.Children[0].Blocked != tc.blocked {
			t.Errorf("%s: children = %+v", tc.name, c.Children)
		}
		if tc.kids[0].Pending != "" && tc.blocked != "" && c.Children[0].Permission != tc.kids[0].Pending {
			t.Errorf("%s: child permission = %q", tc.name, c.Children[0].Permission)
		}
	}
}

func TestTopLevelPermissionShowsInNeedsZach(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "building"})
	a.Status, a.Pending = "running", "Bash: rm /tmp/x"
	c := BuildBoard([]Agent{a}, nil, t0)[0]
	if c.Stage != "zach" || !contains(c.Flags, "waiting on permission") || c.Ask == nil ||
		c.Ask.Title != "Waiting on a permission prompt" || actionIDs(c.Ask) != "stop,reply" || !strings.Contains(c.Ask.Why, "rm /tmp/x") {
		t.Errorf("card = %+v ask = %+v", c, c.Ask)
	}
	// Paseo's own attention flag for the same prompt: one permission flag, not two
	a.RequiresAttention, a.AttentionReason = true, "permission"
	c = BuildBoard([]Agent{a}, nil, t0)[0]
	if got := strings.Join(c.Flags, "|"); got != "needs attention: permission" {
		t.Errorf("flags = %q", got)
	}
}

func TestReviewPendingSkipsBlockedReviewers(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Agent
		want bool
	}{
		{"working", runningReviewer("r1", "c1", time.Minute, ""), true},
		{"quiet but under the stall limit", runningReviewer("r1", "c1", 19*time.Minute, ""), true},
		{"stalled", runningReviewer("r1", "c1", 21*time.Minute, ""), false},
		{"permission prompt, just raised", runningReviewer("r1", "c1", 30*time.Second, "Bash: rm x"), true},
		{"permission prompt for 3 min", runningReviewer("r1", "c1", 3*time.Minute, "Bash: rm x"), false},
		{"Paseo's permission attention flag", func() Agent {
			r := runningReviewer("r1", "c1", 3*time.Minute, "")
			r.RequiresAttention, r.AttentionReason = true, "permission"
			return r
		}(), false},
	} {
		if got := reviewPending([]Agent{tc.r}, "c1", t0); got != tc.want {
			t.Errorf("%s: reviewPending = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBlockedReviewerIsReplacedNotSentToZach(t *testing.T) {
	for _, tc := range []struct {
		name     string
		r        Agent
		replaced bool
	}{
		{"permission prompt", runningReviewer("r1", "c1", 10*time.Minute, "Bash: cd /w; rm /tmp/rv.log"), true},
		{"stalled", runningReviewer("r1", "c1", 30*time.Minute, ""), true},
		{"working", runningReviewer("r1", "c1", time.Minute, ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "review", labelSince: ago(time.Hour)})
			c.LastActivityAt = t0.Add(-time.Hour)
			f, p := newTestFactory(t, c, tc.r)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] == "zach" {
				t.Fatalf("sent to Needs Zach: %v", l)
			}
			replaced := l[labelReviewerRetries] == "1" && p.called("archive", "r1") &&
				len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) == 1
			if replaced != tc.replaced {
				t.Fatalf("replaced = %v, labels = %v, calls %v", replaced, l, p.calls)
			}
			if tc.replaced && p.labels("r1")[labelVerdict] != "superseded" {
				t.Errorf("blocked reviewer not superseded: %v", p.labels("r1"))
			}
		})
	}
}

func TestRerunSupersedesOldReviewers(t *testing.T) {
	for _, status := range []string{"idle", "running"} {
		c := card("c1", map[string]string{labelStage: "review", labelStuck: stuckReview})
		c.Status = status
		old := runningReviewer("r1", "c1", time.Hour, "Bash: rm x")
		done := reviewer("r0", "c1", "code")
		done.Labels[labelPrefix+labelApplied] = "1"
		f, p := newTestFactory(t, c, old, done)
		p.busy["r1"] = 1 // Paseo refuses to archive the running reviewer until it is stopped
		if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "superseded" {
			t.Errorf("%s: old reviewer labels = %v", status, l)
		}
		// marked applied before it is stopped, so no Tick can apply its verdict in between
		if v := strings.Join(p.verbs("r1"), ","); v != "update,inspect,archive,stop,inspect,archive,inspect" {
			t.Errorf("%s: old reviewer calls = %s", status, v)
		}
		if len(p.labels("r0")) != 0 || p.called("stop", "r0") || p.called("archive", "r0") {
			t.Errorf("%s: applied reviewer touched: %v", status, p.verbs("r0"))
		}
		if status == "idle" && len(p.runsWithLabel(labelArg(labelReviewOf, "c1"))) != 1 {
			t.Errorf("no fresh review: %v", p.calls)
		}
		if status == "running" && p.labels("c1")[labelAutoReview] != "code" {
			t.Errorf("review not queued: %v", p.labels("c1"))
		}
	}
}

func TestRefreshReadsPendingPermissions(t *testing.T) {
	f, p := newTestFactory(t)
	writeFile(t, f.cfg.Home+"/.paseo/agents/a/r1.json", `{"id":"r1","lastStatus":"running",
		"createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z"}`)
	writeFile(t, f.cfg.Home+"/.paseo/agents/a/r2.json", `{"id":"r2","lastStatus":"running",
		"createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z"}`)
	p.permits = `[{"id":"q1","agentId":"r1","agentShortId":"r1","name":"Bash","description":"cd /w; rm /tmp/rv.log"},
		{"id":"q2","agentId":"r1","agentShortId":"r1","name":"Edit","description":"-"}]`
	f.Refresh()
	if a := f.agent("r1"); a.Pending != "Bash: cd /w; rm /tmp/rv.log" {
		t.Errorf("r1 pending = %q", a.Pending)
	}
	if a := f.agent("r2"); a.Pending != "" {
		t.Errorf("r2 pending = %q", a.Pending)
	}
	// a failed read leaves the agents without prompts; the poll itself still succeeds
	p.permitErr = errors.New("daemon down")
	f.Now = func() time.Time { return t0.Add(permitEvery) }
	f.Refresh()
	if a, ok := f.agentOK("r1"); !ok || a.Pending != "" {
		t.Errorf("after a failed read: %+v %v", a, ok)
	}
	if h := f.errs[f.cfg.Host]; h != "" {
		t.Errorf("refresh error = %q", h)
	}
}

// permit ls runs at most every permitEvery: Refreshes in between keep the prompts the last read
// found, unless an agent's file flags a permission wait that read did not see.
func TestPermitReadsAreRateLimited(t *testing.T) {
	f, p := newTestFactory(t)
	writeFile(t, f.cfg.Home+"/.paseo/agents/a/r1.json", `{"id":"r1","lastStatus":"running",
		"createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z"}`)
	p.permits = `[{"agentId":"r1","name":"Bash","description":"ls"}]`
	f.Refresh()
	f.Refresh()
	if err := f.reload(); err != nil {
		t.Fatal(err)
	}
	if n := p.countCalls("permit", "ls"); n != 1 {
		t.Errorf("permit ls called %d times within %v, want 1", n, permitEvery)
	}
	if a := f.agent("r1"); a.Pending != "Bash: ls" {
		t.Errorf("r1 pending = %q, want the last read's prompt", a.Pending)
	}

	// a file that flags a permission wait the last read did not see forces a read
	writeFile(t, f.cfg.Home+"/.paseo/agents/a/r2.json", `{"id":"r2","lastStatus":"running","requiresAttention":true,
		"attentionReason":"permission","createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z"}`)
	p.permits = `[{"agentId":"r1","name":"Bash","description":"ls"},{"agentId":"r2","name":"Edit","description":"x"}]`
	f.Refresh()
	if n := p.countCalls("permit", "ls"); n != 2 {
		t.Errorf("permit ls called %d times after a flagged file, want 2", n)
	}
	if a := f.agent("r2"); a.Pending != "Edit: x" {
		t.Errorf("r2 pending = %q", a.Pending)
	}
	f.Refresh() // r2's prompt is known now: no new read
	if n := p.countCalls("permit", "ls"); n != 2 {
		t.Errorf("permit ls called %d times for a known prompt, want 2", n)
	}

	f.Now = func() time.Time { return t0.Add(permitEvery) }
	f.Refresh()
	if n := p.countCalls("permit", "ls"); n != 3 {
		t.Errorf("permit ls called %d times after %v, want 3", n, permitEvery)
	}
}

func TestRefreshSkipsPermitsWhenNothingRuns(t *testing.T) {
	f, p := newTestFactory(t)
	writeFile(t, f.cfg.Home+"/.paseo/agents/a/c1.json", `{"id":"c1","lastStatus":"idle",
		"createdAt":"2026-10-25T19:00:00.000Z","lastActivityAt":"2026-10-25T19:59:00.000Z"}`)
	f.Refresh()
	if n := p.countCalls("permit", "ls"); n != 0 {
		t.Errorf("permit ls called %d times with nothing running", n)
	}
}

func TestPromptsCarryRmRule(t *testing.T) {
	want := "no cd earlier in the same command"
	c := Card{ID: "c1", Title: "x", Host: "host1", Cwd: "/w", Repo: "r", Tier: "low"}
	for _, kind := range []string{"plan", "code", "land"} {
		if !strings.Contains(ReviewPrompt(c, kind, 7478, "unknown"), want) {
			t.Errorf("%s review prompt lacks the rm rule", kind)
		}
	}
	if !strings.Contains(newCardPrompt, want) {
		t.Error("Fellow prompt lacks the rm rule")
	}
	if !strings.Contains(buildMsg("Plan approved.", "none"), want) {
		t.Error("build message lacks the rm rule")
	}
	for name, rule := range map[string]string{"shellRule": shellRule, "scratchSafetyRule": scratchSafetyRule, "scratchCleanupRule": scratchCleanupRule, "worktreeStorageRule": worktreeStorageRule, "worktreeRule": worktreeRule} {
		if strings.Contains(rule, "%") {
			t.Errorf("%s is formatted into newCardPrompt: no %% allowed", name)
		}
	}
}
