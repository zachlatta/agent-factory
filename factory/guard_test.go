package factory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

const publicRepo = "zachlatta/agent-factory"

func publicCard(id string, labels map[string]string) Agent {
	a := card(id, labels)
	a.Repo = publicRepo
	return a
}

// fakeScan makes f.Scan return hits (or err) and records the ranges it was asked for.
func fakeScan(f *Factory, hits []scan.Hit, err error) *[]string {
	var ranges []string
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.Scan = func(_ context.Context, _ string, rng string) ([]scan.Hit, error) {
		ranges = append(ranges, rng)
		return hits, err
	}
	return &ranges
}

func TestGuardedDefaultsToFactoryRepoOnly(t *testing.T) {
	f, _ := newTestFactory(t)
	if !f.guarded(publicRepo) || f.guarded("example/desk") || f.guarded("hackclub/merch") || f.guarded("") {
		t.Errorf("guarded = %v", f.cfg.Guarded)
	}
}

func TestGuardedCardIsScannedBeforeItLandsOrReachesZach(t *testing.T) {
	for _, tc := range []struct {
		name, tier, origin, want string
	}{
		{"idea low", "low", "idea", "landing"},
		{"idea risky", "risky", "idea", "landing"},
		{"idea architecture", "architecture", "idea", "zach"}, // always-ask: architecture he did not ask for
		{"Zach's low", "low", "", "zach"},                     // he asked for it: he sees it finished
	} {
		f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "review", labelTier: tc.tier, labelSource: "board",
			labelOrigin: tc.origin, labelPlanRounds: "1", labelReviewed: reviewedSHA}), reviewer("r1", "c1", "code"))
		ranges := fakeScan(f, nil, nil)
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: "+tc.tier+"\nVERDICT: approve")
		f.ApplyVerdict(f.agent("r1"))
		l := p.labels("c1")
		if l[labelStage] != tc.want {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
		msgs := p.sent("c1")
		if tc.want == "landing" && (len(msgs) != 1 || !strings.Contains(l[labelNote], "scan passed") ||
			!strings.Contains(msgs[0], "bin/land") || !strings.Contains(msgs[0], "--no-verify")) {
			t.Errorf("%s: note %q, sent = %v", tc.name, l[labelNote], msgs)
		}
		if tc.want == "zach" && (!isReadyNote(l[labelNote]) || len(msgs) != 1 || strings.Contains(msgs[0], "Land it now")) {
			t.Errorf("%s: note %q, sent = %v", tc.name, l[labelNote], msgs)
		}
		if len(*ranges) != 1 || (*ranges)[0] != "origin/main..HEAD" {
			t.Errorf("%s: scanned %v", tc.name, *ranges)
		}
	}
}

func TestGuardedInboxCardComesToZachThenLandsAndReports(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "email", labelPlanRounds: "1",
		labelReviewed: reviewedSHA})
	c.Labels[gmailThreadLabel] = "t1"
	f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
	fakeScan(f, nil, nil)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || !isReadyNote(l[labelNote]) {
		t.Fatalf("labels = %v", l)
	}
	f, p = newTestFactory(t, withLabels(c, l))
	fakeScan(f, nil, nil)
	headIs(f, reviewedSHA, nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "bin/land") || !strings.Contains(msgs[0], "t1") {
		t.Errorf("sent = %v", msgs)
	}
}

// Every public-repo card the factory started lands on its own after an approving review and a clean
// scan, with nothing on the always-ask list: ideas, Scouts and scheduled runs. Cards with no source
// label count as Zach's and come to him.
func TestGuardedFactoryCardLandsAfterCleanScan(t *testing.T) {
	sched := publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board", labelPlanRounds: "1"})
	sched.Labels["paseo.schedule-id"] = "s1"
	for name, c := range map[string]Agent{
		"idea": publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board",
			labelOrigin: "idea", labelPlanRounds: "1"}),
		"scout": publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board",
			labelRole: "scout", labelPlanRounds: "1"}),
		"schedule": sched,
		"app":      publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
	} {
		c.Labels[labelPrefix+labelReviewed] = reviewedSHA
		f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
		ranges := fakeScan(f, nil, nil)
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nVERDICT: nits")
		f.ApplyVerdict(f.agent("r1"))
		want := "landing"
		if name == "app" {
			want = "zach"
		}
		if l := p.labels("c1"); l[labelStage] != want {
			t.Errorf("%s: labels = %v", name, l)
		}
		if msgs := p.sent("c1"); want == "landing" && (len(msgs) != 1 || !strings.Contains(msgs[0], "bin/land")) {
			t.Errorf("%s: sent = %v", name, msgs)
		}
		if len(*ranges) != 1 {
			t.Errorf("%s: scanned %v", name, *ranges)
		}
	}
}

