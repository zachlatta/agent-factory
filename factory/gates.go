package factory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The flow: a prompt is submitted (tagged by Zach or by the system), Opus picks the repo, a builder on any
// Frontier model produces the proposed solution, one gate grades it, Zach signs off only when the gate says
// so, and Opus lands it. The gate's id is also its card's stage and its reviewer's kind.
const (
	gateCheck = "gate" // the one gate: meets request and works, design, public safety, sign-off

	// the three gates of the earlier flow: cards left there move to the gate (migrateFlow), and
	// timing and usage history still name them
	oldGateRequest = "request"
	oldGateDesign  = "design"
	oldGateSignoff = "signoff"

	submittedByZach   = "zach"
	submittedBySystem = "system"
)

// isGate reports whether s is the gate.
func isGate(s string) bool { return s == gateCheck }

// isOldGate reports whether s is one of the earlier flow's three gates.
func isOldGate(s string) bool {
	return s == oldGateRequest || s == oldGateDesign || s == oldGateSignoff
}

// isGateSpan reports whether a recorded stage or round kind s is a gate, now or in the earlier flow.
func isGateSpan(s string) bool { return isGate(s) || isOldGate(s) }

// submittedBy is who submitted card a: its tag, else what its source shows. Only an email, a voice memo or
// a card he started in Paseo is surely Zach's. An untagged board card may be an agent's, so it is the
// system's: that costs at most one extra sign-off.
func submittedBy(a Agent) string {
	if s := a.L(labelSubmittedBy); s != "" {
		return s
	}
	switch source(a) {
	case "email", "voice memo", "app":
		return submittedByZach
	}
	return submittedBySystem
}

// submissionTag is the tag a new card gets: Zach only when the board says so or his email or voice memo
// started it. Anything else is automatic, so an agent's own POST never counts as his ask.
func submissionTag(b Body) string {
	if b.Origin == "idea" || b.SubmittedBy == submittedBySystem {
		return submittedBySystem
	}
	if b.SubmittedBy == submittedByZach || fromEmail(b) {
		return submittedByZach
	}
	return submittedBySystem
}

// landable reports whether Zach's Approve & land applies to agent a: not while its stored stage says it is
// still building, picking a repo or in a gate, whatever column the board shows it in. The board reads the
// same answer (Card.CanLand), so it never offers a button the server refuses.
func landable(a Agent) bool {
	st := a.L(labelStage)
	return st != "building" && st != stageRoute && !isGate(st)
}

// landingChoice is the model every landing runs on.
func landingChoice() AgentChoice {
	c, _ := agentByID(defaultAgent)
	return c
}

// builderChoiceLabel is the agent choice that builds card a, kept on factory.builder-agent once a
// lander has taken over its factory.agent label.
func builderChoiceLabel(a Agent) string {
	if id := a.L(labelBuilderAgent); id != "" {
		return id
	}
	return a.L(labelAgent)
}

// stageChoice is the model a's next stage runs on: Opus for landing, the card's builder otherwise.
func stageChoice(a Agent, stage string) (AgentChoice, bool) {
	if stage == "landing" {
		return landingChoice(), true
	}
	if c, ok := agentByID(builderChoiceLabel(a)); ok {
		return c, true
	}
	return sameChoice(a)
}

