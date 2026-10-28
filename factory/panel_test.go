package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func lensReviewer(id, of, lens, panel string) Agent {
	r := reviewer(id, of, "code")
	r.Labels[labelPrefix+labelLens] = lens
	r.Labels[labelPrefix+labelPanel] = panel
	return r
}

// panelCard is a planned card in code review with panel round 1 open.
func panelCard(labels map[string]string) Agent {
	l := map[string]string{labelStage: "review", labelTier: "risky", labelPlanRounds: "1", labelPanel: "1",
		labelSince: ago(10 * time.Minute)}
	for k, v := range labels {
		l[k] = v
	}
	return card("c1", l)
}

func lensOf(run []string) string { return argLabel(run, labelLens) }

func argLabel(run []string, key string) string {
	for i := range run {
		if run[i] == "--label" && i+1 < len(run) && strings.HasPrefix(run[i+1], labelPrefix+key+"=") {
			return strings.TrimPrefix(run[i+1], labelPrefix+key+"=")
		}
	}
	return ""
}

func TestReviewLensesScaleWithRisk(t *testing.T) {
	small := diffStat{files: []string{"factory/x.go", "factory/x_test.go"}, lines: 40}
	cases := []struct {
		name string
		tier string
		d    diffStat
		want []string
	}{
		{"small risky change: one reviewer", "risky", small, nil},
		{"small low change: one reviewer", "low", small, nil},
		{"architecture: panel", "architecture", small, []string{lensCorrectness, lensArchitecture}},
		{"large change: panel", "low", diffStat{files: []string{"a.go"}, lines: panelLines + 1}, []string{lensCorrectness, lensArchitecture}},
		{"tests do not count", "low", diffStat{files: []string{"a.go", "a_test.go"}, lines: 10}, nil},
		{"UI change: panel with UX", "low", diffStat{files: []string{"factory/web/board.html"}, lines: 3},
			[]string{lensCorrectness, lensArchitecture, lensUX}},
		{"secret scan change: panel", "low", diffStat{files: []string{"factory/scan/scan.go"}, lines: 2}, []string{lensCorrectness, lensArchitecture}},
		{"hook change: panel", "low", diffStat{files: []string{".githooks/pre-push"}, lines: 1}, []string{lensCorrectness, lensArchitecture}},
		{"unknown diff: one reviewer", "risky", diffStat{err: errors.New("no git")}, nil},
		{"unknown diff, architecture: panel", "architecture", diffStat{err: errors.New("no git")}, []string{lensCorrectness, lensArchitecture}},
	}
	for _, c := range cases {
		if got := reviewLenses(c.tier, c.d); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: lenses = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDiffStatSkipsTestLines(t *testing.T) {
	f, _ := newTestFactory(t)
	f.GitOut = gitStub(map[string]string{"diff --numstat origin/main...HEAD": "100\t20\tfactory/a.go\n300\t0\tfactory/a_test.go\n-\t-\tlogo.png\n"})
	d := f.diffStat("/w/c1")
	if d.err != nil || d.lines != 120 || len(d.files) != 3 {
		t.Errorf("diffStat = %+v", d)
	}
}

func TestCodeReviewStartsPanelOnOneHead(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelTier: "risky",
		labelPlanRounds: "1", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": "h1\n",
		"diff --numstat origin/main...HEAD":   "200\t10\tfactory/a.go\n4\t1\tfactory/web/board.html\n",
		"diff --name-only origin/main...HEAD": "factory/a.go\nfactory/web/board.html\n"})
	storeLog(t, f, p, "c1", "Built it.\nTIER: risky")
	f.Tick()
	runs := p.runsWithLabel(labelPrefix + labelReviewKind + "=code")
	var lenses []string
	for _, r := range runs {
		lenses = append(lenses, lensOf(r))
		if argLabel(r, labelPanel) != "1" {
			t.Errorf("reviewer not in panel 1: %v", r)
		}
		ui := strings.Contains(r[len(r)-1], "UI check:")
		if ui != (lensOf(r) == lensUX) {
			t.Errorf("%s reviewer has UI rule = %v", lensOf(r), ui)
		}
	}
	if strings.Join(lenses, ",") != "correctness,architecture,ux" {
		t.Fatalf("lenses = %v", lenses)
	}
	if l := p.labels("c1"); l[labelPanel] != "1" || l[labelReviewed] != "h1" || l[labelStage] != "review" {
		t.Errorf("card labels = %v", l)
	}
}

