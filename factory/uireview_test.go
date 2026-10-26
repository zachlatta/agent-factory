package factory

import (
	"strings"
	"testing"
	"time"
)

func TestTouchesUI(t *testing.T) {
	for _, tc := range []struct {
		files []string
		want  bool
	}{
		{[]string{"web/src/routes/+page.svelte"}, true},
		{[]string{"factory/policy.go", "factory/web/board.html"}, true},
		{[]string{"x.css"}, true},
		{[]string{"app/components/Button.tsx"}, true},
		{[]string{"templates/mail.txt"}, true},
		{[]string{"factory/policy.go", "README.md"}, false},
		{[]string{"cmd/web.go"}, false},
		{nil, false},
	} {
		if got := touchesUI(tc.files); got != tc.want {
			t.Errorf("touchesUI(%v) = %v, want %v", tc.files, got, tc.want)
		}
	}
}

func reviewPrompt(t *testing.T, kind string, git map[string]string) (string, *Factory) {
	t.Helper()
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review"}))
	if git != nil {
		f.GitOut = gitStub(git)
	}
	if _, err := f.runReviewer(Card{ID: "c1", Short: "c1", Title: "x", Cwd: "/w/c1", Repo: "example/desk"}, kind, "private", "t", "", "h1"); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelReviewKind, kind))
	if len(runs) != 1 {
		t.Fatalf("runs = %v", p.calls)
	}
	return runs[0][len(runs[0])-1], f
}

func diffFiles(files string) map[string]string {
	return map[string]string{"diff --name-only origin/main...HEAD": files, "status --porcelain": ""}
}

func TestUIReviewRuleOnlyForFrontendChanges(t *testing.T) {
	for _, kind := range []string{"code", "land"} {
		if pr, _ := reviewPrompt(t, kind, diffFiles("web/a.svelte\nmain.go\n")); !strings.Contains(pr, uiReviewRule) {
			t.Errorf("%s review of a frontend change lacks the UI rule:\n%s", kind, pr)
		}
		if pr, _ := reviewPrompt(t, kind, diffFiles("main.go\n")); strings.Contains(pr, "UI check:") {
			t.Errorf("%s review of a Go-only change has the UI rule", kind)
		}
	}
	if pr, _ := reviewPrompt(t, "plan", diffFiles("web/a.svelte\n")); strings.Contains(pr, "UI check:") {
		t.Error("plan review has the UI rule")
	}
}

func TestUIReviewRuleWhenFilesUnknown(t *testing.T) {
	pr, f := reviewPrompt(t, "code", map[string]string{}) // every git call fails
	if !strings.Contains(pr, uiReviewRule) {
		t.Error("git failed: the UI rule must be included")
	}
	if logs := strings.Join(f.log, "\n"); !strings.Contains(logs, "UI rule included") {
		t.Errorf("no log line: %s", logs)
	}
}

func TestUIFeedback(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain": {"Looks fine.\n\nUI feedback:\n- the button is small\n- copy says Sumbit\n\nTIER: low\nVERDICT: nits",
			"UI feedback:\n- the button is small\n- copy says Sumbit"},
		"bold heading":     {"**UI feedback:**\n* tap target too small\n", "UI feedback:\n- tap target too small"},
		"markdown heading": {"## UI feedback\n- header wraps at 390 px\nTIER: low", "UI feedback:\n- header wraps at 390 px"},
		"then paragraph":   {"UI feedback:\n- a\n\nOther notes follow here.\n- not feedback", "UI feedback:\n- a"},
		"none":             {"All good.\nTIER: low\nVERDICT: approve", ""},
		"no bullets":       {"UI feedback:\nnone\nVERDICT: approve", ""},
		"last block":       {"UI feedback:\n- old\n\nRevised.\nUI feedback:\n- new\n", "UI feedback:\n- new"},
	} {
		if got := uiFeedback(tc.in); got != tc.want {
			t.Errorf("%s: uiFeedback = %q, want %q", name, got, tc.want)
		}
	}
	long := "UI feedback:\n" + strings.Repeat("- "+strings.Repeat("x", 99)+"\n", 40)
	if got := uiFeedback(long); len(got) > uiFeedbackCap {
		t.Errorf("len = %d, want at most %d", len(got), uiFeedbackCap)
	}
}

const reviewWithUI = "Clicked through it with computer use.\n\nUI feedback:\n- the Save button is hard to see\n\nTIER: low\nVERDICT: "

func TestUIFeedbackGoesWithTheLandingMessage(t *testing.T) {
	for _, verdict := range []string{"approve", "nits"} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
			reviewer("r1", "c1", "code"))
		p.logs["r1"] = reviewWithUI + verdict
		f.ApplyVerdict(f.agent("r1"))
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") || !strings.Contains(msgs[0], "- the Save button is hard to see") ||
			!strings.Contains(msgs[0], "Do not change code for it") {
			t.Errorf("%s: sent = %v", verdict, msgs)
		}
	}
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	p.logs["r1"] = "TIER: low\nVERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	if msgs := p.sent("c1"); len(msgs) != 1 || strings.Contains(msgs[0], "UI feedback") {
		t.Errorf("no feedback: sent = %v", msgs)
	}
}

func TestUIFeedbackReachesABuilderWaitingForZach(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board",
		labelOrigin: "idea", labelPlanRounds: "1"}), reviewer("r1", "c1", "code"))
	fakeScan(f, nil, nil)
	p.logs["r1"] = reviewWithUI + "approve"
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" {
		t.Fatalf("labels = %v", l)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "- the Save button is hard to see") || !strings.Contains(msgs[0], "Noted") ||
		strings.Contains(msgs[0], "Land it now") {
		t.Fatalf("sent = %v", msgs)
	}
	// the builder answers "Noted" and stops: the card stays with Zach, no review or landing starts
	a := f.agent("c1")
	for k, v := range l {
		a.Labels[labelPrefix+k] = v
	}
	a.LastActivityAt = t0.Add(time.Minute)
	f.SetAgents([]Agent{a})
	if err := f.tickAgent(a, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "zach" {
		t.Errorf("stage after the Noted turn = %q", got)
	}
	if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 0 || len(p.sent("c1")) != 1 {
		t.Errorf("the Noted turn started work: %v", p.calls)
	}
}
