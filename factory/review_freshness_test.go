package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Review freshness: nothing is approved for landing, and no land prompt is sent, unless the card's
// HEAD is still the commit its code review saw (factory.reviewed). A moved HEAD goes back to a fresh
// code review; a public-repo card whose reviewed commit is unknown or unreadable stops for Zach.

const reviewedSHA = "1111111111111111111111111111111111111111"
const movedSHA = "2222222222222222222222222222222222222222"

// headIs makes f.GitOut answer `rev-parse HEAD` with sha (or fail with err when sha is ""); other git
// reads go to the GitOut it replaces (the always-ask file check's clean diff, by default).
func headIs(f *Factory, sha string, err error) {
	prev := f.GitOut
	f.GitOut = func(ctx context.Context, dir string, args ...string) (string, error) {
		if strings.Join(args, " ") == "rev-parse HEAD" {
			if sha == "" {
				return "", err
			}
			return sha + "\n", nil
		}
		if prev == nil {
			return "", errors.New("unexpected git " + strings.Join(args, " "))
		}
		return prev(ctx, dir, args...)
	}
}

func freshCard(public bool, labels map[string]string) Agent {
	if public {
		return publicCard("c1", labels)
	}
	return card("c1", labels)
}

// reviewLabels is a card the factory started (an idea), so an approving review lands it on its own
// when its HEAD is fresh; a requested card would stop finished for Zach first.
func reviewLabels(extra map[string]string) map[string]string {
	l := map[string]string{labelStage: "review", labelTier: "low", labelSource: "board", labelOrigin: "idea",
		labelPlanRounds: "1", labelReviewed: reviewedSHA}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func noLandPrompt(t *testing.T, name string, p *fakePaseo) {
	t.Helper()
	for _, m := range p.sent("c1") {
		if strings.Contains(m, "Land it now") || strings.Contains(m, "bin/land") {
			t.Errorf("%s: a land prompt went out: %q", name, m)
		}
	}
}

// backToReview: the card is queued for a fresh code review that starts as soon as its agent is stopped.
func backToReview(t *testing.T, name string, l map[string]string) {
	t.Helper()
	if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelSince] != "" || l[labelHeld] != "" ||
		!strings.Contains(l[labelNote], "changed since review") {
		t.Errorf("%s: labels = %v", name, l)
	}
}

func TestVerdictLandsWhenHeadIsTheReviewedCommit(t *testing.T) {
	for _, public := range []bool{false, true} {
		f, p := newTestFactory(t, freshCard(public, reviewLabels(nil)), reviewer("r1", "c1", "code"))
		fakeScan(f, nil, nil)
		headIs(f, reviewedSHA, nil)
		storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); l[labelStage] != "landing" {
			t.Errorf("public=%v: labels = %v", public, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") {
			t.Errorf("public=%v: sent = %v", public, msgs)
		}
	}
}

func TestVerdictWithMovedHeadGoesBackToCodeReview(t *testing.T) {
	for _, public := range []bool{false, true} {
		f, p := newTestFactory(t, freshCard(public, reviewLabels(nil)), reviewer("r1", "c1", "code"))
		ranges := fakeScan(f, nil, nil)
		headIs(f, movedSHA, nil)
		storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
		if err := f.ApplyVerdict(f.agent("r1")); err != nil {
			t.Fatal(err)
		}
		name := map[bool]string{false: "private", true: "public"}[public]
		backToReview(t, name, p.labels("c1"))
		noLandPrompt(t, name, p)
		if len(*ranges) != 0 {
			t.Errorf("%s: scanned an unreviewed HEAD: %v", name, *ranges)
		}
	}
}

