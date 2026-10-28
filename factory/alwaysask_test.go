package factory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"**/AGENTS.md", "AGENTS.md", true},
		{"**/AGENTS.md", "sub/dir/AGENTS.md", true},
		{"**/AGENTS.md", "AGENTS.md.bak", false},
		{"deploy/**", "deploy/agent-factory.service", true},
		{"deploy/**", "deployer/main.go", false},
		{"factory/scan/**", "factory/scan/scan.go", true},
		{"factory/policy.go", "factory/policy.go", true},
		{"factory/policy.go", "factory/policy_test.go", false},
		{"**/*.service", "x/y/z.service", true},
		{"**/requirements*.txt", "requirements-dev.txt", true},
		{"[", "[", false}, // a bad pattern matches nothing
	} {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestAlwaysAskFiles(t *testing.T) {
	const diff = "diff --no-renames --name-only origin/main...HEAD"
	const cfg = "show origin/main:" + repoConfigFile
	for _, tc := range []struct {
		name   string
		answer map[string]string
		want   string // substring of the reason; "" means no reason
	}{
		{"nothing on the list", map[string]string{diff: "factory/board.go\nREADME.md\n"}, ""},
		{"agent rules", map[string]string{diff: "README.md\nAGENTS.md\n"}, "AGENTS.md"},
		{"a new service", map[string]string{diff: "deploy/x.service\n"}, "deploy/x.service"},
		{"a dependency", map[string]string{diff: "go.mod\n"}, "go.mod"},
		{"the card edits the repo's list", map[string]string{diff: repoConfigFile + "\n"}, repoConfigFile},
		{"the repo's own pattern on main", map[string]string{diff: "factory/policy.go\n", cfg: `{"alwaysAsk":["factory/policy.go"]}`}, "factory/policy.go"},
		{"no pattern on main for it", map[string]string{diff: "factory/board.go\n", cfg: `{"alwaysAsk":["factory/policy.go"]}`}, ""},
		{"an unreadable list asks", map[string]string{diff: "factory/board.go\n", cfg: `{not json`}, "could not read"},
		{"no diff asks", map[string]string{}, "could not check"},
		{"many files are cut", map[string]string{diff: "go.mod\nAGENTS.md\nCLAUDE.md\ndeploy/a\n"}, "…"},
	} {
		f, _ := newTestFactory(t)
		f.GitOut = gitStub(tc.answer)
		got := f.alwaysAskFiles("/w/c1")
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: alwaysAskFiles = %q, want %q", tc.name, got, tc.want)
		}
	}
	f, _ := newTestFactory(t)
	if got := f.alwaysAskFiles(""); got == "" {
		t.Error("a card with no worktree must ask")
	}
}

// The repo's list is read from origin/main: a card cannot loosen it by editing its own copy.
func TestAlwaysAskReadsTheListFromMain(t *testing.T) {
	var asked []string
	f, _ := newTestFactory(t)
	f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
		asked = append(asked, strings.Join(args, " "))
		return gitStub(map[string]string{
			"diff --no-renames --name-only origin/main...HEAD": "factory/policy.go\n",
			"show origin/main:" + repoConfigFile:               `{"alwaysAsk":["factory/policy.go"]}`,
		})(nil, "", args...)
	}
	if got := f.alwaysAskFiles("/w/c1"); !strings.Contains(got, "factory/policy.go") {
		t.Errorf("reason = %q", got)
	}
	for _, a := range asked {
		if strings.HasPrefix(a, "show") && a != "show origin/main:"+repoConfigFile {
			t.Errorf("read the list from somewhere else: %q", a)
		}
	}
}

// This repo's own list parses and names its gates.
func TestRepoAlwaysAskListParses(t *testing.T) {
	raw, err := os.ReadFile("../" + repoConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var rc repoConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"factory/policy.go", "factory/alwaysask.go", "factory/arbiter.go", "factory/scan/scan.go", "bin/land"} {
		if len(alwaysAskHits([]string{gate}, rc.AlwaysAsk)) != 1 {
			t.Errorf("%s is not on this repo's always-ask list", gate)
		}
	}
}