// Architecture plans Zach has not approved and cards with no plan review still wait for him after
// a clean scan, as in any repo.
func TestGuardedArchitectureAndUnplannedWaitForZach(t *testing.T) {
	for name, c := range map[string]Agent{
		"architecture": publicCard("c1", map[string]string{labelStage: "review", labelTier: "architecture",
			labelSource: "board", labelPlanRounds: "1"}),
		"no plan review": publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board"}),
	} {
		c.Labels[labelPrefix+labelReviewed] = reviewedSHA
		f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
		ranges := fakeScan(f, nil, nil)
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "VERDICT: approve")
		f.ApplyVerdict(f.agent("r1"))
		if l := p.labels("c1"); l[labelStage] != "zach" || !isReadyNote(l[labelNote]) {
			t.Errorf("%s: labels = %v", name, l)
		}
		for _, m := range p.sent("c1") {
			if strings.Contains(m, "Land it now") {
				t.Errorf("%s: a landing message went out before Zach approved: %v", name, m)
			}
		}
		if len(*ranges) != 1 {
			t.Errorf("%s: scanned %v", name, *ranges)
		}
	}
}

func TestGuardedScanHitStopsLanding(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board", labelPlanRounds: "1",
		labelReviewed: reviewedSHA}), reviewer("r1", "c1", "code"))
	fakeScan(f, []scan.Hit{{Rule: "tailnet-hostname", File: "a.go", Line: 3}}, nil)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != "secret scan failed: tailnet-hostname a.go:3" {
		t.Errorf("labels = %v", l)
	}
	if a := askFor(Card{Stage: "zach"}, publicCard("c1", l)); a == nil || !strings.HasPrefix(a.Title, "Private text found before publishing") {
		t.Errorf("ask = %+v", a)
	}
}

func TestGuardedScanErrorFailsClosed(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "review", labelTier: "low", labelSource: "board", labelPlanRounds: "1",
		labelReviewed: reviewedSHA}), reviewer("r1", "c1", "code"))
	fakeScan(f, nil, errors.New("gitleaks not found"))
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "secret scan could not run") {
		t.Errorf("labels = %v", l)
	}
}

func TestGuardedApproveRescans(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: "zach", labelVerdict: "code approve", labelCodeRounds: "1",
		labelNote: "review passed: approve to land", labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c)
	fakeScan(f, []scan.Hit{{Rule: "email", File: "README.md", Line: 9}}, nil)
	headIs(f, reviewedSHA, nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err == nil {
		t.Fatal("approve went through with a scan hit")
	}
	if len(p.sent("c1")) != 0 || p.labels("c1")[labelStage] == "landing" {
		t.Errorf("landing started: %v %v", p.sent("c1"), p.labels("c1"))
	}
	fakeScan(f, nil, nil)
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "bin/land") || !strings.Contains(msgs[0], "--no-verify") {
		t.Errorf("sent = %v", msgs)
	}
}

func TestGuardedDirectTrackGoesToZach(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: "plan", labelSource: "board", labelBase: "abc",
		labelSince: t0.Add(-10 * time.Minute).Format(time.RFC3339)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	ranges := fakeScan(f, nil, nil)
	storeLog(t, f, p, "c1", "Pushed the fix.\nTRACK: direct")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "public repo: finished without review") {
		t.Errorf("labels = %v", l)
	}
	if len(*ranges) != 1 || (*ranges)[0] != "abc..HEAD" {
		t.Errorf("scanned %v", *ranges)
	}
}

func TestGuardedIdeasWaitForZach(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("idea", Body{Request: "public one", Repo: publicRepo}); err != nil {
		t.Fatal(err)
	}
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 0 {
		t.Fatalf("public-repo idea auto-started (%d)", n)
	}
	if _, err := f.Act("idea", Body{Request: "private one"}); err != nil {
		t.Fatal(err)
	}
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("private-repo idea did not start (%d)", n)
	}
}

