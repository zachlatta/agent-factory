package factory

import (
	"encoding/base64"
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
		f.GitOut = gitStub(map[string]string{"merge-base " + reviewedSHA + " origin/main": "base", "diff --no-renames --name-only base.." + reviewedSHA: "", "diff --no-renames --name-only origin/main...HEAD": tc.files, "diff --no-ext-diff --no-renames --unified=0 base.." + reviewedSHA + " -- AGENTS.md": "", "diff --no-ext-diff --no-renames --unified=0 origin/main...HEAD -- AGENTS.md": "+new rule"})
		got := f.finishedWorkReason(a, "architecture", "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: "+tc.finding, tc.asked)
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
		f.GitOut = gitStub(map[string]string{"merge-base " + reviewedSHA + " origin/main": "base", "diff --no-renames --name-only base.." + reviewedSHA: "", "diff --no-renames --name-only origin/main...HEAD": "", "status --porcelain": ""})
		headIs(f, reviewedSHA, nil)
		next := map[string]string{}
		if err := f.passCode(a, next, "code approve", "architecture", "APPROVED BEHAVIOR: unchanged: output matches approved contract\nALWAYS ASK: none\nREPAIR ALWAYS ASK: none", ""); err != nil {
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

func TestApprovalSummaryBounded(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelNote: readyPrefix, labelReadyHead: reviewedSHA}))
	storeLog(t, f, p, "c1", strings.Repeat("summary ", handoffTextCap))
	labels := f.finishedApproval(f.agent("c1"))
	raw, err := base64.RawURLEncoding.DecodeString(labels[labelBehaviorSummary])
	if err != nil || len(raw) > handoffTextCap+10 {
		t.Fatalf("summary length %d: %v", len(raw), err)
	}
}

func TestNewInstructionsCancelRepairApproval(t *testing.T) {
	for _, via := range []string{"board", "email"} {
		t.Run(via, func(t *testing.T) {
			a := card("c1", map[string]string{labelStage: "zach", labelNote: readyPrefix, labelReadyHead: reviewedSHA, labelReviewed: reviewedSHA})
			f, p := newTestFactory(t, a)
			headIs(f, reviewedSHA, nil)
			storeLog(t, f, p, "c1", "CHANGED: bounded output")
			if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
				t.Fatal(err)
			}
			f.cfg.NoStageHandoff = true
			if err := f.returnToBuild(labeled(a, p.labels("c1")), "tests failed", "repair", false); err != nil {
				t.Fatal(err)
			}
			if err := f.setLabels("c1", map[string]string{labelStuck: stuckAgentError}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Act("send", Body{ID: "c1", Via: via, Message: "Change the output contract"}); err != nil {
				t.Fatal(err)
			}
			a = labeled(a, p.labels("c1"))
			if reuseBehaviorApproval(a, "APPROVED BEHAVIOR: unchanged: fixture repair") {
				t.Fatal("new instructions retained approval")
			}
			if got := f.finishedWorkReason(a, "architecture", "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: unchanged: fixture repair", ""); got == "" {
				t.Fatal("must ask again")
			}
		})
	}
}

func TestSteerCancelsRepairApproval(t *testing.T) {
	for _, status := range []string{"idle", "running"} {
		f, p := dupFactory(t, "building", status, map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ", labelBehaviorRepair: "1"})
		closeDup(t, f)
		f.tickSteers()
		a := labeled(f.agent(keepID), p.labels(keepID))
		if reuseBehaviorApproval(a, "APPROVED BEHAVIOR: unchanged: fixture repair") {
			t.Fatalf("%s steer kept approval", status)
		}
	}
}

func TestInterruptedRepairKeepsApproval(t *testing.T) {
	for _, action := range []string{"resume", "rerun"} {
		t.Run(action, func(t *testing.T) {
			a := card("c1", map[string]string{labelStage: "building", labelArchiveWatch: "1", labelStuck: stuckArchived, labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ", labelBehaviorRepair: "1", labelReviewed: reviewedSHA})
			a.ArchivedAt = t0
			f, p := newTestFactory(t, a)
			p.archived[a.ID] = true
			if _, err := f.Act(action, Body{ID: a.ID}); err != nil {
				t.Fatal(err)
			}
			a = labeled(f.agent(a.ID), p.labels(a.ID))
			f.GitOut = gitStub(map[string]string{"merge-base " + reviewedSHA + " origin/main": "base", "diff --no-renames --name-only base.." + reviewedSHA: "", "diff --no-renames --name-only origin/main...HEAD": "", "status --porcelain": ""})
			headIs(f, reviewedSHA, nil)
			if err := f.passCode(a, map[string]string{}, "code approve", "architecture", "APPROVED BEHAVIOR: unchanged: only fixture setup changed\nALWAYS ASK: architecture and approval gate changes\nREPAIR ALWAYS ASK: none", ""); err != nil {
				t.Fatal(err)
			}
			if p.labels(a.ID)[labelStage] != "landing" {
				t.Fatalf("approval lost: %v", p.labels(a.ID))
			}
		})
	}
}

func TestRepairAlwaysAskScope(t *testing.T) {
	for _, tc := range []struct {
		scoped string
		want   bool
	}{
		{"REPAIR ALWAYS ASK: none", false},
		{"", true},
		{"REPAIR ALWAYS ASK: new service", true},
		{"REPAIR ALWAYS ASK: new service\nREPAIR ALWAYS ASK: none", true},
	} {
		f, _ := newTestFactory(t)
		a := card("c1", map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorRepair: "1", labelBehaviorSummary: "c3VtbWFyeQ"})
		f.GitOut = gitStub(map[string]string{"merge-base " + reviewedSHA + " origin/main": "base", "diff --no-renames --name-only base.." + reviewedSHA: "", "diff --no-renames --name-only origin/main...HEAD": ""})
		report := "APPROVED BEHAVIOR: unchanged: approved contract preserved\nALWAYS ASK: architecture change already approved\n" + tc.scoped
		if got := f.finishedWorkReason(a, "architecture", report, ""); (got != "") != tc.want {
			t.Fatalf("%q: %q", tc.scoped, got)
		}
	}
}
