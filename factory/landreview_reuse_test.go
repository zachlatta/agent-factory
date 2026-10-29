package factory

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// landingCard is factory card ag1, sent to land, whose code review saw reviewed and passed approved.
func landingCard(reviewed, approved string, extra map[string]string) Agent {
	l := map[string]string{labelStage: "landing", labelSince: ago(time.Minute), labelReviewed: reviewed,
		labelCodeOK: approved, labelVerdict: "code approve"}
	for k, v := range extra {
		l[k] = v
	}
	return card("ag1", l)
}

// onBoard puts agents on the board and in their Paseo agent files, the current state the land review
// re-reads.
func onBoard(t *testing.T, f *Factory, agents ...Agent) {
	t.Helper()
	os.RemoveAll(filepath.Join(f.cfg.Home, ".paseo", "agents"))
	writeAgents(t, f, agents...)
	f.SetAgents(agents)
}

func TestLandReviewReusesACompletedCodeApproval(t *testing.T) {
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	onBoard(t, f, landingCard(head, head, nil))
	res, err := f.Act("land-review", Body{By: "ag1", Why: "bin/land"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(LandReview)
	if r.Status != "done" || r.Verdict != "approve" || r.Head != head || r.Review != "" || p.runs != 0 {
		t.Errorf("review = %+v runs=%d", r, p.runs)
	}
	if !strings.Contains(r.Findings, "code review") {
		t.Errorf("findings = %q", r.Findings)
	}
	if got, _ := f.landReviews.Get("ag1"); got.Status != "done" || got.Verdict != "approve" || got.Head != head || !got.Card {
		t.Errorf("stored = %+v", got)
	}
	// asking again (the pre-push hook) still spends no review
	if res, err := f.Act("land-review", Body{By: "ag1"}); err != nil || res.(LandReview).Verdict != "approve" || p.runs != 0 {
		t.Errorf("second ask: %+v %v runs=%d", res, err, p.runs)
	}
}

func TestLandReviewRefusesCardsNotAuthorizedToLand(t *testing.T) {
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	for _, tc := range []struct {
		name   string
		agents []Agent
		want   string
	}{
		{"building", []Agent{card("ag1", map[string]string{labelStage: "building", labelReviewed: head, labelCodeOK: head})}, "building"},
		{"in code review", []Agent{card("ag1", map[string]string{labelStage: "review", labelReviewed: head})}, "review"},
		{"waiting on Zach", []Agent{card("ag1", map[string]string{labelStage: "zach", labelReviewed: head, labelCodeOK: head})}, "zach"},
		{"done", []Agent{card("ag1", map[string]string{labelStage: "done", labelReviewed: head})}, "done"},
		{"held", []Agent{landingCard(head, head, map[string]string{labelHeld: "dep1"})}, "held"},
		{"replaced", []Agent{landingCard(head, head, map[string]string{labelReplacedBy: "ag2"})}, "replaced"},
		{"no reviewed commit", []Agent{landingCard("", "", nil)}, "no reviewed commit"},
		{"changes verdict", []Agent{landingCard(head, "", map[string]string{labelVerdict: "code changes"})}, "code changes"},
		{"close verdict", []Agent{landingCard(head, "", map[string]string{labelVerdict: "close"})}, "close"},
		{"no verdict", []Agent{landingCard(head, "", map[string]string{labelVerdict: "code none"})}, "code none"},
		// a stale approval restored Landing while a new review is queued or running at the same HEAD
		{"review queued", []Agent{landingCard(head, head, map[string]string{labelAutoReview: "code"})}, "queued"},
		{"review pending", []Agent{landingCard(head, head, nil), reviewer("r9", "ag1", "code")}, "pending"},
		{"arbiter pending", []Agent{landingCard(head, head, nil), withLabels(reviewer("r9", "ag1", "code"),
			map[string]string{labelArbiter: "1"})}, "pending"},
		{"reviewer", []Agent{reviewer("ag1", "c9", "code")}, "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onBoard(t, f, tc.agents...)
			if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if p.runs != 0 {
				t.Errorf("a reviewer started: %v", p.calls)
			}
			if _, ok := f.landReviews.Get("ag1"); ok {
				t.Error("a review was stored")
			}
		})
	}
	// the board's snapshot is not enough: the card's current agent file and Paseo state decide
	t.Run("dropped since the snapshot", func(t *testing.T) {
		writeAgents(t, f, card("ag1", map[string]string{labelStage: "done", labelReviewed: head, labelCodeOK: head}))
		f.SetAgents([]Agent{landingCard(head, head, nil)})
		if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "done") || p.runs != 0 {
			t.Errorf("err = %v runs=%d", err, p.runs)
		}
	})
	t.Run("no agent file", func(t *testing.T) {
		os.RemoveAll(filepath.Join(f.cfg.Home, ".paseo", "agents"))
		f.SetAgents([]Agent{landingCard(head, head, nil)})
		if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "unreadable") || p.runs != 0 {
			t.Errorf("err = %v runs=%d", err, p.runs)
		}
	})
	t.Run("parked", func(t *testing.T) {
		p.archived["ag1"] = true
		defer delete(p.archived, "ag1")
		onBoard(t, f, landingCard(head, head, map[string]string{labelParked: ago(time.Hour)}))
		if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || p.runs != 0 {
			t.Errorf("err = %v runs=%d", err, p.runs)
		}
	})
}

