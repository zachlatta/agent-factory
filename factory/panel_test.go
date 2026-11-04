package factory

import (
	"context"
	"errors"
	"fmt"
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
	f, p := newTestFactory(t, panelCard(map[string]string{labelReviewed: reviewedSHA}), r1, r2)
	headIs(f, reviewedSHA, nil)
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

func TestPanelLensWithoutVerdictIsReplacedThenTheArbiterDecidesWithoutIt(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	r2.Labels[labelPrefix+labelApplied], r2.Labels[labelPrefix+labelVerdict] = "1", "approve"
	f, p, m := mailFactory(t, panelCard(nil), r1, r2)
	storeLog(t, f, p, "r2", "Fine.\nTIER: risky\nVERDICT: approve")
	agents := []Agent{f.agent("c1"), r1, r2}
	last := r1
	for n := 1; n <= reviewerRetryCap+1; n++ {
		storeLog(t, f, p, last.ID, "I ran out of time.")
		if err := f.ApplyVerdict(f.agent(last.ID)); err != nil {
			t.Fatal(err)
		}
		runs := p.runsWithLabel(labelPrefix + labelLens + "=" + lensCorrectness)
		if n <= reviewerRetryCap {
			if len(runs) != n || argLabel(runs[n-1], labelPanel) != "1" {
				t.Fatalf("replacement %d = %v", n, runs)
			}
		} else if len(runs) != reviewerRetryCap {
			t.Fatalf("replaced more than %d times: %d", reviewerRetryCap, len(runs))
		}
		if l := p.labels("c1"); l[labelStage] != "" {
			t.Fatalf("card moved: %v", l)
		}
		last.Labels[labelPrefix+labelApplied] = "1"
		next := lensReviewer(fmt.Sprintf("r1-%d", n), "c1", lensCorrectness, "1")
		next.CreatedAt = last.CreatedAt.Add(time.Minute)
		agents = append(agents, next)
		f.SetAgents(agents)
		last = next
	}
	arb := p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter)
	if len(arb) != 1 {
		t.Fatalf("arbiter runs = %d", len(arb))
	}
	prompt := arb[0][len(arb[0])-1]
	if !strings.Contains(prompt, "MISSING: correctness review") || !strings.Contains(prompt, "check that area") || !strings.Contains(prompt, "Fine.") {
		t.Errorf("arbiter not told which lens is missing: %s", prompt)
	}
	if l := p.labels("c1"); l[labelStage] == "zach" || len(m.sends()) != 0 {
		t.Errorf("went to Zach: %v %v", l, m.sends())
	}
}

func TestBlockedPanelLensIsReplacedOnItsOwn(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r1.Status, r1.LastActivityAt = "running", t0.Add(-(stallMinutes+1)*time.Minute)
	r2 := lensReviewer("r2", "c1", lensArchitecture, "1")
	r2.Labels[labelPrefix+labelApplied], r2.Labels[labelPrefix+labelVerdict] = "1", "approve"
	c := panelCard(nil)
	c.LastActivityAt = t0.Add(-time.Hour)
	f, p := newTestFactory(t, c, r1, r2)
	f.Tick()
	runs := p.runsWithLabel(labelPrefix + labelReviewKind + "=code")
	if len(runs) != 1 || lensOf(runs[0]) != lensCorrectness || argLabel(runs[0], labelPanel) != "1" {
		t.Fatalf("runs = %v", runs)
	}
	if !p.called("archive", "r1") || p.labels("r1")[labelVerdict] != "superseded" || p.labels("r2")[labelVerdict] != "" {
		t.Errorf("wrong reviewers retired: r1 %v r2 %v", p.labels("r1"), p.labels("r2"))
	}
}

