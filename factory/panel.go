package factory

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A code review is one reviewer or a panel. Small changes get one reviewer with the whole rubric. Larger
// or riskier ones get a panel: a correctness-and-tests reviewer, an architecture-and-consistency
// reviewer and, when the change touches a user interface, a phone UX reviewer. Their verdicts are
// merged into one decision: by the factory when all approve, else by one arbiter. Either way the card
// moves once per round (decide), so the round cap means the same for both.

const (
	lensCorrectness  = "correctness"
	lensArchitecture = "architecture"
	lensUX           = "ux"
	lensArbiter      = "arbiter"
)

// panelLines is the most lines (added plus deleted, outside _test.go files) a change may touch and
// still get one reviewer.
const panelLines = 150

// securityPaths are the secret scan's files: AGENTS.md has them reviewed as security code, so a change
// to any of them always gets the panel.
var securityPaths = []string{"factory/scan/", ".gitleaks.toml", ".githooks/", "bin/"}

// lensNames are the board and log names of each reviewer (a single reviewer has no lens).
var lensNames = map[string]string{lensCorrectness: "correctness review", lensArchitecture: "architecture review",
	lensUX: "phone UX review", lensArbiter: "panel arbiter"}

// lensFocus opens each panel reviewer's prompt: what it covers, and what the others cover.
var lensFocus = map[string]string{
	lensCorrectness: `You are on a review panel: other reviewers cover architecture and consistency (and phone UX
when a screen changes), and an arbiter merges the verdicts. Your lens is CORRECTNESS AND TESTS: does the change do
what was asked; bugs, edge cases, error paths, races and restarts; run the tests and paste the output; new tests
cover the new behaviour and fail without it; deploy steps are safe. Report blocking findings in your lens; still
report any private data in the diff. Leave design and consistency to the architecture reviewer.`,
	lensArchitecture: `You are on a review panel: other reviewers cover correctness and tests (and phone UX when a
screen changes), and an arbiter merges the verdicts. Your lens is ARCHITECTURE AND CONSISTENCY: the change is
whole (one way of doing each thing; every caller, test, doc and README updated; no leftover old path); it fits the
repo's structure and conventions; a refactor covers the whole app; nothing unasked-for; the tier is right. Report
blocking findings in your lens; still report any private data in the diff. Leave line-level bugs and running the
tests to the correctness reviewer.`,
	lensUX: `You are on a review panel: other reviewers cover correctness, tests, architecture and consistency, and
an arbiter merges the verdicts. Your lens is PHONE UX: the screens this change touches, at desktop width and at
phone width (about 390 px): every changed flow works, the layout holds, the text is plain and clear. Report
blocking findings in your lens; still report any private data in the diff. Leave code review to the others.`,
	lensArbiter: `You are the Panel Arbiter: the panel's reviewers (a different agent each) reviewed this change,
each through one lens, and did not all approve. Their reports are below. Decide the review: check each blocking
finding against the code (a quick look each), drop the ones that are wrong or out of scope, merge duplicates, and
write one list of the blocking findings that remain, so the builder gets one clear list. Approve only when none
remains. Your TIER is the highest any reviewer gave that you agree with.`,
}

// reviewSpec is a reviewer's place in a panel: its lens, its panel round and, for the arbiter, the
// reports it decides from. The zero value is a single reviewer.
type reviewSpec struct{ lens, panel, extra string }

// lensResult is a panel reviewer's recorded verdict and tier; done once it finished (with or
// without a verdict).
type lensResult struct {
	done          bool
	verdict, tier string
}

// diffStat is what a card's committed change touches, from git; err when git cannot say.
type diffStat struct {
	files []string
	lines int
	err   error
}

func (f *Factory) diffStat(cwd string) diffStat {
	if cwd == "" {
		return diffStat{err: fmt.Errorf("no worktree")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, cwd, "diff", "--numstat", "origin/main...HEAD")
	if err != nil {
		return diffStat{err: err}
	}
	var d diffStat
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fs := strings.SplitN(line, "\t", 3)
		if len(fs) < 3 {
			continue
		}
		d.files = append(d.files, fs[2])
		if strings.HasSuffix(fs[2], "_test.go") {
			continue
		}
		add, _ := strconv.Atoi(fs[0]) // binary files show "-"
		del, _ := strconv.Atoi(fs[1])
		d.lines += add + del
	}
	return d
}

func touchesSecurity(files []string) bool {
	for _, f := range files {
		for _, p := range securityPaths {
			if f == p || strings.HasPrefix(f, p) {
				return true
			}
		}
	}
	return false
}

// reviewLenses is the full panel for a change, or nil when one reviewer reviews it: a change that is
// not architecture tier, touches no UI and no security path, and changes at most panelLines lines.
// When git cannot say what changed, an architecture change still gets the panel and anything else
// one reviewer, as before panels.
func reviewLenses(tier string, d diffStat) []string {
	ui := d.err == nil && touchesUI(d.files)
	big := d.err == nil && (d.lines > panelLines || touchesSecurity(d.files))
	if tier != "architecture" && !ui && !big {
		return nil
	}
	lenses := []string{lensCorrectness, lensArchitecture}
	if ui {
		lenses = append(lenses, lensUX)
	}
	return lenses
}