// A reviewed commit is only the start of a review: without a completed approval of this HEAD (an older
// card, or one whose approval a new review cleared) the card gets a fresh landing review, and an
// earlier landing-review record is no proof.
func TestLandReviewNeedsACompletedApprovalOfThisHead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		approved func(head string) string
	}{
		{"no completion marker", func(string) string { return "" }},
		{"approval of another commit", func(string) string { return reviewedSHA }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, dir := landSetup(t)
			head := gitRun(t, dir, "rev-parse", "HEAD")
			onBoard(t, f, landingCard(head, tc.approved(head), nil))
			f.landReviews.put(LandReview{By: "ag1", Head: head, Cwd: dir, Card: true, Status: "done", Verdict: "approve", DoneAt: t0}, t0)
			res, err := f.Act("land-review", Body{By: "ag1"})
			if err != nil {
				t.Fatal(err)
			}
			if r := res.(LandReview); r.Status != "running" || r.Head != head || p.runs != 1 {
				t.Errorf("review = %+v runs=%d", r, p.runs)
			}
		})
	}
	// an old landing approval does not stand in for a new code review that is pending
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	onBoard(t, f, landingCard(head, "", nil), reviewer("r9", "ag1", "code"))
	f.landReviews.put(LandReview{By: "ag1", Head: head, Cwd: dir, Card: true, Status: "done", Verdict: "approve", DoneAt: t0}, t0)
	if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "pending") || p.runs != 0 {
		t.Errorf("pending review: err = %v runs=%d", err, p.runs)
	}
}

func TestLandReviewChecksTheCheckoutBeforeReuse(t *testing.T) {
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	onBoard(t, f, landingCard(head, head, nil))
	os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o644)
	if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Errorf("dirty card: err = %v", err)
	}
	// an external agent's finished approval is not handed out for a dirty checkout either
	onBoard(t, f)
	f.landReviews.put(LandReview{By: "ag1", Head: head, Cwd: dir, Status: "done", Verdict: "approve", DoneAt: t0}, t0)
	if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Errorf("dirty external: err = %v", err)
	}
	// nor a running review: the checkout is checked first
	f.landReviews.put(LandReview{By: "ag1", Review: "rev0", Head: head, Cwd: dir, Status: "running"}, t0)
	if _, err := f.Act("land-review", Body{By: "ag1"}); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Errorf("dirty running: err = %v", err)
	}
	if p.runs != 0 {
		t.Errorf("a reviewer started: %v", p.calls)
	}
}

