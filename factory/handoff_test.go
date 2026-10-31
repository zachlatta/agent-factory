package factory

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const handoffLog = "[User] Fellow instructions: plan, then build. Keep the public-repo scan.\n" +
	"PLAN: add the widget\nTIER: low\n" +
	"[User] Plan approved by the plan reviewer. Build it now, as planned. Reviewer notes: SMALL FIX: name the widget\n" +
	"[Tool] Bash\n" +
	"Half the widget is in; tests next."

// farCard is a building card whose Claude limit resets in 3 hours, with a readable session.
func farCard(t *testing.T) (Agent, *Factory, *fakePaseo) {
	a := quotaCard("building")
	a.ID = "old-card"
	a.SessionID = testSession(a.ID)
	a.WorkspaceID = "ws-1"
	a.Labels[labelPrefix+labelAgent] = "opus"
	a.Labels[labelPrefix+labelTier] = "architecture"
	a.Labels[labelPrefix+labelPlanRounds] = "2"
	a.Labels[labelPrefix+labelWaitsOn] = "abc1234"
	a.Labels[gmailThreadLabel] = "thread-1"
	a.LastError = "Individual quota reached. Resets in 3h."
	f, p := newTestFactory(t, a)
	storeLog(t, f, p, a.ID, handoffLog)
	return a, f, p
}

func runArg(c []string, flag string) string {
	for i := range c {
		if c[i] == flag && i+1 < len(c) {
			return c[i+1]
		}
	}
	return ""
}

func handoffRuns(p *fakePaseo) [][]string {
	var out [][]string
	for _, c := range p.calls {
		if c[0] == "run" && strings.Contains(strings.Join(c, " "), labelPrefix+labelReplaces+"=") {
			out = append(out, c)
		}
	}
	return out
}

