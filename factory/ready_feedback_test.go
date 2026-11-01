package factory

import (
	"strings"
	"testing"
)

func TestReadyFeedbackUnchanged(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1",
		labelCodeRounds: "1", labelReady: "1", labelReadyHead: reviewedSHA, labelReadyFeedback: "exchange-2", labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c, reviewer("r2", "c1", "code"))
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r2", "TIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r2"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || f.agent("c1").L(labelReady) != "1" || !isFeedbackNote(l[labelNote]) || l[labelWriting] != "" {
		t.Fatalf("labels = %v", l)
	}
	if !readyStop(labeled(c, l)) {
		t.Fatal("feedback answer must retain finished-work controls")
	}
}

func TestReadyFeedbackMail(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " exchange-2", labelReady: "1"})
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", "DECISION: None.\nOVERVIEW: The preview needs a reload.\nNEEDS FROM ZACH: Nothing.\n\nReload the preview to see the update.")
	f.Tick()
	f.SetAgents([]Agent{labeled(c, p.labels("c1"))})
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %v", sends)
	}
	restarted, _, after := mailFactory(t, labeled(c, p.labels("c1")))
	restarted.Tick()
	if len(after.sends()) != 0 {
		t.Fatal("restart repeated feedback email")
	}
	text := sends[0].Args["text"].(string)
	if !strings.Contains(text, "Reload the preview") || strings.Contains(text, "Ready to try:") {
		t.Fatal(text)
	}
}

func TestFeedbackApprovalLands(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: feedbackPrefix + " reply-1", labelZachThread: "nt1", labelReviewed: reviewedSHA})
	f, p, _ := mailFactory(t, c)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", "The preview needs a reload.")
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m3", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if p.labels("c1")[labelStage] != "landing" {
		t.Fatal(p.labels("c1"))
	}
}

func TestFeedbackRevisionGate(t *testing.T) {
	for _, tc := range []struct {
		name, head, dirty string
		fresh             bool
	}{
		{"changed", movedSHA, "", true}, {"unknown", "", "", true}, {"dirty", reviewedSHA, " M code.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1", labelReady: "1", labelReadyHead: tc.head, labelReadyFeedback: "reply-1", labelReviewed: reviewedSHA})
			f, p := newTestFactory(t, c)
			headIs(f, reviewedSHA, nil)
			dirtyIs(f, tc.dirty, nil)
			if err := f.passCode(c, map[string]string{}, "code approve", "architecture", "", ""); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if isFeedbackNote(l[labelNote]) {
				t.Fatal("unsafe suppression", l)
			}
			if tc.fresh && l[labelNote] != readyNote(2) {
				t.Fatal(l)
			}
		})
	}
}

// A feedback reply queues a new generation while retaining the presentation.
func TestReadyFeedbackQueuesReview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelReadyHead: reviewedSHA, labelCodeRounds: "2"})
	f, p := newTestFactory(t, c)
	if _, err := f.Act("send", Body{ID: "c1", Message: "Explain how to open the preview"}); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelAutoReview] != "code" || l[labelReadyFeedback] == "" || l[labelCodeRoundsFrom] != "2" {
		t.Fatal(l)
	}
	sent := p.sent("c1")
	if len(sent) != 1 || !strings.Contains(sent[0], "Explain how to open") || !strings.Contains(sent[0], "Do not repeat the Ready summary") {
		t.Fatal(sent)
	}
}

func TestReadyFeedbackStateCarried(t *testing.T) {
	old := card("old", map[string]string{labelReady: "2", labelReadyHead: reviewedSHA, labelReadyFeedback: "reply-2", labelAskWhy: "requested"})
	next := copyCarriedLabels(old)
	for _, k := range []string{labelReady, labelReadyHead, labelReadyFeedback, labelAskWhy} {
		if next[labelPrefix+k] != old.L(k) {
			t.Fatalf("%s lost: %v", k, next)
		}
	}
}
