package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestSubmittedByTagsEveryCard(t *testing.T) {
	idea := card("c", map[string]string{labelOrigin: "idea"})
	scout := card("c", map[string]string{labelRole: "scout"})
	sched := card("c", nil)
	sched.Labels["paseo.schedule-id"] = "s1"
	for name, c := range map[string]struct {
		a    Agent
		want string
	}{"untagged board": {card("c", map[string]string{labelSource: "board"}), submittedBySystem}, "tagged board": {card("c", map[string]string{labelSource: "board", labelSubmittedBy: submittedByZach}), submittedByZach}, "email": {card("c", map[string]string{labelSource: "email"}), submittedByZach},
		"voice": {card("c", map[string]string{labelSource: "voice"}), submittedByZach}, "idea": {idea, submittedBySystem},
		"scout": {scout, submittedBySystem}, "schedule": {sched, submittedBySystem}} {
		if got := submittedBy(c.a); got != c.want {
			t.Errorf("%s: submitted by %q, want %q", name, got, c.want)
		}
	}
	tagged := card("c", map[string]string{labelSource: "board", labelSubmittedBy: submittedBySystem})
	if got := submittedBy(tagged); got != submittedBySystem {
		t.Errorf("a tag wins over the source: got %q", got)
	}
}

func TestBuilderStopStartsTheRequestGate(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSource: "board",
		labelAutoReview: gateRequest, labelSince: ago(2 * time.Minute)}))
	storeLog(t, f, p, "c1", "[Assistant] Built it.\nRESULT: code\nTITLE: Built it")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != gateRequest {
		t.Fatalf("stage = %q, want %q", got, gateRequest)
	}
	if !strings.Contains(p.promptOfKind(gateRequest), "unsaid context") {
		t.Errorf("request gate prompt = %q", trunc(p.promptOfKind(gateRequest), 300))
	}
}

func TestGatesRunInOrder(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateRequest, labelSource: "board"}),
		reviewer("r1", "c1", gateRequest))
	storeLog(t, f, p, "r1", "[Assistant] It does what he asked.\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[Assistant] Built.\nRESULT: code")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != gateDesign {
		t.Fatalf("after the request gate, stage = %q", got)
	}
	design := p.promptOfKind(gateDesign)
	if !strings.Contains(design, "commit") || strings.Contains(design, "Read-only.") {
		t.Errorf("the design reviewer does small refactors itself: %q", trunc(design, 300))
	}
}

func TestDesignGatePassMovesToSignOff(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board"}),
		reviewer("r1", "c1", gateDesign))
	storeLog(t, f, p, "r1", "[Assistant] Cohesive.\nVERDICT: nits")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != gateSignoff {
		t.Fatalf("stage = %q, want %q", got, gateSignoff)
	}
}

func TestSignOffNoneGoesToLanding(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateSignoff, labelSource: "board", labelReviewed: "abc", labelAgent: "codex-sol"}),
		reviewer("r1", "c1", gateSignoff))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", "[Assistant] It is what he asked for.\nSIGN OFF: none\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "landing" {
		t.Fatalf("stage = %q, note %q", got, p.labels("c1")[labelNote])
	}
}

func TestSignOffReasonComesToZach(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateSignoff, labelSource: "idea", labelReviewed: "abc"}),
		reviewer("r1", "c1", gateSignoff))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", "[Assistant] System submitted.\nSIGN OFF: the system submitted it\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.Contains(l[labelAskWhy], "the system submitted it") {
		t.Fatalf("stage %q why %q", l[labelStage], l[labelAskWhy])
	}
}

func TestMissingSignOffLineAsksZach(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateSignoff, labelSource: "board", labelReviewed: "abc"}),
		reviewer("r1", "c1", gateSignoff))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", "[Assistant] Fine.\nVERDICT: approve")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "zach" {
		t.Fatalf("a missing line must not land: stage %q", got)
	}
}

func TestSignOffPromptNamesWhoSubmitted(t *testing.T) {
	c := Card{ID: "c1", Title: "t", Repo: "o/r", SubmittedBy: submittedBySystem}
	got := ReviewPrompt(c, gateSignoff, 7478, "private")
	for _, want := range []string{"Submitted by: system", "scheduled run", "SIGN OFF:"} {
		if !strings.Contains(got, want) {
			t.Errorf("sign-off prompt misses %q", want)
		}
	}
	if strings.Contains(got, "TIER:") || strings.Contains(got, "arbiter") {
		t.Errorf("the flow has no tiers or arbiters: %q", got)
	}
}

