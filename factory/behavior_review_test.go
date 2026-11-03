package factory

import (
	"strings"
	"testing"
)

func TestParseApprovedBehavior(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", "unsure"},
		{"APPROVED BEHAVIOR: <unchanged|changed|unsure and reason>", "unsure"},
		{"APPROVED BEHAVIOR: unchanged", "unsure"},
		{"APPROVED BEHAVIOR: unchanged: only fixture timing changed", "unchanged"},
		{"**APPROVED BEHAVIOR:** unchanged — only fixture timing changed", "unchanged"},
		{"APPROVED BEHAVIOR: changed: new output", "changed"},
		{"APPROVED BEHAVIOR: unsure: baseline missing", "unsure"},
		{"APPROVED BEHAVIOR: unchanged: fixtures only\nAPPROVED BEHAVIOR: changed: output differs", "unsure"},
		{"APPROVED BEHAVIOR: unchanged: fixtures only\nAPPROVED BEHAVIOR: unsure: missing report", "unsure"},
		{"APPROVED BEHAVIOR: unchanged: fixtures only\nAPPROVED BEHAVIOR: unchanged: outputs match", "unchanged"},
	} {
		if got := ParseApprovedBehavior(tc.text); got != tc.want {
			t.Errorf("ParseApprovedBehavior(%q)=%q want %q", tc.text, got, tc.want)
		}
	}
}

func TestPanelApprovedBehaviorRequiresEveryFinding(t *testing.T) {
	for _, tc := range []struct{ second, want string }{
		{"APPROVED BEHAVIOR: unchanged: outputs match", "unchanged"},
		{"No assessment", "unsure"},
		{"APPROVED BEHAVIOR: changed: output differs", "unsure"},
		{strings.Repeat("long review text ", 500) + "\nAPPROVED BEHAVIOR: changed: output differs", "unsure"},
	} {
		a := panelCard(map[string]string{labelBehaviorApproval: reviewedSHA})
		r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
		r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
		f, p := newTestFactory(t, a, r1, r2)
		storeLog(t, f, p, "r1", "APPROVED BEHAVIOR: unchanged: outputs match\nVERDICT: approve")
		storeLog(t, f, p, "r2", tc.second+"\nVERDICT: approve")
		reports, err := f.panelReports(a, "1")
		if err != nil {
			t.Fatal(err)
		}
		if got := ParseApprovedBehavior(reports); got != tc.want {
			t.Errorf("second=%q got %s want %s", tc.second, got, tc.want)
		}
	}
}

func TestArbiterGetsApprovedBaseline(t *testing.T) {
	a := card("c1", map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ"})
	f, p := newTestFactory(t, a)
	f.GitOut = gitStub(map[string]string{"diff --no-ext-diff --no-renames " + reviewedSHA + "..HEAD": "fixture repair"})
	if err := f.startArbiter(a, "code", "Review findings", false); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelPrefix + labelArbiter + "=1")
	if len(runs) != 1 {
		t.Fatalf("arbiter runs=%d", len(runs))
	}
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{reviewedSHA, "summary", "fixture repair", "APPROVED BEHAVIOR:"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing %q", want)
		}
	}
}
