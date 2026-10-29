package factory

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Tiers decide how much review a card gets. The plan reviewer sets a card's tier (the higher of
// its call, the builder's and any hint from the idea); a code reviewer can raise it, never lower it.
// "routine" is not a review tier: it marks scheduled runs, which go through no gates. Errands that
// change no code skip the tiers too: the builder does them in one pass and says TRACK: direct.
var Tiers = []string{"low", "risky", "architecture"}

// TierHelp describes each tier. Who sees the finished work before it lands does not depend on the tier
// alone: what Zach asked for always comes to him, and factory-started work only on the always-ask list
// (alwaysask.go).
var TierHelp = map[string]string{
	"routine":      "Scheduled run of an approved job. No per-run review; it surfaces only if it gets stuck.",
	"low":          "Plan review, then code review by another agent. Lands on its own if the factory started it; what Zach asked for comes to him finished first.",
	"risky":        "Plan review and code review must both pass. Lands on its own if the factory started it; what Zach asked for comes to him finished first.",
	"architecture": "Plan review, build, code review, then Zach tries the finished change and approves the landing. A refactor inside the area of the ask does not make a card architecture; one outside it does.",
}

const tierRules = `Tiers (pick the highest that applies):
- architecture: new service, datastore, framework or dependency; a change to how components fit
  together or to a shared interface; a refactor or cleanup outside the area of the ask (architecture
  Zach did not ask for, which always comes to him before landing); anything hard to undo or publicly
  visible. Zach tries the finished change before it lands.
  Refactoring inside the area of the ask is part of the change and does not raise its tier.
- risky: production data, migrations or deletes; money, payments or purchases; auth, secrets or
  permissions; hosts, deploys, cron or infra; email or messages to anyone but Zach; anything that
  runs unattended. Needs plan review and code review sign-off from other agents.
- low: everything else (docs, copy, an isolated bug fix, tests, a small feature that follows
  existing patterns).`

// questionRule teaches agents the QUESTION: block the board shows as multiple choice (ParseQuestions).
const questionRule = `When Zach must decide something, ask it as a block he can answer by tapping, one per question
(at most 4), recommended option first; he can always write his own answer instead:
QUESTION: <one short question>
- <option (recommended)>
- <option>
Use 2 to 6 options, one line each, not in a code block. A yes/no question needs no block.`

// ideaRule lets a card's Fellow or reviewer hand out-of-scope work to the Ideas column (POST /api/idea);
// by names the suggester; note adds a role-specific line.
func ideaRule(port int, by, note string) string {
	return fmt.Sprintf(`
Ideas. If you find work worth doing outside this card's scope (a bug or gap outside the area of the ask), do not do it
here: suggest it as an idea, at most 2 per card, only with a concrete payoff. %sIt never replaces
finishing this card. First check it is not already an idea or a card in flight:
`+"`curl -s localhost:%d/api/ideas`"+` and the peers brief. Then:
curl -s -X POST localhost:%d/api/idea -H 'X-Factory: 1' -H 'Content-Type: application/json' \
  -d '{"repo":"<owner/repo>","by":"%s","tier":"<low|risky|architecture>","priority":"<high|normal|low>","title":"<outcome, at most 60 characters>","summary":"<1-2 plain sentences>","why":"<evidence>","doneWhen":"<check>","request":"<technical brief>"}'
Priority is normal unless your evidence shows it is urgent (high) or can wait (low); ideas start on their own by priority, then age.
`, note, port, port, by)
}

// wholeChangeRule is the consistency rule for every plan, build and review: changes are whole.
const wholeChangeRule = `Whole changes: do each thing one way. A change updates every caller, test, doc and README the old
behaviour touched, and leaves no old path, flag or fallback behind. Grep for the old name or pattern
and change every instance, or list each one left alone and why. Refactoring inside the area of the
ask is part of the job. A partial change (two ways of doing one thing, a stale caller, test or doc,
an old path left in place) is a blocking finding. If the whole change is too big for one card, the
plan splits it into ordered cards that together cover everything.`

var (
	verdictRe  = regexp.MustCompile(`(?i)VERDICT:\s*\**\s*(approve|nits|changes|escalate|close)`)
	tierRe     = regexp.MustCompile(`(?i)TIER:\s*\**\s*(low|risky|architecture)`)
	trackRe    = regexp.MustCompile(`(?i)TRACK:\s*\**\s*(direct)`)
	titleRe    = regexp.MustCompile(`(?im)^[ \t>*_#-]*TITLE[*_]*\s*:[ \t]*(.*)$`)
	smallFixRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*SMALL[ \t]*FIX[*_]*\s*:[ \t]*(.*)$`)
	effectsRe  = regexp.MustCompile(`(?im)^[ \t>*_#-]*BUILD[ \t]+SIDE[ \t]+EFFECTS[*_]*\s*:[ \t]*(.*)$`)
)

// sideEffectsRule asks planners and plan reviewers what building a plan would change outside its worktree
// before landing. A plan goes to building without Zach only when the reviewer writes none and the plan lists none.
const sideEffectsRule = `BUILD SIDE EFFECTS: <none, or what building it changes outside the worktree before landing: prod data
written or deleted, messages sent to anyone, money spent, hosts or services changed>
(none means nothing at all happens outside the worktree while building. Write none alone on that line,
and any reason on the next line: anything else on the line, even "none (code only)", counts as a side effect.)`

// titleCap is the most runes a planner's TITLE line keeps; longer ones are cut at a word.
const titleCap = 60

// smallFixCap is the most runes a plan reviewer's SMALL FIX summary keeps in factory.plan-fix.
const smallFixCap = 200

// ParseTitle returns the last TITLE line's value without markdown, cut to titleCap at a word,
// or "" if there is none. Empty values and the prompt's <placeholder> are skipped.
func ParseTitle(text string) string {
	m := titleRe.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		t := strings.Trim(strings.TrimSpace(m[i][1]), "*_`\"' ")
		if t == "" || strings.HasPrefix(t, "<") {
			continue
		}
		return truncWords(t, titleCap)
	}
	return ""
}

// ParseSmallFix returns the plan reviewer's last SMALL FIX line without markdown, or "" if there is
// none. Empty values and the prompt's <placeholder> are skipped; "none"/"no"/"false"/"n/a" mean not small.
func ParseSmallFix(text string) string {
	m := smallFixRe.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		s := strings.ReplaceAll(m[i][1], "`", "")
		s = strings.Trim(strings.TrimSpace(s), "*_\"' ")
		s = strings.Join(strings.Fields(s), " ")
		if s == "" || strings.HasPrefix(s, "<") {
			continue
		}
		switch strings.ToLower(strings.TrimRight(s, ".;:")) {
		case "none", "no", "false", "n/a":
			return ""
		}
		return trunc(strings.ReplaceAll(s, ",", ";"), smallFixCap)
	}
	return ""
}

// ParseSideEffects returns the last BUILD SIDE EFFECTS line without markdown: "none" when the line is a
// bare none (no, n/a or nothing, with only markdown or a trailing full stop around it), the listed
// effects otherwise, or "" when there is no line. The prompt's <placeholder> is skipped. Anything after
// the none on the same line, even a reason, makes it a list of effects: the reason belongs on the next line.
func ParseSideEffects(text string) string {
	m := effectsRe.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		s := strings.Trim(strings.TrimSpace(strings.ReplaceAll(m[i][1], "`", "")), "*_\"' ")
		if s == "" || strings.HasPrefix(s, "<") {
			continue
		}
		switch strings.ToLower(strings.TrimRight(s, ".;: ")) {
		case "none", "no", "n/a", "nothing":
			return "none"
		}
		return s
	}
	return ""
}

// buildsFirst says an architecture plan may be built before Zach sees it: the plan reviewer found that
// building it changes nothing outside its worktree, and the builder's plan lists no side effect either.
// A missing reviewer line keeps the plan-approval stop.
func buildsFirst(review, plan string) bool {
	b := ParseSideEffects(plan)
	return ParseSideEffects(review) == "none" && (b == "" || b == "none")
}

func lastMatch(re *regexp.Regexp, text string) string {
	m := re.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToLower(m[len(m)-1][1])
}

// ParseVerdict returns the reviewer's last VERDICT line, or "" if there is none.
func ParseVerdict(text string) string { return lastMatch(verdictRe, text) }

// ParseTier returns the last TIER line, or "".
func ParseTier(text string) string { return lastMatch(tierRe, text) }

// ParseTrack returns "direct" when the builder's reply ends with a TRACK: direct line (the prompt asks
// for it as the last line). Only the last non-empty line counts: the Fellow prompt has the same line
// mid-text, and `paseo logs` text cannot always tell a prompt from the reply.
func ParseTrack(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lastMatch(trackRe, lines[len(lines)-1])
}

func tierRank(t string) int {
	for i, x := range Tiers {
		if x == t {
			return i
		}
	}
	return -1
}

// MaxTier is the highest valid tier among ts, or "".
func MaxTier(ts ...string) string {
	best := ""
	for _, t := range ts {
		if tierRank(t) > tierRank(best) {
			best = t
		}
	}
	return best
}