func TestHandoffFarResetStartsOpusAlternativeInSameWorkspace(t *testing.T) {
	a, f, p := farCard(t)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	runs := handoffRuns(p)
	if len(runs) != 1 {
		t.Fatalf("runs = %v", p.calls)
	}
	run := runs[0]
	joined := strings.Join(run, " ")
	if runArg(run, "--provider") != "codex/gpt-6.1-sol" {
		t.Fatalf("Opus failing should go to the best-ranked non-Claude Frontier model: %v", run)
	}
	if runArg(run, "--workspace") != "ws-1" || strings.Contains(joined, "--new-workspace") {
		t.Fatalf("not the same worktree: %v", run)
	}
	for _, want := range []string{
		labelPrefix + labelReplaces + "=old-card", labelPrefix + labelStage + "=building",
		labelPrefix + labelAgent + "=codex-sol", labelPrefix + labelTier + "=architecture",
		labelPrefix + labelPlanRounds + "=2", labelPrefix + labelWaitsOn + "=abc1234",
		labelPrefix + labelAutoReview + "=code", gmailThreadLabel + "=thread-1",
	} {
		if !strings.Contains(joined, "--label "+want) {
			t.Errorf("missing label %s", want)
		}
	}
	if strings.Contains(joined, labelPrefix+labelHandoff) {
		t.Errorf("hand-off state carried to the replacement: %v", run)
	}
	prompt := run[len(run)-1]
	for _, want := range []string{"Fellow instructions: plan, then build", "PLAN: add the widget", "SMALL FIX: name the widget",
		"Half the widget is in", "Opus 5.5", "building the approved plan", "do NOT land"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if l := p.labels(a.ID); parseHandoff(l[labelHandoff]).Phase != "started" || parseHandoff(l[labelHandoff]).New != "new-agent-1" {
		t.Fatalf("hand-off state = %v", l)
	}
	if p.called("archive", a.ID) || len(p.sent(a.ID)) != 0 {
		t.Fatal("old agent archived or resumed before the replacement was confirmed")
	}
	if l := p.labels(a.ID); l[labelStage] == "zach" || l[labelStuck] != "" {
		t.Fatal("hand-off sent the card to Zach or marked it stuck")
	}
}

func TestHandoffNearResetStillWaits(t *testing.T) {
	a, f, p := farCard(t)
	a.LastError = "Individual quota reached. Resets in 1h50m."
	f.SetAgents([]Agent{a})
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(handoffRuns(p)) != 0 || readRecovery(withLabels(a, p.labels(a.ID))).Phase != "waiting" {
		t.Fatalf("a reset under 2 hours must wait for the same model: %v", p.calls)
	}
}

// usedUp gives errored card a a recovery whose same-model retries are all spent.
func usedUp(a Agent) Agent {
	b, _ := json.Marshal(recovery{Stage: a.L(labelStage), Phase: "waiting", Error: "an earlier error", Timed: len(errorBackoff), ResetUsed: true,
		UserAt: a.LastUserMessageAt})
	return withLabels(a, map[string]string{labelRecovery: base64.RawURLEncoding.EncodeToString(b)})
}

func TestHandoffWhenSameModelRecoveryIsUsedUp(t *testing.T) {
	a, f, p := farCard(t)
	a.LastError = "Unrecognized provider error"
	f.SetAgents([]Agent{a})
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(handoffRuns(p)) != 0 {
		t.Fatal("handed off before the same-model retries")
	}
	a = usedUp(a)
	f, p = newTestFactory(t, a)
	storeLog(t, f, p, a.ID, handoffLog)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	if len(handoffRuns(p)) != 1 || p.labels(a.ID)[labelStuck] != "" {
		t.Fatalf("used-up recovery should hand off: %v", p.calls)
	}
}

func TestHandoffNoModelOrNoHistoryFallsBackToStuck(t *testing.T) {
	t.Run("no model", func(t *testing.T) {
		a, f, p := farCard(t)
		a.LastError = "Unrecognized provider error"
		a = usedUp(a)
		for _, prov := range []string{"codex", "antigravity", "muse"} {
			p.providers[prov] = "unavailable"
		}
		f.SetAgents([]Agent{a})
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		if len(handoffRuns(p)) != 0 || p.labels(a.ID)[labelStuck] != stuckAgentError {
			t.Fatalf("with no other model the card should end where recovery ends: %v", p.labels(a.ID))
		}
	})
	t.Run("no history", func(t *testing.T) {
		a := quotaCard("building")
		a.LastError = "Unrecognized provider error"
		a = usedUp(a)
		f, p := newTestFactory(t, a)
		if err := f.tickAgent(a, t0); err != nil {
			t.Fatal(err)
		}
		if len(handoffRuns(p)) != 0 || p.labels(a.ID)[labelStuck] != stuckAgentError {
			t.Fatalf("without the card's instructions there is no hand-off: %v", p.calls)
		}
	})
}

func TestPickHandoff(t *testing.T) {
	all := map[string]string{"claude": "available", "codex": "available", "antigravity": "available", "muse": "available"}
	usage := func(pct int) AgentUsage {
		return AgentUsage{Windows: []UsageWindow{{Label: "7d", RemainingPct: pct, ResetsAt: t0.Add(24 * time.Hour).Format(time.RFC3339)}}}
	}
	for _, tc := range []struct {
		name, failing, want string
		statuses            map[string]string
		usages              map[string]AgentUsage
	}{
		{"codex failing takes Opus", "codex", "opus", all, nil},
		{"opus failing skips all of Claude", "claude", "codex-sol", all, nil},
		{"ranked by quota", "claude", "gemini-argon", all, map[string]AgentUsage{"codex": usage(5), "antigravity": usage(90)}},
		{"exhausted plan skipped", "claude", "gemini-argon", all, map[string]AgentUsage{"codex": usage(0)}},
		{"Opus exhausted", "codex", "gemini-argon", all, map[string]AgentUsage{"claude": usage(0)}},
		{"unavailable skipped", "claude", "gemini-argon", map[string]string{"claude": "available", "codex": "error", "antigravity": "available"}, nil},
		{"none left", "claude", "", map[string]string{"claude": "available"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickHandoff(tc.failing, tc.statuses, tc.usages, t0)
			if got.ID != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q %v, want %q", got.ID, ok, tc.want)
			}
		})
	}
}

// replacementOf is the agent the fake run started for old, as the next poll shows it.
func replacementOf(old Agent, id, status string) Agent {
	r := card(id, map[string]string{labelReplaces: old.ID, labelStage: old.L(labelStage), labelAgent: "codex-sol"})
	r.Provider, r.Status = "codex", status
	return r
}