var (
	resultRe        = regexp.MustCompile(`(?im)^[ \t>*_#-]*RESULT[*_]*\s*:[ \t]*[*_]*(code|runbook|both)\b`)
	returnToBuildRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*RETURN[ \t]+TO[ \t]+BUILD[*_]*\s*:[ \t]*(.*)$`)
	signOffRe       = regexp.MustCompile(`(?im)^[ \t>*_#-]*SIGN[ \t-]*OFF[*_]*\s*:[ \t]*(.*)$`)
)

// ParseResult is the proposed solution's last RESULT line: code, runbook or both, or "".
func ParseResult(text string) string {
	return lastMatch(resultRe, text)
}

// ParseReturnToBuild is a lander's reason to send a runbook back to its builder, or "".
func ParseReturnToBuild(text string) string {
	return lastLineValue(returnToBuildRe, text)
}

// ParseSignOff is the gate's sign-off decision: "none", why Zach must sign off, or "" with no line. The
// line is a grade: green means none; yellow or red mean he signs off, for the reason after the grade.
func ParseSignOff(text string) string {
	s := lastLineValue(signOffRe, text)
	if s == "" {
		return ""
	}
	first := strings.ToLower(strings.Trim(strings.Fields(s)[0], ".;:,*_`()"))
	switch first {
	case "none", "no", "n/a", "nothing", "green":
		return "none"
	case "yellow", "red":
		if why := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s[len(strings.Fields(s)[0]):]), "-–—:;,.()*_` ")); why != "" {
			return strings.TrimRight(why, ")")
		}
	}
	return s
}

// The gate's checks, as its grade lines name them. Each is graded green, yellow or red; public safety is
// n/a outside public repos. Every check but sign-off sends the work back to the builder until it is green.
const (
	checkRequest = "request" // meets request: does what was asked, with the unsaid context
	checkWorks   = "works"   // part of meets request: the gate tested that it works
	checkDesign  = "design"
	checkPublic  = "public" // public repo safety
	checkSignoff = "signoff"
)

// gradeLines are the checks in report order, with the line that grades each and its name for people.
var gradeLines = []struct{ check, line, name string }{
	{checkRequest, "MEETS REQUEST", "meets request"},
	{checkWorks, "WORKS", "meets request: works"},
	{checkDesign, "DESIGN", "design"},
	{checkPublic, "PUBLIC SAFETY", "public safety"},
	{checkSignoff, "SIGN OFF", "sign-off"},
}

var gradeRes = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, g := range gradeLines {
		name := strings.ReplaceAll(g.line, " ", `[ \t-]*`)
		m[g.check] = regexp.MustCompile(`(?im)^[ \t>*_#-]*` + name + `[*_]*\s*:[ \t*_` + "`" + `]*(green|yellow|red|n/a|none)\b`)
	}
	return m
}()

// ParseGrades reads the gate's grade lines: check to green, yellow, red or n/a. A check with no line is
// absent. On the sign-off line, none means green.
func ParseGrades(text string) map[string]string {
	out := map[string]string{}
	for _, g := range gradeLines {
		v := strings.ToLower(lastMatch(gradeRes[g.check], text))
		if v == "none" {
			v = "green"
			if g.check != checkSignoff {
				continue
			}
		}
		if v != "" {
			out[g.check] = v
		}
	}
	return out
}

// gateFailures names the checks that send the work back: any that is not green. Public safety counts only in a
// public repo; sign-off never does, since it only decides whether the work goes to Zach.
func gateFailures(grades map[string]string, public bool) []string {
	var out []string
	for _, g := range gradeLines {
		if g.check == checkSignoff || (g.check == checkPublic && !public) {
			continue
		}
		if grades[g.check] != "green" {
			out = append(out, g.name+" "+cmp.Or(grades[g.check], "not graded"))
		}
	}
	return out
}

// gradesLabel is the grades as one label value: "request:green works:yellow ...", "-" for a check with no grade.
func gradesLabel(grades map[string]string) string {
	parts := make([]string, 0, len(gradeLines))
	for _, g := range gradeLines {
		parts = append(parts, g.check+":"+cmp.Or(grades[g.check], "-"))
	}
	return strings.Join(parts, " ")
}

// lastLineValue is re's last match without markdown; empty values and <placeholders> are skipped.
func lastLineValue(re *regexp.Regexp, text string) string {
	m := re.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		s := strings.Trim(strings.TrimSpace(strings.ReplaceAll(m[i][1], "`", "")), "*_\"' ")
		if s != "" && !strings.HasPrefix(s, "<") {
			return s
		}
	}
	return ""
}