func TestGuardedPromptsCarryPublicRules(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "building"}))
	if _, err := f.Act("new", Body{Request: "fix a typo", Repo: publicRepo}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Act("new", Body{Request: "fix a typo", Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	c, _ := f.card("c1")
	if _, err := f.StartReview(c, "code"); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, call := range p.calls {
		if call[0] == "run" {
			runs = append(runs, call[len(call)-1])
		}
	}
	for _, r := range runs {
		if strings.Contains(r, "<errands>") {
			t.Errorf("unreplaced <errands> in a prompt")
		}
	}
	if len(runs) != 3 || !strings.Contains(runs[0], "This repo is public") || strings.Contains(runs[1], "This repo is public") ||
		!strings.Contains(runs[2], "This repo is public") {
		t.Errorf("prompts: public card %v, desk card %v, review %v", strings.Contains(runs[0], "This repo is public"),
			strings.Contains(runs[1], "This repo is public"), strings.Contains(runs[2], "This repo is public"))
	}
}

func TestUnguardedRepoUnchanged(t *testing.T) {
	f, p := newTestFactory(t, idea("c1", map[string]string{labelStage: "review", labelTier: "low", labelPlanRounds: "1"}),
		reviewer("r1", "c1", "code"))
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		t.Fatal("scanned a repo that is not guarded")
		return nil, nil
	}
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "landing" {
		t.Errorf("labels = %v", l)
	}
}

// The real scan uses origin/main's gitleaks config and this binary's rules, never the card's copy:
// a card that empties .gitleaks.toml and swaps bin/scan for "exit 0" still gets caught.
func TestRunScanIgnoresCardsOwnScannerFiles(t *testing.T) {
	if _, err := exec.LookPath("gitleaks"); err != nil {
		t.Skip("gitleaks not installed")
	}
	origin := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git(origin, "init", "-q", "--bare", "-b", "main")
	wt := t.TempDir()
	git(wt, "clone", "-q", origin, ".")
	git(wt, "config", "user.email", "1+bot@users.noreply.github.com")
	git(wt, "config", "user.name", "bot")
	os.WriteFile(filepath.Join(wt, ".gitleaks.toml"), []byte("[extend]\nuseDefault = true\n"), 0o644)
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "init")
	git(wt, "push", "-q", "origin", "HEAD:main")
	git(wt, "fetch", "-q", "origin")
	// the card: allow-everything config, a no-op scanner, and a token
	os.WriteFile(filepath.Join(wt, ".gitleaks.toml"), []byte("[allowlist]\npaths = ['''.*''']\n"), 0o644)
	os.MkdirAll(filepath.Join(wt, "bin"), 0o755)
	os.WriteFile(filepath.Join(wt, "bin/scan"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(wt, "k.txt"), []byte("token = \"gh"+"p_"+"L76BeHTg"+"TwaXzTj1"+"rkMWpQYR"+"hqngIyT2"+"MoWN\"\n"), 0o644)
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "card")

	f, _ := newTestFactory(t)
	f.GitOut = runGitOut
	f.cfg.PrivateRules = filepath.Join(t.TempDir(), "denylist")
	os.WriteFile(f.cfg.PrivateRules, []byte("\\bnever-matches-anything\\b\n"), 0o600)
	hits, err := f.runScan(context.Background(), wt, "origin/main..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		found = found || strings.HasPrefix(h.Rule, "gitleaks:")
	}
	if !found {
		t.Fatalf("token not caught: %v", hits)
	}
	// and with no private rules file it refuses to run
	f.cfg.PrivateRules = filepath.Join(t.TempDir(), "missing")
	if _, err := f.runScan(context.Background(), wt, "origin/main..HEAD"); err == nil {
		t.Error("ran without the private rules file")
	}
}

// Paseo splits --label values at commas, which failed every update carrying such a note.
func TestLabelValuesHaveNoCommas(t *testing.T) {
	if got := labelArg(labelNote, "scan failed: a, b"); got != labelPrefix+"note=scan failed: a; b" {
		t.Errorf("labelArg = %q", got)
	}
	if a := repoAsk("pick a repo", "a/x,b/y"); a == nil || len(a.Actions) < 2 {
		t.Errorf("old comma-separated candidates not read: %+v", a)
	}
}

func TestGuardedReplyKeepsCodeReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"first pass", map[string]string{labelStage: "plan", labelSource: "email"}},
		{"direct card", map[string]string{labelStage: "zach", labelSource: "email", labelTrack: "direct", labelNote: "public repo: finished without review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t, publicCard("c1", tc.labels))
			if _, err := f.Act("send", Body{ID: "c1", Message: "one more thing"}); err != nil {
				t.Fatal(err)
			}
			if l := p.labels("c1"); l[labelStage] != "building" || l[labelAutoReview] != "code" {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestGuardedDirectReplyStillGetsCodeReview(t *testing.T) {
	c := publicCard("c1", map[string]string{labelStage: "building", labelSource: "email", labelAutoReview: "code", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": "bbb\n"})
	storeLog(t, f, p, "c1", "Pushed the fix.\nTRACK: direct")
	f.Tick()
	runs := p.runsWithLabel(labelArg(labelReviewOf, "c1"))
	if len(runs) != 1 || !contains(runs[0], labelArg(labelReviewKind, "code")) {
		t.Fatalf("code review not started: %v", p.calls)
	}
}