// The moved card really gets a fresh reviewer on the next Tick, which records the new HEAD.
func TestMovedHeadStartsAFreshReview(t *testing.T) {
	f, p := newTestFactory(t, freshCard(false, reviewLabels(nil)), reviewer("r1", "c1", "code"))
	headIs(f, movedSHA, nil)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	if err := f.ApplyVerdict(f.agent("r1")); err != nil {
		t.Fatal(err)
	}
	moved := freshCard(false, reviewLabels(p.labels("c1")))
	f.SetAgents([]Agent{moved})
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": movedSHA + "\n"})
	f.Tick()
	if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 1 {
		t.Fatalf("no fresh code reviewer: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != "review" || l[labelReviewed] != movedSHA {
		t.Errorf("labels = %v", l)
	}
}

func TestGuardedVerdictWithoutReviewedCommitFailsClosed(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", reviewLabels(map[string]string{labelReviewed: ""})), reviewer("r1", "c1", "code"))
	fakeScan(f, nil, nil)
	headIs(f, reviewedSHA, nil)
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	l := p.labels("c1")
	if l[labelStage] != "zach" || !strings.Contains(l[labelNote], "no reviewed commit") || l[labelStuckFrom] != "review" {
		t.Errorf("labels = %v", l)
	}
	noLandPrompt(t, "public", p)
}

// A private card with no reviewed commit (its review could not read git) keeps the older behaviour.
func TestPrivateVerdictWithoutReviewedCommitStillLands(t *testing.T) {
	f, p := newTestFactory(t, card("c1", reviewLabels(map[string]string{labelReviewed: ""})), reviewer("r1", "c1", "code"))
	storeLog(t, f, p, "r1", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelStage] != "landing" {
		t.Errorf("labels = %v", l)
	}
}

func TestVerdictGitFailureFailsClosed(t *testing.T) {
	for _, public := range []bool{false, true} {
		f, p := newTestFactory(t, freshCard(public, reviewLabels(nil)), reviewer("r1", "c1", "code"))
		fakeScan(f, nil, nil)
		headIs(f, "", errors.New("not a git repository"))
		storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
		f.ApplyVerdict(f.agent("r1"))
		l := p.labels("c1")
		if l[labelStage] != "zach" || !strings.Contains(l[labelNote], "could not read HEAD") || l[labelStuckFrom] != "review" {
			t.Errorf("public=%v: labels = %v", public, l)
		}
		noLandPrompt(t, "git failure", p)
	}
}

func approvedLabels(extra map[string]string) map[string]string {
	l := map[string]string{labelStage: "zach", labelVerdict: "code approve", labelCodeRounds: "1", labelTier: "architecture",
		labelPlanRounds: "1", labelNote: "review passed: approve to land", labelReviewed: reviewedSHA}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

// Zach's approval does not replace the review: a HEAD that moved after review is reviewed again.
func TestZachApprovalWithMovedHeadGoesBackToCodeReview(t *testing.T) {
	for _, public := range []bool{false, true} {
		f, p := newTestFactory(t, freshCard(public, approvedLabels(nil)))
		ranges := fakeScan(f, nil, nil)
		headIs(f, movedSHA, nil)
		if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		name := map[bool]string{false: "private", true: "public"}[public]
		backToReview(t, name, p.labels("c1"))
		noLandPrompt(t, name, p)
		if len(*ranges) != 0 {
			t.Errorf("%s: scanned %v", name, *ranges)
		}
	}
}

func TestZachApprovalWithReviewedHeadLands(t *testing.T) {
	for _, public := range []bool{false, true} {
		f, p := newTestFactory(t, freshCard(public, approvedLabels(nil)))
		ranges := fakeScan(f, nil, nil)
		headIs(f, reviewedSHA, nil)
		if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
			t.Fatal(err)
		}
		if l := p.labels("c1"); l[labelStage] != "landing" || l[labelNote] != "approved by Zach" {
			t.Errorf("public=%v: labels = %v", public, l)
		}
		if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach approved") {
			t.Errorf("public=%v: sent = %v", public, msgs)
		}
		if public && len(*ranges) != 1 {
			t.Errorf("public card not scanned: %v", *ranges)
		}
	}
}

func TestGuardedZachApprovalFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		err    error
		want   string
	}{
		"no reviewed commit": {map[string]string{labelReviewed: ""}, nil, "no reviewed commit"},
		"git failure":        {nil, errors.New("git broke"), "could not read HEAD"},
	} {
		f, p := newTestFactory(t, publicCard("c1", approvedLabels(tc.labels)))
		fakeScan(f, nil, nil)
		if tc.err != nil {
			headIs(f, "", tc.err)
		} else {
			headIs(f, reviewedSHA, nil)
		}
		if _, err := f.Act("approve", Body{ID: "c1"}); err == nil {
			t.Errorf("%s: approve went through", name)
		}
		if l := p.labels("c1"); l[labelStage] != "zach" || !strings.Contains(l[labelNote], tc.want) {
			t.Errorf("%s: labels = %v", name, l)
		}
		noLandPrompt(t, name, p)
	}
}