func TestSmallCodeChangeGetsOneReviewer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelTier: "risky",
		labelPlanRounds: "1", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": "h1\n",
		"diff --numstat origin/main...HEAD": "20\t5\tfactory/a.go\n", "diff --name-only origin/main...HEAD": "factory/a.go\n"})
	storeLog(t, f, p, "c1", "Built it.\nTIER: risky")
	f.Tick()
	runs := p.runsWithLabel(labelPrefix + labelReviewKind + "=code")
	if len(runs) != 1 || lensOf(runs[0]) != "" {
		t.Fatalf("runs = %v", runs)
	}
}

func TestUnanimousPanelLandsWithoutArbiter(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	r2.CreatedAt = r1.CreatedAt.Add(time.Second)
	f, p := newTestFactory(t, panelCard(nil), r1, r2)
	storeLog(t, f, p, "r1", "Tests pass.\nTIER: risky\nVERDICT: approve")
	storeLog(t, f, p, "r2", "Fits the repo.\n- nit: rename x\nTIER: risky\nVERDICT: nits")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "" || l[labelCodeRounds] != "" {
		t.Fatalf("one panel reviewer moved the card: %v", l)
	}
	if l := p.labels("r1"); l[labelApplied] != "1" || l[labelVerdict] != "approve" || l[labelLensTier] != "risky" {
		t.Errorf("r1 labels = %v", l)
	}
	if err := f.ApplyVerdict(f.agent("r2")); err != nil { // the snapshot still shows r1 unapplied
		t.Fatal(err)
	}
	l := p.labels("c1")
	// passed: on to the finished-work rule (Zach asked for this card, so he sees it before it lands)
	if l[labelStage] != "zach" || l[labelNote] != readyNote(1) || l[labelCodeRounds] != "1" || l[labelVerdict] != "code nits" || l[labelPanel] != "" {
		t.Fatalf("card labels = %v", l)
	}
	if runs := p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter); len(runs) != 0 {
		t.Errorf("arbiter started on a unanimous panel")
	}
	if ct, _ := f.timings.get("c1"); len(ct.Rounds) != 1 {
		t.Errorf("rounds recorded = %v", ct.Rounds)
	}
}

func TestSplitPanelGoesToOneArbiterThatDecidesOnce(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	f, p := newTestFactory(t, panelCard(nil), r1, r2)
	storeLog(t, f, p, "r1", "Tests pass.\nTIER: risky\nVERDICT: approve")
	storeLog(t, f, p, "r2", "Leaves the old path in ask.go.\nTIER: architecture\nVERDICT: changes")
	for _, id := range []string{"r1", "r2"} {
		if err := f.ApplyVerdict(f.agent(id)); err != nil {
			t.Fatal(err)
		}
	}
	runs := p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter)
	if len(runs) != 1 {
		t.Fatalf("arbiter runs = %d: %v", len(runs), p.calls)
	}
	prompt := runs[0][len(runs[0])-1]
	if !strings.Contains(prompt, "Panel Arbiter") || !strings.Contains(prompt, "Leaves the old path") || !strings.Contains(prompt, "Tests pass") {
		t.Errorf("arbiter prompt lacks the reports: %s", prompt)
	}
	if l := p.labels("c1"); l[labelStage] != "" || l[labelCodeRounds] != "" || l[labelPanel] != "" {
		t.Fatalf("card moved before the arbiter: %v", l)
	}
	// a late re-apply of a panel reviewer does not start a second arbiter
	if err := f.closePanel(f.agent("c1")); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter)); n != 1 {
		t.Errorf("arbiter runs = %d", n)
	}

	a := lensReviewer("a1", "c1", lensArbiter, "1")
	f.SetAgents(append(f.agents(), a))
	storeLog(t, f, p, "a1", "Blocking: the old path in ask.go stays.\nTIER: risky\nVERDICT: changes")
	if err := f.ApplyVerdict(f.agent("a1")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelCodeRounds] != "1" || l[labelTier] != "risky" {
		t.Fatalf("card labels = %v", l)
	}
	if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "old path in ask.go stays") {
		t.Errorf("sent = %v", s)
	}
}

