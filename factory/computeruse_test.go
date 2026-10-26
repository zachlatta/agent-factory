package factory

import (
	"strings"
	"testing"
)

const computerUseHeading = "Computer use through PDW:"

func checkComputerUsePrompt(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		computerUseHeading,
		`command -v pdw`,
		`pdw call skills__skill_read --data '{"name":"start-here"}'`,
		`pdw call skills__skill_read --data '{"name":"openai-computer-use"}'`,
		"pdw describe", "pdw call", "separate shell calls",
		"does not require a local desktop", "Do not open a browser at startup",
		"`tailscale ip -4`", "dry-run mode the repo's README documents",
		"Close every tab you opened", "stop every server you started",
		"does not authorize", "Do not change host settings",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("computer-use prompt lacks %q", want)
		}
	}
	if got := strings.Count(prompt, computerUseHeading); got != 1 {
		t.Errorf("shared rule appears %d times, want once", got)
	}
}

func TestFellowsGetComputerUseForEveryAgent(t *testing.T) {
	for _, a := range AgentChoices {
		t.Run(a.ID, func(t *testing.T) {
			f, p := newTestFactory(t)
			if _, err := f.Act("new", Body{Request: "Check a public page", Agent: a.ID}); err != nil {
				t.Fatal(err)
			}
			runs := p.runsWithLabel(labelArg(labelStage, "plan"))
			if len(runs) != 1 {
				t.Fatalf("runs = %v", runs)
			}
			prompt := runs[0][len(runs[0])-1]
			checkComputerUsePrompt(t, prompt)
			for _, want := range []string{"direct track", "Click through your own UI changes", "before code review", "do NOT land"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("Fellow prompt lacks %q", want)
				}
			}
		})
	}
}

func TestUIReviewsShareComputerUseAndKeepRestrictions(t *testing.T) {
	for _, kind := range []string{"code", "land"} {
		prompt, _ := reviewPrompt(t, kind, diffFiles("web/page.html\n"))
		checkComputerUsePrompt(t, prompt)
		for _, want := range []string{
			"Read-only", "Do not edit, commit, push, land",
			"phone width", "UI feedback:", "gateway is full",
			"skip the check", "a skip alone is not", "except inside such a dry run",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s review lacks %q", kind, want)
			}
		}
	}
	for _, tc := range []struct{ kind, files string }{
		{"plan", "web/page.html\n"}, {"code", "main.go\n"}, {"land", "main.go\n"},
	} {
		prompt, _ := reviewPrompt(t, tc.kind, diffFiles(tc.files))
		if strings.Contains(prompt, computerUseHeading) {
			t.Errorf("%s review of %q received browser instructions", tc.kind, tc.files)
		}
	}
}

func TestScoutsAndPickersDoNotGetComputerUse(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk", "example/ops")
	if _, err := f.Act("scout", Body{Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("new", Body{Request: "Choose a repo", Repo: "auto"}); err != nil {
		t.Fatal(err)
	}
	for _, run := range runArgs(p) {
		if strings.Contains(run[len(run)-1], computerUseHeading) {
			t.Error("Scout or picker received browser instructions")
		}
	}
}
