package factory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

const planLog = "[User] Fellow instructions: plan, then build. Keep the public-repo scan.\n" +
	"[Tool] Bash\n" +
	"PLAN: add the widget\nTIER: low"

// planCard is a card in plan review with a readable session, a model and an email thread.
func planCard(t *testing.T, extra ...Agent) (Agent, *Factory, *fakePaseo) {
	a := card("c1", map[string]string{labelStage: "plan-review", labelSource: "email", labelAgent: "opus",
		labelTier: "low", labelBase: "base1", labelTitled: "given", labelZachThread: "zt-1"})
	a.Labels[gmailThreadLabel] = "thread-1"
	a.WorkspaceID = "ws-1"
	f, p := newTestFactory(t, append([]Agent{a}, extra...)...)
	storeLog(t, f, p, a.ID, planLog)
	f.GitOut = gitStub(map[string]string{
		"log --oneline origin/main..HEAD": "abc1234 Add the widget",
		"diff --stat origin/main...HEAD":  " factory/widget.go | 12 ++++++\n 1 file changed",
	})
	return a, f, p
}

func stageRuns(p *fakePaseo) [][]string {
	var out [][]string
	for _, c := range handoffRuns(p) {
		if strings.Contains(strings.Join(c, " "), stageHandoffMarker) {
			out = append(out, c)
		}
	}
	return out
}

func TestPlanApprovedBuildsInAFreshAgent(t *testing.T) {
	a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "plan is sound\nBUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent(a.ID); len(msgs) != 0 {
		t.Fatalf("the old session was resumed: %v", msgs)
	}
	runs := stageRuns(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", p.calls)
	}
	run := runs[0]
	joined := strings.Join(run, " ")
	if runArg(run, "--provider") != ReviewerModel || runArg(run, "--workspace") != "ws-1" || runArg(run, "--title") != a.Title {
		t.Fatalf("not the same model, worktree and title: %v", run)
	}
	for _, want := range []string{
		labelPrefix + labelReplaces + "=c1", labelPrefix + labelStage + "=building", labelPrefix + labelAutoReview + "=code",
		labelPrefix + labelAgent + "=opus", labelPrefix + labelTier + "=low", labelPrefix + labelPlanRounds + "=1",
		labelPrefix + labelSource + "=email", labelPrefix + labelZachThread + "=zt-1", gmailThreadLabel + "=thread-1",
		labelPrefix + labelBase + "=base1", labelPrefix + labelTitled + "=given",
	} {
		if !strings.Contains(joined, "--label "+want) {
			t.Errorf("missing label %s", want)
		}
	}
	if strings.Contains(joined, labelPrefix+labelModelHandoff) {
		t.Error("a stage swap is not a model hand-off")
	}
	prompt := run[len(run)-1]
	for _, want := range []string{
		stageHandoffMarker, "Fellow instructions: plan, then build", "PLAN: add the widget", "abc1234 Add the widget",
		"factory/widget.go | 12", "Plan approved by the plan reviewer", "Build it now, as planned",
		"c1", testSession("c1"), "paseo logs", "pdw readme agent-sessions", "marts_ai_conversations", "pdw search",
		"Do not start over",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if n := strings.Count(prompt, "PLAN: add the widget"); n != 1 {
		t.Errorf("the plan is in the prompt %d times, want once", n)
	}
	s := parseHandoff(p.labels(a.ID)[labelHandoff])
	if s.Phase != "started" || s.Kind != "stage" || s.New != "new-agent-1" {
		t.Fatalf("hand-off state = %+v", s)
	}
	if p.labels(a.ID)[labelStage] != "building" {
		t.Fatalf("old agent labels = %v", p.labels(a.ID))
	}
}