func TestArbiterWithoutRulingIsReplacedOnceThenStuck(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "1")
	r1.Labels[labelPrefix+labelApplied], r1.Labels[labelPrefix+labelVerdict] = "1", "changes"
	a1 := lensReviewer("a1", "c1", lensArbiter, "1")
	f, p, m := mailFactory(t, panelCard(map[string]string{labelPanel: ""}), r1, a1)
	storeLog(t, f, p, "r1", "Blocking: x.\nVERDICT: changes")
	storeLog(t, f, p, "a1", "I could not decide.")
	if err := f.ApplyVerdict(f.agent("a1")); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter)); n != 1 {
		t.Fatalf("arbiter replacements = %d", n)
	}
	a2 := lensReviewer("a2", "c1", lensArbiter, "1")
	a2.CreatedAt = a1.CreatedAt.Add(time.Minute)
	f.SetAgents(append(f.agents(), a2))
	storeLog(t, f, p, "a2", "Still nothing.")
	if err := f.ApplyVerdict(f.agent("a2")); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if l[labelStuck] != stuckReview || l[labelStage] == "zach" || len(m.sends()) != 0 {
		t.Fatalf("labels %v mails %v", l, m.sends())
	}
	if n := len(p.runsWithLabel(labelPrefix + labelLens + "=" + lensArbiter)); n != 1 {
		t.Errorf("a third arbiter ran: %d", n)
	}
}

// A ruling arbiter whose gate then fails (the round cap) keeps that gate: only a missing ruling marks
// the card stuck. TestPanelRoundCapStillApplies covers the ruling side.

func TestPanelRoundCapStillApplies(t *testing.T) {
	r1 := lensReviewer("r1", "c1", lensCorrectness, "3")
	r1.Labels[labelPrefix+labelApplied], r1.Labels[labelPrefix+labelVerdict] = "1", "approve"
	a := lensReviewer("a1", "c1", lensArbiter, "3")
	f, p := newTestFactory(t, panelCard(map[string]string{labelCodeRounds: "2", labelPanel: ""}), r1, a)
	storeLog(t, f, p, "r1", "ALWAYS ASK: none")
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
	f.GitOut = gitStub(map[string]string{"rev-parse HEAD": "h1\n", "status --porcelain": ""})
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

func TestNewestRoundsReviewerGivesTheAskDetail(t *testing.T) {
	// round 1 split and its arbiter asked for changes; round 2's reviewers all approved (no arbiter)
	c := Card{ID: "c1", Children: []Child{
		{ID: "lens-1", Role: "reviewer", Kind: "code", Lens: lensArchitecture, Panel: "1"},
		{ID: "arb-1", Role: "reviewer", Kind: "code", Lens: lensArbiter, Panel: "1"},
		{ID: "lens-2", Role: "reviewer", Kind: "code", Lens: lensCorrectness, Panel: "2"},
	}}
	if got := askSource(c, &Ask{From: "reviewer"}); got != "lens-2" {
		t.Errorf("askSource = %s, want the newest round's reviewer", got)
	}
	c.Children = c.Children[:2]
	if got := askSource(c, &Ask{From: "reviewer"}); got != "arb-1" {
		t.Errorf("askSource = %s, want the split round's arbiter", got)
	}

	arb := lensReviewer("arb", "c1", lensArbiter, "1")
	late := lensReviewer("late", "c1", lensCorrectness, "2")
	late.CreatedAt = arb.CreatedAt.Add(time.Hour)
	f, _ := newTestFactory(t, card("c1", nil), arb, late)
	if r, ok := f.newestReviewer("c1"); !ok || r.ID != "late" {
		t.Errorf("newestReviewer = %s, want the newest round's reviewer", r.ID)
	}
}

func TestPanelReportsAlwaysAsk(t *testing.T) {
	cases := []struct {
		name    string
		reports []string
		want    string
	}{
		{"earlier ask", []string{"ALWAYS ASK: changes gates", "ALWAYS ASK: none"}, "changes gates"},
		{"multiple asks", []string{"ALWAYS ASK: changes gates", "ALWAYS ASK: adds a service", "ALWAYS ASK: none"}, "changes gates; adds a service"},
		{"duplicate asks", []string{"ALWAYS ASK: changes gates", "ALWAYS ASK: changes gates"}, "changes gates"},
		{"missing answer", []string{"Looks good.", "ALWAYS ASK: none"}, "correctness review did not say whether it needs you"},
		{"missing report", []string{"", "ALWAYS ASK: none"}, "correctness review did not say whether it needs you"},
		{"no UX reviewer", []string{"ALWAYS ASK: none", "ALWAYS ASK: none"}, "none"},
		{"three all none", []string{"ALWAYS ASK: none", "ALWAYS ASK: none", "ALWAYS ASK: none"}, "none"},
		{"beyond truncation", []string{strings.Repeat("detail ", 1100) + "\nALWAYS ASK: changes gates", "ALWAYS ASK: none"}, "changes gates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := panelCard(nil)
			agents := []Agent{c}
			for i := range tc.reports {
				r := lensReviewer(fmt.Sprintf("r%d", i), c.ID, []string{lensCorrectness, lensArchitecture, lensUX}[i], "1")
				agents = append(agents, r)
			}
			f, p := newTestFactory(t, agents...)
			for i, report := range tc.reports {
				f.noteLens(agents[i+1].ID, lensResult{done: true, verdict: "approve"})
				if report == "" {
					f.noteLens(agents[i+1].ID, lensResult{done: true})
				} else {
					storeLog(t, f, p, agents[i+1].ID, report)
				}
			}
			got, err := f.panelReports(c, "1")
			if err != nil {
				t.Fatal(err)
			}
			if want := "\n\nALWAYS ASK: " + tc.want; !strings.HasSuffix(got, want) {
				t.Errorf("report must end with %q, got %q", want, got)
			}
			if gotAsk := ParseAlwaysAsk(got); gotAsk != tc.want {
				t.Errorf("parsed ask = %q, want %q", gotAsk, tc.want)
			}
		})
	}
}