// noOwnCommits reports whether the worktree at cwd surely has nothing unpublished. A failed read says no.
func (f *Factory) noOwnCommits(cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, cwd, "rev-list", "origin/main..HEAD")
	return err == nil && strings.TrimSpace(out) == ""
}

// publishedScanNote scans what a public repo's card already pushed, or "" when it passes or the repo is
// private. A hit, or a scan that cannot run, is a note that waits for Zach.
func (f *Factory) publishedScanNote(a Agent) string {
	if !f.guarded(a.Repo) {
		return ""
	}
	found, err := "", error(nil)
	if base := a.L(labelBase); base == "" {
		err = errors.New("no base commit to scan from")
	} else {
		found, err = f.scanCard(a.Cwd, base+"..HEAD")
	}
	switch {
	case err != nil:
		return trunc(landDiffNotePrefix+": already published"+landDiffScanMark+"could not run: "+err.Error(), 300)
	case found != "":
		return trunc(landDiffNotePrefix+": already published"+landDiffScanMark+"found: "+found, 300)
	}
	return ""
}

// readOnlyProdRule is what the builder may touch. It is a rule the agent follows, not enforced.
const readOnlyProdRule = `Read-only against production: everything here is read-only against prod and every external system.
In the worktree do whatever the work needs, including starting dev servers bound to loopback or this host's
tailnet IP. Make no external change: no prod writes, deploys, restarts, package installs on hosts, settings,
messages or spending. Those wait for the landing step, as a runbook. ` + noLiveChangesRule

// proposedSolutionRule is what step 3 produces.
const proposedSolutionRule = `Build the proposed solution now, all of it, in this pass. Do not write a plan and stop. Keep it to the
smallest thing that fully does what was asked (settings count, not only code). It is one or both of:
- Code changes: commits on this card's branch with a clean worktree, plus proof it works that fits the change: a
  webp video for UI work (kept in the evidence folder below, out of Git, under 5 MB: record frames with the board
  helper or a browser, join them with img2webp, and name it the way the evidence rule says), test output or a before-and-after
  log when there is nothing to show on screen. Name the proof in your summary.
- Live system changes: a runbook in your final message under a RUNBOOK: heading. Investigate first, read-only, so
  it lists exactly what will happen (which host, which package from which version to which, which setting from
  what to what, the exact commands), what each step should show, how to roll back, and the final check. "ssh in
  and see what needs updating" is not a runbook. In a public repo, keep private details out of anything committed.
` + readOnlyProdRule + `
Name any deploy steps in your summary: the landing step runs them.
End your final message with one line, RESULT: code, RESULT: runbook or RESULT: both, then stop. Do NOT land.
One gate checks it next: does it meet the request and work, is it well designed, is it safe to publish in a public
repo, and does Zach need to sign off.`

