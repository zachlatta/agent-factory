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
	a := card("c1", map[string]string{labelStage: gateCheck, labelSource: "email", labelAgent: "opus",
		labelBase: "base1", labelTitled: "given", labelZachThread: "zt-1"})
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

// landCard is a card in code review whose reviewed commit is recorded, with a readable session.

func TestDroppedDependencyRebuildsInAFreshAgent(t *testing.T) {
	a, f, p := landCard(t)
	a = withLabels(a, map[string]string{labelStage: "landing", labelHeld: "dep1", labelWaitsOn: "dep1"})
	f.SetAgents([]Agent{a})
	if err := f.rebuildWithoutDep(a, "dep1", "dropped"); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(a.ID)) != 0 {
		t.Fatalf("no fresh agent: %v", p.calls)
	}
	r := card("new-agent-1", nil)
	r.Labels = quietRunLabels(runs[0])
	if r.L(labelStage) != "building" || r.L(labelHeld) != "" || r.L(labelWaitsOn) != "" {
		t.Fatalf("labels = %v", r.Labels)
	}
}

func TestParkedCardIsWokenNotReplaced(t *testing.T) {
	a, f, p := landCard(t)
	a = withLabels(a, map[string]string{labelStage: "landing", labelParked: "1"})
	a.ArchivedAt = t0
	p.archived[a.ID] = true
	f.SetAgents([]Agent{a})
	if err := f.sendLanding(a, "Land it now."); err != nil {
		t.Fatal(err)
	}
	if len(stageRuns(p)) != 0 || len(p.sent(a.ID)) != 1 {
		t.Fatalf("a parked card should be woken by a send: %v", p.calls)
	}
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

// landCard is a card in code review whose reviewed commit is recorded, with a readable session.
func landCard(t *testing.T) (Agent, *Factory, *fakePaseo) {
	a := idea("c1", map[string]string{labelStage: gateCheck, labelAgent: "opus", labelRoundsRequest: "1",
		labelRoundsDesign: "2", labelRoundsSignoff: "1", labelReviewed: "rev1", labelZachThread: "zt-1"})
	a.Labels[gmailThreadLabel] = "thread-1"
	f, p := newTestFactory(t, a, reviewer("r1", "c1", gateCheck))
	storeLog(t, f, p, a.ID, planLog+"\n[User] Build it now, as planned.\nBuilt it. Deploy steps: none.")
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "rev1\n", "status --porcelain": "", "diff --no-renames --name-only origin/main...HEAD": "widget.go\n", "log --oneline origin/main..HEAD": "rev1 Add the widget",
		"diff --stat origin/main...HEAD": " widget.go | 3 +++"})
	return a, f, p
}

func TestLandingRunsInAFreshAgentThatPassesTheLandingGate(t *testing.T) {
	a, f, p := landCard(t)
	storeLog(t, f, p, "r1", "Fine.\nALWAYS ASK: none\nTIER: low\nMEETS REQUEST: green\nWORKS: green\nDESIGN: green\nPUBLIC SAFETY: green\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if p.labels(a.ID)[labelStage] != "landing" {
		t.Fatalf("the card did not move to landing: %v", p.labels(a.ID))
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(a.ID)) != 0 {
		t.Fatalf("landing did not start a fresh agent: %v", p.calls)
	}
	run := runs[0]
	prompt := run[len(run)-1]
	for _, want := range []string{"Land it now", "Built it. Deploy steps: none.", "deploy steps and any runbook", "PLAN: add the widget"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// the new agent as Paseo shows it: exactly the labels it was started with
	r := card("new-agent-1", nil)
	r.Labels = quietRunLabels(run)
	r.Status = "running"
	old := withLabels(a, p.labels(a.ID))
	if why := cardLandBlock(r); why != "" {
		t.Fatalf("the fresh lander is not authorized: %s (labels %v)", why, r.Labels)
	}
	for k, want := range map[string]string{labelReviewed: "rev1", labelCodeOK: "rev1",
		labelZachThread: "zt-1", labelStage: "landing"} {
		if r.L(k) != want {
			t.Errorf("%s = %q, want %q", k, r.L(k), want)
		}
	}
	if r.Labels[gmailThreadLabel] != "thread-1" {
		t.Error("the email thread did not follow the lander")
	}
	f.SetAgents([]Agent{old, r})
	f.tickHandoffs(t0.Add(time.Minute))
	if why := cardLandBlock(withLabels(old, p.labels(a.ID))); why == "" {
		t.Fatal("the replaced agent can still land")
	}
	if got := resolveCardID(f.byID(), "c1"); got != "new-agent-1" {
		t.Fatalf("card resolves to %s", got)
	}
}

func TestFailedLandingReturnsInAFreshAgentWithItsCount(t *testing.T) {
	a, f, p := landCard(t)
	a = withLabels(a, map[string]string{labelStage: "landing"})
	f.SetAgents([]Agent{a})
	if err := f.returnToBuild(a, "landing failed", landFailMsg("push rejected"), false); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(a.ID)) != 0 {
		t.Fatalf("no fresh agent: %v", p.calls)
	}
	r := card("new-agent-1", nil)
	r.Labels = quietRunLabels(runs[0])
	if r.L(labelStage) != "building" || r.L(labelAutoReview) != gateCheck || readRecovery(r).Lands != 1 {
		t.Fatalf("labels = %v (lands %d)", r.Labels, readRecovery(r).Lands)
	}
	if !strings.Contains(runs[0][len(runs[0])-1], "push rejected") {
		t.Error("the failure is not in the handoff")
	}
}

func TestWorkLeftAfterLandingRunsInAFreshAgent(t *testing.T) {
	a, f, p := landCard(t)
	a = withLabels(a, map[string]string{labelStage: "landing", labelOrigin: ""})
	f.SetAgents([]Agent{a})
	c, _ := f.card(a.ID)
	if err := f.landed(a, c, "DECISION: none\nOVERVIEW: landed.\nNEEDS FROM ZACH: Nothing.\n\nLanded.\nREMAINING:\n- agent: measure the tokens"); err != nil {
		t.Fatal(err)
	}
	runs := stageRuns(p)
	if len(runs) != 1 || len(p.sent(a.ID)) != 0 {
		t.Fatalf("work left did not start a fresh agent: %v", p.calls)
	}
	r := card("new-agent-1", nil)
	r.Labels = quietRunLabels(runs[0])
	if r.L(labelStage) != "building" || r.L(labelAutoReview) != gateCheck || r.L(labelLandedVerified) != "1" || r.L(labelWorkLeftRounds) != "1" {
		t.Fatalf("labels = %v", r.Labels)
	}
	if !strings.Contains(runs[0][len(runs[0])-1], "measure the tokens") {
		t.Error("the work left is not in the handoff")
	}
}

func TestStageHandoffConfirmsThenArchivesOld(t *testing.T) {
	a, f, p := planCard(t, reviewer("r1", "c1", gateCheck))
	storeLog(t, f, p, "r1", "- fix it\nVERDICT: changes")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	old := withLabels(a, p.labels(a.ID))
	r := card("new-agent-1", map[string]string{labelReplaces: a.ID, labelStage: "building", labelAgent: "opus", labelAutoReview: gateCheck})
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

func TestStageHandoffRestart(t *testing.T) {
	start := func(t *testing.T) (Agent, *Factory, *fakePaseo) {
		a, f, p := planCard(t, reviewer("r1", "c1", gateCheck))
		storeLog(t, f, p, "r1", "- fix it\nVERDICT: changes")
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
		if msgs := p.sent(a.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "sent this back") || len(stageRuns(p)) != 1 {
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