func TestReworkRoundAlwaysRechecksCorrectness(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	r3 := lensReviewer("r3", "c1", lensUX, "1")
	r1.Labels[labelPrefix+labelApplied], r1.Labels[labelPrefix+labelVerdict] = "1", "approve"
	r2.Labels[labelPrefix+labelApplied], r2.Labels[labelPrefix+labelVerdict] = "1", "changes"
	r3.Labels[labelPrefix+labelApplied], r3.Labels[labelPrefix+labelVerdict] = "1", "approve"
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelTier: "risky",
		labelPlanRounds: "1", labelCodeRounds: "1", labelReviewed: "h1", labelSince: ago(10 * time.Minute)})
	f, p := newTestFactory(t, c, r1, r2, r3)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": "h2\n",
		"diff --numstat origin/main...HEAD": "300\t0\tfactory/a.go\n2\t0\tfactory/web/board.html\n",
		"diff --name-only h1..HEAD":         "factory/a.go\n"})
	storeLog(t, f, p, "c1", "Restructured as asked.\nTIER: risky")
	f.Tick()
	var lenses []string
	for _, r := range p.runsWithLabel(labelPrefix + labelReviewKind + "=code") {
		lenses = append(lenses, lensOf(r))
		if argLabel(r, labelPanel) != "2" {
			t.Errorf("rework reviewer not in panel 2: %v", r)
		}
	}
	// correctness approved round 1 but the code changed; the UX reviewer approved and the fix touched no UI
	if strings.Join(lenses, ",") != "correctness,architecture" {
		t.Fatalf("rework lenses = %v", lenses)
	}
}

func TestPanelReviewerWithoutVerdictRerunsOnceThenAsksZach(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	r2.Labels[labelPrefix+labelApplied], r2.Labels[labelPrefix+labelVerdict] = "1", "approve"
	f, p := newTestFactory(t, panelCard(nil), r1, r2)
	storeLog(t, f, p, "r1", "I ran out of time.")
	storeLog(t, f, p, "r2", "Fine.\nTIER: risky\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelPrefix + labelLens + "=" + lensCorrectness)
	if len(runs) != 1 || argLabel(runs[0], labelPanel) != "1" {
		t.Fatalf("re-run = %v", runs)
	}
	if l := p.labels("c1"); l[labelStage] != "" {
		t.Fatalf("card moved: %v", l)
	}
	r1.Labels[labelPrefix+labelApplied] = "1"
	r4 := lensReviewer("r4", "c1", lensCorrectness, "1")
	r4.CreatedAt = r1.CreatedAt.Add(time.Minute)
	f.SetAgents([]Agent{f.agent("c1"), r1, r2, r4})
	storeLog(t, f, p, "r4", "Again no verdict.")
	if err := f.ApplyVerdict(f.agent("r4")); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != noVerdictNote {
		t.Fatalf("card labels = %v", l)
	}
	if n := len(p.runsWithLabel(labelPrefix + labelLens + "=" + lensCorrectness)); n != 1 {
		t.Errorf("re-ran twice: %d", n)
	}
}

func TestPanelRoundCapStillApplies(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "3")
	r1.Labels[labelPrefix+labelApplied], r1.Labels[labelPrefix+labelVerdict] = "1", "approve"
	a := lensReviewer("a1", "c1", lensArbiter, "3")
	f, p := newTestFactory(t, panelCard(map[string]string{labelCodeRounds: "2", labelPanel: ""}), r1, a)
	storeLog(t, f, p, "a1", "Still blocking.\nTIER: risky\nVERDICT: changes")
	if err := f.ApplyVerdict(f.agent("a1")); err != nil {
		t.Fatal(err)
	}
	// the third decided round is the cap: the gate arbiter settles it, as after a single reviewer
	if l := p.labels("c1"); l[labelCodeRounds] != "3" || l[labelArbitrated] != "code" || l[labelNote] != arbiterNote {
		t.Fatalf("card labels = %v", l)
	}
}