func TestGateFailureGoesBackToBuildAndThatGateRunsAgain(t *testing.T) {
	for _, gate := range []string{gateRequest, gateDesign} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gate, labelSource: "board"}),
			reviewer("r1", "c1", gate))
		storeLog(t, f, p, "r1", "[Assistant] Does not hold.\nVERDICT: changes")
		storeLog(t, f, p, "c1", "[Assistant] Built.")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] != "building" || l[labelAutoReview] != gate {
			t.Errorf("%s: stage %q auto-review %q", gate, l[labelStage], l[labelAutoReview])
		}
	}
}

func TestReworkCapMarksTheCardStuck(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board", roundsLabel(gateDesign): "2"}),
		reviewer("r1", "c1", gateDesign))
	storeLog(t, f, p, "r1", "[Assistant] Still wrong.\nVERDICT: changes")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStuck] == "" {
		t.Fatalf("labels %v", l)
	}
}

func TestLandingRunsOnOpusAndABuilderReturnRestoresItsModel(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelAgent: "codex-sol"})
	if got := landingChoice(); got.ID != defaultAgent || got.Thinking != "medium" {
		t.Errorf("landing choice = %+v", got)
	}
	if got := builderChoiceLabel(c); got != "codex-sol" {
		t.Errorf("first landing keeps the builder's model on record: %q", got)
	}
	c = card("c1", map[string]string{labelAgent: "opus", labelBuilderAgent: "codex-sol"})
	got, ok := stageChoice(c, "building")
	if !ok || got.ID != "codex-sol" {
		t.Errorf("a return to build runs the builder's model: %+v", got)
	}
	got, ok = stageChoice(c, "landing")
	if !ok || got.ID != "opus" {
		t.Errorf("landing runs Opus: %+v", got)
	}
}

func TestLanderSendsBigRunbookDriftBackToBuild(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "landing", labelSource: "board",
		labelSince: ago(2 * time.Minute), labelResult: "runbook"}))
	storeLog(t, f, p, "c1", "[Assistant] The host moved.\nRETURN TO BUILD: the service moved to another host")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != gateRequest {
		t.Fatalf("labels %v", l)
	}
}

func TestRunbookOnlyCardFinishesWithoutCommits(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "landing", labelSource: "board",
		labelSince: ago(2 * time.Minute), labelResult: "runbook", labelReviewed: "abc", labelBase: "abc"}))
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "abc\n", "rev-list origin/main..HEAD": ""})
	storeLog(t, f, p, "c1", "[Assistant] Ran every step and verified it.\nREMAINING: none")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "landed" {
		t.Fatalf("stage = %q, note %q", got, p.labels("c1")[labelNote])
	}
}

func TestParseResultAndReturnToBuild(t *testing.T) {
	for text, want := range map[string]string{"x\nRESULT: code": "code", "RESULT: **runbook**": "runbook", "RESULT: both\n": "both", "RESULT: <code|runbook|both>": "", "none": ""} {
		if got := ParseResult(text); got != want {
			t.Errorf("ParseResult(%q) = %q, want %q", text, got, want)
		}
	}
	if got := ParseReturnToBuild("RETURN TO BUILD: <why>\nRETURN TO BUILD: **host moved**"); got != "host moved" {
		t.Errorf("ParseReturnToBuild = %q", got)
	}
}

func TestPlanCapacityWeightsLeaveHeadroom(t *testing.T) {
	week := func(pct int) AgentUsage {
		return AgentUsage{Windows: []UsageWindow{{Label: "7d", RemainingPct: pct, ResetsAt: t0.Add(84 * time.Hour).Format(time.RFC3339Nano)}}}
	}
	statuses := map[string]string{"claude": "available", "codex": "available", "muse": "available", "antigravity": "available"}
	// 90% of Claude counts as 72%, 80% of Codex as 64%: an honest 70% Gemini beats Codex
	_, got := rankAgentChoices(statuses, map[string]AgentUsage{"claude": week(90), "codex": week(80), "antigravity": week(70), "muse": week(0)}, t0)
	if got != "opus" {
		t.Errorf("best = %q, want opus", got)
	}
	ordered, _ := rankAgentChoices(statuses, map[string]AgentUsage{"claude": week(80), "codex": week(80), "antigravity": week(70), "muse": week(0)}, t0)
	if ordered[0].ID != "gemini-argon" {
		t.Errorf("first = %q, want gemini-argon (70%% beats 64%%)", ordered[0].ID)
	}
}

