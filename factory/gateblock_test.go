package factory

import (
	"strings"
	"testing"
	"time"
)

// blockedGate is a gate reply that finds work only Zach can unblock: it escalates with a question.
const blockedGate = "[Assistant] The reader needs a web login the host does not have.\n" +
	"NEEDS FROM ZACH: how the factory should log in to read the quota.\n" +
	"QUESTION: How should the factory log in?\n- Save a web login and renew it (recommended)\n- Paste one when it expires\n" +
	"MEETS REQUEST: red - no login, so the board still shows unknown\nWORKS: yellow - never run end to end\n" +
	"DESIGN: green - clean\nPUBLIC SAFETY: green - scan passes\nSIGN OFF: green - he asked for it\nVERDICT: escalate"

func TestGateEscalateWithRedChecksAsksZachNotTheBuilder(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board",
		labelSubmittedBy: submittedByZach, labelReviewed: "abc"}), reviewer("r1", "c1", gateCheck))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", blockedGate)
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != gateBlockedNote {
		t.Fatalf("stage %q note %q: a blocked gate waits on Zach", l[labelStage], l[labelNote])
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("the builder is not sent back: %q", p.sent("c1"))
	}
	ask := askFor(Card{ID: "c1", Stage: "zach"}, card("c1", l))
	if ask == nil || ask.From != "reviewer" || ask.Kind != askQuestion || ask.BoardOnly {
		t.Fatalf("ask = %+v, want the gate's question, emailed in his thread", ask)
	}
}

func TestGateFailingTheSameCommitAgainAsksZach(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board",
		labelSubmittedBy: submittedByZach, labelReviewed: "abc", labelRoundsGate: "1", labelGateFailedHead: "abc"}),
		reviewer("r1", "c1", gateCheck))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", gateReply("MEETS REQUEST", "red - still shows unknown", "PUBLIC SAFETY", "n/a"))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != gateBlockedNote {
		t.Fatalf("stage %q note %q: no new commit since a failed pass is not a build problem", l[labelStage], l[labelNote])
	}
}

func TestGateFailingANewCommitGoesBackAndRemembersIt(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board",
		labelSubmittedBy: submittedByZach, labelReviewed: "def", labelRoundsGate: "1", labelGateFailedHead: "abc"}),
		reviewer("r1", "c1", gateCheck))
	headIs(f, "def", nil)
	storeLog(t, f, p, "r1", gateReply("MEETS REQUEST", "red - still shows unknown", "PUBLIC SAFETY", "n/a"))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" {
		t.Fatalf("stage %q: new work that fails goes back to the builder", l[labelStage])
	}
	if l[labelGateFailedHead] != "def" {
		t.Errorf("failed head = %q, want def", l[labelGateFailedHead])
	}
}

func TestStuckGateAskShowsTheGateReportAndLeadsWithReply(t *testing.T) {
	a := card("c", map[string]string{labelStage: gateCheck, labelStuck: stuckGate})
	ask := askFor(Card{ID: "c", Stage: gateCheck}, a)
	if ask.From != "reviewer" {
		t.Errorf("from = %q: the gate's findings explain a gate stop, not the builder's report", ask.From)
	}
	if len(ask.Actions) == 0 || ask.Actions[0].ID != "reply" || !ask.Actions[0].Primary {
		t.Errorf("actions = %+v: reply leads", ask.Actions)
	}
	if strings.Contains(ask.Why, "automatic retries") {
		t.Errorf("why = %q: gate rounds are not automatic retries", ask.Why)
	}
}

func TestRerunStuckGateOnTheSameCommitSendsTheReportToTheBuilder(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelStuck: stuckGate, labelSource: "board",
		labelReviewed: "abc", labelGateFailedHead: "abc", labelRoundsGate: "3"}))
	headIs(f, "abc", nil)
	f.recordRound("c1", gateCheck, 3, t0.Add(-time.Minute), "changes", "MEETS REQUEST: red - the login is missing")
	if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if p.promptOfKind(gateCheck) != "" {
		t.Errorf("no gate starts on a commit it already failed")
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAutoReview] != gateCheck || l[labelStuck] != "" {
		t.Fatalf("labels = %v: the builder gets another try", l)
	}
	if sent := strings.Join(p.sent("c1"), "\n"); !strings.Contains(sent, "the login is missing") {
		t.Errorf("the builder is not given the gate's findings: %q", trunc(sent, 400))
	}
}

func TestAnswerToAGateStopCarriesTheGateReport(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelNote: gateBlockedNote, labelSource: "board",
		labelReviewed: "abc", labelRoundsGate: "2"}))
	f.recordRound("c1", gateCheck, 2, t0.Add(-time.Minute), "escalate", "QUESTION: How should the factory log in?")
	if _, err := f.Act("send", Body{ID: "c1", Message: "Save a web login"}); err != nil {
		t.Fatal(err)
	}
	sent := strings.Join(p.sent("c1"), "\n")
	if !strings.Contains(sent, "Save a web login") || !strings.Contains(sent, "How should the factory log in?") {
		t.Errorf("sent = %q: the builder needs the question his answer is to", trunc(sent, 600))
	}
	if l := p.labels("c1"); l[labelStage] != "building" || l[labelRoundsGate] != "" {
		t.Errorf("labels = %v: his answer starts a fresh gate generation", l)
	}
}

func TestPromptsRouteBlockersToZach(t *testing.T) {
	gate := gateRubric[gateCheck]
	for _, want := range []string{"VERDICT: escalate", "QUESTION:", "only Zach"} {
		if !strings.Contains(gate, want) {
			t.Errorf("gate prompt misses %q", want)
		}
	}
	for name, s := range map[string]string{"build": proposedSolutionRule, "rework": reworkGateMsg([]string{"works red"}, 1, 3, "notes")} {
		if !strings.Contains(s, blockedRule) {
			t.Errorf("%s prompt misses the blocked rule", name)
		}
	}
}