var persona = map[string]string{
	"plan": "You are the Plan Reviewer: a DIFFERENT agent instance from the one that wrote this plan. " +
		"Your job is to find what is wrong with the plan before any code exists. Agreeing is not your job; " +
		"a rejected bad plan is the most valuable thing you can produce.",
	"code": "You are the Review Attending: a DIFFERENT agent instance from the one that did this work. " +
		"Your job is to find what is wrong before it lands. Agreeing is not your job; assume there is a bug " +
		"until you have checked.",
}

// a landing review is a code review for an agent outside the card flow: same persona and rubric
func init() { persona["land"], rubric["land"] = persona["code"], rubric["code"] }

var rubric = map[string]string{
	"plan": `Check the plan (the builder's latest messages) against Zach's request and follow-ups (section below):
- Does it solve what Zach actually asked, not a nearby problem?
- Diagnosis: is the cause or approach grounded in evidence the agent looked at?
- Files, deploy steps (if any), tests, risks and open questions named? Missing facts recorded as questions, not guessed?
- Scope: nothing outside the area of the ask. Over about 1,000 changed lines means it must be split.
- Cheaper or safer approach available?
- Whole change: does the plan name every caller, test, doc and README it must update, and remove the old
  path (rule below)? A partial change is a blocking finding.
- Tier: is the builder's TIER right? Set your own.
- Build side effects: would building it (not landing it) change anything outside the worktree: prod data
  written or deleted, messages sent to anyone, money spent, hosts or services changed? Check the plan's
  BUILD SIDE EFFECTS line against its approach and write your own, right before TIER:
  ` + sideEffectsRule + `
  A plan whose line you and the builder write as none is built without asking Zach. Any other answer, or no
  line, stops it for Zach to allow the build first: that is the only thing he is asked before building.`,
	"code": `Check the work against Zach's request and follow-ups (section below) and its plan if any:
- Read the diff: ` + "`git -C <cwd> log --oneline origin/main..HEAD`, `git -C <cwd> diff origin/main...HEAD`" + `
  (the worktree was clean when this review started: the committed work is what lands). If the work already
  landed, find its commits on origin/main.
- Correct? Run the repo's tests or the narrowest check that proves it, and paste the command and its actual
  output (last lines) in your reply. Re-check claims, numbers and links in the builder's final answer.
- Deploy and live changes: no live changes (install scripts, enabling or restarting services, resizing VMs,
  changing external settings) belong in build before code review; they wait for the landing step. If any
  deploy steps are named in the builder's summary or plan, check that they match the diff and are safe to run
  (including what happens if a step fails part-way or is run twice).
- Tests: none skipped or weakened. If new tests exist, revert the change locally in a scratch copy and
  confirm they fail (skip if impractical, and say so).
- Scope: nothing outside the area of the ask. No secrets, tokens or private data committed. Committed scripts, tests and work
  items must not use /tmp for work that belongs in the worktree (tests using t.TempDir() are fine).
- <visibility> If the repo is public or unknown, any private data in the diff is a blocking finding:
  names or emails of people (other than Zach's public identity), Zach's personal details, internal
  hostnames or tailnet URLs (*.ts.net), local paths (/home/<user>/...), and content from <errands> or PDW.
- Repo fit: follows that repo's AGENTS.md/README conventions.
- Whole change (rule below): grep for the old name, path or pattern. Is every caller, test, doc and README
  updated, with no old path left? A partial change is a blocking finding.
- Tier: the card is "<tier>". Does the change touch anything above that? Say so with TIER below.
- Always ask: does Zach have to see it before it lands even if he did not ask for it? Write, right before TIER:
  ` + alwaysAskRule + `
  The factory also checks which files the diff touches. A missing line counts as an ask.`,
}

// ReviewPrompt is the prompt for a fresh reviewer agent of the given kind (plan, code or land); port is
// the factory's, for the peers brief; vis is the repo's visibility ("public", "private" or "unknown").
func ReviewPrompt(c Card, kind string, port int, vis string) string {
	cwd := c.Cwd
	if cwd == "" {
		cwd = "."
	}
	tier := c.Tier
	if tier == "" {
		tier = "unset"
	}
	branch := c.Branch
	if branch == "" {
		branch = "n/a"
	}
	r := strings.NewReplacer("<cwd>", cwd, "<tier>", tier, "<visibility>", visibilityLine(vis)).Replace(rubric[kind]) + "\n\n" + reviewPeersRule(port, kind, c.ID)
	if kind == "plan" {
		r += "\n" + planSmallFixRule
	} else if c.PlanFix != "" {
		r += "\n" + planFixRule(c.PlanFix)
	}
	if c.Decided {
		r += "\n" + decidedRule
	}
	r += ideaRule(port, "reviewer of "+c.Short, "Posting an idea is the only write you may make. ")
	return fmt.Sprintf(`%s

Card: "%s" (Paseo agent %s on %s).
Working directory: %s
Repo: %s  Branch: %s

Read the builder's whole conversation first: `+"`paseo logs %s --tail 200`"+`.

%s

%s

%s

%s

Rules:
- Read-only. Do not edit, commit, push, land, send email, create PDW mutations, or message the builder.
  Use a scratch copy in .factory-scratch/ (listed in .git/info/exclude) if you need to try something.
- Blocking findings only: each one concrete (file:line or exact claim), with why it matters.
  Style preferences and nits are not blocking.
- Allowed verdicts: approve, nits (approve; list nits, no new work), changes (blocking findings),
  escalate (a decision only Zach can make: a real-world side effect, or something he must see before it
  lands; an arbiter agent checks it first; ask it as a QUESTION: block, below), close (duplicate, already done, or not needed).
  Do not escalate a design disagreement or a tier: if the rounds do not settle, an arbiter agent decides, not
  Zach. Zach sees finished work he asked for, and the always-ask list, anyway.
- Keep it short: at most 8 bullets.
- `+shellRule+`
- `+scratchSafetyRule+`

%s

`+overviewRule+`

End your final message with exactly two lines:
TIER: <low|risky|architecture>
VERDICT: <approve|nits|changes|escalate|close>
`, persona[kind], c.Title, c.ID, c.Host, cwd, c.Repo, branch, c.ID, requestSection(c.Request), r, tierRules, wholeChangeRule, questionRule)
}

// requestSection shows reviewers Zach's request and his follow-ups (see peers.go).
func requestSection(r *CardRequest) string {
	b := []string{"Zach's request and follow-ups (newest last; a follow-up overrides the first request where they conflict):"}
	if r == nil || (r.Base == "" && len(r.FollowUps) == 0) {
		return b[0] + "\nNot found in the session file: read the builder's first message and any later messages from Zach."
	}
	if r.Base != "" {
		b = append(b, "Request:\n"+truncWords(r.Base, reviewBlockCap))
	}
	fus := r.FollowUps
	if n := len(fus) - reviewFollowUpCap; n > 0 {
		b = append(b, fmt.Sprintf("(%d earlier follow-ups not shown: they are in the builder's conversation.)", n))
		fus = fus[n:]
	}
	for _, fu := range fus {
		b = append(b, fu.label()+":\n"+truncWords(fu.Text, reviewBlockCap))
	}
	return strings.Join(b, "\n\n")
}

// decidedRule is added to reviews of a card where Zach answered one of its stops.
const decidedRule = `- Zach has already ruled on points in this card: his messages are in the builder's conversation. Treat
  them as settled: do not raise them again as blocking. Flag only new problems, or a change that does not do
  what he chose.`

const planSmallFixRule = `- Small blocking fixes: when your verdict is changes and every blocking finding is a small, concrete fix the
  builder can apply while building and a code reviewer can check (such as a missing item in a list, an Ordering:
  or Waits on: line, or a missed call site or test — not a design disagreement, wrong diagnosis or approach, an
  unsplit >1,000-line change, or a large blocker), add one line right before TIER:
  SMALL FIX: <one-line summary of the fixes the builder must apply>
  At the plan-review rework cap, a plan with that line goes to build with those fixes applied and verified by
  code review; without it, an arbiter agent decides.`

func planFixRule(fix string) string {
	return "- Last-round plan fixes: this card went to build from the final plan-review round with small blocking fixes to apply (" +
		fix + "). The last plan review is in the builder's conversation. Verify in the diff that the builder applied every blocking fix from that review; any unapplied fix is a blocking finding."
}