func TestSuggestedBuilderIsAlwaysFrontier(t *testing.T) {
	statuses := map[string]string{"muse": "available", "claude": "available"}
	week := AgentUsage{Windows: []UsageWindow{{Label: "7d", RemainingPct: 100}}}
	low := AgentUsage{Windows: []UsageWindow{{Label: "7d", RemainingPct: 10}}}
	_, got := rankAgentChoices(statuses, map[string]AgentUsage{"muse": week, "claude": low}, t0)
	if c, _ := agentByID(got); !c.Frontier {
		t.Errorf("suggested %q is not a Frontier model", got)
	}
}

func TestOldFlowCardsMoveToTheNewFlow(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelSource: "board", labelAutoReview: "", labelReviewed: "abc"}),
		card("c2", map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)}),
		reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "c2", "[Assistant] Plan.\nTIER: low")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	storeLog(t, f, p, "r1", "[Assistant] Old verdict.\nVERDICT: approve")
	for _, id := range []string{"c1", "c2"} {
		if err := f.tickAgent(f.agent(id), t0); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.labels("c1")[labelAutoReview]; got != gateRequest {
		t.Errorf("a card under old review gets the new gates: auto-review %q", got)
	}
	if got := p.labels("c2")[labelStage]; got != "building" {
		t.Errorf("a planned card builds: stage %q", got)
	}
}

// passSignoffForTest calls passSignoff with the arguments older tests name.
func (f *Factory) passSignoffForTest(t Agent, labels map[string]string, verdict, _, notes, _ string) error {
	return f.passSignoff(t, labels, verdict, notes)
}

func signoffNone(s string) string { return s + "\nSIGN OFF: none" }

func TestLandingSwapStartsOpusAndKeepsTheBuilderOnRecord(t *testing.T) {
	a := card("c1", map[string]string{labelStage: gateSignoff, labelSource: "board", labelReviewed: "abc", labelAgent: "codex-sol"})
	a.WorkspaceID = "ws-1"
	f, p := newTestFactory(t, a, reviewer("r1", "c1", gateSignoff))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "c1", "[User] Zach's request:\nDo it\n[Assistant] Built.\nRESULT: code")
	storeLog(t, f, p, "r1", "[Assistant] Fine.\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelReplaces, "c1"))
	if len(runs) != 1 {
		t.Fatalf("no fresh lander: %v", p.calls)
	}
	got := strings.Join(runs[0], " ")
	for _, want := range []string{"claude/claude-opus-5-5", "--thinking medium", labelArg(labelAgent, "opus"), labelArg(labelBuilderAgent, "codex-sol")} {
		if !strings.Contains(got, want) {
			t.Errorf("lander run lacks %q: %s", want, got)
		}
	}
}

func TestMissingGateVerdictReplacesTheReviewerThenMarksStuck(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board"}), reviewer("r1", "c1", gateDesign))
	storeLog(t, f, p, "r1", "[Assistant] no verdict line")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelReviewKind, gateDesign))) != 1 || p.labels("c1")[labelReviewerRetries] != "1" {
		t.Fatalf("not replaced: %v", p.labels("c1"))
	}
	f2, p2 := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board", labelReviewerRetries: "2"}), reviewer("r1", "c1", gateDesign))
	storeLog(t, f2, p2, "r1", "[Assistant] no verdict line")
	if err := f2.ApplyVerdict(f2.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if p2.labels("c1")[labelStuck] != stuckReview {
		t.Fatalf("cap did not mark it stuck: %v", p2.labels("c1"))
	}
}

