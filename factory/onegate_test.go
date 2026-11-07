package factory

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// greenGate is a gate reply where every check passes and Zach need not sign off.
const greenGate = "[Assistant] Checked.\nMEETS REQUEST: green - does what was asked\nWORKS: green - go test ./... passed\n" +
	"DESIGN: green - cohesive\nPUBLIC SAFETY: n/a\nSIGN OFF: green - he asked for exactly this\nVERDICT: approve"

func gateReply(kv ...string) string {
	lines := map[string]string{"MEETS REQUEST": "green", "WORKS": "green", "DESIGN": "green", "PUBLIC SAFETY": "green", "SIGN OFF": "green"}
	for i := 0; i+1 < len(kv); i += 2 {
		lines[kv[i]] = kv[i+1]
	}
	var b strings.Builder
	b.WriteString("[Assistant] Checked.\n")
	for _, k := range []string{"MEETS REQUEST", "WORKS", "DESIGN", "PUBLIC SAFETY", "SIGN OFF"} {
		if v := lines[k]; v != "" {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	b.WriteString("VERDICT: approve")
	return b.String()
}

func TestBuilderStopStartsTheOneGate(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSource: "board",
		labelAutoReview: gateCheck, labelSince: ago(2 * time.Minute)}))
	storeLog(t, f, p, "c1", "[Assistant] Built it.\nRESULT: code\nTITLE: Built it")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != gateCheck {
		t.Fatalf("stage = %q, want %q", got, gateCheck)
	}
	prompt := p.promptOfKind(gateCheck)
	for _, want := range []string{"gatekeeper", "MEETS REQUEST:", "WORKS:", "DESIGN:", "PUBLIC SAFETY:", "SIGN OFF:",
		"green", "yellow", "red", "unsaid context", "latest origin/main", "Small refactors", "Do not read the start-here skill"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("gate prompt misses %q", want)
		}
	}
	if strings.Contains(prompt, "Read-only. Do not edit") {
		t.Errorf("the gate does small fixes itself, so it is not read-only")
	}
}

func TestAllGreenGateLandsWithoutZach(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board", labelSubmittedBy: submittedByZach, labelReviewed: "abc"}),
		reviewer("r1", "c1", gateCheck))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", greenGate)
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "landing" {
		t.Fatalf("stage = %q, note %q", l[labelStage], l[labelNote])
	}
	if got := l[labelGrades]; got != "request:green works:green design:green public:n/a signoff:green" {
		t.Errorf("grades label = %q", got)
	}
	if l[roundsLabel(gateCheck)] != "1" {
		t.Errorf("gate rounds = %q", l[roundsLabel(gateCheck)])
	}
}

func TestSignOffNotGreenComesToZach(t *testing.T) {
	for _, grade := range []string{"red - the system submitted it", "yellow - it adds a new table he did not ask for"} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "idea", labelReviewed: "abc"}),
			reviewer("r1", "c1", gateCheck))
		headIs(f, "abc", nil)
		storeLog(t, f, p, "r1", gateReply("PUBLIC SAFETY", "n/a", "SIGN OFF", grade))
		storeLog(t, f, p, "c1", "[Assistant] Built.")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		why := strings.SplitN(grade, " - ", 2)[1]
		if l[labelStage] != "zach" || !strings.Contains(l[labelAskWhy], why) {
			t.Errorf("%s: stage %q why %q", grade, l[labelStage], l[labelAskWhy])
		}
	}
}

func TestSignOffNeverSendsWorkBack(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "idea", labelReviewed: "abc"}),
		reviewer("r1", "c1", gateCheck))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", gateReply("PUBLIC SAFETY", "n/a", "SIGN OFF", "red - system work"))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] == "building" {
		t.Fatalf("a red sign-off goes to Zach, not the builder: %v", l)
	}
}