const newCardPrompt = `You are the Fellow on Zach's agent factory board: you own this card from plan to landing.

Zach's request:
%s

Work in this repo (%s). Read its AGENTS.md and README first.

` + fellowComputerUseRule + `

%s
On either track, your first reply (the plan, or the direct result) includes one line naming the card:
TITLE: <the outcome in at most 60 characters, plain words, no file names>

First decide the track. Direct track: the request is an errand that changes no code: a question,
suggestions, research, a report or an email to Zach, or a PDW mutation he approves himself. Those
come out better from one strong pass than from plan and review agents. On the
direct track, do NOT write a plan: do the whole thing now, in one pass, as well as you can. Create your
<errands> work item directory (work/YYYY-MM-DD-short-name/) in this worktree first and do all your work
there, following the worktree and scratch rules below. Check
every fact at its source, including anything that changed in the last day (a rebooked flight, a
moved meeting: look at <errands> work items on origin/main from today). Deliver the result, record it in
a <errands> work item, land it (commit, then land it the way that repo's AGENTS.md says), and end your final message with the line
TRACK: direct
The direct track commits only its own <errands> work item (work/...): no other files, no other repo.
Anything else (code, config, infra, deletes, money, messages to anyone but Zach) is the build track:

Step 1 now: write a PLAN only, then stop. No file edits yet. The plan has: goal (Zach's words),
what you found (evidence), approach, files to change, deploy steps (if any; they run in the landing
step after code review, not during build), how you'll test it, risks, open questions,
and an estimate of changed lines (over 1,000 means split into smaller cards). Right before TIER:, one line:
` + sideEffectsRule + `
End the plan with one line: TIER: <low|risky|architecture>.

%s

%s

` + worktreeRule + `

` + boardBuilderRule + `

` + overviewRule + `

A different agent reviews the plan; Zach does not approve plans. He is asked before building only when
building it has side effects; otherwise it is built and reviewed, and he tries the finished change before it
lands if he asked for it (or it is on the always-ask list). When you get
"Plan approved", build it. Don't improvise: if scope must change, stop and say so. Commit on your
branch, but do NOT land or make live changes (install scripts, enable or restart services, resize VMs,
change external settings) until the factory tells you to land after code review. If stuck, stop and write a handoff: what
you tried, what you know, what you need. You run unattended: never use AskUserQuestion or plan mode.
Put the plan's open questions, and anything you need from Zach, in this form:
%s
`

const landMsg = "Land it now the way this repo lands work: read its AGENTS.md and README (and how recent commits reached " +
	"main) and do that. Land exactly the reviewed commits: no new commits or amends. If the rebase conflicts or anything must change, stop and say so " +
	"instead of landing. Then run any deploy steps from your reviewed summary and verify the landing and deploy (if a deploy step fails after the push, stop and report it; do not improvise a fix on the live system). Then stop with a one-line summary. " + overviewReminder + " The agent factory deploys itself " +
	"from its repo's main within a minute of landing (and rolls back if it does not come up): never run its install.sh " +
	"to deploy. " + shellRule + " " + scratchCleanupRule

// sideEffectMsg asks the builder of a plan whose build has side effects (or they were not ruled out) for the one
// decision Zach makes before anything is built.
const sideEffectMsg = "Your plan passed review, but building it would change something outside its worktree (or that " +
	"was not ruled out), so nothing is built until Zach allows it. That is the only thing he is asked before building. " +
	"Write him one short decision, in plain words for reading on a phone. Start with three lines:\n" +
	"DECISION: <allow this build to ...?, in one line, naming the side effect>\n" +
	"OVERVIEW: <exactly what building it changes outside the worktree (which data, which message to whom, how much " +
	"money, which host or service) and why the build needs it, at most 2 short sentences>\n" +
	"NEEDS FROM ZACH: <approve to build, or say what to change>\n" +
	"If the build can avoid the side effect (test data, a dry run, doing it at landing instead), say so and recommend " +
	"that. Do not ask him about the design: other agents reviewed the plan. Do not start building; stop after this."

const noLiveChangesRule = "Do not make live changes (install scripts, enable or restart services, resize VMs, change external settings) before code review: do them in the landing step after review and verify them."

func restate(kind string) string {
	if kind == "plan" {
		return "the whole revised plan (not just the changes)"
	}
	return "a short summary (including the deploy steps as built, if any)"
}

// reworkMsg sends a builder its reviewer's requested changes.
func reworkMsg(kind string, round, limit int, notes string) string {
	return fmt.Sprintf("A reviewer (a different agent) requested changes to your %s (round %d of %d). "+
		"Address each blocking finding, or explain why it is wrong. Then stop with %s; it will be "+
		"re-reviewed. %s Do not land. %s\n\n%s\n\n%s", kind, round, limit, restate(kind), overviewReminder, noLiveChangesRule, worktreeRule, notes)
}

func buildMsg(prefix, notes string) string {
	return prefix + " Build it now, as planned. Don't improvise: if the scope has to change, stop and say so " +
		"instead. " + noLiveChangesRule + " When done, commit everything on your branch and leave the worktree clean, " +
		"but do NOT land; stop with a short summary (including the deploy steps as built, if any). " + overviewReminder + " A code reviewer checks it next. Once it passes, Zach sees the finished work before it lands if he asked for it " +
		"or it is on the always-ask list; otherwise the factory lands it.\n\n" + worktreeRule + "\n\nReviewer notes:\n" + notes
}

func buildWithFixesMsg(notes string) string {
	return buildMsg("The last plan review found only small, concrete blocking fixes (in Reviewer notes below). "+
		"Apply these fixes, then build it now, as planned; do not stop for another plan review, and say in your summary how you applied them.", notes)
}

// findings is the reviewer's last message, trimmed for forwarding to the builder.
func findings(text string) string {
	if i := strings.LastIndex(text, "[Assistant]"); i >= 0 {
		text = text[i:]
	}
	text = strings.TrimSpace(text)
	if len(text) > 3500 {
		text = text[len(text)-3500:]
	}
	return text
}

// reviewNotes is the reviewer's whole final message (full, as its caller read it), for the builder and
// the brief. text (the last few log lines, where the verdict is) is the fallback when full is empty.
func reviewNotes(full, text string) string {
	if strings.TrimSpace(full) != "" {
		return trunc(full, 8000)
	}
	return findings(text)
}

// unreadableReview keeps outside landing requests in their own result protocol. They
// are not board cards: after the history grace expires their caller receives an escalation.
func (f *Factory) unreadableReview(rev Agent, cause error) error {
	if rev.L(labelReviewKind) == "land" {
		if f.Now().Before(historyDeadline(rev)) {
			return nil
		}
		if err := f.finishLandReview(rev, "escalate", "history unavailable: landing review could not read its session file"); err != nil {
			return err
		}
		if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: "escalate"}); err != nil {
			return err
		}
		return f.deferArchive(rev.ID)
	}
	if target, ok := f.agentOK(rev.L(labelReviewOf)); ok {
		return f.waitForHistory(target, rev, cause)
	}
	return cause
}

// ApplyVerdict records a reviewer's verdict once (factory.applied) and moves its card: at once for a
// single reviewer, a panel arbiter or the gate arbiter, once its whole panel has finished for a panel
// reviewer (panel.go).
func (f *Factory) ApplyVerdict(rev Agent) error {
	target, kind := rev.L(labelReviewOf), rev.L(labelReviewKind)
	if kind == "" {
		kind = "code"
	}
	var g *cardGate
	if kind != "land" {
		g = f.gate(resolveCardID(f.byID(), target))
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.followUp.IsZero() && !rev.CreatedAt.After(g.followUp) {
			// the card got a follow-up after this review started: the verdict does not cover it
			if err := f.setLabels(rev.ID, retireLabels("superseded", f.Now())); err != nil {
				return err
			}
			f.logf("reviewer %s: %s had a follow-up since it started, verdict retired", short(rev.ID), short(target))
			return f.deferArchive(rev.ID)
		}
	}
	text, err := storedText(f.cfg.Home, rev, 6)
	if err != nil {
		return f.unreadableReview(rev, err)
	}
	if t, ok := f.agentOK(target); ok && t.L(labelStage) == "zach" && strings.HasPrefix(t.L(labelNote), "history unavailable") {
		return nil // history appearing later must not answer Zach's pending decision
	}
	verdict := ParseVerdict(text)
	full, err := f.storedReply(rev)
	if err != nil {
		return f.unreadableReview(rev, err)
	}
	notes := reviewNotes(full, text)
	if kind == "land" {
		if verdict == "" {
			verdict = "escalate"
		}
		if err := f.finishLandReview(rev, verdict, notes); err != nil {
			return err // nothing recorded on the reviewer yet, so the next Tick retries
		}
		if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: verdict}); err != nil {
			return err
		}
		f.deferArchive(rev.ID)
		return nil
	}
	target = resolveCardID(f.byID(), target)
	t, ok := f.agentOK(target)
	if ok {
		// Zach may have dropped the card while it was in review: a message would revive it. A parked
		// card is only waiting on Zach, so its verdict is sent (which wakes it).
		gone := t.gone()
		if !gone && !t.parked() {
			if gone, err = f.liveGone(t); err != nil {
				return err // nothing recorded yet, so the next Tick retries
			}
		}
		if gone {
			if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: verdict}); err != nil {
				return err
			}
			f.deferArchive(rev.ID)
			f.logf("reviewer %s: target %s archived, verdict not sent", short(rev.ID), short(target))
			return nil
		}
	}
	lens := rev.L(labelLens)
	if ok && lens != "" && lens != lensArbiter {
		return f.lensDone(rev, t, verdict, ParseTier(text))
	}
	consume := func() error {
		if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: verdict}); err != nil {
			return err
		}
		f.deferArchive(rev.ID)
		return nil
	}
	if !ok {
		if err := consume(); err != nil {
			return err
		}
		f.logf("reviewer %s: target %s not found", short(rev.ID), short(target))
		return nil
	}
	return f.decide(t, reviewOutcome{kind: kind, verdict: verdict, tier: ParseTier(text), notes: notes,
		started: rev.CreatedAt, lens: lens, panel: rev.L(labelPanel), gate: rev.L(labelArbiter) != "", source: rev}, consume)
}