// gateRubric is the gate's job. It may edit (small refactors and public-safety fixes), so it is not read-only.
var gateRubric = map[string]string{
	gateCheck: `You are the gatekeeper: a DIFFERENT agent from the one that built this. You decide whether the work in this
worktree moves forward or goes back to the builder for more work. Agreeing is not your job.
Do not read the start-here skill: this prompt has every rule you need. Audit the changes against the original request (below) and
grade each check green, yellow or red. Green means nothing is left to do; yellow means minor issues; red means it
is wrong or missing. Anything but green on checks 1 to 3 sends the work back to the builder, and you run again
when it stops, until they are green.
Read the request and follow-ups below, the builder's final message, and the diff:
` + "`git -C <cwd> log --oneline origin/main..HEAD`, `git -C <cwd> diff origin/main...HEAD`" + `

1. Meets request: does it do what was asked? Two grades.
   - MEETS REQUEST: it does everything that was asked, not a nearby or smaller thing, plus the unsaid context
     it needed: what else the old behaviour did, who depends on it in this repo and outside it, and what the
     requester would assume without saying. Anything cut, deferred or narrowed is not green unless the request
     allows it.
   - WORKS: test it yourself; do not take the builder's word or proof for it. Run the tests and the build, then
     try the change the way it will be used: for UI, start a dev server in the worktree (loopback or this host's
     tailnet IP) and use it with computer use, and check the builder's webp video against what you see; for
     a backend or CLI change, run the commands or requests it changes; for data or audit work, re-run the key
     queries and confirm the numbers. For each, paste the command and its last lines. A runbook is not run: check every
     step read-only against the live system (targets exist, values and commands are right, expected results,
     rollback and the final check hold); a step that says "check what needs doing" is red. If something cannot
     be tested without changing production, grade WORKS yellow and say what was not tested.
2. DESIGN: is it well designed? Code quality, correct abstractions, one way of doing each thing, and the codebase
   or system stays cohesive as it evolves. It must reflect the latest origin/main: fetch, and rebase the branch
   if it is behind. Needed refactors are done instead of small local patches. Small refactors: do them yourself
   in this worktree, run go vet and the tests (or the repo's own checks), and commit on the branch. Big ones
   (a redesign or more than about 150 changed lines): grade DESIGN yellow or red and say exactly what to do.
   Committed scripts, tests and work items must not use /tmp for work that belongs in the worktree (tests using
   t.TempDir() are fine). The deploy steps and runbook steps must match the diff and be safe to run, including what
   happens if a step fails part-way or is run twice. No live changes belong before landing, and nothing was changed
   outside the worktree while building.
3. PUBLIC SAFETY: <visibility> In a public repo, check every commit that would be published, not only the final
   diff: run the repo's secret scan (bin/scan) if it has one and paste its result; no secrets, internal hostnames
   or tailnet URLs (*.ts.net), IP addresses, local paths (/home/<user>/...), private repo names, content from
   <errands> or PDW, names or emails of people other than Zach's public identity and GitHub noreply,
   thread, message or session ids, quotes from Zach, incident history or personal facts in the diff, the commit
   messages or the commit authors; every commit uses a GitHub noreply email; nothing weakens the secret scan
   (factory/scan/, .gitleaks.toml, .githooks/, bin/), which is always red. A small leak you can remove yourself:
   rewrite the unpublished commits to drop it (a later commit that deletes it is not enough, the push would
   publish the earlier one), re-run the scan, then grade. In a private repo write PUBLIC SAFETY: n/a.
4. SIGN OFF: does Zach need to sign off before it goes live? This never sends the work back.
   Submitted by: <submitted-by>
   Source: <source> (schedule means a scheduled run; an idea or scout is new work the system proposed)
   - Submitted by Zach, and the changes are the things he asked for: green, including deletes, spending and
     messages he asked for.
   - Submitted by Zach, but it includes or proposes larger architectural changes beyond what he asked for: red.
   - Submitted by the system: red, always, except a scheduled run of a job he already approved: green.
   Unsure: yellow. After yellow or red, say in plain words why he must sign off.
If you changed anything, grade the code as it is after your change and commit it; the factory checks your
final commit again.

End with these lines, then the VERDICT line, each grade one word followed by " - " and one short reason:
MEETS REQUEST: <green|yellow|red> - <why>
WORKS: <green|yellow|red> - <what you ran>
DESIGN: <green|yellow|red> - <why>
PUBLIC SAFETY: <green|yellow|red|n/a> - <why>
SIGN OFF: <green|yellow|red> - <why>
Use VERDICT: approve when checks 1 to 3 are green, VERDICT: changes otherwise, or VERDICT: close for a
duplicate or something no longer needed.`,
}

// gateTitle names a gate's reviewer agent.
func gateTitle(_, card string) string {
	return "Gate: " + trunc(card, 60)
}

// reworkGateMsg sends a builder the gate's findings; the gate runs again when it stops.
func reworkGateMsg(failed []string, round, limit int, notes string) string {
	what := "these checks are not green: " + strings.Join(failed, "; ")
	if len(failed) == 0 {
		what = "it asked for changes"
	}
	return fmt.Sprintf("The gate (a different agent) sent this back (round %d of %d): %s. Address each finding, or explain why "+
		"it is wrong. Commit on your branch, leave the worktree clean, and stop with a short summary (including the deploy steps as built, if any) ending in the RESULT: line; "+
		"restate any runbook whole. The gate runs again. %s Do not land. %s\n\n%s\n\n%s", round, limit, what, overviewReminder, noLiveChangesRule, worktreeRule, notes)
}