func TestStageHandoffConfirmsThenArchivesOld(t *testing.T) {
	a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	old := withLabels(a, p.labels(a.ID))
	r := card("new-agent-1", map[string]string{labelReplaces: a.ID, labelStage: "building", labelAgent: "opus", labelAutoReview: "code"})
	r.Status = "running"
	f.SetAgents([]Agent{old, r})
	f.reap(t0)
	if p.called("archive", a.ID) {
		t.Fatal("reap archived an unconfirmed predecessor")
	}
	if err := f.tickAgent(old, t0); err != nil || len(p.sent(a.ID)) != 0 {
		t.Fatalf("tickAgent moved the old agent mid-swap: %v", p.calls)
	}
	f.tickHandoffs(t0.Add(time.Minute))
	l := p.labels(a.ID)
	if parseHandoff(l[labelHandoff]).Phase != "confirmed" || l[labelReplacedBy] != "new-agent-1" || !p.called("archive", a.ID) {
		t.Fatalf("not confirmed and archived: %v", l)
	}
	byID := map[string]Agent{old.ID: withLabels(old, l), r.ID: r}
	if resolveCardID(byID, "c1") != "new-agent-1" || !sameCard(byID, "c1", "new-agent-1") {
		t.Fatal("the card does not resolve to its new agent")
	}
	if !lineageHasThread(byID, r, "thread-1") || lineageEmailThread(byID, r) != "thread-1" {
		t.Fatal("the email thread did not follow the card")
	}
}

func TestCodeReworkRunsInAFreshAgentWithFindings(t *testing.T) {
	// the build ran in a fresh agent: its first message is a stage handoff with the plan in it
	b := card("b1", map[string]string{labelStage: "review", labelAgent: "opus", labelTier: "low", labelPlanRounds: "1",
		labelReplaces: "c0"})
	b.WorkspaceID = "ws-1"
	root := card("c0", map[string]string{labelStage: "building", labelReplacedBy: "b1",
		labelHandoff: handoffState{Phase: "confirmed", New: "b1", Choice: "opus", At: t0, Tries: 1, Kind: "stage"}.String()})
	root.ArchivedAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, root, b, reviewer("r1", "b1", "code"))
	storeLog(t, f, p, root.ID, planLog)
	storeLog(t, f, p, b.ID, "[User] "+stageHandoffMarker+" step: build.\n=== Approved plan ===\nPLAN: add the widget\nTIER: low\n"+
		"=== This step ===\nBuild it now, as planned.\n[Tool] Bash\nBuilt the widget; tests pass.")
	storeLog(t, f, p, "r1", "BLOCKING: the widget leaks a file handle\nVERDICT: changes")
	f.GitOut = gitStub(map[string]string{"log --oneline origin/main..HEAD": "abc1234 Add the widget", "diff --stat origin/main...HEAD": ""})
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(b.ID)) != 0 {
		t.Fatalf("code rework did not start a fresh agent: %v", p.calls)
	}
	joined := strings.Join(runs[0], " ")
	if !strings.Contains(joined, labelPrefix+labelReplaces+"=b1") || !strings.Contains(joined, labelPrefix+labelStage+"=building") ||
		!strings.Contains(joined, labelPrefix+labelCodeRounds+"=1") {
		t.Fatalf("labels: %v", runs[0])
	}
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{"Fellow instructions: plan, then build", "PLAN: add the widget", "Built the widget; tests pass.",
		"the widget leaks a file handle", "requested changes to your code", "c0", "b1", testSession("c0"), testSession("b1")} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "=== This step ===\nBuild it now") {
		t.Error("the previous stage's handoff was nested in the new one")
	}
}

func TestPlanReworkAndLandingStayInTheSameSession(t *testing.T) {
	t.Run("plan rework", func(t *testing.T) {
		a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "r1", "BLOCKING: name the widget\nVERDICT: changes")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if len(stageRuns(p)) != 0 || len(p.sent(a.ID)) != 1 {
			t.Fatalf("plan rework should resume the planner: %v", p.calls)
		}
	})
	t.Run("landing", func(t *testing.T) {
		a := card("c1", map[string]string{labelStage: "review", labelAgent: "opus", labelTier: "low", labelPlanRounds: "1"})
		f, p := newTestFactory(t, a, reviewer("r1", "c1", "code"))
		storeLog(t, f, p, a.ID, planLog)
		storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if len(stageRuns(p)) != 0 || len(p.sent(a.ID)) != 1 {
			t.Fatalf("landing should resume the builder: %v", p.calls)
		}
	})
}

