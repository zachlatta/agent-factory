package factory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The flow: a prompt is submitted (tagged by Zach or by the system), Opus picks the repo, a builder on any
// Frontier model produces the proposed solution, three gates check it in order (does it meet the request,
// is it well designed, does Zach need to sign off), and Opus lands it. A gate's id is also its card's
// stage and its reviewer's kind.
const (
	gateRequest = "request" // does it meet the request and the unsaid context it needed
	gateDesign  = "design"  // is it well designed, on the latest main; small refactors are done here
	gateSignoff = "signoff" // does Zach need to sign off before it goes live

	submittedByZach   = "zach"
	submittedBySystem = "system"
)

// isGate reports whether s is one of the three gates.
func isGate(s string) bool { return s == gateRequest || s == gateDesign || s == gateSignoff }

// nextGate is the gate after g, or "" after the last.
func nextGate(g string) string {
	switch g {
	case gateRequest:
		return gateDesign
	case gateDesign:
		return gateSignoff
	}
	return ""
}

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

// ParseSignOff is the sign-off gate's decision: "none", why Zach must sign off, or "" with no line.
func ParseSignOff(text string) string {
	s := lastLineValue(signOffRe, text)
	if s == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimRight(strings.Fields(s)[0], ".;:,")) {
	case "none", "no", "n/a", "nothing":
		return "none"
	}
	return s
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
  webp video for UI work (kept in the evidence folder below, out of Git), test output or a before-and-after
  log when there is nothing to show on screen. Name the proof in your summary.
- Live system changes: a runbook in your final message under a RUNBOOK: heading. Investigate first, read-only, so
  it lists exactly what will happen (which host, which package from which version to which, which setting from
  what to what, the exact commands), what each step should show, how to roll back, and the final check. "ssh in
  and see what needs updating" is not a runbook. In a public repo, keep private details out of anything committed.
` + readOnlyProdRule + `
Name any deploy steps in your summary: the landing step runs them.
End your final message with one line, RESULT: code, RESULT: runbook or RESULT: both, then stop. Do NOT land.
Three gates check it next: does it meet the request, is it well designed, and does Zach need to sign off.`

// gateRubric is each gate's job.
var gateRubric = map[string]string{
	gateRequest: `You are the Request Gate: a DIFFERENT agent from the one that built this. Check whether the proposed solution
meets the original request and any unsaid context it needed. Agreeing is not your job.
- Read the request and follow-ups below, then the builder's final message and the diff:
  ` + "`git -C <cwd> log --oneline origin/main..HEAD`, `git -C <cwd> diff origin/main...HEAD`" + `
- Does it do everything that was asked, not a nearby or smaller thing? Anything cut, deferred or narrowed fails
  unless the request allows it.
- Unsaid context: what else did the old behaviour do, and who depends on it, in this repo and outside it? What
  would the requester assume without saying?
- Proof: open it. A UI change needs a webp video that shows it working. Otherwise test output or a before-and-after
  log. Run the narrowest check that proves it and paste the command and its last lines.
- A runbook must be fully investigated: exact targets, values, commands, expected results, rollback and a final
  check. Spot-check its facts read-only. Any step that says "check what needs doing" fails.