// reviewOutcome is one review round's result: a single reviewer's, a panel arbiter's, or a panel's
// merged verdict; or the gate arbiter's ruling (arbiter.go), which settles a gate and is no round.
type reviewOutcome struct {
	kind, verdict, tier, notes string
	started                    time.Time // the round's first reviewer started
	lens, panel                string    // the panel arbiter's lens and panel round; "" for the others
	gate                       bool      // the gate arbiter's ruling
	source                     Agent     // the deciding reviewer, for the history grace; zero for a merged panel
}

// decide moves card t once per review round. consume, when set, records the deciding reviewer's
// verdict; it runs after the plan is read, so an unreadable plan leaves the review to retry. The
// caller holds t's gate: a round started before t's last follow-up does not cover it and moves nothing.
func (f *Factory) decide(t Agent, r reviewOutcome, consume func() error) error {
	target, kind, verdict, notes := t.ID, r.kind, r.verdict, r.notes
	g := f.gate(target)
	if !g.followUp.IsZero() && !r.started.After(g.followUp) {
		f.logf("%s review round of %s started before its last follow-up: not applied", kind, short(target))
		return nil
	}
	if t.L(labelStage) == "landed" || g.finished {
		// a finished card stays finished: only Zach's message reopens it
		f.logf("%s verdict for finished %s not applied", kind, short(target))
		if consume != nil {
			return consume()
		}
		return nil
	}
	src := r.source
	if src.ID == "" {
		src = t
	}
	// The plan's tier, build side effects and dependency must be readable before
	// consuming its review; an absent plan cannot be treated as a low-tier plan.
	builderTier, builderPlan, builderReply := "", "", ""
	if kind == "plan" {
		var err error
		builderPlan, err = storedText(f.cfg.Home, t, 60)
		if err != nil {
			return f.waitForHistory(t, src, err)
		}
		builderReply, err = f.storedReply(t)
		if err != nil {
			return f.waitForHistory(t, src, err)
		}
		builderTier = ParseTier(builderPlan)
	}
	if consume != nil {
		if err := consume(); err != nil {
			return err
		}
	}
	tier := MaxTier(t.L(labelTier), r.tier, builderTier)
	if tier == "" && kind == "plan" {
		tier = "low"
	}
	labels := map[string]string{}
	if t.L(labelReviewerRetries) != "" {
		labels[labelReviewerRetries] = "" // a verdict or ruling counts: its replacements start afresh
	}
	if kind == "plan" {
		labels[labelPlanFix] = "" // a plan verdict replaces the last round's fixes
	}
	if tier != "" {
		labels[labelTier] = tier
	}
	if verdict == "" {
		// no verdict: never a question for Zach. A missing ruling gets one replacement arbiter, then the
		// card is marked stuck (arbiterFailed); a single reviewer is replaced (replaceReviewer).
		switch {
		case r.gate:
			return f.gateArbiterFailed(t, kind, "ended without a ruling")
		case r.lens == lensArbiter:
			return f.arbiterFailed(t, kind, r.panel, "ended without a verdict")
		}
		return f.replaceReviewer(t, kind, "ended without a verdict")
	}
	if r.gate {
		// the arbiter's ruling settles the gate; it is not a review round
		err := f.applyArbiter(t, kind, verdict, notes, tier, builderPlan, builderReply, labels)
		f.logf("%s arbiter ruled %s for %s (tier %s)", kind, verdict, short(target), tier)
		return err
	}
	var err error
	roundsKey := roundsLabel(kind)
	rounds := atoi(t.L(roundsKey)) + 1
	labels[roundsKey] = fmt.Sprint(rounds)
	gen := rounds // the round within this review generation, which the cap counts
	if kind == "code" {
		gen -= atoi(t.L(labelCodeRoundsFrom))
	}
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(target, labels)
	}
	now := iso(f.Now())
	smallFix := ""
	if kind == "plan" && verdict == "changes" && rounds >= f.cfg.ReworkCap {
		smallFix = ParseSmallFix(notes)
	}
	switch {
	case (verdict == "approve" || verdict == "nits") && kind == "plan":
		err = f.passPlan(t, labels, "plan "+verdict, tier, notes, builderPlan, builderReply, "", "Plan approved by the plan reviewer.")
	case verdict == "approve" || verdict == "nits":
		err = f.passCode(t, labels, "code "+verdict, tier, notes, "")
	case verdict == "changes" && gen < f.cfg.ReworkCap:
		back := "building"
		if kind == "plan" {
			back = "plan"
			labels[labelPlanFix] = ""
		}
		if err = set(labelStage, back, labelVerdict, kind+" changes", labelSince, now, labelAutoReview, kind); err == nil {
			if kind == "code" {
				err = f.advance(labeled(t, labels), "code-review fixes", reworkMsg(kind, gen, f.cfg.ReworkCap, notes))
			} else {
				err = f.advance(labeled(t, labels), "plan-review fixes", reworkMsg(kind, gen, f.cfg.ReworkCap, notes))
			}
		}
	case verdict == "changes" && kind == "plan" && smallFix != "":
		err = f.passPlan(t, labels, "plan fixes", tier, notes, builderPlan, builderReply, smallFix, "")
	case verdict == "close":
		err = set(labelStage, "zach", labelVerdict, "close", labelNote, "reviewer suggests closing")
	case arbitrated(t, kind):
		// the arbiter already ruled on this gate and it still did not settle: the board shows it
		err = set(labelStage, "zach", labelVerdict, kind+" "+verdict, labelNote, arbiterStuckNote, labelStuckFrom, "")
	default:
		// rework cap or escalation: an arbiter agent rules instead of Zach
		labels[labelVerdict] = kind + " " + verdict
		if err = f.setLabels(target, labels); err == nil {
			err = f.startArbiter(t, kind, notes, verdict == "escalate")
		}
	}
	f.recordRound(target, kind, rounds, r.started, verdict)
	f.logf("%s verdict %s for %s (round %d, tier %s)", kind, verdict, short(target), rounds, tier)
	return err
}

// passPlan moves card t on from a plan that passed review (or the arbiter, or the last round with small
// fixes): to building, or to Zach first when building it has side effects he has not allowed. fixes are
// blocking fixes the builder applies while building; prefix opens the build message ("" for the
// small-fix message).
func (f *Factory) passPlan(t Agent, labels map[string]string, verdict, tier, notes, builderPlan, builderReply, fixes, prefix string) error {
	// the approved plan may name a card this one lands after (deps.go)
	if id := ParseWaitsOn(builderReply, t.ID); id != t.L(labelWaitsOn) {
		labels[labelWaitsOn], labels[labelWaitsDone] = id, ""
	}
	labels[labelPlanFix] = fixes
	if t.L(labelZachPlan) != "approved" && !buildsFirst(notes, builderPlan) {
		return f.sideEffectStop(t, labels, notes)
	}
	labels[labelStage], labels[labelVerdict], labels[labelSince], labels[labelAutoReview] = "building", verdict, iso(f.Now()), "code"
	msg := buildMsg(prefix, notes)
	switch {
	case prefix == "":
		labels[labelNote] = "building with last-round plan fixes"
		msg = buildWithFixesMsg(notes)
	case fixes != "":
		labels[labelNote] = "building with the arbiter's fixes"
	}
	if err := f.setLabels(t.ID, labels); err != nil {
		return err
	}
	return f.advance(labeled(t, labels), "build", msg)
}

// sideEffectNote is the Needs Zach note of a plan whose build has side effects: the one thing Zach is asked
// before anything is built.
const sideEffectNote = "building has side effects: approve to build"

// sideEffectStop holds card t's passed plan for Zach, because building it has side effects (or they were not
// ruled out). The factory emails him once the builder has written the decision (mailZach).
// Its verdict label keeps the plan pending (planPending), so Zach's reply goes back to plan review.
func (f *Factory) sideEffectStop(t Agent, labels map[string]string, notes string) error {
	labels[labelStage], labels[labelVerdict], labels[labelAwaiting] = "zach", "plan side effects", "plan"
	labels[labelNote], labels[labelWriting] = sideEffectNote, iso(f.Now())
	if err := f.setLabels(t.ID, labels); err != nil {
		return err
	}
	return f.send(t.ID, sideEffectMsg+"\n\nReviewer notes:\n"+notes)
}

