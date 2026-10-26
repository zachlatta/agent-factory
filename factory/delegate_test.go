package factory

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func delegateOpts() []delegateOption {
	opus, _ := agentByID("opus")
	sol, _ := agentByID("codex-sol")
	muse, _ := agentByID("muse-wonder")
	return []delegateOption{{opus, "80% 7d left"}, {sol, "usage unknown"}, {muse, "55% 5h left"}}
}

func TestDelegateTextCommands(t *testing.T) {
	txt := delegateText(delegateOpts(), false)
	for _, want := range []string{
		"paseo run -d -q --provider claude/claude-opus-5-5 --thinking medium --mode bypassPermissions --title",
		"paseo run -d -q --provider codex/gpt-6.1-sol --thinking medium --mode full-access --title",
		"paseo run -d -q --provider muse/muse-wonder-full-1.0 --mode allowAll --title",
		"80% 7d left", "55% 5h left", "sends this card's work to OpenAI", "sends this card's work to Meta",
		"paseo wait <id> --timeout 100",
		"paseo logs <id> --filter text --tail 1",
		"paseo ls -g -q --label paseo.parent-agent-id=$PASEO_AGENT_ID",
		"paseo archive <id>",
		"Before you end any turn",
		"At most 6 sub-agents",
		"overrides",
		"never in /tmp",
		".factory-scratch/",
		".git/info/exclude",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("delegate text lacks %q:\n%s", want, txt)
		}
	}
	for _, bad := range []string{"run run", "%!", "Gemini 4 Argon"} {
		if strings.Contains(txt, bad) {
			t.Errorf("delegate text has %q:\n%s", bad, txt)
		}
	}
	// every wait fits Claude Code's 120-second shell default
	for _, m := range regexp.MustCompile(`--timeout (\d+)`).FindAllStringSubmatch(txt, -1) {
		if n, _ := strconv.Atoi(m[1]); n >= 120 {
			t.Errorf("wait timeout %d s does not fit a 120 s shell", n)
		}
	}
	if strings.Contains(txt, "read-only") {
		t.Errorf("card text should let sub-agents edit assigned files:\n%s", txt)
	}
}