func TestZachApprovalBuildsInAFreshAgent(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan", labelTier: "architecture", labelAgent: "opus"})
	f, p := newTestFactory(t, a)
	storeLog(t, f, p, a.ID, planLog+"\n[User] Write a short version for Zach.\nDECISION: approve the widget?")
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(a.ID)) != 0 {
		t.Fatalf("Zach's approval did not start a fresh builder: %v", p.calls)
	}
	joined := strings.Join(runs[0], " ")
	for _, want := range []string{labelPrefix + labelStage + "=building", labelPrefix + labelZachPlan + "=approved", labelPrefix + labelAutoReview + "=code"} {
		if !strings.Contains(joined, "--label "+want) {
			t.Errorf("missing label %s", want)
		}
	}
	prompt := runs[0][len(runs[0])-1]
	if !strings.Contains(prompt, "PLAN: add the widget") || !strings.Contains(prompt, "Zach allowed this build") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if l := p.labels(a.ID); l[labelStage] != "building" || l[labelZachPlan] != "approved" {
		t.Fatalf("old agent labels = %v", l)
	}
}

func TestStageHandoffFallsBackToResume(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Factory, *fakePaseo, *Agent)
	}{
		{"switched off", func(f *Factory, _ *fakePaseo, _ *Agent) { f.cfg.NoStageHandoff = true }},
		{"unknown model", func(f *Factory, _ *fakePaseo, a *Agent) {
			a.Labels[labelPrefix+labelAgent] = ""
			f.SetAgents([]Agent{*a, f.agent("r1")})
		}},
		{"no first message", func(f *Factory, p *fakePaseo, a *Agent) {
			storeLog(t, f, p, a.ID, "PLAN: add the widget\nTIER: low")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
			storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
			tc.setup(f, p, &a)
			if err := f.ApplyVerdict(f.agent("r1")); err != nil {
				t.Fatal(err)
			}
			if len(stageRuns(p)) != 0 {
				t.Fatalf("swapped anyway: %v", p.calls)
			}
			if msgs := p.sent(a.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "Build it now") {
				t.Fatalf("not resumed: %v", msgs)
			}
		})
	}
	t.Run("matched by provider and model", func(t *testing.T) {
		a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
		a.Labels[labelPrefix+labelAgent] = ""
		a.Model = "claude-sonnet-5-5"
		f.SetAgents([]Agent{a, f.agent("r1")})
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if runs := stageRuns(p); len(runs) != 1 || runArg(runs[0], "--provider") != "claude/claude-sonnet-5-5" {
			t.Fatalf("runs = %v", p.calls)
		}
	})
}

