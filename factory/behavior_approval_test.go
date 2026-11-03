package factory

import (
	"strings"
	"testing"
)

func TestRepairApprovalReasons(t *testing.T) {
	for _, tc := range []struct{ finding, asked, files, want string }{
		{"unchanged: only the test setup changed", "", "", ""},
		{"changed: output changed", "", "", "you asked"},
		{"unsure: insufficient evidence", "", "", "you asked"},
		{"", "", "", "you asked"},
		{"unchanged: only setup changed", "arbiter decision", "", "arbiter decision"},
		{"unchanged: only setup changed", "", "AGENTS.md", "AGENTS.md"},
	} {
		f, _ := newTestFactory(t)
		a := card("c1", map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorRepair: "1", labelBehaviorSummary: "c3VtbWFyeQ"})
		f.GitOut = gitStub(map[string]string{"diff --no-renames --name-only " + reviewedSHA + "..HEAD": tc.files})
		got := f.finishedWorkReason(a, "architecture", "ALWAYS ASK: none\nAPPROVED BEHAVIOR: "+tc.finding, tc.asked)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q", tc, got)
		}
	}
}

func TestApprovalOnlyFinishedWork(t *testing.T) {
	f, _ := newTestFactory(t)
	for _, note := range []string{readyPrefix, "scan failed"} {
		a := card("c1", map[string]string{labelNote: note, labelReadyHead: reviewedSHA})
		labels := f.finishedApproval(a)
		if (labels[labelBehaviorApproval] != "") != (note == readyPrefix) {
			t.Fatalf("%s: %v", note, labels)
		}
	}
}

func TestFinishedApprovalRecoveryAndFreshReview(t *testing.T) {
	for _, held := range []bool{false, true} {
		labels := map[string]string{labelStage: "zach", labelNote: readyPrefix, labelReadyHead: reviewedSHA, labelReviewed: reviewedSHA, labelTier: "architecture", labelPlanRounds: "1"}
		if held {
			labels[labelWaitsOn] = "d0e0001"
		}
		f, p := newTestFactory(t, card("c1", labels), dep("building", false))
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "c1", "CHANGED: Output stays bounded.")
		if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		a := labeled(f.agent("c1"), p.labels("c1"))
		if a.L(labelBehaviorApproval) != reviewedSHA || a.L(labelBehaviorSummary) == "" {
			t.Fatalf("missing approval: %v", p.labels("c1"))
		}
		f.cfg.NoStageHandoff = true
		if err := f.returnToBuild(a, "tests failed", "repair", false); err != nil {
			t.Fatal(err)
		}
		a = labeled(a, p.labels("c1"))
		carried := copyCarriedLabels(a)
		if carried[labelPrefix+labelBehaviorApproval] != reviewedSHA || carried[labelPrefix+labelBehaviorRepair] != "1" {
			t.Fatalf("lost at handoff: %v", carried)
		}
		f.GitOut = gitStub(map[string]string{"diff --no-renames --name-only " + reviewedSHA + "..HEAD": "", "status --porcelain": ""})
		headIs(f, reviewedSHA, nil)
		next := map[string]string{}
		if err := f.passCode(a, next, "code approve", "architecture", "APPROVED BEHAVIOR: unchanged: output matches approved contract\nALWAYS ASK: none", ""); err != nil {
			t.Fatal(err)
		}
		if got := p.labels("c1")[labelStage]; got != "landing" {
			t.Fatalf("stage %s labels %v", got, p.labels("c1"))
		}
		if got := newCodeGeneration(a); got[labelBehaviorApproval] != "" || got[labelBehaviorSummary] != "" {
			t.Fatal("new work kept authorization")
		}
	}
}

func TestEmailFinishedApproval(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: readyPrefix, labelReadyHead: reviewedSHA, labelReviewed: reviewedSHA, labelZachThread: "nt1"})
	f, p, _ := mailFactory(t, a)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "c1", "CHANGED: Output stays bounded.")
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if p.labels("c1")[labelBehaviorApproval] != reviewedSHA {
		t.Fatalf("email lost approval: %v", p.labels("c1"))
	}
}