func TestDirectReplyFinishesWithoutGates(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAutoReview: gateRequest, labelSince: ago(2 * time.Minute)})
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "[Assistant] The answer is 4.\nTRACK: direct")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if p.labels("c1")[labelTrack] != "direct" || p.runs != 0 {
		t.Fatalf("labels %v runs %d", p.labels("c1"), p.runs)
	}
}

func TestPublicRepoScanStillStopsASignedOffCard(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: gateSignoff, labelSource: "board", labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c, reviewer("r1", "c1", gateSignoff))
	fakeScan(f, []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, nil)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "[Assistant] Fine.\nSIGN OFF: none\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan") {
		t.Fatalf("a scan hit must stop it: %v", l)
	}
}

func TestDesignGateCommitMovesTheReviewedHead(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board", labelReviewed: "old"}), reviewer("r1", "c1", gateDesign))
	headIs(f, "refactored", nil)
	storeLog(t, f, p, "r1", "[Assistant] Small refactor committed.\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelReviewed] != "refactored" || l[labelStage] != gateSignoff {
		t.Fatalf("sign-off must read the refactored head: %v", l)
	}
}

func TestReplyMovesTheCardToTheRightStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		stage  string
		queued string
	}{
		{"landing stays", map[string]string{labelStage: "landing"}, "landing", ""},
		{"a built card goes back through the gates", map[string]string{labelStage: gateDesign, labelVerdict: "request approve", labelResult: "code"}, "building", gateRequest},
		{"an errand stays on its pass", map[string]string{labelStage: "building", labelTrack: "direct"}, "building", gateRequest},
		{"a picker picks again", map[string]string{labelStage: stageRoute, labelRouting: "1"}, stageRoute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t, card("c1", tc.labels))
			if _, err := f.Act("send", Body{ID: "c1", Message: "what do you need from me?"}); err != nil {
				t.Fatal(err)
			}
			if l := p.labels("c1"); l[labelStage] != tc.stage || l[labelAutoReview] != tc.queued || l[labelSince] == "" {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestRerunRestartsEachGate(t *testing.T) {
	for _, from := range []string{gateRequest, gateDesign, gateSignoff} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: from, labelStuck: stuckReview}))
		if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		if runs := p.runsWithLabel(labelArg(labelReviewKind, from)); len(runs) != 1 {
			t.Errorf("%s: runs = %v", from, p.calls)
		}
	}
}

func TestInboxCardWithCommitsAndNoResultLineGetsTheGates(t *testing.T) {
	c := inboxCard("c1", map[string]string{labelStage: "building", labelSource: "email", labelAutoReview: gateRequest, labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"rev-list origin/main..HEAD": "abc\n", "status --porcelain": "", "rev-parse HEAD": "abc\n"})
	storeLog(t, f, p, "c1", "Fixed it and committed.")
	f.Tick()
	if p.labels("c1")[labelTrack] == "direct" || len(p.runsWithLabel(labelArg(labelReviewKind, gateRequest))) != 1 {
		t.Fatalf("commits must not skip the gates: %v", p.labels("c1"))
	}
}

func TestPublicDirectFirstPassIsScannedAndStops(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: "building", labelSource: "board", labelBase: "abc", labelAutoReview: gateRequest,
		labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	ranges := fakeScan(f, nil, nil)
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "bbb\n", "status --porcelain": ""})
	storeLog(t, f, p, "c1", "Pushed the fix.\nTRACK: direct")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "public repo: finished without review") {
		t.Errorf("labels = %v", l)
	}
	if len(*ranges) != 1 || (*ranges)[0] != "abc..HEAD" {
		t.Errorf("scanned %v", *ranges)
	}
}

func TestPublishedWorkInAPublicRepoIsScannedAtSignOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		hits []scan.Hit
		zach bool
	}{{"clean", nil, false}, {"a hit waits for Zach", []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, true}} {
		a := publicCard("c1", reviewLabels(map[string]string{labelBase: "abc", labelStage: gateSignoff}))
		f, p := newTestFactory(t, a)
		fakeScan(f, tc.hits, nil)
		if got := f.publishedScanNote(a) != ""; got != tc.zach {
			t.Errorf("%s: note = %v", tc.name, f.publishedScanNote(a))
		}
		_ = p
	}
}