// passCode moves card t on from code that passed review (or the arbiter): a public repo's scan first, then
// to Zach when finishedWorkReason says he sees it before it lands (asked is a reason already given),
// otherwise to landing.
func (f *Factory) passCode(t Agent, labels map[string]string, verdict, tier, notes, asked string) error {
	target, now := t.ID, iso(f.Now())
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(target, labels)
	}
	if hold := f.reviewHold(t, t.Cwd, t.Repo); hold != nil {
		// the approval is for the reviewed commit only: what the card has now is not approved
		maps.Copy(labels, clearHeldApproval(t, hold))
		labels[labelVerdict] = verdict
		return f.setLabels(target, labels)
	}
	// the passed review's identity: bin/land's landing review reuses it only for this HEAD
	labels[labelCodeOK] = strings.TrimSpace(t.L(labelReviewed))
	fb := uiFeedback(notes)
	scanned := ""
	if f.guarded(t.Repo) {
		scanned = f.scanNote(t.Cwd, "") // public repo: the factory's own scan, before any landing
	}
	var err error
	if scanned != "" && !scanPassed(scanned) {
		// a scan hit or error stops the card; a clean public-repo card goes on like any other
		err = set(labelStage, "zach", labelVerdict, verdict, labelNote, scanned)
	} else if why := f.finishedWorkReason(t, tier, notes, asked); why != "" {
		// the one stop for Zach, Ready for you: the builder writes what changed, the factory runs the
		// preview (tryout.go), and mailZach waits for both
		n := readyRound(t)
		if err = set(labelStage, "zach", labelVerdict, verdict, labelNote, readyNote(n), labelReady, fmt.Sprint(n),
			labelAskWhy, why, labelWriting, now); err == nil {
			msg := readyMsg(why)
			if fb != "" {
				// one message: a second would interrupt the builder writing the first
				msg += uiFeedbackPreviewNote + fb
				fb = ""
			}
			err = f.send(target, msg)
		}
	} else {
		note := "auto-landing (" + tier + ")"
		if scanned != "" {
			note += ": public repo: scan passed at review"
		}
		if id, _ := f.pendingDep(t); id != "" {
			// the land prompt waits for that card; the next Tick sends it or asks Zach (tickHeld)
			if err = set(labelStage, "landing", labelVerdict, verdict, labelSince, now, labelHeld, id,
				labelNote, holdNote(id)); err == nil && fb != "" {
				err = f.send(target, depHoldMsg(id, fb))
			}
			return err
		}
		if err = set(labelStage, "landing", labelVerdict, verdict, labelSince, now, labelNote, note); err == nil {
			msg := fmt.Sprintf("Code review passed (%s tier). %s", tier, f.landMsgFor(t))
			if fb != "" {
				msg += uiFeedbackLandMsg(fb)
			}
			err = f.sendLanding(labeled(t, labels), msg)
		}
	}
	if err == nil && fb != "" && labels[labelStage] == "zach" {
		err = f.send(target, uiFeedbackHoldMsg(fb))
	}
	return err
}

// StartReview starts a fresh reviewer agent for card c in the card's own workspace, or for a larger or
// riskier code change a panel of them (panel.go), and returns the single reviewer's id ("" for a panel).
// A code review needs a clean worktree, so that what it reads is what lands: a dirty one gets the
// builder a message to commit first, and the review stays queued until it stops again.
func (f *Factory) StartReview(c Card, kind string) (string, error) {
	g := f.gate(c.ID)
	g.mu.Lock()
	defer g.mu.Unlock()
	return f.startReview(g, c, kind)
}

// startReview is StartReview for a caller holding card c's gate g. A follow-up delivered since the
// builder last acted (in the newest list) has not been answered yet: no review starts, and the
// follow-up's own labels keep the next step queued.
func (f *Factory) startReview(g *cardGate, c Card, kind string) (string, error) {
	if g.finished {
		f.logf("%s: finished by Zach, no %s review", c.Short, kind)
		return "", nil
	}
	if !g.delivered.IsZero() && !f.agent(c.ID).LastActivityAt.After(g.delivered) {
		f.logf("%s: follow-up not answered yet, %s review waits", c.Short, kind)
		return "", nil
	}
	// a new review is a new generation: no earlier code approval stands for it, even if this one
	// waits on a dirty tree or fails to start
	if f.agent(c.ID).L(labelCodeOK) != "" {
		if err := f.setLabels(c.ID, map[string]string{labelCodeOK: ""}); err != nil {
			return "", err
		}
	}
	labels := map[string]string{labelAutoReview: "", labelSince: iso(f.Now()), labelCodeOK: ""}
	if kind == "code" && c.Cwd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		dirty, err := f.dirtyStatus(ctx, c.Cwd)
		head, herr := f.GitOut(ctx, c.Cwd, "rev-parse", "HEAD")
		cancel()
		switch {
		case err != nil || herr != nil:
			// not a git checkout (or git failed): review anyway; the landing check then has no
			// reviewed commit and keeps the older on-main check
			f.logf("%s: no clean-tree check before review: %v %v", c.Short, err, herr)
			labels[labelReviewed] = ""
		case dirty != "":
			return "", f.askToCommit(c, dirty)
		default:
			labels[labelReviewed] = strings.TrimSpace(head)
			labels[labelDirtyAsks] = ""
		}
	}
	rid, lenses, panel := "", []string(nil), ""
	if kind == "code" {
		lenses, panel = f.reviewPanel(c)
	}
	if lenses != nil {
		if err := f.startLenses(c, lenses, panel, ""); err != nil {
			return "", err
		}
	} else {
		var err error
		rid, err = f.runReviewer(c, kind, reviewSpec{}, f.visibility(c.Cwd), "Review ("+kind+"): "+trunc(c.Title, 60), "", "")
		if err != nil {
			return "", err
		}
		f.logf("started %s review %s for %s (%s)", kind, short(rid), c.Short, trunc(c.Title, 40))
	}
	labels[labelStage] = "review"
	if kind == "plan" {
		labels[labelStage] = "plan-review"
	}
	if err := f.setLabels(c.ID, labels); err != nil {
		return rid, err
	}
	return rid, nil
}

// dirtyAskCap is how many times a builder is asked to commit before review; then Zach decides.
const dirtyAskCap = 2

// askToCommit tells a builder whose worktree is dirty to commit before code review, and keeps the
// review queued. After dirtyAskCap asks the card goes to Needs Zach.
func (f *Factory) askToCommit(c Card, dirty string) error {
	asks := atoi(f.agent(c.ID).L(labelDirtyAsks)) + 1
	if asks > dirtyAskCap {
		f.logf("%s worktree still not clean after %d asks", c.Short, dirtyAskCap)
		return f.setLabels(c.ID, map[string]string{labelStage: "zach", labelAutoReview: "",
			labelNote: "worktree not clean for review", labelReviewerRetries: ""})
	}
	if err := f.setLabels(c.ID, map[string]string{labelStage: "building", labelAutoReview: "code",
		labelSince: iso(f.Now()), labelDirtyAsks: fmt.Sprint(asks), labelReviewerRetries: ""}); err != nil {
		return err
	}
	f.logf("%s: worktree not clean, asked the builder to commit before review", c.Short)
	return f.send(c.ID, "Before code review the worktree must be clean, so the reviewer sees exactly what lands. "+
		"Commit everything that belongs to this work on your branch (do NOT land), "+
		"then stop with a short summary (including the deploy steps as built, if any). "+noLiveChangesRule+" "+overviewReminder+" The review starts when you stop.\n\n"+worktreeRule+"\n\ngit status --porcelain:\n"+trunc(dirty, 1500))
}

// staleReviewNote and the unverified notes have no comma: label values cannot hold one (labelArg).
// Every note of a card held because its reviewed commit could not be checked starts with
// unverifiedReviewNote; its Re-run starts a fresh code review (stuck-from review).
const (
	staleReviewNote      = "changed since review: fresh code review"
	unverifiedReviewNote = "reviewed commit not verified: "
	noReviewedNote       = unverifiedReviewNote + "public repo with no reviewed commit recorded"
)