func TestPanelReportsUnreadable(t *testing.T) {
	c := panelCard(nil)
	r := lensReviewer("r1", c.ID, lensCorrectness, "1")
	f, _ := newTestFactory(t, c, r)
	f.noteLens(r.ID, lensResult{done: true, verdict: "approve"})
	if _, err := f.panelReports(c, "1"); err == nil || !strings.Contains(err.Error(), "report unreadable") {
		t.Fatalf("expected unreadable report error, got %v", err)
	}
}

func TestPanelArbiterPreservesAsks(t *testing.T) {
	for _, tc := range []struct{ name, lens, arbiter, want string }{
		{"lens ask", "ALWAYS ASK: changes gates", "ALWAYS ASK: none", "changes gates"},
		{"union", "ALWAYS ASK: changes gates; adds a service", "ALWAYS ASK: changes gates; new architecture", "changes gates; adds a service; new architecture"},
		{"missing lens", "Looks good", "ALWAYS ASK: none", "correctness review did not say whether it needs you"},
		{"missing arbiter", "ALWAYS ASK: none", "Looks good", "the arbiter did not say whether it needs you"},
		{"all none", "ALWAYS ASK: none", "ALWAYS ASK: none", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := panelCard(map[string]string{labelSource: "idea"})
			r := lensReviewer("r1", c.ID, lensCorrectness, "2")
			old := lensReviewer("old", c.ID, lensArchitecture, "1")
			f, p := newTestFactory(t, c, r, old)
			storeLog(t, f, p, r.ID, tc.lens)
			storeLog(t, f, p, old.ID, "ALWAYS ASK: stale concern")
			notes, err := f.panelArbiterNotes(c, "2", tc.arbiter)
			if err != nil {
				t.Fatal(err)
			}
			if got := ParseAlwaysAsk(notes); got != tc.want {
				t.Fatalf("ask = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPanelArbiterRepairAsk(t *testing.T) {
	c := panelCard(map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ", labelBehaviorRepair: "1"})
	r := lensReviewer("r1", c.ID, lensCorrectness, "1")
	f, p := newTestFactory(t, c, r)
	f.GitOut = gitStub(nil)
	storeLog(t, f, p, r.ID, "ALWAYS ASK: none\nREPAIR ALWAYS ASK: repair changes gates\nAPPROVED BEHAVIOR: unchanged: fixture repair")
	notes, err := f.panelArbiterNotes(c, "1", "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: unchanged: fixture repair")
	if err != nil {
		t.Fatal(err)
	}
	if !reuseBehaviorApproval(c, notes) {
		t.Fatal("repair approval was not reused")
	}
	if got := f.finishedWorkReason(c, "risky", notes, ""); !strings.Contains(got, "repair changes gates") {
		t.Fatalf("reason = %q", got)
	}
}

func TestPanelArbiterApprovedBehavior(t *testing.T) {
	for _, tc := range []struct {
		name, lens, arbiter, want string
		reuse                     bool
	}{
		{"missing arbiter", "unchanged", "", "unsure", false},
		{"lens changed", "changed", "unchanged", "unsure", false},
		{"arbiter changed", "unchanged", "changed", "unsure", false},
		{"lens unsure", "unsure", "unchanged", "unsure", false},
		{"unanimous unchanged", "unchanged", "unchanged", "unchanged", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := panelCard(map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ", labelBehaviorRepair: "1"})
			r := lensReviewer("r1", c.ID, lensCorrectness, "1")
			f, p := newTestFactory(t, c, r)
			storeLog(t, f, p, r.ID, "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: "+tc.lens+": fixture comparison")
			arbiterNotes := "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none"
			if tc.arbiter != "" {
				arbiterNotes += "\nAPPROVED BEHAVIOR: " + tc.arbiter + ": fixture comparison"
			}
			notes, err := f.panelArbiterNotes(c, "1", arbiterNotes)
			if err != nil {
				t.Fatal(err)
			}
			if got := ParseApprovedBehavior(notes); got != tc.want {
				t.Errorf("behavior = %q, want %q", got, tc.want)
			}
			if got := reuseBehaviorApproval(c, notes); got != tc.reuse {
				t.Errorf("reuse = %v, want %v", got, tc.reuse)
			}
		})
	}
}

func TestPanelArbiterMissingLensRepairAsk(t *testing.T) {
	c := panelCard(map[string]string{labelBehaviorApproval: reviewedSHA, labelBehaviorSummary: "c3VtbWFyeQ", labelBehaviorRepair: "1"})
	r := lensReviewer("r1", c.ID, lensCorrectness, "1")
	missing := lensReviewer("r2", c.ID, lensArchitecture, "1")
	f, p := newTestFactory(t, c, r, missing)
	f.GitOut = gitStub(nil)
	f.noteLens(missing.ID, lensResult{done: true})
	storeLog(t, f, p, r.ID, "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: unchanged: fixture repair")
	notes, err := f.panelArbiterNotes(c, "1", "ALWAYS ASK: none\nREPAIR ALWAYS ASK: none\nAPPROVED BEHAVIOR: unchanged: fixture repair")
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseApprovedBehavior(notes); got != "unsure" {
		t.Errorf("behavior = %q, want unsure", got)
	}
	if reuseBehaviorApproval(c, notes) {
		t.Fatal("missing lens reused repair approval")
	}
	if got := f.finishedWorkReason(c, "risky", notes, ""); !strings.Contains(got, "architecture review did not say whether it needs you") {
		t.Fatalf("missing lens repair ask lost: reason = %q", got)
	}
}

func TestPanelArbiterUnreadableDoesNotConsume(t *testing.T) {
	c := panelCard(nil)
	c.LastActivityAt = t0.Add(-time.Hour)
	r := lensReviewer("r1", c.ID, lensCorrectness, "1")
	f, p := newTestFactory(t, c, r)
	consumed := false
	err := f.decide(c, reviewOutcome{kind: "code", lens: lensArbiter, panel: "1", verdict: "approve", notes: "ALWAYS ASK: none"}, func() error { consumed = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if consumed || p.labels(c.ID)[labelCodeRounds] != "" || p.labels(c.ID)[labelNote] != "history unavailable: decision paused" {
		t.Fatalf("unreadable review advanced: consumed=%v labels=%v", consumed, p.labels(c.ID))
	}
}

func TestPanelArbiterDecisionKeepsLensAsk(t *testing.T) {
	c := panelCard(map[string]string{labelSource: "idea", labelReviewed: reviewedSHA})
	r := lensReviewer("r1", c.ID, lensCorrectness, "1")
	f, p := newTestFactory(t, c, r)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, r.ID, "ALWAYS ASK: changes gates")
	consumed := false
	if err := f.decide(c, reviewOutcome{kind: "code", lens: lensArbiter, panel: "1", verdict: "approve", notes: "ALWAYS ASK: none"}, func() error { consumed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if l := p.labels(c.ID); !consumed || l[labelStage] != "zach" || !strings.Contains(l[labelAskWhy], "changes gates") {
		t.Fatalf("lens ask lost: consumed=%v labels=%v", consumed, l)
	}
}