func TestStageHandoffRestart(t *testing.T) {
	start := func(t *testing.T) (Agent, *Factory, *fakePaseo) {
		a, f, p := planCard(t, reviewer("r1", "c1", "plan"))
		storeLog(t, f, p, "r1", "BUILD SIDE EFFECTS: none\nTIER: low\nVERDICT: approve")
		p.runErr = fmt.Errorf("paseo run timed out")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		p.runErr = nil
		if s := parseHandoff(p.labels(a.ID)[labelHandoff]); s.Phase != "starting" || s.Kind != "stage" {
			t.Fatalf("state = %+v", s)
		}
		return withLabels(a, p.labels(a.ID)), f, p
	}
	t.Run("a second try sends the same prompt", func(t *testing.T) {
		a, f, p := start(t)
		first := stageRuns(p)[0] // the run that failed
		// a fresh process: the prompt comes from the state dir
		f2 := New(f.cfg, p)
		f2.Now, f2.GitOut, f2.Sleep = f.Now, f.GitOut, f.Sleep
		f2.quiet = f.quiet
		f2.SetAgents([]Agent{a})
		f2.tickHandoffs(t0.Add(3 * time.Minute))
		runs := stageRuns(p)
		if len(runs) != 2 || runs[1][len(runs[1])-1] != first[len(first)-1] {
			t.Fatalf("retry prompt differs or no retry: %v", runs)
		}
	})
	t.Run("out of tries resumes the old agent", func(t *testing.T) {
		a, f, p := start(t)
		s := parseHandoff(a.L(labelHandoff))
		s.Tries = handoffTries
		if err := f.saveHandoff(a.ID, s, nil); err != nil {
			t.Fatal(err)
		}
		a = withLabels(a, p.labels(a.ID))
		f.SetAgents([]Agent{a})
		f.tickHandoffs(t0.Add(3 * time.Minute))
		if msgs := p.sent(a.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "Build it now") || len(stageRuns(p)) != 1 {
			t.Fatalf("not resumed: %v", p.calls)
		}
		if parseHandoff(p.labels(a.ID)[labelHandoff]).Phase != "failed" || p.labels(a.ID)[labelStage] == "zach" {
			t.Fatalf("labels = %v", p.labels(a.ID))
		}
	})
	t.Run("a replacement that never appears resumes the old agent", func(t *testing.T) {
		a, f, p := planCard(t)
		a = withLabels(a, map[string]string{labelStage: "building",
			labelHandoff: handoffState{Phase: "started", New: "new-agent-7", Choice: "opus", At: t0, Tries: 1, Kind: "stage"}.String()})
		f.SetAgents([]Agent{a})
		if err := f.saveStageHandoff(a.ID, stageHandoff{Prompt: "p", Msg: "Build it now, as planned."}); err != nil {
			t.Fatal(err)
		}
		f.tickHandoffs(t0.Add(6 * time.Minute))
		if msgs := p.sent(a.ID); len(msgs) != 1 || !strings.HasPrefix(msgs[0], "Build it now, as planned.") {
			t.Fatalf("not resumed: %v", p.calls)
		}
	})
}

func TestModelHandoffStillAllowedAfterStageSwaps(t *testing.T) {
	root := card("c0", map[string]string{labelStage: "building", labelReplacedBy: "b1",
		labelHandoff: handoffState{Phase: "confirmed", New: "b1", Choice: "opus", At: t0, Tries: 1, Kind: "stage"}.String()})
	root.ArchivedAt = t0.Add(-time.Hour)
	b := card("b1", map[string]string{labelStage: "building", labelAgent: "opus", labelReplaces: "c0"})
	f, _ := newTestFactory(t, root, b)
	if !f.handoffAllowed(f.agent("b1")) {
		t.Fatal("a stage swap used up the card's model hand-off")
	}
	m := card("m1", map[string]string{labelStage: "building", labelAgent: "codex-sol", labelReplaces: "b1", labelModelHandoff: "1"})
	b.Labels[labelPrefix+labelHandoff] = handoffState{Phase: "confirmed", New: "m1", Choice: "codex-sol", At: t0, Tries: 1}.String()
	b.ArchivedAt = t0
	s := card("s1", map[string]string{labelStage: "building", labelAgent: "codex-sol", labelReplaces: "m1"})
	f.SetAgents(applyLineageLabels([]Agent{root, b, m, s}))
	if f.handoffAllowed(f.agent("s1")) {
		t.Fatal("a second model hand-off after a stage swap")
	}
}

func TestStagePromptSkipsEmptyGit(t *testing.T) {
	a, f, _ := planCard(t)
	f.GitOut = func(context.Context, string, ...string) (string, error) { return "", fmt.Errorf("no git") }
	prompt, err := f.stagePrompt(a, mustChoice(t, "opus"), "build", "Build it now, as planned.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "no commits") || !strings.HasPrefix(prompt, stageHandoffMarker) {
		t.Fatalf("prompt:\n%s", prompt)
	}
}

func mustChoice(t *testing.T, id string) AgentChoice {
	t.Helper()
	c, ok := agentByID(id)
	if !ok {
		t.Fatal(id)
	}
	return c
}