// blocking reports whether a reviewer's verdict asks for more work (or gave none).
func blocking(verdict string) bool { return verdict != "approve" && verdict != "nits" }

// reviewPanel picks the reviewers for card c's code review, and the round their panel belongs to.
// nil means one reviewer. A rework round re-runs the correctness reviewer (the code changed), every
// reviewer that did not approve, and the UX reviewer when the fix touched a UI.
func (f *Factory) reviewPanel(c Card) ([]string, string) {
	all := reviewLenses(c.Tier, f.diffStat(c.Cwd))
	if all == nil {
		return nil, ""
	}
	a := f.agent(c.ID)
	round := atoi(a.L(labelCodeRounds)) + 1
	panel := fmt.Sprint(round)
	prev := f.panelLenses(c.ID, fmt.Sprint(round-1))
	if len(prev) == 0 {
		return all, panel
	}
	fixUI := false
	if slices.Contains(all, lensUX) {
		fixUI = true // unknown: re-check the UI
		if old := a.L(labelReviewed); old != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := f.GitOut(ctx, c.Cwd, "diff", "--name-only", old+"..HEAD")
			cancel()
			if err == nil {
				fixUI = touchesUI(strings.Fields(out))
			}
		}
	}
	var lenses []string
	for _, l := range all {
		r, ran := prev[l]
		switch {
		case l == lensCorrectness, !ran, blocking(f.lensResultOf(r).verdict), l == lensUX && fixUI:
			lenses = append(lenses, l)
		}
	}
	return lenses, panel
}

// panelLenses maps each lens of card id's panel round to its newest reviewer (superseded ones left out).
func (f *Factory) panelLenses(id, panel string) map[string]Agent {
	out := map[string]Agent{}
	if panel == "" {
		return out
	}
	byID := f.byID()
	for _, r := range f.agents() {
		l := r.L(labelLens)
		if r.L(labelRole) != "reviewer" || l == "" || l == lensArbiter || r.L(labelPanel) != panel ||
			!sameCard(byID, r.L(labelReviewOf), id) || f.lensResultOf(r).verdict == "superseded" {
			continue
		}
		if cur, ok := out[l]; !ok || r.CreatedAt.After(cur.CreatedAt) {
			out[l] = r
		}
	}
	return out
}

// lensResultOf is r's recorded result, this process's record first (the snapshot may predate it).
// The zero value means r has not finished.
func (f *Factory) lensResultOf(r Agent) lensResult {
	f.panelMu.Lock()
	res, ok := f.lenses[r.ID]
	f.panelMu.Unlock()
	if ok {
		return res
	}
	if r.L(labelApplied) == "" {
		return lensResult{}
	}
	return lensResult{done: true, verdict: r.L(labelVerdict), tier: r.L(labelLensTier)}
}

func (f *Factory) noteLens(id string, res lensResult) {
	f.panelMu.Lock()
	defer f.panelMu.Unlock()
	if f.lenses == nil {
		f.lenses = map[string]lensResult{}
	}
	f.lenses[id] = res
}

// openPanel is card a's open panel round, or "" when none is open.
func (f *Factory) openPanel(a Agent) string {
	f.panelMu.Lock()
	defer f.panelMu.Unlock()
	if p, ok := f.panels[a.ID]; ok {
		return p
	}
	return a.L(labelPanel)
}

func (f *Factory) notePanel(id, panel string) {
	f.panelMu.Lock()
	defer f.panelMu.Unlock()
	if f.panels == nil {
		f.panels = map[string]string{}
	}
	f.panels[id] = panel
}

// lensDone records a panel reviewer's verdict. The card moves only once its whole panel has one.
// A reviewer that gave none is re-run once on its own.
func (f *Factory) lensDone(rev, t Agent, verdict, tier string) error {
	if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: verdict, labelLensTier: tier}); err != nil {
		return err
	}
	f.noteLens(rev.ID, lensResult{done: true, verdict: verdict, tier: tier})
	f.deferArchive(rev.ID)
	panel := rev.L(labelPanel)
	if panel == "" || f.openPanel(t) != panel {
		f.logf("%s of %s finished after its panel closed", lensNames[rev.L(labelLens)], short(t.ID))
		return nil
	}
	lens := rev.L(labelLens)
	if verdict == "" && !f.lensRerun(t.ID, panel, lens) {
		c, ok := f.card(t.ID)
		if !ok {
			return nil
		}
		f.logf("%s of %s ended without verdict; re-running it once", lensNames[lens], short(t.ID))
		return f.startLenses(c, []string{lens}, panel, "")
	}
	return f.closePanel(t)
}