func TestFollowUpStartsANewGateGeneration(t *testing.T) {
	a := card("c1", map[string]string{labelRoundsRequest: "3", labelRoundsDesign: "3", labelRoundsSignoff: "1", labelReviewerRetries: "2"})
	got := newCodeGeneration(a)
	for _, k := range []string{labelRoundsRequest, labelRoundsDesign, labelRoundsSignoff, labelReviewerRetries} {
		if v, ok := got[k]; !ok || v != "" {
			t.Errorf("%s not reset: %q", k, v)
		}
	}
}

func TestLegacyStuckReviewMovesToARealGate(t *testing.T) {
	f, p, _ := mailFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "review"}))
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != gateRequest || l[labelStuck] != stuckReview {
		t.Fatalf("labels = %v", l)
	}
}

func TestBuilderQuestionWaitsForZach(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAutoReview: gateRequest, labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "[Assistant] I need one answer first.\nQUESTION: Which login?\n- The new one (recommended)\n- The old one\n")
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], noteDirectAsk) || p.runs != 0 {
		t.Fatalf("a builder's question must reach Zach: %v, runs %d", l, p.runs)
	}
	withAnswer := withLabels(c, l)
	f.SetAgents([]Agent{withAnswer})
	if _, err := f.Act("send", Body{ID: "c1", Message: "The new one."}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != gateRequest {
		t.Fatalf("his answer goes back to the builder: %v", l)
	}
}

func TestResultWithAQuestionStillGoesThroughTheGates(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAutoReview: gateRequest, labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	storeLog(t, f, p, "c1", "[Assistant] Built.\nQUESTION: Also rename it?\n- No (recommended)\n- Yes\n\nRESULT: code")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != gateRequest {
		t.Fatalf("a finished solution goes to the gates: %v", l)
	}
}

// A recovery working from a list older than Zach's follow-up starts no reviewer.
func TestStaleRecoveryAfterFollowUpStartsNoReviewer(t *testing.T) {
	gone := reviewer("r1", "c1", gateDesign)
	gone.Status, gone.LastActivityAt = "running", t0.Add(-time.Hour) // stalled without a verdict
	c := card("c1", map[string]string{labelStage: gateDesign, labelSource: "board", labelReviewed: reviewedSHA, labelSince: ago(time.Hour)})
	c.LastActivityAt = t0.Add(-10 * time.Minute)
	f, p := newTestFactory(t, c, gone)
	headIs(f, reviewedSHA, nil)
	dirtyIs(f, "", nil)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Also handle the empty case."}); err != nil {
		t.Fatal(err)
	}
	queued := p.labels("c1")
	if err := f.checkStuck(c, t0.Add(5*time.Minute)); err != nil { // the list predates the follow-up
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelRole, "reviewer")); len(runs) != 0 {
		t.Fatalf("stale recovery started reviewers: %v", runs)
	}
	if l := p.labels("c1"); l[labelStage] != queued[labelStage] || l[labelSince] != queued[labelSince] || l[labelReviewerRetries] != "" {
		t.Errorf("stale recovery changed the follow-up's queue: %v, was %v", l, queued)
	}
}

// A card whose fresh agent cannot start resumes its old session with the same stage message.
func TestStageHandoffFallsBackToResume(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateDesign, labelSource: "board", labelAgent: "opus"}), reviewer("r1", "c1", gateDesign))
	f.cfg.NoStageHandoff = true
	storeLog(t, f, p, "c1", "[Assistant] Built.\nRESULT: code")
	storeLog(t, f, p, "r1", "[Assistant] - fix it\nVERDICT: changes")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "sent this back") {
		t.Fatalf("not resumed: %v", msgs)
	}
}

func TestSubmissionTagDefaultsToSystem(t *testing.T) {
	for _, tc := range []struct {
		b    Body
		want string
	}{
		{Body{SubmittedBy: submittedByZach}, submittedByZach},
		{Body{Origin: "email", Thread: "t"}, submittedByZach},
		{Body{Origin: "voice"}, submittedByZach},
		{Body{}, submittedBySystem},
		{Body{Origin: "idea", SubmittedBy: submittedByZach}, submittedBySystem},
		{Body{SubmittedBy: submittedBySystem}, submittedBySystem},
	} {
		if got := submissionTag(tc.b); got != tc.want {
			t.Errorf("%+v: tag %q, want %q", tc.b, got, tc.want)
		}
	}
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "Do a thing"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelSubmittedBy, submittedBySystem)); len(runs) != 1 {
		t.Fatalf("an agent's untagged request is the system's: %v", p.calls)
	}
}