// landRunbookRule tells the lander how to run a runbook. Big drift goes back to the builder.
const landRunbookRule = "If this card has a runbook, it was written read-only earlier: before running it, check that it still " +
	"matches the live system. Correct minor changes yourself (a newer patch version, a moved path) and say what you changed. If there " +
	"are huge changes (the target or the approach no longer holds), change nothing more and end with one line: " +
	"RETURN TO BUILD: <what changed>. The builder reworks it for the new state. "

// migrateFlow moves a card left in a column of the old flow to the new one, once it has stopped: a card
// under plan review builds, and a card under code review goes to the gate. Its old reviewers are
// retired. A parked card is not woken: it stays waiting on Zach. It reports whether it moved the card.
func (f *Factory) migrateFlow(a Agent) (bool, error) {
	st := a.L(labelStage)
	if a.L(labelRouting) != "" && st == "plan" {
		return true, f.setLabels(a.ID, map[string]string{labelStage: stageRoute})
	}
	if st == "zach" && a.L(labelAwaiting) == "plan" {
		// it waited for Zach to allow a build, which nothing asks now
		if a.parked() {
			return true, f.setLabels(a.ID, map[string]string{labelAwaiting: "", labelNote: "the factory no longer asks before building: reply to build it"})
		}
		st = "plan"
	}
	if isOldGate(a.L(labelStuckFrom)) {
		if err := f.setLabels(a.ID, map[string]string{labelStuckFrom: gateCheck}); err != nil {
			return true, err
		}
	}
	if isOldGate(st) {
		return f.migrateOldGate(a)
	}
	if st != "plan" && st != "plan-review" && st != "review" && !(st == "building" && (a.L(labelAutoReview) == "code" || a.L(labelAutoReview) == "plan")) {
		return false, nil
	}
	if !a.stopped() || a.archived() {
		return false, nil
	}
	g := f.gate(a.ID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := f.supersedeReviewers(a.ID); err != nil {
		return true, err
	}
	reply, _ := f.storedReply(a)
	built := st == "review" || st == "building" || ParseResult(reply) != ""
	f.logf("%s: moving from old stage %s to the gate", short(a.ID), st)
	if built {
		return true, f.setLabels(a.ID, map[string]string{labelStage: "building", labelAutoReview: gateCheck, labelSince: "", labelNote: "moved to the new gates"})
	}
	labels := map[string]string{labelStage: "building", labelAutoReview: gateCheck, labelSince: iso(f.Now()), labelNote: "moved to the new flow: building"}
	labels[labelAwaiting] = ""
	if err := f.setLabels(a.ID, labels); err != nil {
		return true, err
	}
	return true, f.advance(labeled(a, labels), "build", "The factory no longer reviews plans. Build this now.\n\n"+proposedSolutionRule)
}

// migrateOldGate moves a card left in one of the three earlier gates to the one gate. A card marked stuck
// there stays stuck, now in the gate column. Otherwise, once its builder has stopped, its old reviewers are
// retired and the gate runs from the start: a verdict of the old gates covers none of the new checks.
func (f *Factory) migrateOldGate(a Agent) (bool, error) {
	if a.L(labelStuck) != "" {
		return true, f.setLabels(a.ID, map[string]string{labelStage: gateCheck})
	}
	if !a.stopped() || a.archived() {
		return false, nil
	}
	g := f.gate(a.ID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := f.supersedeReviewers(a.ID); err != nil {
		return true, err
	}
	f.logf("%s: moving from old gate %s to the gate", short(a.ID), a.L(labelStage))
	return true, f.setLabels(a.ID, map[string]string{labelStage: "building", labelAutoReview: gateCheck, labelSince: "",
		labelNote: "moved to the one gate"})
}
