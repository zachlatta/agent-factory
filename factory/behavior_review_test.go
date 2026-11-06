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
	diff, err := f.repairRangeDiff(context.Background(), dir, approved)
	if err != nil || strings.Contains(diff, "unrelated rule") || !strings.Contains(diff, "repair") {
		t.Fatalf("range diff %q: %v", diff, err)
	}
}

func TestApprovedBehaviorPromptShowsTheRepairDiff(t *testing.T) {
	f, _ := newTestFactory(t)
	f.GitOut = gitStub(map[string]string{"merge-base abc origin/main": "base\n", "range-diff --no-color --no-ext-diff base..abc origin/main..HEAD": "the repair diff\n"})
	a := card("c1", map[string]string{labelBehaviorApproval: "abc", labelBehaviorSummary: "c3VtbWFyeQ"})
	got := f.approvedBehaviorPrompt(a)
	for _, want := range []string{"APPROVED BEHAVIOR:", "REPAIR SIGN OFF:", "the repair diff", "summary"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q: %s", want, got)
		}
	}
}
