package factory

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Tiers decide how much review a card gets. The plan reviewer sets a card's tier (the higher of
// its call, the builder's and any hint from the idea); a code reviewer can raise it, never lower it.
// "routine" is not a review tier: it marks scheduled runs, which go through no gates. Errands that
// change no code skip the tiers too: the builder does them in one pass and says TRACK: direct.
var Tiers = []string{"low", "risky", "architecture"}

var TierHelp = map[string]string{
	"routine":      "Scheduled run of an approved job. No per-run review; it surfaces only if it gets stuck.",
	"low":          "Plan review, then code review by another agent, then lands on its own.",
	"risky":        "Plan review and code review must both pass, then lands on its own.",
	"architecture": "Zach approves the plan before any code; code review, then lands on its own.",
}

const tierRules = `Tiers (pick the highest that applies):
- architecture: new service, datastore, framework or dependency; a change to how components fit
  together or to a shared interface; any refactor or cleanup (these must cover the whole app, see
  below); anything hard to undo or publicly visible. Zach approves the plan before code is written.
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
Ideas. If you find work worth doing outside this card's scope (a bug, a gap, a follow-up), do not do it
here: suggest it as an idea, at most 2 per card, only with a concrete payoff. %sIt never replaces
finishing this card. First check it is not already an idea or a card in flight:
`+"`curl -s localhost:%d/api/ideas`"+` and the peers brief. Then:
curl -s -X POST localhost:%d/api/idea -H 'X-Factory: 1' -H 'Content-Type: application/json' \
  -d '{"repo":"<owner/repo>","by":"%s","tier":"<low|risky|architecture>","title":"<outcome, at most 60 characters>","summary":"<1-2 plain sentences>","why":"<evidence>","doneWhen":"<check>","request":"<technical brief>"}'
`, note, port, port, by)
}

const wholeAppRule = `Refactors and cleanups cover the whole app: find every instance of the pattern across the repo
(grep for it) and change all of them, or list each one left alone and why. A partial refactor that
leaves two ways of doing the same thing is a blocking finding. If that is too big for one card,
the plan splits it into ordered cards that together cover everything.`