// reviewHold reports why card a may not be approved for landing or sent a land prompt yet: nil when
// its HEAD is still the commit its code review saw (factory.reviewed) and its worktree is clean, else
// the labels that move it. A moved HEAD or uncommitted work goes back to a fresh code review (since
// cleared, so it starts once the agent is stopped); a HEAD or status that cannot be read stops the
// card for Zach. A card with no reviewed commit keeps the older on-main checks in a private repo,
// but stops in a public one, where nothing unreviewed may be published. Zach's approval is no
// review: it is checked the same way.
func (f *Factory) reviewHold(a Agent, cwd, repo string) map[string]string {
	zach := func(note string) map[string]string {
		return map[string]string{labelStage: "zach", labelNote: note, labelStuckFrom: "review",
			labelHeld: "", labelAutoReview: ""}
	}
	reviewed := strings.TrimSpace(a.L(labelReviewed))
	if reviewed == "" {
		if f.guarded(repo) {
			return zach(noReviewedNote)
		}
		return nil
	}
	if cwd == "" {
		return zach(unverifiedReviewNote + "could not read HEAD: no worktree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head, err := f.GitOut(ctx, cwd, "rev-parse", "HEAD")
	if err != nil {
		return zach(unverifiedReviewNote + "could not read HEAD: " + trunc(err.Error(), 150))
	}
	stale := map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: "", labelHeld: "",
		labelNote: staleReviewNote}
	if head = strings.TrimSpace(head); head != reviewed {
		f.logf("%s HEAD %s is not the reviewed %s: fresh code review", short(a.ID), short(head), short(reviewed))
		return stale
	}
	// uncommitted work on the reviewed commit was not reviewed either: it is committed and reviewed first
	dirty, err := f.dirtyStatus(ctx, cwd)
	if err != nil {
		return zach(unverifiedReviewNote + "could not read worktree status: " + trunc(err.Error(), 150))
	}
	if dirty != "" {
		f.logf("%s has uncommitted work on the reviewed %s: fresh code review", short(a.ID), short(reviewed))
		return stale
	}
	return nil
}

// runReviewer starts a read-only reviewer of the given kind for c. For a landing review, why is the
// agent's note and head the commit under review.
func (f *Factory) runReviewer(c Card, kind string, spec reviewSpec, vis, title, why, head string) (string, error) {
	if a, ok := f.agentOK(c.ID); ok && c.Request == nil {
		r := f.cardRequest(a)
		c.Request = &r
	}
	prompt := strings.ReplaceAll(ReviewPrompt(c, kind, f.cfg.Port, vis), "<errands>", f.errandsName())
	if f.guarded(c.Repo) {
		prompt += publicReviewRule
	}
	if spec.lens != "" {
		prompt = lensFocus[spec.lens] + "\n\n" + prompt
		if spec.extra != "" {
			prompt += "\n\n" + spec.extra
		}
	}
	if spec.lens == lensUX || (spec.lens == "" && kind != "plan" && f.uiChange(c)) {
		if c.Repo == "zachlatta/agent-factory" {
			prompt += boardLayoutRule
		} else {
			prompt += uiReviewRule
		}
	}
	if kind == "land" {
		if why == "" {
			why = "(none given)"
		}
		prompt = fmt.Sprintf("This is a LANDING REVIEW: agent %s asks before it lands commit %s on main. What lands "+
			"is `git -C %s log origin/main..%s`. Its note: %s\n\n%s", c.ID, head, c.Cwd, head, why, prompt)
	}
	var labels map[string]string
	if spec.lens != "" {
		labels = map[string]string{labelLens: spec.lens, labelPanel: spec.panel}
	}
	return f.startReviewer(c, kind, title, prompt, labels)
}

// startReviewer starts a read-only reviewer agent of kind for c with prompt (the sub-agent rule is
// added here), with labels besides its role, card and kind.
func (f *Factory) startReviewer(c Card, kind, title, prompt string, labels map[string]string) (string, error) {
	// reviewerRunArgs: reviewers, landing reviewers and arbiters too, stay on the default agent (Opus 5.5)
	args := append(reviewerRunArgs(),
		"--title", title,
		"--label", labelArg(labelRole, "reviewer"), "--label", labelArg(labelReviewOf, c.ID),
		"--label", labelArg(labelReviewKind, kind), "--background", "--quiet")
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", labelArg(k, labels[k]))
	}
	if c.WorkspaceID != "" {
		args = append(args, "--workspace", c.WorkspaceID)
	}
	if c.Cwd != "" {
		args = append(args, "--cwd", c.Cwd)
	}
	out, err := f.paseo(append(args, prompt+f.delegateRule(true))...)
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

// Tick runs the factory's automatic moves (Sinai's label-in / label-out stages).
func (f *Factory) Tick() {
	if err := f.reconcileQuiet(); err != nil {
		f.logf("quiet labeling: %v", err)
	}
	f.reconcileTimings()
	f.denyPrompts(f.agents())
	now := f.Now()
	p := f.beginPass()
	defer f.endPass()
	f.tickHot(p) // agents the watcher saw stop go first (watch.go)
	for _, a := range f.agents() {
		if f.stopping.Load() {
			return
		}
		f.tickHot(p)
		if p.done[a.ID] {
			continue
		}
		if cur, ok := f.agentByID(a.ID); ok {
			a = cur // a hot pass may have reread the files since this list was taken
		}
		p.done[a.ID] = true
		if err := f.tickAgent(a, now); err != nil {
			f.logf("rule error on %s: %v", short(a.ID), err)
		}
	}
	f.drainArchives(p) // reviewer and scout archives, after every move
	if f.stopping.Load() {
		return
	}
	f.tickSteers()
	if err := f.tickIdeas(); err != nil {
		f.logf("idea error: %v", err)
	}
	f.tickHandoffs(now)
	f.reap(now)
	f.tickPreviews(now)
	f.checkReplies(now)
	f.probeMail()
	f.recordShipped()
	f.tickDigest()
}

// actedGrace: a card counts as having acted only this long after its last move (see tickAgent).
const actedGrace = 45 * time.Second

func (f *Factory) tickAgent(a Agent, now time.Time) error {
	if a.L(labelRole) == "" && replacedPredecessor(f.byID(), a) {
		return nil
	}
	old := now.Sub(a.CreatedAt) > time.Minute
	switch {
	case quietAnchor(a):
		return nil
	case a.L(labelRole) == "reviewer":
		if a.L(labelApplied) == "" && (a.Status == "idle" || a.Status == "closed") && old {
			return f.ApplyVerdict(a)
		}
		return nil
	case a.L(labelRole) == "scout":
		if a.stopped() && old {
			f.logf("scout %s finished", short(a.ID))
			return f.deferArchive(a.ID)
		}
		return nil
	}
	if f.handoffBusy(a) {
		return nil // tickHandoffs moves it until its replacement is confirmed
	}
	if handled, err := f.tickError(a, now); handled || err != nil || a.Status == "error" {
		return err
	}
	if a.L(labelStuck) != "" && a.Status == "running" && !a.archived() {
		// running again (a reply, Resume or Re-run): automatic recovery starts afresh
		if err := f.setLabels(a.ID, map[string]string{labelStuck: ""}); err != nil {
			return err
		}
	}
	routing := a.L(labelRouting) != ""
	if a.L(labelStage) == "plan" && a.L(labelSource) == "board" && a.L(labelBase) == "" && !routing && !a.archived() {
		f.recordBase(a)
	}
	if st := a.L(labelStage); st != "" && st != "zach" && a.L(labelTold) != "" && !a.archived() {
		// left Needs Zach: the next wait is emailed even if its note is the same
		if err := f.setLabels(a.ID, map[string]string{labelTold: ""}); err != nil {
			return err
		}
	}
	f.markDepLanded(a)
	f.clearNudges(a)
	if heldApprovedParked(a) && a.Status != "running" {
		// Zach approved this card while its dependency was in flight, then it was parked: release it
		// (or stop it for him) once the dependency lands. No other parked card is moved.
		if c, ok := f.card(a.ID); ok {
			return f.tickHeld(a, c)
		}
		return nil
	}
	if !a.stopped() || a.L(labelStuck) != "" {
		return nil // a card marked stuck has no automatic move until it runs again or Zach acts
	}
	// "acted": the agent did something after the last move and is idle again. The 45 s grace covers
	// the gap between `paseo send` and the agent flipping to running.
	since := parseTime(a.L(labelSince))
	acted := !since.IsZero() && a.LastActivityAt.After(since) && now.Sub(since) > actedGrace
	c, ok := f.card(a.ID)
	if !ok {
		return nil
	}
	switch {
	case routing && a.L(labelStage) == "plan" && (acted || a.L(labelRouted) != ""):
		if live, err := f.stillStopped(a); !live {
			return err
		}
		return f.tickPicker(a)
	case a.L(labelAutoReview) != "" && (acted || since.IsZero()) && a.L(labelStage) != "landed": // Done needs no review
		if live, err := f.stillStopped(a); !live {
			return err
		}
		// A landed-diff recovery must review the published HEAD even when there are no
		// unpushed commits. A verification carried from an earlier landing cannot cover it.
		if a.L(labelStage) == "building" && a.L(labelAutoReview) == "code" && a.L(labelLandedVerified) == "1" && !readRecovery(a).LandedDiff {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			commits, err := f.GitOut(ctx, c.Cwd, "rev-list", "origin/main..HEAD")
			cancel()
			if err == nil && strings.TrimSpace(commits) == "" {
				reply, err := f.storedReply(a)
				if err != nil {
					return f.waitForHistory(a, a, err)
				}
				return f.finishLanding(a, c, reply)
			}
		}
		if a.L(labelAutoReview) == "code" && a.L(labelPlanRounds) == "" && !f.guarded(c.Repo) {
			direct, err := f.directReply(a)
			if err != nil {
				return f.waitForHistory(a, a, err)
			}
			if direct {
				// A direct card sent to code review: finish its errand instead of reviewing it.
				return f.markDirect(a, c)
			}
		}
		_, err := f.StartReview(c, a.L(labelAutoReview))
		return err
	case a.L(labelStage) == "plan" && (a.L(labelSource) == "board" || fromInbox(a)) && acted:
		if live, err := f.stillStopped(a); !live {
			return err
		}
		// only the agent's own last reply: the Fellow prompt itself has a "TRACK: direct" line
		text, err := f.storedReply(a)
		if err != nil {
			return f.waitForHistory(a, a, err)
		}
		f.nameCard(a, text)
		if ParseTrack(text) == "direct" {
			return f.markDirect(a, c)
		}
		if fromInbox(a) {
			// an inbox card that wrote no plan (no TIER line) did an errand, as email agents did before;
			// its last reply may be a line after its final command, so read recent session text
			text, err := storedText(f.cfg.Home, a, 60)
			if err != nil {
				return f.waitForHistory(a, a, err)
			}
			if ParseTier(text) == "" {
				return f.markDirect(a, c)
			}
		}
		_, err = f.StartReview(c, "plan")
		return err
	case a.L(labelStage) == "landing" && a.L(labelHeld) != "":
		return f.tickHeld(a, c)
	case a.L(labelStage) == "landing" && acted:
		if live, err := f.stillStopped(a); !live {
			return err
		}
		if f.landReviewPending(a) {
			return nil // bin/land is waiting on its landing review: the landing has no result yet
		}
		if f.landRunning(a) {
			return nil // this agent's bin/land is still running: its scan, tests or push are not done
		}
		// Read before recording a landing: history retries must not repeat that write. Only this
		// agent's own reply counts: a replaced agent's report is not this landing's result.
		reply, err := storedReplyOne(f.cfg.Home, a)
		if err != nil {
			return f.waitForHistory(a, a, err)
		}
		if strings.TrimSpace(reply) == "" {
			// the turn ended on a command or its result: a landing still running, or one that
			// stalled, which the stopped-step recovery nudges and then re-runs
			return f.checkStuck(a, now)
		}
		switch status, why := f.checkLanding(a, c.Cwd); status {
		case landNotOnMain:
			if _, found := ParseRemaining(reply); !found && a.L(labelLandRun) != "" {
				// its bin/land run is over but the reply has no REMAINING line, which the final report
				// ends with: a progress note written while it ran, and the agent has not reported yet.
				// One that never reports goes through the stopped-step recovery.
				return f.checkStuck(a, now)
			}
			// a push failure or rebase conflict: the card fixes its branch, then code review again
			f.logf("%s landing not verified: %s", short(a.ID), why)
			return f.returnToBuild(a, "landing not verified", landFailMsg(why), false)
		case landDiffers:
			f.logf("%s landed diff differs from the reviewed one: %s", short(a.ID), why)
			return f.landedDiffDiffers(a, c, why)
		}
		f.recordLanded(a, c.Cwd)
		return f.finishLanding(a, c, reply)
	case a.L(labelStage) == "zach":
		if moved, err := f.migrateWait(a, c); moved || err != nil {
			return err
		}
		if f.Mail == nil || lineageTold(f.byID(), a) == waitNote(a) || !mailable(c.Ask) {
			return nil
		}
		if live, err := f.stillStopped(a); !live {
			return err
		}
		return f.mailZach(a, c)
	}
	return f.checkStuck(a, now)
}

const stuckAfter = 2 * time.Minute

func historyDeadline(source Agent) time.Time {
	ref := source.LastActivityAt
	if source.CreatedAt.After(ref) {
		ref = source.CreatedAt
	}
	if since := parseTime(source.L(labelSince)); since.After(ref) {
		ref = since
	}
	return ref.Add(stuckAfter)
}

// waitForHistory retries a stopped turn's unreadable session for two minutes. It never
// substitutes empty text for a decision, consumes a verdict, or sends the agent a message.
func (f *Factory) waitForHistory(card, source Agent, cause error) error {
	f.logf("%s decision history not read: %v", short(card.ID), cause)
	if f.Now().Before(historyDeadline(source)) || card.gone() || card.L(labelStage) == "zach" {
		return nil
	}
	if live, err := f.stillStopped(card); !live {
		return err
	}
	return f.setLabels(card.ID, map[string]string{
		labelStage: "zach", labelNote: "history unavailable: decision paused",
		labelStuckFrom: card.L(labelStage), labelAutoReview: "",
	})
}

// stuckWhy names the working columns, where automatic recovery runs, and what a stopped card there is missing.
var stuckWhy = map[string]string{
	"plan":        "Planner stopped, nothing queued",
	"building":    "Builder stopped, nothing queued",
	"plan-review": "Reviewer gone, no verdict",
	"review":      "Reviewer gone, no verdict",
	"landing":     "Landing agent stopped without landing",
}

// landReviewPending reports whether card a's landing review is still deciding: its record is running
// and its reviewer is listed, not archived, and either running or stopped with its verdict not yet
// applied. Failed, blocked and missing reviewers do not hide a final report or its recovery path.
func (f *Factory) landReviewPending(a Agent) bool {
	lr, ok := f.landReviews.Get(a.ID)
	if !ok || lr.Status != "running" {
		return false
	}
	r, ok := f.agentOK(lr.Review)
	return ok && lr.Review != "" && !r.archived() && !reviewerBlocked(f.agents(), r, f.Now()) &&
		(r.Status == "running" || r.L(labelApplied) == "")
}

// checkStuck recovers a stopped factory card when no automatic move is coming for it: nothing should
// sit waiting. It runs after the other rules, so a card reaching it had no move this Tick. A review
// column replaces its gone or blocked reviewer (replaceReviewer); plan, building and landing get a
// nudge, then a re-run of their step (stepStuck). Needs Zach, Done and cards marked stuck are skipped.
func (f *Factory) checkStuck(a Agent, now time.Time) error {
	stage := a.L(labelStage)
	if stuckWhy[stage] == "" || a.L(labelStuck) != "" {
		return nil
	}
	ref := a.LastActivityAt
	if since := parseTime(a.L(labelSince)); since.After(ref) {
		ref = since
	}
	if now.Sub(ref) < stuckAfter {
		return nil
	}
	switch stage {
	case "building":
		if a.L(labelAutoReview) != "" {
			return nil // starts once the agent acts
		}
	case "plan-review", "review":
		agents := f.agents()
		if reviewPending(agents, a.ID, now) || unappliedStoppedReviewer(agents, a.ID) {
			return nil
		}
		// review recovery starts and decides reviews: it holds the card's gate, and a review that
		// started before a delivered follow-up is not recovered (the follow-up queued the next one)
		g := f.gate(a.ID)
		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.delivered.IsZero() && !parseTime(a.L(labelSince)).After(g.delivered) {
			f.logf("%s: review predates its last follow-up, not recovered", short(a.ID))
			return nil
		}
		if stage == "review" && f.panelReady(a) {
			return f.closePanel(a) // its last reviewer finished but the panel did not close (an unreadable report)
		}
		if live, err := f.stillStopped(a); !live {
			return err
		}
		return f.recoverReview(a, stage, agents, now)
	}
	if live, err := f.stillStopped(a); !live {
		return err
	}
	c, ok := f.card(a.ID)
	if !ok {
		return nil
	}
	return f.stepStuck(a, c, stage)
}

// rerunReviewLocked starts a fresh review of kind for card a, for a caller holding a's gate g.
func (f *Factory) rerunReviewLocked(g *cardGate, a Agent, kind string) error {
	c, ok := f.card(a.ID)
	if !ok {
		c = Card{ID: a.ID, Short: short(a.ID), Title: a.Title, Host: a.Host, Cwd: a.Cwd, Repo: a.Repo,
			WorkspaceID: a.WorkspaceID, Tier: a.L(labelTier)}
	}
	_, err := f.startReview(g, c, kind)
	return err
}

// unappliedStoppedReviewer reports whether card id has an idle or closed reviewer whose verdict
// has not been applied yet. ApplyVerdict picks those up (archived or not), so checkStuck leaves
// them to ApplyVerdict instead of starting a second reviewer in the same Tick.
func unappliedStoppedReviewer(agents []Agent, id string) bool {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	for _, r := range agents {
		if r.L(labelRole) == "reviewer" && sameCard(byID, r.L(labelReviewOf), id) && r.L(labelApplied) == "" &&
			(r.Status == "idle" || r.Status == "closed") {
			return true
		}
	}
	return false
}

// blockedReviewerOf is card id's first reviewer that is errored, stalled or waiting on a permission
// prompt (reviewerBlocked). A retired one is not: it was already replaced, and the reap archives it.
func blockedReviewerOf(agents []Agent, id string, now time.Time) (Agent, bool) {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	for _, r := range agents {
		if r.L(labelRole) == "reviewer" && sameCard(byID, r.L(labelReviewOf), id) && !retiredReviewer(r) && reviewerBlocked(agents, r, now) {
			return r, true
		}
	}
	return Agent{}, false
}

// reviewPending reports whether card id has a reviewer that will still produce a verdict: one that
// is running, or stopped with its verdict not yet applied (ApplyVerdict picks it up). A retired
// reviewer (retiredReviewer) is not, even while it runs. A reviewer
// archived without a verdict, or gone from the list (archived over Config.Window ago), is not, and
// neither is one stuck on a permission prompt or stalled (reviewerBlocked): its card goes to Needs
// Zach with a re-run, and a verdict it gives later is still applied.
func reviewPending(agents []Agent, id string, now time.Time) bool {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	for _, r := range agents {
		if r.L(labelRole) != "reviewer" || !sameCard(byID, r.L(labelReviewOf), id) || r.archived() || retiredReviewer(r) ||
			reviewerBlocked(agents, r, now) {
			continue
		}
		if r.Status == "running" || r.L(labelApplied) == "" {
			return true
		}
	}
	return false
}

// recordBase stores a new board card's worktree HEAD as factory.base, so landing checks can tell
// its own commits from the commit it started on. A failed read retries on the next Tick.
func (f *Factory) recordBase(a Agent) {
	if a.Cwd == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, a.Cwd, "rev-parse", "HEAD")
	if head := strings.TrimSpace(out); err == nil && head != "" {
		if err := f.setLabels(a.ID, map[string]string{labelBase: head}); err != nil {
			f.logf("%s: recording base: %v", short(a.ID), err)
		}
	}
}