func TestDelegateTextReviewerIsReadOnly(t *testing.T) {
	txt := delegateText(delegateOpts(), true)
	for _, want := range []string{"read-only", "Edit nothing", "paseo archive <id>", "Before you end any turn"} {
		if !strings.Contains(txt, want) {
			t.Errorf("reviewer delegate text lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "files you assign") {
		t.Errorf("reviewer text lets sub-agents edit:\n%s", txt)
	}
}

func TestDelegateRuleListsAvailableProviders(t *testing.T) {
	f, _ := newTestFactory(t)
	f.provMu.Lock()
	f.prov.status = map[string]string{"claude": "available", "codex": "available", "antigravity": "error", "muse": "unavailable"}
	f.prov.listed = f.Now()
	f.provMu.Unlock()
	f.usageMu.Lock()
	f.usage.byProvider, f.usage.fetched = map[string]AgentUsage{"codex": {Summary: "42% 7d left"}}, f.Now()
	f.usageMu.Unlock()
	txt := f.delegateRule(false)
	if !strings.Contains(txt, "ChatGPT Pro; 42% 7d left") || !strings.Contains(txt, "Claude Max; usage unknown") {
		t.Errorf("usage missing:\n%s", txt)
	}
	if !strings.Contains(txt, "--provider claude/claude-opus-5-5") || !strings.Contains(txt, "--provider codex/gpt-6-astra") {
		t.Errorf("available providers missing:\n%s", txt)
	}
	if strings.Contains(txt, "antigravity/") || strings.Contains(txt, "muse/") {
		t.Errorf("unavailable providers listed:\n%s", txt)
	}
}

func TestDelegateRuleKeepsDefaultWhenUnlisted(t *testing.T) {
	f, _ := newTestFactory(t)
	f.provMu.Lock()
	f.prov.status = map[string]string{}
	f.prov.listed = f.Now()
	f.provMu.Unlock()
	txt := f.delegateRule(true)
	if !strings.Contains(txt, "--provider "+ReviewerModel) || strings.Contains(txt, "codex/") {
		t.Errorf("want only the default agent when Paseo lists nothing:\n%s", txt)
	}
}

func TestBuildBoardFoldsGrandchildren(t *testing.T) {
	parent := card("c1", map[string]string{labelStage: "review"})
	rev := runningReviewer("r1", "c1", time.Minute, "")
	sub := Agent{ID: "s1", Model: "gpt-6.1-sol", Provider: "codex", Status: "running", CreatedAt: t0, LastActivityAt: t0.Add(-time.Minute),
		Pending: "Bash: rm /tmp/x", Labels: map[string]string{"paseo.parent-agent-id": "r1"}}
	cards := BuildBoard([]Agent{parent, rev, sub}, nil, t0)
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want only c1", cards)
	}
	c := cards[0]
	if len(c.Children) != 2 || c.Children[1].ID != "s1" || c.Children[1].Role != "subagent" || c.Children[1].Model != "gpt-6.1-sol" {
		t.Fatalf("children = %+v", c.Children)
	}
	if !contains(c.Flags, "subagent waiting on permission") {
		t.Errorf("flags = %q", c.Flags)
	}
}

func TestChildModelFallsBackToProvider(t *testing.T) {
	parent := card("c1", map[string]string{labelStage: "building"})
	sub := Agent{ID: "s1", Provider: "muse", Status: "idle", Labels: map[string]string{"paseo.parent-agent-id": "c1"}}
	if c := BuildBoard([]Agent{parent, sub}, nil, t0)[0]; c.Children[0].Model != "muse" {
		t.Errorf("children = %+v", c.Children)
	}
}

func TestActiveSubagentKeepsCardFromStalling(t *testing.T) {
	quiet := card("c1", map[string]string{labelStage: "building"})
	quiet.Status, quiet.LastActivityAt = "running", t0.Add(-30*time.Minute)
	busy := Agent{ID: "s1", Status: "running", LastActivityAt: t0.Add(-time.Minute), Labels: map[string]string{"paseo.parent-agent-id": "c1"}}
	c := BuildBoard([]Agent{quiet, busy}, nil, t0)[0]
	if c.Stage != "building" || strings.Contains(strings.Join(c.Flags, "|"), "stalled") {
		t.Errorf("card with a working sub-agent: stage %q flags %q", c.Stage, c.Flags)
	}
	// once its sub-agent is quiet too, the card is stalled
	busy.LastActivityAt = t0.Add(-25 * time.Minute)
	c = BuildBoard([]Agent{quiet, busy}, nil, t0)[0]
	if c.Stage != "zach" || !contains(c.Flags, "stalled 30 min") {
		t.Errorf("all quiet: stage %q flags %q", c.Stage, c.Flags)
	}
}

func TestStoppedCardFlagsRunningSubagent(t *testing.T) {
	idle := card("c1", map[string]string{labelStage: "review"})
	sub := Agent{ID: "s1", Status: "running", LastActivityAt: t0.Add(-time.Minute), Labels: map[string]string{"paseo.parent-agent-id": "c1"}}
	if c := BuildBoard([]Agent{idle, sub}, nil, t0)[0]; !contains(c.Flags, subagentRunningFlag) {
		t.Errorf("flags = %q", c.Flags)
	}
	// while the card agent runs, a running sub-agent is normal
	running := idle
	running.Status, running.LastActivityAt = "running", t0
	if c := BuildBoard([]Agent{running, sub}, nil, t0)[0]; contains(c.Flags, subagentRunningFlag) {
		t.Errorf("flags = %q", c.Flags)
	}
	// an archived sub-agent is not running
	sub.ArchivedAt = t0
	if c := BuildBoard([]Agent{idle, sub}, nil, t0)[0]; contains(c.Flags, subagentRunningFlag) {
		t.Errorf("flags = %q", c.Flags)
	}
}

func TestReviewerWaitingOnSubagentIsNotBlocked(t *testing.T) {
	r := runningReviewer("r1", "c1", 25*time.Minute, "")
	sub := Agent{ID: "s1", Status: "running", LastActivityAt: t0.Add(-2 * time.Minute), Labels: map[string]string{"paseo.parent-agent-id": "r1"}}
	agents := []Agent{r, sub}
	if reviewerBlocked(agents, r, t0) || !reviewPending(agents, "c1", t0) {
		t.Error("a reviewer whose sub-agent works should still count as pending")
	}
	if !reviewerBlocked([]Agent{r}, r, t0) {
		t.Error("a quiet reviewer with no sub-agent is blocked")
	}
	// the sub-agent's own sub-agent counts too
	sub.LastActivityAt = t0.Add(-25 * time.Minute)
	deep := Agent{ID: "s2", Status: "running", LastActivityAt: t0, Labels: map[string]string{"paseo.parent-agent-id": "s1"}}
	if reviewerBlocked([]Agent{r, sub, deep}, r, t0) {
		t.Error("a nested working sub-agent should keep the reviewer pending")
	}
}

func TestHealthCountsGrandchildrenOnTheirCard(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review"})
	rev := runningReviewer("r1", "c1", time.Minute, "")
	sub := Agent{ID: "s1", Status: "running", LastActivityAt: t0, Labels: map[string]string{"paseo.parent-agent-id": "r1"}}
	f, _ := newTestFactory(t, c, rev, sub)
	n := f.agentCounts()
	if len(n.Cards) != 1 || n.Cards[0].ID != "c1" || n.Cards[0].Live != 3 {
		t.Errorf("cards = %+v", n.Cards)
	}
}

func TestPromptsOfferSubagents(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building"}))
	if _, err := f.Act("new", Body{Request: "fix a typo", Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	c, _ := f.card("c1")
	if _, err := f.StartReview(c, "code"); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, call := range p.calls {
		if call[0] == "run" {
			runs = append(runs, call[len(call)-1])
		}
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	fellow, rev := runs[0], runs[1]
	if !strings.Contains(fellow, "Sub-agents. Zach allows") || !strings.Contains(fellow, "You are a sub-agent of a factory card") {
		t.Errorf("Fellow prompt lacks the sub-agent section")
	}
	if !strings.Contains(rev, "Sub-agents. Zach allows") || !strings.Contains(rev, "You are a read-only sub-agent") ||
		strings.Contains(rev, "You are a sub-agent of a factory card") {
		t.Errorf("review prompt lacks the read-only sub-agent section")
	}
	if strings.Contains(fellow, "%!") || strings.Contains(rev, "%!") {
		t.Errorf("formatting accident in a prompt")
	}
}