func TestBoardTagsItsCardsAsZachs(t *testing.T) {
	if !strings.Contains(boardPage(t), "body.submittedBy='zach'") {
		t.Error("the New card dialog must tag its cards as Zach's")
	}
}

func TestSignOffPromptGivesTheSource(t *testing.T) {
	got := ReviewPrompt(Card{ID: "c1", Title: "t", Repo: "o/r", SubmittedBy: submittedBySystem, Source: "schedule"}, gateSignoff, 7478, "private")
	if !strings.Contains(got, "Source: schedule") {
		t.Errorf("the gate cannot tell a scheduled run: %q", trunc(got, 400))
	}
}

func TestEscalationAtSignOffNeverLands(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateSignoff, labelSource: "board", labelReviewed: "abc"}), reviewer("r1", "c1", gateSignoff))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", "[Assistant] Not sure.\nSIGN OFF: none\nVERDICT: escalate")
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "zach" {
		t.Fatalf("an escalation must reach Zach: stage %q", got)
	}
}

// No page offers a step of the old flow: a button left behind would start something else.
func TestPagesNameOnlyTheGates(t *testing.T) {
	for _, name := range []string{"web/board.html", "web/transcript.html"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range []string{"Review plan", "Review code", "kind:'plan'", "kind:'code'", "arbiter", "k.lens", "Allow &amp; build", "plans and builds"} {
			if strings.Contains(string(b), old) {
				t.Errorf("%s still has %q", name, old)
			}
		}
	}
}

// An answer to Zach's feedback on the version he saw lands only when nothing still needs him.
func TestFeedbackAnswerGoesAheadOrWaits(t *testing.T) {
	const none = "SIGN OFF: none\nVERDICT: approve"
	for _, tc := range []struct {
		name, reply, review string
		extra               map[string]string
		landing             bool
		hold                string
	}{
		{name: "asks nothing", reply: answerNothing, review: none, landing: true},
		{name: "asks something", reply: "DECISION: Land the totals?\nOVERVIEW: x.\nNEEDS FROM ZACH: Reply yes.", review: none},
		{name: "the gate says sign off", reply: answerNothing, review: "SIGN OFF: it rewrites a gate\nVERDICT: approve", hold: "it rewrites a gate"},
		{name: "the gate is silent", reply: answerNothing, review: "VERDICT: approve", hold: "the sign-off gate did not say whether it needs you"},
		{name: "a reason from the Ready stop holds", reply: answerNothing, review: none,
			extra: map[string]string{labelAskWhy: "it rewrites a gate"}, hold: "it rewrites a gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inThread(feedbackCard(tc.extra), "t1")
			f, p, m := mailFactory(t, c)
			m.threads["t1"] = []threadMsg{{ID: "m1"}}
			headIs(f, reviewedSHA, nil)
			storeLog(t, f, p, "c1", tc.reply)
			if err := f.passSignoff(c, map[string]string{}, "signoff approve", tc.review); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if tc.landing {
				if l[labelStage] != "landing" {
					t.Fatalf("labels = %v", l)
				}
				return
			}
			if l[labelStage] != "zach" {
				t.Fatalf("it must wait for him: %v", l)
			}
			if tc.hold != "" && !strings.Contains(l[labelFeedbackHold], tc.hold) {
				t.Errorf("hold = %q, want %q", l[labelFeedbackHold], tc.hold)
			}
		})
	}
}

func TestFeedbackGateKeepsAnEarlierReason(t *testing.T) {
	f, _ := newTestFactory(t)
	a := card("c1", map[string]string{labelAskWhy: "it rewrites a gate; " + feedbackAgainWhy})
	if got := f.feedbackGate(a, "SIGN OFF: none"); got != "it rewrites a gate" {
		t.Errorf("reason = %q", got)
	}
	if got := f.feedbackGate(card("c1", nil), "SIGN OFF: none"); got != "" {
		t.Errorf("no reason = %q", got)
	}
}

