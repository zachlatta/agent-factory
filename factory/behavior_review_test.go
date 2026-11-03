package factory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	f.GitOut = gitStub(map[string]string{"merge-base " + reviewedSHA + " origin/main": "base", "range-diff --no-color --no-ext-diff base.." + reviewedSHA + " origin/main..HEAD": "fixture repair"})
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

func TestRepairComparisonAfterMainAdvances(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_COMMITTER_NAME=Test", "GIT_AUTHOR_EMAIL=test@users.noreply.github.com", "GIT_COMMITTER_EMAIL=test@users.noreply.github.com")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(file, text string) {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	write("base", "base")
	git("add", ".")
	git("commit", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("checkout", "-b", "card")
	write("feature", "approved")
	git("add", ".")
	git("commit", "-m", "feature")
	approved := git("rev-parse", "HEAD")
	git("checkout", "main")
	write("AGENTS.md", "unrelated rule")
	git("add", ".")
	git("commit", "-m", "other work")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("checkout", "card")
	git("rebase", "main")
	write("fixture_test.go", "repair")
	git("add", ".")
	git("commit", "-m", "repair")
	f, _ := newTestFactory(t)
	f.GitOut = func(ctx context.Context, cwd string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = cwd
		out, err := c.Output()
		return string(out), err
	}
	files, err := f.repairFiles(context.Background(), dir, approved)
	if err != nil || files != "fixture_test.go" {
		t.Fatalf("repair files %q: %v", files, err)
	}
	diff, err := f.repairRangeDiff(context.Background(), dir, approved)
	if err != nil || strings.Contains(diff, "unrelated rule") || !strings.Contains(diff, "repair") {
		t.Fatalf("range diff %q: %v", diff, err)
	}
	if reason := f.alwaysAskFilesSince(dir, "repair:"+approved); reason != "" {
		t.Fatal(reason)
	}
	write("AGENTS.md", "new repair rule")
	git("add", ".")
	git("commit", "-m", "repair rule")
	if reason := f.alwaysAskFilesSince(dir, "repair:"+approved); !strings.Contains(reason, "AGENTS.md") {
		t.Fatal(reason)
	}
}