// lensRerun reports whether lens already ran twice in card id's panel round.
func (f *Factory) lensRerun(id, panel, lens string) bool {
	n := 0
	byID := f.byID()
	for _, r := range f.agents() {
		if r.L(labelRole) == "reviewer" && r.L(labelLens) == lens && r.L(labelPanel) == panel &&
			sameCard(byID, r.L(labelReviewOf), id) && f.lensResultOf(r).verdict != "superseded" {
			n++
		}
	}
	return n > 1
}

// closePanel decides card t's open panel once every reviewer in it has a result: all approve, and
// the factory decides with their notes merged; a reviewer still without a verdict after its re-run,
// and the round has no verdict; anything else, and one arbiter decides from their reports.
func (f *Factory) closePanel(t Agent) error {
	panel := f.openPanel(t)
	lenses := f.panelLenses(t.ID, panel)
	if len(lenses) == 0 {
		return nil
	}
	verdict, tier, split, started := "approve", "", false, time.Time{}
	for _, r := range lenses {
		res := f.lensResultOf(r)
		if !res.done {
			return nil // still reviewing
		}
		tier = MaxTier(tier, res.tier)
		if started.IsZero() || r.CreatedAt.Before(started) {
			started = r.CreatedAt
		}
		switch {
		case res.verdict == "":
			verdict = ""
		case blocking(res.verdict):
			split = true
		case res.verdict == "nits" && verdict == "approve":
			verdict = "nits"
		}
	}
	notes, err := f.panelReports(t, panel)
	if err != nil {
		return err
	}
	if err := f.setLabels(t.ID, map[string]string{labelPanel: ""}); err != nil {
		return err
	}
	f.notePanel(t.ID, "")
	switch {
	case verdict == "":
		// a reviewer gave no verdict even after its re-run: the round has none (decide asks Zach)
		f.logf("panel %s of %s has a reviewer without verdict", panel, short(t.ID))
		return f.decide(t, reviewOutcome{kind: "code", tier: tier, notes: notes, started: started, noRerun: true}, nil)
	case !split:
		f.logf("panel %s of %s agrees: %s", panel, short(t.ID), verdict)
		return f.decide(t, reviewOutcome{kind: "code", verdict: verdict, tier: tier, notes: notes, started: started}, nil)
	}
	f.logf("panel %s of %s split; starting the arbiter", panel, short(t.ID))
	return f.startPanelArbiter(t, panel)
}

// panelReports is the reports of card t's panel round, one section per reviewer, for the arbiter and
// for the builder when the factory merges them itself.
func (f *Factory) panelReports(t Agent, panel string) (string, error) {
	lenses := f.panelLenses(t.ID, panel)
	var out []string
	for _, l := range []string{lensCorrectness, lensArchitecture, lensUX} {
		r, ok := lenses[l]
		if !ok {
			continue
		}
		full, err := f.storedReply(r)
		if err != nil {
			return "", fmt.Errorf("%s report unreadable: %w", lensNames[l], err)
		}
		name := lensNames[l]
		out = append(out, "## "+strings.ToUpper(name[:1])+name[1:]+" ("+orDash(f.lensResultOf(r).verdict)+")\n"+
			trunc(strings.TrimSpace(full), 6000))
	}
	return strings.Join(out, "\n\n"), nil
}

// startPanelArbiter starts the one reviewer that decides card t's split panel round from its reports. It
// is a review round of its own, unlike the gate arbiter (arbiter.go) that settles a gate at its cap.
func (f *Factory) startPanelArbiter(t Agent, panel string) error {
	c, ok := f.card(t.ID)
	if !ok {
		return nil
	}
	reports, err := f.panelReports(t, panel)
	if err != nil {
		return err
	}
	return f.startLenses(c, []string{lensArbiter}, panel, "The panel's reports, in full:\n\n"+reports)
}

func orDash(s string) string {
	if s == "" {
		return "no verdict"
	}
	return s
}

// startLenses starts one reviewer per lens for card c's code review panel round. reports is the
// arbiter's input. The card's panel stays open until every lens reviewer has a result; an arbiter
// starts after it closed.
func (f *Factory) startLenses(c Card, lenses []string, panel, reports string) error {
	vis := f.visibility(c.Cwd)
	for _, l := range lenses {
		rid, err := f.runReviewer(c, "code", reviewSpec{lens: l, panel: panel, extra: reports}, vis,
			"Review ("+lensNames[l]+"): "+trunc(c.Title, 50), "", "")
		if err != nil {
			return err
		}
		f.logf("started %s %s for %s (panel %s)", lensNames[l], short(rid), c.Short, panel)
	}
	if len(lenses) == 1 && lenses[0] == lensArbiter {
		return nil
	}
	if err := f.setLabels(c.ID, map[string]string{labelPanel: panel}); err != nil {
		return err
	}
	f.notePanel(c.ID, panel)
	return nil
}

// panelReady reports whether card a has an open panel whose reviewers all finished.
func (f *Factory) panelReady(a Agent) bool {
	lenses := f.panelLenses(a.ID, f.openPanel(a))
	if len(lenses) == 0 {
		return false
	}
	for _, r := range lenses {
		if !f.lensResultOf(r).done {
			return false
		}
	}
	return true
}