// waitNote says why a Needs Zach card waits; Zach is emailed it once per note (mail.go).
func waitNote(a Agent) string {
	if n := a.L(labelNote); n != "" {
		return n
	}
	return "waiting on Zach"
}

// directReply reports whether card a's last reply ends with TRACK: direct and its recent session text has no
// TIER line (a plan).
func (f *Factory) directReply(a Agent) (bool, error) {
	reply, err := f.storedReply(a)
	if err != nil {
		return false, err
	}
	if ParseTrack(reply) != "direct" {
		return false, nil
	}
	text, err := storedText(f.cfg.Home, a, 60)
	return err == nil && ParseTier(text) == "", err
}

// markDirect labels card a as a direct-track card, so Zach's later replies keep it on that track
// (errandPass), then finishes it.
func (f *Factory) markDirect(a Agent, c Card) error {
	if err := f.setLabels(a.ID, map[string]string{labelTrack: "direct", labelAutoReview: ""}); err != nil {
		return err
	}
	return f.finishDirect(a, c)
}

// finishDirect closes a card the builder did in one pass (TRACK: direct): no plan or code review,
// but its work must be on origin/main like any other landing.
func (f *Factory) finishDirect(a Agent, c Card) error {
	if f.guarded(c.Repo) {
		// no direct track in a public repo: whatever it did waits for Zach, with a scan of its commits
		note := "public repo: finished without review"
		if base := a.L(labelBase); base != "" {
			if found, err := f.scanCard(c.Cwd, base+"..HEAD"); err != nil || found != "" {
				note += fmt.Sprintf("; scan: %s%v", found, err)
			}
		}
		f.logf("%s direct card in a public repo: %s", short(a.ID), note)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: note})
	}
	if ok, why := f.landedOnMain(c.Cwd, a.L(labelBase)); !ok {
		return f.returnDirect(a, why)
	}
	// the direct track pushes only its own work item; anything else needed the build track's reviews
	outside, err := f.ownFilesOutside(c.Cwd, "work/")
	switch {
	case err != nil:
		f.logf("%s direct card scope not checked: %v", short(a.ID), err)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: "direct card scope not verified: " + trunc(err.Error(), 150)})
	case len(outside) > 0:
		f.logf("%s direct card changed %v", short(a.ID), outside)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach",
			labelNote: "direct card changed files outside its work item without review: " + trunc(strings.Join(outside, ", "), 150)})
	}
	f.recordLanded(a, c.Cwd)
	f.logf("%s done in one pass", short(a.ID))
	return f.setLabels(a.ID, map[string]string{labelStage: "landed", labelNote: "done in one pass"})
}