func TestHandoffConfirmsThenArchivesOld(t *testing.T) {
	a, f, p := farCard(t)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	old := withLabels(a, p.labels(a.ID))
	r := replacementOf(a, "new-agent-1", "running")
	f.SetAgents([]Agent{old, r})
	// reap must not archive the replaced predecessor before confirmation
	old.Status = "idle"
	f.SetAgents([]Agent{old, r})
	f.reap(t0)
	if p.called("archive", a.ID) {
		t.Fatal("reap archived an unconfirmed predecessor")
	}
	f.tickHandoffs(t0.Add(time.Minute))
	l := p.labels(a.ID)
	if parseHandoff(l[labelHandoff]).Phase != "confirmed" || l[labelReplacedBy] != "new-agent-1" || !p.called("archive", a.ID) {
		t.Fatalf("not confirmed and archived: %v", l)
	}
	if p.sent(a.ID) != nil || l[labelStage] == "zach" || l[labelStuck] != "" {
		t.Fatal("old agent messaged or sent to Zach")
	}
}

func TestHandoffReplacementErrorNeverHandsOffAgain(t *testing.T) {
	a, f, p := farCard(t)
	if err := f.tickAgent(a, t0); err != nil {
		t.Fatal(err)
	}
	old := withLabels(a, p.labels(a.ID))
	old.Labels[labelPrefix+labelReplacedBy] = "new-agent-1"
	old.ArchivedAt = t0
	r := replacementOf(a, "new-agent-1", "error")
	r.LastError = "Unrecognized provider error"
	r.Labels[labelPrefix+labelAutoReview] = "code"
	r = usedUp(r)
	f2, p2 := newTestFactory(t, old, r)
	storeLog(t, f2, p2, r.ID, handoffLog)
	if err := f2.tickAgent(r, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(handoffRuns(p2)) != 0 || p2.labels(r.ID)[labelStuck] != stuckAgentError {
		t.Fatalf("second hand-off or no stuck marker: %v", p2.calls)
	}
}

func TestHandoffResumesAfterRestart(t *testing.T) {
	t.Run("starting with a replacement adopts it", func(t *testing.T) {
		a, _, _ := farCard(t)
		a.Labels[labelPrefix+labelHandoff] = handoffState{Phase: "starting", Choice: "codex-sol", At: t0, Tries: 1}.String()
		r := replacementOf(a, "new-agent-9", "running")
		f, p := newTestFactory(t, a, r)
		storeLog(t, f, p, a.ID, handoffLog)
		f.tickHandoffs(t0.Add(time.Minute))
		if s := parseHandoff(p.labels(a.ID)[labelHandoff]); s.Phase != "started" || s.New != "new-agent-9" || len(handoffRuns(p)) != 0 {
			t.Fatalf("not adopted: %v %v", s, p.calls)
		}
	})
	t.Run("starting without a replacement starts once more, then gives up", func(t *testing.T) {
		a, _, _ := farCard(t)
		a.Labels[labelPrefix+labelHandoff] = handoffState{Phase: "starting", Choice: "codex-sol", At: t0, Tries: 1}.String()
		f, p := newTestFactory(t, a)
		storeLog(t, f, p, a.ID, handoffLog)
		f.tickHandoffs(t0.Add(time.Minute))
		if len(handoffRuns(p)) != 0 {
			t.Fatal("restarted within the grace")
		}
		// a busy hand-off keeps the other rules off the card
		if err := f.tickAgent(a, t0.Add(time.Minute)); err != nil || len(p.calls) != 0 {
			t.Fatalf("tickAgent moved a card mid-hand-off: %v", p.calls)
		}
		f.tickHandoffs(t0.Add(3 * time.Minute))
		if len(handoffRuns(p)) != 1 {
			t.Fatalf("no second try: %v", p.calls)
		}
		a = withLabels(a, map[string]string{labelHandoff: handoffState{Phase: "starting", Choice: "codex-sol", At: t0, Tries: 2}.String()})
		f, p = newTestFactory(t, a)
		storeLog(t, f, p, a.ID, handoffLog)
		f.tickHandoffs(t0.Add(3 * time.Minute))
		if len(handoffRuns(p)) != 0 || parseHandoff(p.labels(a.ID)[labelHandoff]).Phase != "failed" || p.labels(a.ID)[labelStuck] != stuckAgentError {
			t.Fatalf("third try or no stuck marker: %v", p.calls)
		}
	})
	t.Run("started never confirmed archives nothing", func(t *testing.T) {
		a, _, _ := farCard(t)
		a.Labels[labelPrefix+labelHandoff] = handoffState{Phase: "started", New: "new-agent-1", Choice: "codex-sol", At: t0, Tries: 1}.String()
		r := replacementOf(a, "new-agent-1", "error")
		f, p := newTestFactory(t, a, r)
		f.tickHandoffs(t0.Add(time.Minute))
		if len(p.calls) != 0 {
			t.Fatalf("acted within the grace: %v", p.calls)
		}
		f.tickHandoffs(t0.Add(6 * time.Minute))
		if parseHandoff(p.labels(a.ID)[labelHandoff]).Phase != "failed" || p.called("archive", a.ID) || p.labels(a.ID)[labelStuck] != "" {
			t.Fatalf("unconfirmed: %v", p.calls)
		}
	})
}

func TestFailedHandoffPredecessorLeavesWithTheCard(t *testing.T) {
	failed := func() (Agent, Agent) {
		a := card("old-card", map[string]string{labelStage: "building",
			labelHandoff: handoffState{Phase: "failed", New: "new-agent-1", Choice: "codex-sol", At: t0, Tries: 1}.String()})
		r := replacementOf(a, "new-agent-1", "idle")
		return a, r
	}
	t.Run("reap keeps it while the card works, not in the parking list", func(t *testing.T) {
		a, r := failed()
		a.Labels[labelPrefix+labelStage] = "zach"
		a.LastActivityAt = t0.Add(-48 * time.Hour)
		f, p := newTestFactory(t, a, r)
		f.reap(t0)
		if p.called("archive", a.ID) || p.labels(a.ID)[labelParked] != "" {
			t.Fatalf("predecessor archived or parked: %v", p.calls)
		}
	})
	t.Run("reap archives it once the card landed", func(t *testing.T) {
		a, r := failed()
		r.Labels[labelPrefix+labelStage] = "landed"
		f, p := newTestFactory(t, a, r)
		f.reap(t0)
		if !p.called("archive", a.ID) {
			t.Fatalf("not archived after landing: %v", p.calls)
		}
	})
	t.Run("an errored one goes when the card is archived", func(t *testing.T) {
		a, r := failed()
		a.Status = "error"
		r.ArchivedAt = t0
		f, p := newTestFactory(t, a, r)
		f.tickHandoffs(t0)
		if !p.called("archive", a.ID) {
			t.Fatalf("not archived: %v", p.calls)
		}
	})
	t.Run("Drop archives the whole lineage", func(t *testing.T) {
		a, r := failed()
		f, p := newTestFactory(t, a, r)
		if _, err := f.Act("archive", Body{ID: r.ID}); err != nil {
			t.Fatal(err)
		}
		if !p.called("archive", r.ID) || !p.called("archive", a.ID) {
			t.Fatalf("lineage not archived: %v", p.calls)
		}
	})
}

func TestHandoffStateRoundTrip(t *testing.T) {
	s := handoffState{Phase: "started", New: "abc", Choice: "opus", At: t0, Tries: 2}
	if got := parseHandoff(s.String()); got != s {
		t.Fatalf("%+v != %+v", got, s)
	}
	if got := parseHandoff(handoffState{Phase: "starting", Choice: "opus", At: t0, Tries: 1}.String()); got.New != "" {
		t.Fatalf("empty new id = %q", got.New)
	}
	if parseHandoff("garbage").Phase != "" {
		t.Fatal("parsed garbage")
	}
}

func TestFailedHandoffArchivePersistsCompletion(t *testing.T) {
	for _, path := range []string{"tick", "reap"} {
		t.Run(path, func(t *testing.T) {
			a := card("predecessor", map[string]string{labelStage: "building", labelArchiveWatch: "1",
				labelHandoff: handoffState{Phase: "failed", New: "successor", Choice: "codex-sol", At: t0}.String()})
			r := replacementOf(a, "successor", "idle")
			r.Labels[labelPrefix+labelStage] = "landed"
			f, p := newTestFactory(t, a, r)
			if path == "tick" {
				f.tickHandoffs(t0)
			} else {
				f.reap(t0)
			}
			if !p.called("archive", a.ID) || p.labels(a.ID)[labelFinished] != "1" {
				t.Fatalf("archive missing durable completion: %v %v", p.calls, p.labels(a.ID))
			}
			a = withLabels(a, p.labels(a.ID))
			a.ArchivedAt = t0
			f, _ = newTestFactory(t, a)
			if interruptedArchive(f.agent(a.ID)) || Derive(a) != "landed" {
				t.Fatal("retired predecessor became interrupted without successor")
			}
		})
	}
}