var (
	verdictRe  = regexp.MustCompile(`(?i)VERDICT:\s*\**\s*(approve|nits|changes|escalate|close)`)
	tierRe     = regexp.MustCompile(`(?i)TIER:\s*\**\s*(low|risky|architecture)`)
	trackRe    = regexp.MustCompile(`(?i)TRACK:\s*\**\s*(direct)`)
	titleRe    = regexp.MustCompile(`(?im)^[ \t>*_#-]*TITLE[*_]*\s*:[ \t]*(.*)$`)
	smallFixRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*SMALL[ \t]*FIX[*_]*\s*:[ \t]*(.*)$`)
)

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
- Scope: nothing unasked-for. Over about 1,000 changed lines means it must be split.
- Cheaper or safer approach available?
- Refactor or cleanup: does it cover the whole app (rule below)?
- Tier: is the builder's TIER right? Set your own.`,
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
- Scope: no unrelated files. No secrets, tokens or private data committed.
- <visibility> If the repo is public or unknown, any private data in the diff is a blocking finding:
  names or emails of people (other than Zach's public identity), Zach's personal details, internal
  hostnames or tailnet URLs (*.ts.net), local paths (/home/<user>/...), and content from <errands> or PDW.
- Repo fit: follows that repo's AGENTS.md/README conventions.
- Refactor or cleanup: every instance across the repo changed (grep for the old pattern)?
- Tier: the card is "<tier>". Does the change touch anything above that? Say so with TIER below.`,
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
  Use a scratch copy (/tmp) if you need to try something.
- Blocking findings only: each one concrete (file:line or exact claim), with why it matters.
  Style preferences and nits are not blocking.
- Allowed verdicts: approve, nits (approve; list nits, no new work), changes (blocking findings),
  escalate (a decision only Zach can make; ask it as a QUESTION: block, below), close (duplicate, already done, or not needed).
  Architecture-tier work goes to Zach anyway; do not escalate just for that.
- Keep it short: at most 8 bullets.
- `+shellRule+`

%s

`+overviewRule+`

End your final message with exactly two lines:
TIER: <low|risky|architecture>
VERDICT: <approve|nits|changes|escalate|close>
`, persona[kind], c.Title, c.ID, c.Host, cwd, c.Repo, branch, c.ID, requestSection(c.Request), r, tierRules, wholeAppRule, questionRule)
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

// decidedRule is added to reviews of a card where Zach accepted the builder's recommendations.
const decidedRule = `- Zach has already ruled on open points in this card. His choices are in the builder's conversation ("Zach
  accepts the recommendations in your brief", or a message from Zach). Treat them as settled: do not raise them
  again as blocking. Flag only new problems, or a change that does not do what he chose.`

const planSmallFixRule = `- Small blocking fixes: when your verdict is changes and every blocking finding is a small, concrete fix the
  builder can apply while building and a code reviewer can check (such as a missing item in a list, an Ordering:
  or Waits on: line, or a missed call site or test — not a design disagreement, wrong diagnosis or approach, an
  unsplit >1,000-line change, or a large blocker), add one line right before TIER:
  SMALL FIX: <one-line summary of the fixes the builder must apply>
  At the plan-review rework cap, a plan with that line goes to build with those fixes applied and verified by
  code review; without it, the card stops in Needs Zach.`

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
direct track, do NOT write a plan: do the whole thing now, in one pass, as well as you can. Check
every fact at its source, including anything that changed in the last day (a rebooked flight, a
moved meeting: look at <errands> work items on origin/main from today). Deliver the result, record it in
a <errands> work item, land it (commit, then land it the way that repo's AGENTS.md says), and end your final message with the line
TRACK: direct
The direct track commits only its own <errands> work item (work/...): no other files, no other repo.
Anything else (code, config, infra, deletes, money, messages to anyone but Zach) is the build track:

Step 1 now: write a PLAN only, then stop. No file edits yet. The plan has: goal (Zach's words),
what you found (evidence), approach, files to change, deploy steps (if any; they run in the landing
step after code review, not during build), how you'll test it, risks, open questions,
and an estimate of changed lines (over 1,000 means split into smaller cards).
End the plan with one line: TIER: <low|risky|architecture>.

%s

%s

` + shellRule + `

` + overviewRule + `

A different agent reviews the plan (architecture-tier plans also go to Zach). When you get
"Plan approved", build it. Don't improvise: if scope must change, stop and say so. Commit on your
branch, but do NOT land or make live changes (install scripts, enable or restart services, resize VMs,
change external settings) until the factory tells you to land after code review. If stuck, stop and write a handoff: what
you tried, what you know, what you need. You run unattended: never use AskUserQuestion or plan mode.
Put the plan's open questions, and anything you need from Zach, in this form:
%s
`

const landMsg = "Land it now the way this repo lands work: read its AGENTS.md and README (and how recent commits reached " +
	"main) and do that. Land exactly the reviewed commits: no new commits or amends. If the rebase conflicts or anything must change, stop and say so " +
	"instead of landing. Then run any deploy steps from your reviewed summary and verify the landing and deploy (if a deploy step fails after the push, stop and report it; do not improvise a fix on the live system), then stop with a one-line summary. " + overviewReminder + " The agent factory deploys itself " +
	"from its repo's main within a minute of landing (and rolls back if it does not come up): never run its install.sh " +
	"to deploy."

// summaryMsg asks the builder of an architecture plan for the version Zach reads on his phone.
const summaryMsg = "Your plan passed review, and because it is an architecture change it now waits for Zach's " +
	"approval. Write a short version for Zach, in plain words for reading on a phone. Start with three lines:\n" +
	"DECISION: <approve this plan?, in one line>\n" +
	"OVERVIEW: <what changes and why, at most 3 short sentences>\n" +
	"NEEDS FROM ZACH: <approve the plan, or answer the questions below, and what happens after>\n" +
	"Then at most 3 bullets: what could go wrong and how you will check it. Then any question he must answer, " +
	"each as a QUESTION: block (the question, then 2 to 6 options as - lines, recommended first). Do not " +
	"start building; stop after the summary."

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
		"re-reviewed. %s Do not land. %s\n\n%s", kind, round, limit, restate(kind), overviewReminder, noLiveChangesRule, notes)
}

func buildMsg(prefix, notes string) string {
	return prefix + " Build it now, as planned. Don't improvise: if the scope has to change, stop and say so " +
		"instead. " + noLiveChangesRule + " When done, commit everything on your branch and leave the worktree clean (delete or gitignore " +
		"scratch files), but do NOT land; stop with a short summary (including the deploy steps as built, if any). " + overviewReminder + " A code reviewer checks it next, and the factory lands it once the review passes.\n\n" + shellRule + "\n\nReviewer notes:\n" + notes
}

func buildWithFixesMsg(notes string) string {
	return buildMsg("The last plan review found only small, concrete blocking fixes (in Reviewer notes below). "+
		"Apply these fixes, then build it now, as planned; do not stop for another plan review, and say in your summary how you applied them.", notes)
}

func summaryWithFixesMsg(notes string) string {
	return "The last plan review found only small, concrete blocking fixes (below). Fold those fixes into your plan, and " +
		"because it is an architecture change it now waits for Zach's approval. Write a short version for Zach (with the " +
		"fixes folded in), in plain words for reading on a phone. Start with three lines:\n" +
		"DECISION: <approve this plan?, in one line>\n" +
		"OVERVIEW: <what changes and why, at most 3 short sentences>\n" +
		"NEEDS FROM ZACH: <approve the plan, or answer the questions below, and what happens after>\n" +
		"Then at most 3 bullets: what could go wrong and how you will check it. Then any question he must answer, " +
		"each as a QUESTION: block (the question, then 2 to 6 options as - lines, recommended first). Do not " +
		"start building; stop after the summary.\n\nReviewer notes:\n" + notes
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

// reviewNotes is the reviewer's whole final message, for the builder and the brief. text (the last few log
// lines, where the verdict is) is the fallback when no full reply can be read.
func (f *Factory) reviewNotes(rev Agent, text string) string {
	if full, err := f.lastReply(rev); err == nil && strings.TrimSpace(full) != "" {
		return trunc(full, 8000)
	}
	return findings(text)
}

// briefMsg asks the builder of a card that reached the rework cap (or was escalated) for a brief Zach can act
// on: he has not followed the review rounds. It carries the reviewer's whole final review, which the builder
// has not seen, and names each button of ask by its exact label.
func briefMsg(kind string, escalated bool, notes string, ask *Ask) string {
	var b strings.Builder
	if escalated {
		fmt.Fprintf(&b, "The %s reviewer (a different agent) escalated a decision only Zach can make, so this card now "+
			"waits on Zach.", kind)
	} else {
		fmt.Fprintf(&b, "Review of your %s did not settle after the last round, so this card now waits on Zach. You "+
			"have not seen this final review yet; read it before you write.", kind)
	}
	b.WriteString(" Zach has not followed the review rounds and may not remember what this card is.\n\n" +
		"The reviewer's final review, in full:\n<<<\n" + notes + "\n>>> End of review\n\n" +
		"Write a brief for Zach, in plain words he can read on a phone (no file paths, no jargon). Start it with " +
		"these lines, which his board shows on top of the card:\n" +
		"DECISION: <the decision he must make, one line>\n" +
		"OVERVIEW: <what this card is for (his request) and where it stands (planned or built; nothing has " +
		"landed), at most 3 short sentences>\n" +
		"NEEDS FROM ZACH: <what you need from him>, then one line for each button on his board, saying what it " +
		"does, by these exact labels:\n")
	for _, x := range ask.Actions {
		fmt.Fprintf(&b, "- \"%s\": %s\n", x.Label, buttonMeaning(x, ask))
	}
	b.WriteString("Then, after a blank line:\n" +
		"1. Each point from that review that is still open: the reviewer's view, stated fairly even where you " +
		"disagree, and your view.\n")
	n := 2
	if escalated {
		b.WriteString("2. The reviewer's question for Zach, quoted word for word from the review above, then the " +
			"options and the one you recommend, as a QUESTION: block (below).\n")
		n++
	}
	fmt.Fprintf(&b, "%d. Each decision Zach must make, with the options and the one you recommend, each as a "+
		"QUESTION: block (below).\n", n)
	b.WriteString("\nThe board shows each QUESTION: block as buttons he taps; his answers come to you as a " +
		"message that quotes each question. " + questionRule + "\n\nThen stop. Do not build, edit files or land.")
	return b.String()
}

// buttonMeaning says what one brief-card button does, for the brief.
func buttonMeaning(x Action, ask *Ask) string {
	switch x.ID {
	case "recommend":
		return "you do what this brief recommends (" + recommendDoes(ask) + ")"
	case "approve":
		if x.Label == btnBuildAsIs {
			return "you build the plan as it stands; the open points are ignored"
		}
		return "the code lands as it stands; the open points are ignored"
	case "reply":
		return "his own instructions or answer come to you as a message"
	case "close":
		return "the card is dropped: archived, nothing lands"
	}
	return x.Label
}

func recommendDoes(ask *Ask) string {
	for _, x := range ask.Actions {
		if x.Label == btnBuildAsIs {
			return "revise the plan to match, then build; a code review still runs before it lands"
		}
	}
	return "apply it, then a code review runs before it lands"
}

// ApplyVerdict moves a card after its reviewer finished. Runs once per reviewer (factory.applied).
func (f *Factory) ApplyVerdict(rev Agent) error {
	target, kind := rev.L(labelReviewOf), rev.L(labelReviewKind)
	if kind == "" {
		kind = "code"
	}
	text, err := f.lastText(rev.ID, 6)
	if err != nil {
		return err
	}
	verdict := ParseVerdict(text)
	notes := f.reviewNotes(rev, text)
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
		f.paseo("archive", rev.ID)
		return nil
	}
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
			f.paseo("archive", rev.ID)
			f.logf("reviewer %s: target %s archived, verdict not sent", short(rev.ID), short(target))
			return nil
		}
	}
	if err := f.setLabels(rev.ID, map[string]string{labelApplied: "1", labelVerdict: verdict}); err != nil {
		return err
	}
	defer f.paseo("archive", rev.ID)
	if !ok {
		f.logf("reviewer %s: target %s not found", short(rev.ID), short(target))
		return nil
	}
	if verdict == "" && t.L(labelNoVerdict) == "" {
		// first no-verdict in this round: re-run once on its own before asking Zach
		if err := f.setLabels(target, map[string]string{labelNoVerdict: "1"}); err != nil {
			return err
		}
		f.logf("%s review for %s ended without verdict; re-running once", kind, short(target))
		return f.rerunReview(t, kind)
	}
	roundsKey := roundsLabel(kind)
	rounds := atoi(t.L(roundsKey)) + 1
	builderTier := ""
	if kind == "plan" {
		bt, _ := f.lastText(target, 60)
		builderTier = ParseTier(bt)
	}
	tier := MaxTier(t.L(labelTier), ParseTier(text), builderTier)
	if tier == "" && kind == "plan" {
		tier = "low"
	}
	zachOK := t.L(labelZachPlan) == "approved"
	labels := map[string]string{roundsKey: fmt.Sprint(rounds)}
	if t.L(labelNoVerdict) != "" {
		labels[labelNoVerdict] = ""
	}
	if tier != "" {
		labels[labelTier] = tier
	}
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(target, labels)
	}
	now := iso(f.Now())
	smallFix := ""
	if kind == "plan" {
		labels[labelPlanFix] = ""
		if verdict == "changes" && rounds >= f.cfg.ReworkCap {
			smallFix = ParseSmallFix(notes)
		}
	}
	if (verdict == "approve" || verdict == "nits" || smallFix != "") && kind == "plan" {
		// the approved plan may name a card this one lands after (deps.go)
		if plan, err := f.lastReply(t); err != nil {
			f.logf("%s: plan not read for a Waits on line: %v", short(target), err)
		} else if id := ParseWaitsOn(plan, t.ID); id != t.L(labelWaitsOn) {
			labels[labelWaitsOn], labels[labelWaitsDone] = id, ""
		}
	}
	switch {
	case (verdict == "approve" || verdict == "nits") && kind == "plan":
		if tier == "architecture" && !zachOK {
			// the factory emails Zach the summary once the builder has written it (mailZach)
			note := "architecture: approve the plan to build"
			if err = set(labelStage, "zach", labelVerdict, "plan "+verdict, labelAwaiting, "plan", labelNote, note,
				labelWriting, now); err == nil {
				err = f.send(target, summaryMsg)
			}
		} else {
			if err = set(labelStage, "building", labelVerdict, "plan "+verdict, labelSince, now, labelAutoReview, "code"); err == nil {
				err = f.send(target, buildMsg("Plan approved by the plan reviewer.", notes))
			}
		}
	case verdict == "approve" || verdict == "nits":
		fb := uiFeedback(notes)
		scanned := ""
		if f.guarded(t.Repo) {
			scanned = f.scanNote(t.Cwd, "") // public repo: the factory's own scan, before any landing
		}
		switch {
		case scanned != "" && !scanPassed(scanned):
			// a scan hit or error stops the card; a clean public-repo card lands like any other
			err = set(labelStage, "zach", labelVerdict, "code "+verdict, labelNote, scanned)
		case tier == "architecture" && !zachOK:
			err = set(labelStage, "zach", labelVerdict, "code "+verdict, labelNote, "review found an architecture change: approve to land")
		case t.L(labelPlanRounds) == "":
			// a session that never had a plan review (sent to review by hand): Zach lands it. Not the
			// tier label: every verdict records one, so a reworked hand-sent card has it by round 2.
			// plan-rounds shows a plan review ran, not that it passed; a plan escalated to Zach that he
			// then moves on himself can still auto-land.
			err = set(labelStage, "zach", labelVerdict, "code "+verdict, labelNote, "review passed: approve to land")
		default:
			note := "auto-landing (" + tier + ")"
			if scanned != "" {
				note += ": public repo: scan passed at review"
			}
			if id, _ := f.pendingDep(t); id != "" {
				// the land prompt waits for that card; the next Tick sends it or asks Zach (tickHeld)
				if err = set(labelStage, "landing", labelVerdict, "code "+verdict, labelSince, now, labelHeld, id,
					labelNote, holdNote(id)); err == nil && fb != "" {
					err = f.send(target, depHoldMsg(id, fb))
				}
				break
			}
			if err = set(labelStage, "landing", labelVerdict, "code "+verdict, labelSince, now, labelNote, note); err == nil {
				msg := fmt.Sprintf("Code review passed (%s tier). %s", tier, f.landMsgFor(t))
				if fb != "" {
					msg += uiFeedbackLandMsg(fb)
				}
				err = f.send(target, msg)
			}
		}
		if err == nil && fb != "" && labels[labelStage] == "zach" {
			err = f.send(target, uiFeedbackHoldMsg(fb))
		}
	case verdict == "changes" && rounds < f.cfg.ReworkCap:
		back := "building"
		if kind == "plan" {
			back = "plan"
		}
		if err = set(labelStage, back, labelVerdict, kind+" changes", labelSince, now, labelAutoReview, kind); err == nil {
			err = f.send(target, reworkMsg(kind, rounds, f.cfg.ReworkCap, notes))
		}
	case verdict == "changes" && kind == "plan" && smallFix != "":
		if tier == "architecture" && !zachOK {
			note := "architecture: approve the plan to build"
			if err = set(labelStage, "zach", labelVerdict, "plan changes", labelAwaiting, "plan", labelNote, note,
				labelWriting, now, labelPlanFix, smallFix); err == nil {
				err = f.send(target, summaryWithFixesMsg(notes))
			}
		} else {
			if err = set(labelStage, "building", labelVerdict, "plan fixes", labelSince, now, labelAutoReview, "code",
				labelNote, "building with last-round plan fixes", labelPlanFix, smallFix); err == nil {
				err = f.send(target, buildWithFixesMsg(notes))
			}
		}
	case verdict == "close":
		err = set(labelStage, "zach", labelVerdict, "close", labelNote, "reviewer suggests closing")
	case verdict == "":
		// no question to put to Zach: offer a fresh review (rerun reads stuck-from)
		from := "review"
		if kind == "plan" {
			from = "plan-review"
		}
		err = set(labelStage, "zach", labelVerdict, kind+" none", labelNote, noVerdictNote, labelStuckFrom, from)
	default:
		// rework cap or escalate: Zach hasn't followed the rounds, so the builder writes him a brief first
		why := "reviewer escalated"
		if verdict == "changes" {
			why = "rework cap reached"
		}
		labels[labelVerdict], labels[labelNote] = kind+" "+verdict, why
		next := t
		next.Labels = map[string]string{}
		for k, v := range t.Labels {
			next.Labels[k] = v
		}
		for k, v := range labels {
			next.Labels[labelPrefix+k] = v
		}
		next.Labels[labelPrefix+labelStage] = "zach"
		next.Status = "idle"
		ask := briefAsk(next)
		msg := briefMsg(kind, verdict == "escalate", notes, ask)
		if err = set(labelStage, "zach", labelWriting, now); err == nil {
			err = f.send(target, msg)
		}
	}
	f.recordRound(target, kind, rounds, rev.CreatedAt, verdict)
	f.logf("%s verdict %s for %s (round %d, tier %s)", kind, verdict, short(target), rounds, tier)
	return err
}

// StartReview starts a fresh reviewer agent for card c in the card's own workspace. A code review
// needs a clean worktree, so that what it reads is what lands: a dirty one gets the builder a message
// to commit first, and the review stays queued until it stops again.
func (f *Factory) StartReview(c Card, kind string) (string, error) {
	labels := map[string]string{labelAutoReview: "", labelSince: iso(f.Now())}
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
	rid, err := f.runReviewer(c, kind, f.visibility(c.Cwd), "Review ("+kind+"): "+trunc(c.Title, 60), "", "")
	if err != nil {
		return "", err
	}
	labels[labelStage] = "review"
	if kind == "plan" {
		labels[labelStage] = "plan-review"
	}
	if err := f.setLabels(c.ID, labels); err != nil {
		return rid, err
	}
	f.logf("started %s review %s for %s (%s)", kind, short(rid), c.Short, trunc(c.Title, 40))
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
			labelNote: "worktree not clean for review", labelNoVerdict: ""})
	}
	if err := f.setLabels(c.ID, map[string]string{labelStage: "building", labelAutoReview: "code",
		labelSince: iso(f.Now()), labelDirtyAsks: fmt.Sprint(asks), labelNoVerdict: ""}); err != nil {
		return err
	}
	f.logf("%s: worktree not clean, asked the builder to commit before review", c.Short)
	return f.send(c.ID, "Before code review the worktree must be clean, so the reviewer sees exactly what lands. "+
		"Commit everything that belongs to this work on your branch (do NOT land), delete or gitignore scratch "+
		"files, then stop with a short summary (including the deploy steps as built, if any). "+noLiveChangesRule+" "+overviewReminder+" The review starts when you stop.\n\ngit status --porcelain:\n"+trunc(dirty, 1500))
}

// runReviewer starts a read-only reviewer of the given kind for c. For a landing review, why is the
// agent's note and head the commit under review.
func (f *Factory) runReviewer(c Card, kind, vis, title, why, head string) (string, error) {
	// reviewerRunArgs: reviewers, landing reviewers too, stay on the default agent (Opus 5.5)
	args := append(reviewerRunArgs(),
		"--title", title,
		"--label", labelArg(labelRole, "reviewer"), "--label", labelArg(labelReviewOf, c.ID),
		"--label", labelArg(labelReviewKind, kind), "--background", "--quiet")
	if c.WorkspaceID != "" {
		args = append(args, "--workspace", c.WorkspaceID)
	}
	if c.Cwd != "" {
		args = append(args, "--cwd", c.Cwd)
	}
	if a, ok := f.agentOK(c.ID); ok && c.Request == nil {
		r := f.cardRequest(a)
		c.Request = &r
	}
	prompt := strings.ReplaceAll(ReviewPrompt(c, kind, f.cfg.Port, vis), "<errands>", f.errandsName())
	if f.guarded(c.Repo) {
		prompt += publicReviewRule
	}
	if kind != "plan" && f.uiChange(c) {
		prompt += uiReviewRule
	}
	prompt += f.delegateRule(true)
	if kind == "land" {
		if why == "" {
			why = "(none given)"
		}
		prompt = fmt.Sprintf("This is a LANDING REVIEW: agent %s asks before it lands commit %s on main. What lands "+
			"is `git -C %s log origin/main..%s`. Its note: %s\n\n%s", c.ID, head, c.Cwd, head, why, prompt)
	}
	out, err := f.paseo(append(args, prompt)...)
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

// Tick runs the factory's automatic moves (Sinai's label-in / label-out stages).
func (f *Factory) Tick() {
	now := f.Now()
	for _, a := range f.agents() {
		if f.stopping.Load() {
			return
		}
		if err := f.tickAgent(a, now); err != nil {
			f.logf("rule error on %s: %v", short(a.ID), err)
		}
	}
	if f.stopping.Load() {
		return
	}
	f.tickSteers()
	if err := f.tickIdeas(); err != nil {
		f.logf("idea error: %v", err)
	}
	f.reap(now)
	f.checkReplies(now)
	f.probeMail()
}

func (f *Factory) tickAgent(a Agent, now time.Time) error {
	old := now.Sub(a.CreatedAt) > time.Minute
	switch {
	case a.L(labelRole) == "reviewer":
		if a.L(labelApplied) == "" && (a.Status == "idle" || a.Status == "closed") && old {
			return f.ApplyVerdict(a)
		}
		return nil
	case a.L(labelRole) == "scout":
		if a.stopped() && old {
			f.logf("scout %s finished", short(a.ID))
			_, err := f.paseo("archive", a.ID)
			return err
		}
		return nil
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
	if !a.stopped() {
		return nil
	}
	// "acted": the agent did something after the last move and is idle again. The 45 s grace covers
	// the gap between `paseo send` and the agent flipping to running.
	since := parseTime(a.L(labelSince))
	acted := !since.IsZero() && a.LastActivityAt.After(since) && now.Sub(since) > 45*time.Second
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
	case a.L(labelAutoReview) != "" && (acted || since.IsZero()):
		if live, err := f.stillStopped(a); !live {
			return err
		}
		if a.L(labelAutoReview) == "code" && a.L(labelPlanRounds) == "" && !f.guarded(c.Repo) && f.directReply(a) {
			// a direct card sent to code review (say, by a reply before errandPass): its work is
			// already on main, so finish it as direct instead of asking Zach to approve landing
			return f.markDirect(a, c)
		}
		_, err := f.StartReview(c, a.L(labelAutoReview))
		return err
	case a.L(labelStage) == "plan" && (a.L(labelSource) == "board" || fromInbox(a)) && acted:
		if live, err := f.stillStopped(a); !live {
			return err
		}
		// only the agent's own last reply: the Fellow prompt itself has a "TRACK: direct" line
		text, err := f.lastReply(a)
		if err == nil {
			f.nameCard(a, text)
		}
		if err == nil && ParseTrack(text) == "direct" {
			return f.markDirect(a, c)
		}
		if fromInbox(a) {
			// an inbox card that wrote no plan (no TIER line) did an errand, as email agents did before;
			// its last reply may be a line after its final command, so read the recent log
			if text, err := f.lastText(a.ID, 60); err == nil && ParseTier(text) == "" {
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
		if ok, why := f.landedOnMain(c.Cwd, a.L(labelBase)); !ok {
			// stays unarchived so Zach can read what the agent did
			f.logf("%s landing not verified: %s", short(a.ID), why)
			return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: "landing not verified"})
		}
		if reviewed := a.L(labelReviewed); reviewed != "" {
			if ok, why := f.landedAsReviewed(c.Cwd, reviewed); !ok {
				f.logf("%s landed diff differs from the reviewed one: %s", short(a.ID), why)
				return f.setLabels(a.ID, map[string]string{labelStage: "zach",
					labelNote: "landed diff differs from the reviewed one: " + trunc(why, 200)})
			}
		}
		f.recordLanded(a, c.Cwd)
		// landed: Done only when the agent says nothing is left (remaining.go)
		reply, err := f.lastReply(a)
		if err != nil {
			f.logf("%s landing reply not read: %v", short(a.ID), err)
		}
		if note := remainingNote(reply); note != "" {
			f.logf("%s landed with work left: %s", short(a.ID), note)
			return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: note})
		}
		f.logf("%s finished landing", short(a.ID))
		return f.setLabels(a.ID, map[string]string{labelStage: "landed", labelNote: "landed"})
	case a.L(labelStage) == "zach" && f.Mail != nil && a.L(labelTold) != waitNote(a) && mailable(c.Ask):
		if live, err := f.stillStopped(a); !live {
			return err
		}
		return f.mailZach(a, c)
	}
	return f.checkStuck(a, now)
}

const stuckAfter = 2 * time.Minute

// stuckWhy names what a card in a working column is missing, or "" when its next move is pending.
var stuckWhy = map[string]string{
	"plan":        "Planner stopped, nothing queued",
	"building":    "Builder stopped, nothing queued",
	"plan-review": "Reviewer gone, no verdict",
	"review":      "Reviewer gone, no verdict",
	"landing":     "Landing agent stopped without landing",
}

// checkStuck moves a stopped factory card to Needs Zach when no automatic move is coming for it:
// nothing should sit waiting. It runs after the other rules,
// so a card reaching it had no move this Tick. Needs Zach and Done are never stuck.
func (f *Factory) checkStuck(a Agent, now time.Time) error {
	stage := a.L(labelStage)
	why := stuckWhy[stage]
	if why == "" {
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
		if !hasBlockedReviewer(agents, a.ID, now) && a.L(labelNoVerdict) == "" {
			if live, err := f.stillStopped(a); !live {
				return err
			}
			for _, r := range agents {
				if r.L(labelRole) == "reviewer" && r.L(labelReviewOf) == a.ID && r.archived() && r.L(labelApplied) == "" {
					if err := f.setLabels(r.ID, map[string]string{labelApplied: "1", labelVerdict: ""}); err != nil {
						return err
					}
				}
			}
			if err := f.setLabels(a.ID, map[string]string{labelNoVerdict: "1"}); err != nil {
				return err
			}
			kind := "code"
			if stage == "plan-review" {
				kind = "plan"
			}
			f.logf("%s %s reviewer gone without verdict; re-running once", short(a.ID), kind)
			return f.rerunReview(a, kind)
		}
	}
	if live, err := f.stillStopped(a); !live {
		return err
	}
	f.logf("%s stuck in %s: %s", short(a.ID), stage, why)
	labels := map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: stage,
		labelAutoReview: ""}
	if a.L(labelNoVerdict) != "" {
		labels[labelNoVerdict] = ""
	}
	return f.setLabels(a.ID, labels)
}

func (f *Factory) rerunReview(a Agent, kind string) error {
	c, ok := f.card(a.ID)
	if !ok {
		c = Card{ID: a.ID, Short: short(a.ID), Title: a.Title, Host: a.Host, Cwd: a.Cwd, Repo: a.Repo,
			WorkspaceID: a.WorkspaceID, Tier: a.L(labelTier)}
	}
	_, err := f.StartReview(c, kind)
	return err
}

// unappliedStoppedReviewer reports whether card id has an idle or closed reviewer whose verdict
// has not been applied yet. ApplyVerdict picks those up (archived or not), so checkStuck leaves
// them to ApplyVerdict instead of starting a second reviewer in the same Tick.
func unappliedStoppedReviewer(agents []Agent, id string) bool {
	for _, r := range agents {
		if r.L(labelRole) == "reviewer" && r.L(labelReviewOf) == id && r.L(labelApplied) == "" &&
			(r.Status == "idle" || r.Status == "closed") {
			return true
		}
	}
	return false
}

// hasBlockedReviewer reports whether card id has a live running reviewer that is stalled or
// waiting on a permission prompt (reviewerBlocked).
func hasBlockedReviewer(agents []Agent, id string, now time.Time) bool {
	for _, r := range agents {
		if r.L(labelRole) == "reviewer" && r.L(labelReviewOf) == id && reviewerBlocked(agents, r, now) {
			return true
		}
	}
	return false
}

// reviewPending reports whether card id has a reviewer that will still produce a verdict: one that
// is running, or stopped with its verdict not yet applied (ApplyVerdict picks it up). A reviewer
// archived without a verdict, or gone from the list (archived over Config.Window ago), is not, and
// neither is one stuck on a permission prompt or stalled (reviewerBlocked): its card goes to Needs
// Zach with a re-run, and a verdict it gives later is still applied.
func reviewPending(agents []Agent, id string, now time.Time) bool {
	for _, r := range agents {
		if r.L(labelRole) != "reviewer" || r.L(labelReviewOf) != id || r.archived() || reviewerBlocked(agents, r, now) {
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

// directReply reports whether card a's last reply ends with TRACK: direct and its recent log has no
// TIER line (a plan).
func (f *Factory) directReply(a Agent) bool {
	reply, err := f.lastReply(a)
	if err != nil || ParseTrack(reply) != "direct" {
		return false
	}
	text, err := f.lastText(a.ID, 60)
	return err == nil && ParseTier(text) == ""
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
		f.logf("%s direct card not on main: %s", short(a.ID), why)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: "landing not verified"})
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
		// the reply drops its TIER line (stripTrailer, lastMessage): look for it in the recent log
		if text, err := f.lastText(a.ID, 60); err != nil || ParseTier(text) == "" {
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