// nameCard renames board card a once, from the TITLE line of its first plan (or direct result),
// reply being its last message. Titles given by the caller (factory.titled=given) and inbox cards keep theirs; a reply
// that is neither a plan nor a direct result (a question) leaves the card for its next reply. A
// failed rename is logged and does not hold up the card.
func (f *Factory) nameCard(a Agent, reply string) {
	if a.L(labelSource) != "board" || a.L(labelTitled) != "" {
		return
	}
	if ParseTrack(reply) != "direct" {
		// storedReply strips the TIER line: read it from the session text
		if text, err := storedText(f.cfg.Home, a, 60); err != nil || ParseTier(text) == "" {
			return
		}
	}
	title := ParseTitle(reply)
	if title == "" || title == a.Title {
		if err := f.setLabels(a.ID, map[string]string{labelTitled: "kept"}); err != nil {
			f.logf("%s title not marked kept: %v", short(a.ID), err)
		}
		return
	}
	if _, err := f.paseo("agent", "update", a.ID, "--name", title, "--label", labelArg(labelTitled, "planner")); err != nil {
		f.logf("%s rename failed: %v", short(a.ID), err)
		return
	}
	f.renameBranch(a, title)
	f.logf("%s renamed: %s → %s", short(a.ID), a.Title, title)
}

func (f *Factory) renameBranch(a Agent, title string) {
	want := branchSlug(title)
	if a.Cwd == "" || want == a.Branch {
		return
	}
	branch := f.freeBranch(a.Cwd, want)
	if branch == "" || branch == a.Branch {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Git(ctx, a.Cwd, "branch", "-m", branch); err != nil {
		f.logf("%s branch rename failed: %v", short(a.ID), err)
	}
}

// freeBranch picks a local branch name in dir starting from slug (slug, slug-2, ..., slug-50) that
// does not already exist in refs/heads/, so Paseo branches off main instead of reusing an old ref.
// It returns "" if all candidates are taken.
func (f *Factory) freeBranch(dir, slug string) string {
	if dir == "" {
		return slug
	}
	if slug == "" {
		slug = "card"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 1; i <= 50; i++ {
		cand := slug
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", slug, i)
		}
		if _, err := f.GitOut(ctx, dir, "rev-parse", "--verify", "refs/heads/"+cand); err != nil {
			return cand
		}
	}
	return ""
}

// branchSlug turns a card title into a git branch slug of lowercase ASCII letters and digits
// joined by single hyphens, cut at a word boundary to at most 50 characters.
func branchSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		default:
			dash = true
		}
	}
	slug := b.String()
	if len(slug) > 50 {
		slug = slug[:50]
		if i := strings.LastIndexByte(slug, '-'); i >= 25 {
			slug = slug[:i]
		}
		slug = strings.TrimRight(slug, "-")
	}
	if slug == "" {
		return "card"
	}
	return slug
}

// stillStopped re-checks with Paseo, just before an automatic move, that agent a is still idle and
// not archived. The snapshot a came from can be a poll old: a card Zach dropped since must not get a
// reviewer or a landing move. A failed check skips the move; the next Tick tries again.
func (f *Factory) stillStopped(a Agent) (bool, error) {
	archived, running, err := f.liveState(a.ID)
	if err != nil {
		return false, err
	}
	if archived { // parked too: the factory never wakes a parked card on its own
		f.logf("%s archived since the last poll, no move", short(a.ID))
	}
	return !archived && !running, nil
}

// landedOnMain reports whether the checkout at cwd has its HEAD on origin/main, and why not.
// A failed fetch still checks the local origin/main: a landing push updates it, and a
// stale ref only fails safe. With base (the card's starting HEAD), HEAD must also have moved: a
// card that never committed is not landed. Not caught: cards from before factory.base, a worktree
// fast-forwarded without a commit of its own, and uncommitted changes left in the worktree.
func (f *Factory) landedOnMain(cwd, base string) (bool, string) {
	if cwd == "" {
		return false, "no cwd"
	}
	if _, err := os.Stat(cwd); err != nil {
		return false, "cwd gone: " + cwd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if base != "" {
		head, err := f.GitOut(ctx, cwd, "rev-parse", "HEAD")
		if err != nil {
			return false, "reading HEAD: " + err.Error()
		}
		if strings.TrimSpace(head) == base {
			return false, "no new commit since the card started"
		}
	}
	if err := f.Git(ctx, cwd, "fetch", "-q", "origin", "main"); err != nil {
		f.logf("fetch in %s failed, checking local origin/main: %v", cwd, err)
	}
	if err := f.Git(ctx, cwd, "merge-base", "--is-ancestor", "HEAD", "origin/main"); err != nil {
		return false, "HEAD not on origin/main: " + err.Error()
	}
	return true, ""
}

type landCheck int

const (
	landVerified landCheck = iota
	landNotOnMain
	landDiffers
)

// checkLanding verifies that card a's worktree at cwd has its commits on origin/main and, when a
// reviewed commit is recorded, that what landed matches what was reviewed.
func (f *Factory) checkLanding(a Agent, cwd string) (landCheck, string) {
	if ok, why := f.landedOnMain(cwd, a.L(labelBase)); !ok {
		return landNotOnMain, why
	}
	if reviewed := strings.TrimSpace(a.L(labelReviewed)); reviewed != "" {
		if ok, why := f.landedAsReviewed(cwd, reviewed); !ok {
			return landDiffers, why
		}
	}
	return landVerified, ""
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// truncWords cuts s to at most n runes at a word boundary and marks the cut with "…".
func truncWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n-1])
	if i := strings.LastIndexAny(cut, " \t\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:.-") + "…"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