- Nothing was changed outside the worktree while building: production stays read-only until landing.
- <visibility> If the repo is public or unknown, any private data in the diff is a blocking finding:
  names or emails of people (other than Zach's public identity), Zach's personal details, internal
  hostnames or tailnet URLs (*.ts.net), local paths (/home/<user>/...), and content from <errands> or PDW.`,
	gateDesign: `You are the Design Gate: a DIFFERENT agent from the one that built this. The request gate already passed; do not
redo it. Your job is that as the codebase and the system evolve, everything stays well designed and cohesive.
- Read the diff: ` + "`git -C <cwd> diff origin/main...HEAD`" + `. It must reflect the latest origin/main: fetch, and rebase the
  branch if it is behind.
- Code quality, correct abstractions, one way of doing each thing, nothing that fights the surrounding design.
- Needed refactors get done instead of small local patches. Small ones: do them yourself in this worktree,
  run go vet and the tests (or the repo's own checks), and commit on the branch. Then approve. Big ones: send them
  back with VERDICT: changes, saying exactly what to do. Big means a redesign or more than about 150 changed lines.
- Committed scripts, tests and work items must not use /tmp for work that belongs in the worktree (tests using t.TempDir() are fine).
- Runbooks: is this the right way to change the live system, cohesive with how it already works?
- Deploy and live changes: none belong before landing. Check that deploy steps and runbook steps match the diff
  and are safe to run, including what happens if a step fails part-way or is run twice.
- Never send anything to Zach. You decide here; the next gate alone decides whether he signs off.`,
	gateSignoff: `You are the Sign-off Gate: a separate agent. Decide only whether Zach must sign off before this goes live.
Do not re-review quality or correctness: two gates already did.
Submitted by: <submitted-by>
Source: <source> (schedule means a scheduled run; an idea or scout is new work the system proposed)
Read the request and follow-ups below, the builder's final message, and the final diff (it may include refactors from the design gate):
` + "`git -C <cwd> diff origin/main...HEAD`" + `
- Submitted by Zach, and the changes are the things he asked for: no sign-off, including deletes, spending and
  messages he asked for.
- Submitted by Zach, but it includes or proposes larger architectural changes beyond what he asked for: sign-off.
- Submitted by the system: sign-off, always, except a scheduled run of a job he already approved: no sign-off.
Write, right before VERDICT:
SIGN OFF: <none, or in plain words why Zach must sign off>
A missing line counts as a sign-off. Use VERDICT: approve either way, or VERDICT: close for a duplicate or something no longer needed.`,
}

// gateTitle names a gate's reviewer agent.
func gateTitle(kind, card string) string {
	names := map[string]string{gateRequest: "Request gate", gateDesign: "Design gate", gateSignoff: "Sign-off gate"}
	return names[kind] + ": " + trunc(card, 60)
}

// reworkGateMsg sends a builder a gate's failure; that gate runs again when it stops.
func reworkGateMsg(kind string, round, limit int, notes string) string {
	name := map[string]string{gateRequest: "request gate", gateDesign: "design gate", gateSignoff: "sign-off gate"}[kind]
	return fmt.Sprintf("The %s (a different agent) sent this back (round %d of %d). Address each finding, or explain why "+
		"it is wrong. Commit on your branch, leave the worktree clean, and stop with a short summary (including the deploy steps as built, if any) ending in the RESULT: line; "+
		"restate any runbook whole. That gate runs again. %s Do not land. %s\n\n%s\n\n%s", name, round, limit, overviewReminder, noLiveChangesRule, worktreeRule, notes)
}

// landRunbookRule tells the lander how to run a runbook. Big drift goes back to the builder.
const landRunbookRule = "If this card has a runbook, it was written read-only earlier: before running it, check that it still " +
	"matches the live system. Correct minor changes yourself (a newer patch version, a moved path) and say what you changed. If there " +
	"are huge changes (the target or the approach no longer holds), change nothing more and end with one line: " +
	"RETURN TO BUILD: <what changed>. The builder reworks it for the new state. "

// migrateFlow moves a card left in a column of the old flow to the new one, once it has stopped: a card
// under plan review builds, and a card under code review goes through the gates. Its old reviewers are
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
	f.logf("%s: moving from old stage %s to the three gates", short(a.ID), st)
	if built {
		return true, f.setLabels(a.ID, map[string]string{labelStage: "building", labelAutoReview: gateRequest, labelSince: "", labelNote: "moved to the new gates"})
	}
	labels := map[string]string{labelStage: "building", labelAutoReview: gateRequest, labelSince: iso(f.Now()), labelNote: "moved to the new flow: building"}
	labels[labelAwaiting] = ""
	if err := f.setLabels(a.ID, labels); err != nil {
		return true, err
	}
	return true, f.advance(labeled(a, labels), "build", "The factory no longer reviews plans. Build this now.\n\n"+proposedSolutionRule)
}