func TestParseAlwaysAsk(t *testing.T) {
	for in, want := range map[string]string{
		"ALWAYS ASK: none": "none",
		"**ALWAYS ASK:** None. It only changes copy.":          "none",
		"ALWAYS ASK: no, inside the ask":                       "none",
		"- ALWAYS ASK: it changes the landing gate\nTIER: low": "it changes the landing gate",
		"ALWAYS ASK: <none, or why Zach must see this>":        "",
		"no line": "",
	} {
		if got := ParseAlwaysAsk(in); got != want {
			t.Errorf("ParseAlwaysAsk(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ParseAlwaysAsk(alwaysAskRule); got != "" {
		t.Errorf("the rule matched itself: %q", got)
	}
}

func TestFinishedWorkReason(t *testing.T) {
	sched := card("c1", map[string]string{labelPlanRounds: "1"})
	sched.Labels["paseo.schedule-id"] = "s1"
	inbox := inboxCard("c1", map[string]string{labelPlanRounds: "1"})
	for _, tc := range []struct {
		name   string
		a      Agent
		tier   string
		review string
		want   string // "" lands on its own
	}{
		{"idea, nothing on the list", idea("c1", map[string]string{labelPlanRounds: "1"}), "low", "ALWAYS ASK: none", ""},
		{"scheduled run", sched, "risky", "ALWAYS ASK: none", ""},
		{"Scout", card("c1", map[string]string{labelRole: "scout", labelPlanRounds: "1"}), "low", "ALWAYS ASK: none", ""},
		{"board card", card("c1", map[string]string{labelSource: "board", labelPlanRounds: "1"}), "low", "ALWAYS ASK: none", "you asked for it"},
		{"email card", inbox, "low", "ALWAYS ASK: none", "you asked for it"},
		{"unknown origin", card("c1", map[string]string{labelPlanRounds: "1"}), "low", "ALWAYS ASK: none", "you asked for it"},
		{"idea, architecture", idea("c1", map[string]string{labelPlanRounds: "1"}), "architecture", "ALWAYS ASK: none", "architecture"},
		{"idea, reviewer asks", idea("c1", map[string]string{labelPlanRounds: "1"}), "low", "ALWAYS ASK: it changes a gate", "it changes a gate"},
		{"idea, no reviewer line", idea("c1", map[string]string{labelPlanRounds: "1"}), "low", "VERDICT: approve", "did not say"},
		{"idea, no plan review", idea("c1", nil), "low", "ALWAYS ASK: none", "no plan review"},
	} {
		f, _ := newTestFactory(t)
		if got := f.finishedWorkReason(tc.a, tc.tier, tc.review, ""); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
	// the file check counts for work the factory started
	f, _ := newTestFactory(t)
	f.GitOut = gitStub(map[string]string{"diff --no-renames --name-only origin/main...HEAD": "AGENTS.md\n"})
	if got := f.finishedWorkReason(idea("c1", map[string]string{labelPlanRounds: "1"}), "low", "ALWAYS ASK: none", ""); !strings.Contains(got, "AGENTS.md") {
		t.Errorf("file check: reason = %q", got)
	}
}

// A factory-started card that touches a gate file stops for Zach even when its reviewer saw nothing.
func TestIdeaTouchingAGateComesToZach(t *testing.T) {
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	f.GitOut = gitStub(map[string]string{"diff --no-renames --name-only origin/main...HEAD": ".githooks/pre-push\n"})
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "zach" || !isReadyNote(l[labelNote]) || !strings.Contains(l[labelAskWhy], ".githooks/pre-push") {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || strings.Contains(msgs[0], "Land it now") || !strings.Contains(msgs[0], ".githooks/pre-push") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestCodeRubricAsksForTheAlwaysAskLine(t *testing.T) {
	prompt := ReviewPrompt(Card{ID: "c1", Repo: "example/desk"}, "code", 7478, "unknown")
	if !strings.Contains(prompt, "ALWAYS ASK: <none") {
		t.Errorf("code review prompt lacks the ALWAYS ASK line")
	}
}