func TestGateColumnsKeepTheirStyling(t *testing.T) {
	if !strings.Contains(boardPage(t), "['request','design','signoff'].includes(s[0])?'gate'") {
		t.Error("the gate columns lost their styling")
	}
}

func TestRunbookLabelDoesNotSkipTheCheckOnCommits(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "landing", labelSource: "board",
		labelSince: ago(2 * time.Minute), labelResult: "runbook", labelReviewed: "abc", labelBase: "base"}))
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "abc\n", "rev-list origin/main..HEAD": "abc\n"})
	f.Git = func(_ context.Context, _ string, args ...string) error {
		if args[0] == "merge-base" {
			return errors.New("not an ancestor")
		}
		return nil
	}
	storeLog(t, f, p, "c1", "[Assistant] Ran every step.\nREMAINING: none")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got == "landed" {
		t.Fatal("commits that never reached main must not finish the card")
	}
}

func TestBuilderTitleNamesTheCard(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAutoReview: gateRequest, labelSince: ago(2 * time.Minute)}))
	storeLog(t, f, p, "c1", "[Assistant] Built it.\nTITLE: **Cards get short titles**\nRESULT: code")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if names := p.names("c1"); len(names) != 1 || names[0] != "Cards get short titles" {
		t.Fatalf("names = %v", names)
	}
}

func TestLandMessagesCarryTheRunbookCheck(t *testing.T) {
	for name, msg := range map[string]string{"private": landMsg, "public": guardedLandMsg, "rerun": rerunLandMsg(false), "rerun public": rerunLandMsg(true)} {
		if !strings.Contains(msg, "still matches the live system") || !strings.Contains(msg, "RETURN TO BUILD:") {
			t.Errorf("%s landing lacks the runbook check", name)
		}
	}
}

func TestScanHitOnPublishedWorkAtSignOffWaitsForZach(t *testing.T) {
	for _, tc := range []struct {
		name string
		hits []scan.Hit
		zach bool
	}{{"a hit waits for Zach", []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, true}, {"clean goes on", nil, false}} {
		a := freshCard(false, reviewLabels(map[string]string{labelLandedVerified: "1", labelBase: "base1"}))
		a.Repo = publicRepo
		f, p := newTestFactory(t, a, reviewer("r1", "c1", gateSignoff))
		fakeScan(f, tc.hits, nil)
		f.GitOut = gitStub(map[string]string{"rev-parse HEAD": reviewedSHA, "status --porcelain": "", "merge-base origin/main " + reviewedSHA: reviewedSHA,
			"rev-list --reverse --no-merges " + reviewedSHA + ".." + reviewedSHA: ""})
		storeLog(t, f, p, "r1", "[Assistant] Fine.\nSIGN OFF: none\nVERDICT: approve")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if tc.zach && (l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], landDiffNotePrefix)) {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
		if !tc.zach && l[labelLiveCompletion] != reviewedSHA {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
	}
}

func TestScheduledRunCarriesItsSource(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "Run the job", schedule: "nightly"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelOrigin, "schedule")); len(runs) != 1 {
		t.Fatalf("no schedule source: %v", p.calls)
	}
	if got := source(card("c", map[string]string{labelOrigin: "schedule"})); got != "schedule" {
		t.Errorf("source = %q", got)
	}
}

func TestStageStepMsgReadsTheStepOfAHandoff(t *testing.T) {
	prompt := stageHandoffMarker + " You are...\n\n=== Branch ===\nx\n\n=== This step ===\nBuild this now.\n"
	if got := stageStepMsg(prompt); got != "Build this now." {
		t.Errorf("step = %q", got)
	}
	if got := stageStepMsg(prompt + "\n" + delegateMarker + " the rule"); got != "Build this now." {
		t.Errorf("a trailing delegate section stays out: %q", got)
	}
	if got := stageStepMsg("Not a handoff."); got != "" {
		t.Errorf("not a handoff: %q", got)
	}
}