func TestAnyNonGreenCheckGoesBackToTheBuilder(t *testing.T) {
	for _, tc := range []struct{ line, grade, name string }{
		{"MEETS REQUEST", "yellow - skipped the export", "meets request"},
		{"WORKS", "red - the test fails", "works"},
		{"DESIGN", "yellow - duplicates the parser", "design"},
		{"WORKS", "", "works"}, // a missing grade is not green
	} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board", labelSubmittedBy: submittedByZach}),
			reviewer("r1", "c1", gateCheck))
		storeLog(t, f, p, "r1", gateReply("PUBLIC SAFETY", "n/a", tc.line, tc.grade))
		storeLog(t, f, p, "c1", "[Assistant] Built.")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] != "building" || l[labelAutoReview] != gateCheck {
			t.Errorf("%s %q: stage %q auto-review %q", tc.line, tc.grade, l[labelStage], l[labelAutoReview])
			continue
		}
		sent := strings.Join(p.sent("c1"), "\n")
		if !strings.Contains(sent, "gate") || !strings.Contains(strings.ToLower(sent), tc.name) {
			t.Errorf("%s: the builder is not told which check failed: %q", tc.line, trunc(sent, 400))
		}
	}
}

func TestChangesVerdictGoesBackEvenWhenGradesAreGreen(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board"}),
		reviewer("r1", "c1", gateCheck))
	storeLog(t, f, p, "r1", strings.Replace(greenGate, "VERDICT: approve", "VERDICT: changes", 1))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "building" {
		t.Fatalf("stage %q", l[labelStage])
	}
}

func TestPublicSafetyCountsInPublicReposOnly(t *testing.T) {
	// private: no PUBLIC SAFETY line is fine
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board", labelSubmittedBy: submittedByZach, labelReviewed: "abc"}),
		reviewer("r1", "c1", gateCheck))
	headIs(f, "abc", nil)
	storeLog(t, f, p, "r1", gateReply("PUBLIC SAFETY", ""))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("c1")[labelStage]; got != "landing" {
		t.Errorf("private repo without a public grade: stage %q note %q", got, p.labels("c1")[labelNote])
	}
	// public: yellow, red or missing goes back
	for _, grade := range []string{"yellow - names an internal host", "red - a token in config.go", "", "n/a"} {
		f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: gateCheck, labelSource: "board", labelSubmittedBy: submittedByZach, labelReviewed: reviewedSHA}),
			reviewer("r1", "c1", gateCheck))
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", gateReply("PUBLIC SAFETY", grade))
		storeLog(t, f, p, "c1", "[Assistant] Built.")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); l[labelStage] != "building" {
			t.Errorf("public repo, public safety %q: stage %q", grade, l[labelStage])
		}
	}
}

func TestPublicRepoGatePromptHasTheSafetyCheck(t *testing.T) {
	c := Card{ID: "c1", Title: "t", Repo: publicRepo, SubmittedBy: submittedByZach}
	got := ReviewPrompt(c, gateCheck, 7478, "public")
	for _, want := range []string{"noreply", "bin/scan", "rewrite", "weaken"} {
		if !strings.Contains(got, want) {
			t.Errorf("public safety check misses %q", want)
		}
	}
}

func TestOneGateRoundCapMarksTheCardStuck(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board", roundsLabel(gateCheck): "2"}),
		reviewer("r1", "c1", gateCheck))
	storeLog(t, f, p, "r1", gateReply("DESIGN", "red - wrong layer"))
	storeLog(t, f, p, "c1", "[Assistant] Built.")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStuck] == "" || (l[labelStage] != "" && l[labelStage] != gateCheck) {
		t.Fatalf("labels %v", l)
	}
}

func TestParseGrades(t *testing.T) {
	text := "**MEETS REQUEST:** Green - all of it\nWORKS: `yellow` (could not run the UI)\nDESIGN: red\nPUBLIC SAFETY: N/A\nSIGN OFF: green\nVERDICT: changes"
	g := ParseGrades(text)
	want := map[string]string{checkRequest: "green", checkWorks: "yellow", checkDesign: "red", checkPublic: "n/a", checkSignoff: "green"}
	for k, v := range want {
		if g[k] != v {
			t.Errorf("%s = %q, want %q", k, g[k], v)
		}
	}
	if g := ParseGrades("MEETS REQUEST: <green|yellow|red>\nMEETS REQUEST: green"); g[checkRequest] != "green" {
		t.Errorf("a placeholder line counts: %v", g)
	}
}