func TestLandReviewOfARebasedCardIsFreshAndRecordsTheNewHead(t *testing.T) {
	f, p, dir := landSetup(t)
	old := gitRun(t, dir, "rev-parse", "HEAD")
	head := commitFile(t, dir, "work/a/conflict-fix.md", "resolved\n") // the rebase changed the candidate
	onBoard(t, f, landingCard(old, old, nil))
	res, err := f.Act("land-review", Body{By: "ag1", Why: "bin/land"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(LandReview)
	if r.Status != "running" || r.Head != head || p.runs != 1 {
		t.Fatalf("review = %+v runs=%d", r, p.runs)
	}
	onBoard(t, f, landingCard(old, old, nil), reviewer(r.Review, "ag1", "land"))
	storeLog(t, f, p, r.Review, "[Assistant] Read the rebased diff; tests pass.\nVERDICT: approve")
	f.Tick()
	got, _ := f.landReviews.Get("ag1")
	if got.Status != "done" || got.Verdict != "approve" || got.Head != head {
		t.Errorf("stored = %+v", got)
	}
	if l := p.labels("ag1"); l[labelReviewed] != head || l[labelCodeOK] != head {
		t.Errorf("labels = %v, want reviewed and approved %s", l, short(head))
	}
}

func TestLandReviewOfACardCancelledWhileItRunsIsNotUsable(t *testing.T) {
	for _, tc := range []struct {
		name string
		// board is the tick's snapshot, file the card's agent file, archived Paseo's state
		board, file func(old string) Agent
		archived    bool
	}{
		{name: "dropped", board: func(old string) Agent {
			a := landingCard(old, old, nil)
			a.ArchivedAt = t0
			return a
		}},
		{name: "sent to Zach", board: func(old string) Agent {
			return card("ag1", map[string]string{labelStage: "zach", labelReviewed: old})
		}},
		{name: "held", board: func(old string) Agent { return landingCard(old, old, map[string]string{labelHeld: "dep1"}) }},
		{name: "new code review", board: func(old string) Agent {
			return landingCard(old, "", map[string]string{labelStage: "review"})
		}},
		{name: "gone from the board", board: func(string) Agent { return Agent{} }},
		// the snapshot still shows Landing; current state does not
		{name: "moved since the snapshot", board: func(old string) Agent { return landingCard(old, old, nil) },
			file: func(old string) Agent {
				return card("ag1", map[string]string{labelStage: "building", labelAutoReview: "code"})
			}},
		{name: "archived since the snapshot", board: func(old string) Agent { return landingCard(old, old, nil) }, archived: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, dir := landSetup(t)
			old := gitRun(t, dir, "rev-parse", "HEAD")
			commitFile(t, dir, "work/a/conflict-fix.md", "resolved\n")
			onBoard(t, f, landingCard(old, old, nil))
			res, err := f.Act("land-review", Body{By: "ag1"})
			if err != nil {
				t.Fatal(err)
			}
			rid := res.(LandReview).Review
			agents := []Agent{reviewer(rid, "ag1", "land")}
			if a := tc.board(old); a.ID != "" {
				agents = append(agents, a)
			}
			onBoard(t, f, agents...)
			if tc.file != nil {
				writeAgents(t, f, tc.file(old))
			}
			p.archived["ag1"] = tc.archived
			storeLog(t, f, p, rid, "[Assistant] Looks fine.\nVERDICT: approve")
			f.Tick()
			got, _ := f.landReviews.Get("ag1")
			if got.Status != "done" || got.Verdict == "approve" || got.Verdict == "nits" ||
				!strings.Contains(got.Findings, "no longer authorized") {
				t.Errorf("stored = %+v", got)
			}
			if l := p.labels("ag1"); l[labelReviewed] != "" || l[labelCodeOK] != "" {
				t.Errorf("approval recorded on a cancelled card: %v", l)
			}
		})
	}
}

func TestLandReviewReusesAnIdenticalExternalApprovalOnly(t *testing.T) {
	f, p, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	if _, err := f.Act("land-review", Body{By: "ag1"}); err != nil {
		t.Fatal(err)
	}
	f.SetAgents([]Agent{card("ag1", nil), reviewer("new-agent-1", "ag1", "land")})
	storeLog(t, f, p, "new-agent-1", "[Assistant] Fine.\nVERDICT: nits")
	f.Tick()
	res, err := f.Act("land-review", Body{By: "ag1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(LandReview); r.Status != "done" || r.Verdict != "nits" || r.Head != head || p.runs != 1 {
		t.Errorf("same HEAD: %+v runs=%d", r, p.runs)
	}
	// a changed HEAD gets a fresh review of the actual candidate
	head2 := commitFile(t, dir, "work/a/more.md", "more\n")
	res, err = f.Act("land-review", Body{By: "ag1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := res.(LandReview); r.Status != "running" || r.Head != head2 || p.runs != 2 {
		t.Errorf("new HEAD: %+v runs=%d", r, p.runs)
	}
	// a rejected review is asked again, not reused
	f.SetAgents([]Agent{card("ag1", nil), reviewer("new-agent-2", "ag1", "land")})
	storeLog(t, f, p, "new-agent-2", "[Assistant] Broken.\nVERDICT: changes")
	f.Tick()
	if res, err := f.Act("land-review", Body{By: "ag1"}); err != nil || res.(LandReview).Status != "running" || p.runs != 3 {
		t.Errorf("after changes: %+v %v runs=%d", res, err, p.runs)
	}
}

// The completion marker is set only where a passed code review (or the arbiter) moves the card on,
// for the reviewed HEAD, and every new review clears it.
func TestCodeApprovalMarker(t *testing.T) {
	apply := func(t *testing.T, c, rev Agent, head, ruling string) map[string]string {
		t.Helper()
		f, p := newTestFactory(t, c, rev)
		fakeScan(f, nil, nil)
		headIs(f, head, nil)
		storeLog(t, f, p, c.ID, "Plan: make the requested change.\nBUILD SIDE EFFECTS: none\nTIER: low")
		storeLog(t, f, p, rev.ID, ruling)
		if err := f.ApplyVerdict(f.agent(rev.ID)); err != nil {
			t.Fatal(err)
		}
		return p.labels(c.ID)
	}
	arb := withLabels(reviewer("a1", "c1", "code"), map[string]string{labelArbiter: "1"})
	atCap := reviewLabels(map[string]string{labelCodeRounds: "3", labelArbitrated: "code", labelNote: arbiterNote})
	for _, tc := range []struct {
		name, ruling, head, want string
		c, rev                   Agent
	}{
		{"approve", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", reviewedSHA, reviewedSHA, idea("c1", reviewLabels(nil)), reviewer("r1", "c1", "code")},
		{"approve, Zach sees it first", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", reviewedSHA, reviewedSHA, card("c1", reviewLabels(nil)), reviewer("r1", "c1", "code")},
		{"arbiter approve", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", reviewedSHA, reviewedSHA, idea("c1", atCap), arb},
		{"arbiter escalate", "ALWAYS ASK: it rewrites the landing gate\nTIER: low\nVERDICT: escalate", reviewedSHA, reviewedSHA, idea("c1", atCap), arb},
		{"changes", "TIER: low\nVERDICT: changes", reviewedSHA, "", idea("c1", reviewLabels(nil)), reviewer("r1", "c1", "code")},
		{"moved HEAD", "ALWAYS ASK: none\nTIER: low\nVERDICT: approve", movedSHA, "", idea("c1", reviewLabels(nil)), reviewer("r1", "c1", "code")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if l := apply(t, tc.c, tc.rev, tc.head, tc.ruling); l[labelCodeOK] != tc.want {
				t.Errorf("code-approved = %q, want %q (labels %v)", l[labelCodeOK], tc.want, l)
			}
		})
	}
	// a new review clears it
	c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: ago(10 * time.Minute),
		labelCodeOK: reviewedSHA, labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(map[string]string{"status --porcelain": "", "rev-parse HEAD": reviewedSHA + "\n"})
	storeLog(t, f, p, "c1", "Built the requested change.\nTIER: low")
	f.Tick()
	if l := p.labels("c1"); p.runs != 1 || l[labelStage] != "review" {
		t.Fatalf("no review started: %v", l)
	} else if v, ok := l[labelCodeOK]; !ok || v != "" {
		t.Errorf("code-approved not cleared: %v", l)
	}
	if !contains(carriedFactoryLabels, labelCodeOK) {
		t.Error("code-approved is not carried to a replacement card")
	}
}

// A review that cannot start (a dirty tree, a failed launch) still ends the earlier approval.
func TestCodeApprovalMarkerClearsWhenTheReviewDoesNotStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty string
		err   error
	}{
		{"dirty tree", "?? scratch.txt\n", nil},
		{"launch fails", "", errors.New("paseo run: daemon unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: ago(10 * time.Minute),
				labelCodeOK: reviewedSHA, labelReviewed: reviewedSHA})
			f, p := newTestFactory(t, c)
			p.runErr = tc.err
			f.GitOut = gitStub(map[string]string{"status --porcelain": tc.dirty, "rev-parse HEAD": reviewedSHA + "\n"})
			cd, _ := f.card("c1")
			f.StartReview(cd, "code")
			if v, ok := p.labels("c1")[labelCodeOK]; !ok || v != "" {
				t.Errorf("code-approved not cleared: %v", p.labels("c1"))
			}
		})
	}
}

// The arbiter's approval, and Zach's approval of an arbiter's escalation, are completed code approvals.
func TestLandReviewReusesAnArbiterApproval(t *testing.T) {
	for _, verdict := range []string{"code arbiter approve", "code arbiter nits", "code arbiter escalate"} {
		t.Run(verdict, func(t *testing.T) {
			f, p, dir := landSetup(t)
			head := gitRun(t, dir, "rev-parse", "HEAD")
			onBoard(t, f, landingCard(head, head, map[string]string{labelVerdict: verdict, labelArbitrated: "code"}))
			res, err := f.Act("land-review", Body{By: "ag1"})
			if err != nil || res.(LandReview).Verdict != "approve" || p.runs != 0 {
				t.Errorf("%+v %v runs=%d", res, err, p.runs)
			}
		})
	}
}

// Once a card may no longer land, its stored approval is not handed out on a read either.
func TestLandReviewReadAfterCancel(t *testing.T) {
	f, _, dir := landSetup(t)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	onBoard(t, f, landingCard(head, head, nil))
	if _, err := f.Act("land-review", Body{By: "ag1"}); err != nil {
		t.Fatal(err)
	}
	get := func() string {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/land-review/ag1", nil))
		var r LandReview
		json.Unmarshal(rec.Body.Bytes(), &r)
		return r.Verdict
	}
	if v := get(); v != "approve" {
		t.Fatalf("before: %q", v)
	}
	// dropped since: the board snapshot still shows Landing, the agent file does not
	writeAgents(t, f, card("ag1", map[string]string{labelStage: "done", labelReviewed: head, labelCodeOK: head}))
	if v := get(); v == "approve" || v == "nits" {
		t.Errorf("after cancel: %q", v)
	}
	if r, _ := f.landReviews.Get("ag1"); r.Verdict != "approve" {
		t.Errorf("the stored record changed: %+v", r) // reads only; the record is history
	}
}