func TestPublicRepoPanelStillScans(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	c := panelCard(map[string]string{labelReviewed: "h1"}) // the approval is for the reviewed commit only
	c.Repo = "zachlatta/agent-factory"
	f, p := newTestFactory(t, c, r1, r2)
	f.cfg.Guarded = []string{"zachlatta/agent-factory"}
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "h1\n"})
	scanned := false
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		scanned = true
		return nil, errors.New("scanner missing")
	}
	storeLog(t, f, p, "r1", "ok\nTIER: risky\nVERDICT: approve")
	storeLog(t, f, p, "r2", "ok\nTIER: risky\nVERDICT: approve")
	for _, id := range []string{"r1", "r2"} {
		if err := f.ApplyVerdict(f.agent(id)); err != nil {
			t.Fatal(err)
		}
	}
	if !scanned {
		t.Fatal("no scan before landing a public-repo panel")
	}
	if l := p.labels("c1"); l[labelStage] != "zach" {
		t.Errorf("failed scan did not stop the card: %v", l)
	}
}

func TestStuckCheckClosesAFinishedPanel(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	for _, r := range []Agent{r1, r2} {
		r.Labels[labelPrefix+labelApplied], r.Labels[labelPrefix+labelVerdict] = "1", "approve"
	}
	c := panelCard(nil)
	c.LastActivityAt = t0.Add(-10 * time.Minute)
	f, p := newTestFactory(t, c, r1, r2)
	storeLog(t, f, p, "r1", "ok\nVERDICT: approve")
	storeLog(t, f, p, "r2", "ok\nVERDICT: approve")
	if err := f.checkStuck(f.agent("c1"), t0); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelCodeRounds] != "1" || l[labelVerdict] != "code approve" {
		t.Fatalf("card labels = %v", l)
	}
}

func TestDecidingReviewerGivesTheAskDetail(t *testing.T) {
	c := Card{ID: "c1", Children: []Child{
		{ID: "single", Role: "reviewer", Kind: "code"},
		{ID: "lens-a", Role: "reviewer", Kind: "code", Lens: lensCorrectness, Panel: "2"},
		{ID: "arb", Role: "reviewer", Kind: "code", Lens: lensArbiter, Panel: "2"},
		{ID: "lens-b", Role: "reviewer", Kind: "code", Lens: lensArchitecture, Panel: "3"},
	}}
	if got := askSource(c, &Ask{From: "reviewer"}); got != "arb" {
		t.Errorf("askSource = %s, want the arbiter", got)
	}
	c.Children = c.Children[1:2]
	if got := askSource(c, &Ask{From: "reviewer"}); got != "lens-a" {
		t.Errorf("askSource = %s, want the only reviewer", got)
	}

	arb := lensReviewer("arb", "c1", lensArbiter, "1")
	late := lensReviewer("late", "c1", lensCorrectness, "2")
	late.CreatedAt = arb.CreatedAt.Add(time.Hour)
	f, _ := newTestFactory(t, card("c1", nil), arb, late)
	if r, ok := f.newestReviewer("c1"); !ok || r.ID != "arb" {
		t.Errorf("newestReviewer = %s, want the arbiter", r.ID)
	}
}