func TestParseSignOffReadsGrades(t *testing.T) {
	for in, want := range map[string]string{
		"SIGN OFF: green":                          "none",
		"SIGN OFF: green - he asked for this":      "none",
		"SIGN OFF: none":                           "none",
		"SIGN OFF: red - the system submitted it":  "the system submitted it",
		"SIGN OFF: yellow — adds a new dependency": "adds a new dependency",
		"SIGN OFF: red":                            "red",
	} {
		if got := ParseSignOff(in); got != want {
			t.Errorf("ParseSignOff(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOldGateCardsMoveToTheOneGate(t *testing.T) {
	for _, old := range []string{"request", "design", "signoff"} {
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: old, labelSource: "board", labelReviewed: "abc"}),
			reviewer("r1", "c1", old))
		storeLog(t, f, p, "c1", "[Assistant] Built.\nRESULT: code")
		storeLog(t, f, p, "r1", "[Assistant] Old verdict.\nVERDICT: approve")
		if err := f.tickAgent(f.agent("c1"), t0); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] != gateCheck && !(l[labelStage] == "building" && l[labelAutoReview] == gateCheck) {
			t.Errorf("%s: stage %q auto-review %q", old, l[labelStage], l[labelAutoReview])
		}
	}
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "building", labelSource: "board", labelAutoReview: "design", labelSince: ago(2 * time.Minute)}))
	storeLog(t, f, p, "c1", "[Assistant] Built.\nRESULT: code")
	if err := f.tickAgent(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != gateCheck {
		t.Errorf("a card queued for an old gate goes to the one gate: %v", l)
	}
}

func TestOldGateVerdictMovesNothing(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: gateCheck, labelSource: "board"}), reviewer("r1", "c1", "design"))
	storeLog(t, f, p, "r1", "[Assistant] Old.\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "" || l[labelAutoReview] != "" {
		t.Fatalf("an old reviewer's verdict moved the card: %v", l)
	}
}

func TestBoardHasOneGateColumn(t *testing.T) {
	var ids []string
	for _, s := range Stages {
		ids = append(ids, s[0])
	}
	got := strings.Join(ids, " ")
	if got != "idea route building gate zach landing landed" {
		t.Fatalf("stages = %q", got)
	}
	if Derive(card("c1", map[string]string{labelStage: "design"})) != gateCheck {
		t.Errorf("an old gate column shows as the gate")
	}
}

func TestNewCardBranchesFromFreshOriginMain(t *testing.T) {
	f, p := newTestFactory(t)
	f.Git, f.GitOut = runGit, runGitOut
	remote := filepath.Join(f.cfg.Home, "remote")
	initTestRepo(t, remote, "main")
	repoDir := filepath.Join(f.cfg.Home, "dev", "example/desk")
	if out, err := exec.Command("git", "clone", "-q", remote, repoDir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	// main moves on upstream after the checkout was cloned
	if err := f.Git(context.Background(), remote, "-c", "user.email=1+bot@users.noreply.github.com", "-c", "user.name=bot", "commit", "-q", "--allow-empty", "-m", "newer"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("new", Body{Request: "Rotate the backup key", Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(runArgs(p)[0], "--base"); got != "origin/main" {
		t.Errorf("--base = %q, want origin/main", got)
	}
	upstream, _ := f.GitOut(context.Background(), remote, "rev-parse", "HEAD")
	fetched, _ := f.GitOut(context.Background(), repoDir, "rev-parse", "origin/main")
	if strings.TrimSpace(upstream) != strings.TrimSpace(fetched) {
		t.Errorf("origin/main was not fetched before branching: %q vs %q", fetched, upstream)
	}
}

func TestRunGateButtonStartsTheOneGate(t *testing.T) {
	for _, kind := range []string{gateCheck, "request", ""} { // "request": a board page from before the one gate
		f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board"}))
		storeLog(t, f, p, "c1", "[Assistant] Built.\nRESULT: code")
		if _, err := f.Act("review", Body{ID: "c1", Kind: kind}); err != nil {
			t.Fatal(err)
		}
		if got := p.labels("c1")[labelStage]; got != gateCheck || len(p.runsWithLabel(labelArg(labelReviewKind, gateCheck))) != 1 {
			t.Errorf("kind %q: stage %q, gate runs %d", kind, got, len(p.runsWithLabel(labelArg(labelReviewKind, gateCheck))))
		}
	}
}