func TestSameChoiceReadsTheLabelThenTheModel(t *testing.T) {
	if c, ok := sameChoice(card("c", map[string]string{labelAgent: "codex-sol"})); !ok || c.ID != "codex-sol" {
		t.Errorf("by label: %+v %v", c, ok)
	}
	a := card("c", nil)
	a.Provider, a.Model = "claude", "claude-sonnet-5-5"
	if c, ok := sameChoice(a); !ok || c.ID != "sonnet" {
		t.Errorf("by provider and model: %+v %v", c, ok)
	}
	a.Model = ""
	if _, ok := sameChoice(a); ok {
		t.Error("an unknown model must not match")
	}
	a.Model = "no-such-model"
	if _, ok := sameChoice(a); ok {
		t.Error("an unlisted model must not match")
	}
}

func TestRepoPickerCarriesTheSchedule(t *testing.T) {
	f, p := newTestFactory(t)
	writeFile(t, filepath.Join(f.cfg.Home, "dev", "acme", "ops", "README.md"), "ops")
	res, err := f.Act("new", Body{Request: "Run the job", Repo: autoRepo, schedule: "nightly", SubmittedBy: submittedBySystem})
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]string)["id"]
	r, ok := f.routes.get(id)
	if !ok || r.Schedule != "nightly" {
		t.Fatalf("route = %+v", r)
	}
	a := card(id, map[string]string{labelStage: stageRoute, labelRouting: "1"})
	f.SetAgents([]Agent{a})
	if err := f.startRouted(a, "acme/ops"); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelOrigin, "schedule")); len(runs) != 1 {
		t.Fatalf("the card lost its schedule through repo picking: %v", p.calls)
	}
}

func TestApproveRefusedWhileTheGatesRun(t *testing.T) {
	for _, stage := range []string{"building", gateRequest, gateDesign, gateSignoff} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: stage, labelSource: "board", labelReviewed: reviewedSHA}))
		headIs(f, reviewedSHA, nil)
		dirtyIs(f, "", nil)
		if _, err := f.Act("approve", Body{ID: "c1"}); err == nil {
			t.Errorf("%s: approve went around the gates", stage)
		}
		if l := p.labels("c1"); l[labelStage] == "landing" || len(p.sent("c1")) != 0 {
			t.Errorf("%s: labels %v", stage, l)
		}
	}
}

func TestPagesOfferLandOnlyWhereItApplies(t *testing.T) {
	for _, name := range []string{"web/board.html", "web/transcript.html"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "canLand(") || !strings.Contains(string(b), "c.canLand") {
			t.Errorf("%s shows Approve & land on every card", name)
		}
	}
}

func TestTiersAreGone(t *testing.T) {
	for _, name := range []string{"web/board.html", "web/transcript.html"} {
		b, _ := os.ReadFile(name)
		if strings.Contains(string(b), "c.tier") || strings.Contains(string(b), "B.tiers") {
			t.Errorf("%s still shows tiers", name)
		}
	}
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSource: "board"}))
	c, _ := f.card("c1")
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), `"tier"`) {
		t.Errorf("a card still carries a tier: %s", raw)
	}
	var b Body
	if err := json.Unmarshal([]byte(`{"repo":"example/desk","title":"Do a thing","summary":"s","why":"w","doneWhen":"d","request":"r","tier":"risky"}`), &b); err != nil {
		t.Fatalf("a caller still sending a tier must not break: %v", err)
	}
	if _, err := f.Act("idea", b); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(f.ideas.List())
	if strings.Contains(string(raw), `"tier"`) {
		t.Errorf("an idea still keeps a tier: %s", raw)
	}
}

func TestCanLandMatchesWhatTheServerAccepts(t *testing.T) {
	stalled := card("s1", map[string]string{labelStage: "building", labelSource: "board"})
	stalled.Status, stalled.LastActivityAt = "running", t0.Add(-time.Hour) // the board shows it in Needs Zach
	adhoc := card("a1", nil)                                               // no factory stage at all
	adhoc.RequiresAttention, adhoc.AttentionReason = true, "finished"
	waiting := card("w1", map[string]string{labelStage: "zach", labelSource: "board"})
	f, _ := newTestFactory(t, stalled, adhoc, waiting)
	for id, want := range map[string]bool{"s1": false, "a1": true, "w1": true} {
		c, ok := f.card(id)
		if !ok || c.CanLand != want || landable(f.agent(id)) != want {
			t.Errorf("%s: stage %q canLand %v, want %v", id, c.Stage, c.CanLand, want)
		}
	}
}