func TestDependencyReleaseChecksReviewFreshness(t *testing.T) {
	for _, public := range []bool{false, true} {
		for name, tc := range map[string]struct {
			sha     string
			err     error
			reviewd string
		}{
			"unchanged":   {reviewedSHA, nil, reviewedSHA},
			"moved":       {movedSHA, nil, reviewedSHA},
			"git failure": {"", errors.New("git broke"), reviewedSHA},
			"no identity": {reviewedSHA, nil, ""},
		} {
			c := heldCard(map[string]string{labelReviewed: tc.reviewd})
			if public {
				c.Repo = publicRepo
			}
			f, p := newTestFactory(t, c, dep("landed", false))
			fakeScan(f, nil, nil)
			headIs(f, tc.sha, tc.err)
			f.Tick()
			l := p.labels("c1")
			msgs := p.sent("c1")
			label := name
			if public {
				label = "public " + name
			}
			switch {
			case name == "unchanged" || (name == "no identity" && !public):
				if len(msgs) != 1 || !strings.Contains(msgs[0], "Land it now") || l[labelHeld] != "" {
					t.Errorf("%s: not released: labels %v sent %v", label, l, msgs)
				}
			case name == "moved":
				backToReview(t, label, l)
				noLandPrompt(t, label, p)
			default:
				if l[labelStage] != "zach" || l[labelHeld] != "" || l[labelStuckFrom] != "review" {
					t.Errorf("%s: labels = %v", label, l)
				}
				noLandPrompt(t, label, p)
			}
		}
	}
}

func TestRerunLandingChecksReviewFreshness(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelStuckFrom: "landing",
		labelNote: "stuck", labelReviewed: reviewedSHA}))
	headIs(f, movedSHA, nil)
	if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	backToReview(t, "rerun", p.labels("c1"))
	noLandPrompt(t, "rerun", p)
}

// A card held because its reviewed commit could not be checked recovers through Re-run, which starts
// a fresh code review that records the commit it reads.
func TestUnverifiedReviewRerunStartsAFreshReview(t *testing.T) {
	f, p := newTestFactory(t, publicCard("c1", map[string]string{labelStage: "zach", labelStuckFrom: "review",
		labelNote: noReviewedNote, labelTier: "low", labelPlanRounds: "1"}))
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": movedSHA + "\n"})
	if _, err := f.Act("rerun", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if len(p.runsWithLabel(labelArg(labelReviewKind, "code"))) != 1 {
		t.Fatalf("no fresh code reviewer: %v", p.calls)
	}
	if l := p.labels("c1"); l[labelStage] != "review" || l[labelReviewed] != movedSHA {
		t.Errorf("labels = %v", l)
	}
	noLandPrompt(t, "rerun", p)
}

// An arbiter's code ruling goes through the same gate as a reviewer's verdict (passCode): its approve
// lands only the reviewed commit, and its escalation brings Zach only what was reviewed.
func TestArbiterCodeRulingChecksReviewFreshness(t *testing.T) {
	for _, tc := range []struct {
		name, ruling string
		head         string
		headErr      error
	}{
		{"approve moved", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", movedSHA, nil},
		{"escalate moved", "ALWAYS ASK: it rewrites the landing gate\nTIER: low\nVERDICT: escalate", movedSHA, nil},
		{"approve git failure", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", "", errors.New("not a git repository")},
	} {
		c := idea("c1", reviewLabels(map[string]string{labelCodeRounds: "3", labelArbitrated: "code", labelNote: arbiterNote}))
		arb := reviewer("a1", "c1", "code")
		arb.Labels[labelPrefix+labelArbiter] = "1"
		f, p := newTestFactory(t, c, arb)
		fakeScan(f, nil, nil)
		headIs(f, tc.head, tc.headErr)
		storeLog(t, f, p, "a1", tc.ruling)
		if err := f.ApplyVerdict(f.agent("a1")); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if tc.headErr == nil {
			backToReview(t, tc.name, l)
		} else if l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], unverifiedReviewNote) || l[labelStuckFrom] != "review" {
			t.Errorf("%s: labels = %v", tc.name, l)
		}
		if isFinishedNote(l[labelNote]) || len(p.sent("c1")) != 0 {
			t.Errorf("%s: labels = %v, sent = %v", tc.name, l, p.sent("c1"))
		}
		noLandPrompt(t, tc.name, p)
	}
}
