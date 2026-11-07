package factory

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// questionRule teaches agents the QUESTION: block the board shows as multiple choice (ParseQuestions).
const questionRule = `When Zach must decide something, ask it as a block he can answer by tapping, one per question
(at most 4), recommended option first; he can always write his own answer instead:
QUESTION: <one short question>
- <option (recommended)>
- <option>
Use 2 to 6 options, one line each, not in a code block. A yes/no question needs no block.
Keep all the questions and their options to 60 words together. An email prints them whole or not at all:
past that it sends him to the board, and a one-word yes does nothing.`

// ideaRule lets a card's Fellow or reviewer hand out-of-scope work to the Ideas column (POST /api/idea);
// by names the suggester; note adds a role-specific line.
func ideaRule(port int, by, note string) string {
	return fmt.Sprintf(`
Ideas. If you find work worth doing outside this card's scope (a bug or gap outside the area of the ask), do not do it
here: suggest it as an idea, at most 2 per card, only with a concrete payoff. %sIt never replaces
finishing this card. First check it is not already an idea or a card in flight:
`+"`curl -s localhost:%d/api/ideas`"+` and the peers brief. Then:
curl -s -X POST localhost:%d/api/idea -H 'X-Factory: 1' -H 'Content-Type: application/json' \
  -d '{"repo":"<owner/repo>","by":"%s","priority":"<high|normal|low>","title":"<outcome, at most 60 characters>","summary":"<1-2 plain sentences>","why":"<evidence>","doneWhen":"<check>","request":"<technical brief>"}'
Priority is normal unless your evidence shows it is urgent (high) or can wait (low); ideas start on their own by priority, then age.
`, note, port, port, by)
}

// wholeChangeRule is the consistency rule for every plan, build and review: changes are whole.
const wholeChangeRule = `Whole changes: do each thing one way. A change updates every caller, test, doc and README the old
behaviour touched, and leaves no old path, flag or fallback behind. Grep for the old name or pattern
and change every instance, or list each one left alone and why. Refactoring inside the area of the
ask is part of the job. A partial change (two ways of doing one thing, a stale caller, test or doc,
an old path left in place) is a blocking finding.`

var (
	verdictRe = regexp.MustCompile(`(?i)VERDICT:\s*\**\s*(approve|nits|changes|escalate|close)`)
	trackRe   = regexp.MustCompile(`(?i)TRACK:\s*\**\s*(direct)`)
	titleRe   = regexp.MustCompile(`(?im)^[ \t>*_#-]*TITLE[*_]*\s*:[ \t]*(.*)$`)
)

// titleCap is the most runes a planner's TITLE line keeps; longer ones are cut at a word.
const titleCap = 60

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

func lastMatch(re *regexp.Regexp, text string) string {
	m := re.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToLower(m[len(m)-1][1])
}

// ParseVerdict returns the reviewer's last VERDICT line, or "" if there is none.
func ParseVerdict(text string) string { return lastMatch(verdictRe, text) }

// ParseTrack returns "direct" when the builder's reply ends with a TRACK: direct line (the prompt asks
// for it as the last line). Only the last non-empty line counts: the Fellow prompt has the same line
// mid-text, and `paseo logs` text cannot always tell a prompt from the reply.
func ParseTrack(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lastMatch(trackRe, lines[len(lines)-1])
}

// landRubric is a landing review's job: an agent outside the card flow asks before it lands.
const landRubric = `You are the Review Attending: a DIFFERENT agent instance from the one that did this work. Find what is wrong before it lands.
Check the work against the request and follow-ups (section below):
- Read the diff: ` + "`git -C <cwd> log --oneline origin/main..HEAD`, `git -C <cwd> diff origin/main...HEAD`" + `
- Correct? Run the repo's tests or the narrowest check that proves it, and paste the command and its last lines.
- Tests: none skipped or weakened. No secrets, tokens or private data committed.
- <visibility> If the repo is public or unknown, any private data in the diff is a blocking finding:
  names or emails of people (other than Zach's public identity), Zach's personal details, internal
  hostnames or tailnet URLs (*.ts.net), local paths (/home/<user>/...), and content from <errands> or PDW.
- Repo fit and whole change (rule below).`

// reviewerRules are the rules every reviewer follows, read-only or not (the gate has its own edit rule).
const reviewerReadOnly = `- Read-only. Do not edit, commit, push, land, send email, create PDW mutations, or message the builder.
  Use a scratch copy in .factory-scratch/ (listed in .git/info/exclude) if you need to try something.`

const gateEdits = `- Edit only for a small refactor or to remove a public-safety leak, on this card's branch, with a clean worktree
  and the tests passing when you stop. Dev servers for testing run in the worktree, bound to loopback or this host's
  tailnet IP, and you stop them before you end. Read production; never change it. Do not push, land, change anything
  outside the worktree, send email, create PDW mutations, or message the builder.`

// ReviewPrompt is the prompt for a fresh reviewer agent of the given kind (a gate, or land); port is the
// factory's, for the peers brief; vis is the repo's visibility ("public", "private" or "unknown").
func ReviewPrompt(c Card, kind string, port int, vis string) string {
	cwd := c.Cwd
	if cwd == "" {
		cwd = "."
	}
	branch := c.Branch
	if branch == "" {
		branch = "n/a"
	}
	by := c.SubmittedBy
	if by == "" {
		by = submittedBySystem
	}
	body := gateRubric[kind]
	if kind == "land" {
		body = landRubric
	}
	r := strings.NewReplacer("<cwd>", cwd, "<submitted-by>", by, "<source>", cmp.Or(c.Source, "unknown"), "<visibility>", visibilityLine(vis)).Replace(body) + "\n\n" + reviewPeersRule(port, kind, c.ID)
	if c.Decided {
		r += "\n" + decidedRule
	}
	edits := reviewerReadOnly
	if kind == gateCheck {
		edits = gateEdits
	}
	verdicts := "approve (it passes), nits (it passes; list nits, no new work), changes (blocking findings; it goes back to the builder), close (duplicate, already done, or not needed)"
	r += ideaRule(port, "reviewer of "+c.Short, "Posting an idea is the only outside write you may make. ")
	return fmt.Sprintf(`%s

Card: "%s" (Paseo agent %s on %s).
Working directory: %s
Repo: %s  Branch: %s

Read the builder's conversation: `+"`paseo logs %s --tail 200`"+`.

%s

%s

Rules:
%s
- Blocking findings only: each one concrete (file:line or exact claim), with why it matters. Style preferences are not blocking.
- Allowed verdicts: %s. There is no one to escalate to: decide.
- Keep it short: at most 8 bullets.
- `+shellRule+`
- `+scratchSafetyRule+`

%s

`+overviewRule+`

End your final message with exactly this line:
VERDICT: <approve|nits|changes|close>
`, r, c.Title, c.ID, c.Host, cwd, c.Repo, branch, c.ID, requestSection(c.Request), wholeChangeRule, edits, verdicts, questionRule)
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

const newCardPrompt = `You are the Builder on Zach's agent factory board: you own this card until its proposed solution is ready.

Zach's request:
%s

Work in this repo (%s). Read its AGENTS.md and README first.

` + fellowComputerUseRule + `

%s
Your first reply includes one line naming the card:
TITLE: <the outcome in at most 60 characters, plain words, no file names>

First decide the track. Direct track: the request is an errand that changes nothing: a question, suggestions,
research, a report or a note for Zach, or a PDW mutation he approves himself (including an email or message to
someone other than Zach that is sent only through a PDW mutation he approves, such as gmail.send_email: write the
message, queue the mutation, reply to Zach with an Approve button, and end TRACK: direct). Do the whole thing now,
in one pass, as well as you can. Create your <errands> work item directory (work/YYYY-MM-DD-short-name/) in this worktree first and do all your work
there, following the worktree and scratch rules below. Check
every fact at its source, including anything that changed in the last day (a rebooked flight, a
moved meeting: look at <errands> work items on origin/main from today). Deliver the result, record it in
a <errands> work item, land it (commit, then land it the way that repo's AGENTS.md says), and end your final message with the line
TRACK: direct
The direct track commits only its own <errands> work item (work/...): no other files, no other repo.
A direct result that asks Zach something (a DECISION that is not None, a NEEDS FROM ZACH that is not
Nothing, or a QUESTION block) waits for him once its work has landed, and his reply comes back to you on
this track. If the ask has a deadline, add one line right after NEEDS FROM ZACH, an ISO time with its
time zone:
DUE: 2035-01-31T22:00:00-05:00
Anything else (code, config, infra, deletes, money, bulk sends, or messages to anyone but Zach sent without a PDW review) is the build track:

` + proposedSolutionRule + `

%s

` + worktreeRule + `

` + boardBuilderRule + `

` + overviewRule + `

If this card cannot finish until another card lands, say so with one line: Waits on: <short id> (it lands after that card)
or Waits on: <short id> before build (stop now; the factory restarts you once it lands).
If stuck, stop and write a handoff: what you tried, what you know, what you need. You run unattended: never use
AskUserQuestion or plan mode. Anything you need from Zach goes in this form:
%s
`

const landMsg = "Land it now the way this repo lands work: read its AGENTS.md and README (and how recent commits reached " +
	"main) and do that. Land exactly the reviewed commits: no new commits or amends. If the rebase conflicts or anything must change, stop and say so " +
	"instead of landing. " + landRunbookRule + "Then run any deploy steps or runbook from the reviewed summary and verify the landing, deploy and live result (if a step fails after the push, stop and report it; do not improvise on the live system). Then stop with a one-line summary. " + overviewReminder + " The agent factory deploys itself " +
	"from its repo's main within a minute of landing (and rolls back if it does not come up): never run its install.sh " +
	"to deploy. " + shellRule + " " + scratchCleanupRule

const noLiveChangesRule = "Do not make live changes (install scripts, enable or restart services, resize VMs, change external settings): they wait for the landing step, as a runbook."

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

// ApplyVerdict records a gate reviewer's verdict once (factory.applied) and moves its card.
func (f *Factory) ApplyVerdict(rev Agent) error {
	target, kind := rev.L(labelReviewOf), rev.L(labelReviewKind)
	if kind == "" {
		kind = oldGateRequest // a reviewer from before review kinds were recorded: nothing to apply
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
		gone := t.gone() || interruptedArchive(t)
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
	if !isGate(kind) {
		// a reviewer from an earlier flow: the card was moved to the gate (migrateFlow)
		f.logf("reviewer %s: old review kind %q, verdict not applied", short(rev.ID), kind)
		return consume()
	}
	return f.decide(t, reviewOutcome{kind: kind, verdict: verdict, notes: notes, started: rev.CreatedAt}, consume)
}

// reviewOutcome is one gate round's result.
type reviewOutcome struct {
	kind, verdict, notes string
	started              time.Time // when the gate's reviewer started
}

// decide moves card t once per gate round: when every check but sign-off is green, sign-off decides between
// Zach and landing; otherwise the work goes back to the builder and the gate runs again when it stops. consume
// records the reviewer's verdict. The caller holds t's gate: a round started before t's last follow-up moves nothing.
func (f *Factory) decide(t Agent, r reviewOutcome, consume func() error) error {
	target, kind, verdict, notes := t.ID, r.kind, r.verdict, r.notes
	g := f.gate(target)
	if !g.followUp.IsZero() && !r.started.After(g.followUp) {
		f.logf("%s gate round of %s started before its last follow-up: not applied", kind, short(target))
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
	if consume != nil {
		if err := consume(); err != nil {
			return err
		}
	}
	labels := map[string]string{}
	if t.L(labelReviewerRetries) != "" {
		labels[labelReviewerRetries] = "" // a verdict counts: its replacements start afresh
	}
	if verdict == "" {
		return f.replaceReviewer(t, kind, "ended without a verdict")
	}
	roundsKey := roundsLabel(kind)
	rounds := atoi(t.L(roundsKey)) + 1
	labels[roundsKey] = fmt.Sprint(rounds)
	labels[labelVerdict] = kind + " " + verdict
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(target, labels)
	}
	grades := ParseGrades(notes)
	labels[labelGrades] = gradesLabel(grades)
	failed := gateFailures(grades, f.guarded(t.Repo))
	head := strings.TrimSpace(t.L(labelReviewed))
	var err error
	switch {
	case verdict == "close":
		err = set(labelStage, "zach", labelNote, "reviewer suggests closing")
	case verdict == "escalate" && len(failed) > 0,
		len(failed) > 0 && head != "" && head == t.L(labelGateFailedHead):
		// blocked on Zach: the gate says only he can unblock it, or the builder changed nothing since the
		// gate last sent this commit back. Another round would get the same answer.
		err = set(labelStage, "zach", labelSince, iso(f.Now()), labelNote, gateBlockedNote, labelAutoReview, "")
	case verdict == "escalate" && len(failed) == 0:
		// no one to escalate to but Zach: it never lands on its own
		err = f.passSignoff(t, labels, kind+" "+verdict, notes+"\nSIGN OFF: red - the gate could not decide this itself")
	case (verdict == "approve" || verdict == "nits") && len(failed) == 0:
		err = f.passSignoff(t, labels, kind+" "+verdict, notes)
	case rounds < f.cfg.ReworkCap:
		if err = set(labelStage, "building", labelSince, iso(f.Now()), labelAutoReview, kind, labelGateFailedHead, head); err == nil {
			err = f.advance(labeled(t, labels), "gate fixes", reworkGateMsg(failed, rounds, f.cfg.ReworkCap, notes))
		}
	default:
		// the round cap: the card stays in the gate, marked stuck, on the board only
		labels[labelGateFailedHead] = head
		if err = f.setLabels(target, labels); err == nil {
			err = f.markStuck(labeled(t, labels), stuckGate)
		}
	}
	f.recordRound(target, kind, rounds, r.started, verdict, notes)
	f.logf("%s gate %s for %s (round %d)", kind, verdict, short(target), rounds)
	return err
}

// passSignoff moves card t on from the gate's sign-off: a public repo's scan first, then to Zach when the gate
// (finishedWorkReason) says he signs off, otherwise to landing. An answer to his feedback on the version he was shown returns to him, unless it asks
// nothing and no reason but "you asked for it" holds (feedbackGate): then it is emailed in its thread, if any, and goes on to landing.
func (f *Factory) passSignoff(t Agent, labels map[string]string, verdict, notes string) error {
	target, now := t.ID, iso(f.Now())
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(target, labels)
	}
	disposition, hold := f.assessReview(t, t.Cwd)
	if disposition == reviewAlreadyLanded {
		if note := f.publishedScanNote(t); note != "" {
			// something already public that the factory's scan did not clear: Zach is told at once
			labels[labelStage], labels[labelVerdict], labels[labelNote] = "zach", verdict, note
			return f.setLabels(t.ID, labels)
		}
		if err := f.setLabels(t.ID, labels); err != nil {
			return err
		}
		return f.beginLiveCompletion(labeled(t, labels))
	}
	if hold != nil {
		// Exact code approval and held release expire here. Finished behavior authorization
		// survives separately and requires a fresh unchanged finding after recovery.
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
	scanFailed := scanned != "" && !scanPassed(scanned)
	sameVersion := t.L(labelReadyFeedback) != "" && t.L(labelReadyHead) != "" && t.L(labelReadyHead) == strings.TrimSpace(t.L(labelReviewed))
	goesAhead, held := false, ""
	if sameVersion && !scanFailed {
		if reply, rerr := f.askReply(t); rerr == nil && feedbackAsksNothing(reply) {
			held = f.feedbackGate(t, notes)
			goesAhead = held == ""
		}
	}
	readyWhy := func() string {
		if goesAhead {
			return ""
		}
		return feedbackReadyReason(t, f.finishedWorkReason(t, notes))
	}
	var err error
	if scanFailed {
		// a scan hit or error stops the card; a clean public-repo card goes on like any other
		err = set(labelStage, "zach", labelVerdict, verdict, labelNote, scanned)
	} else if sameVersion && !goesAhead {
		// reviewHold above established a clean HEAD matching this reviewed revision.
		if held != "" {
			labels[labelAskWhy] = held // the board's reason; the email and board read labelFeedbackHold
		}
		err = set(labelStage, "zach", labelVerdict, verdict, labelNote, feedbackPrefix+" "+t.L(labelReadyFeedback),
			labelWriting, "", labelFeedbackHold, held)
	} else if why := readyWhy(); why != "" {
		// the one stop for Zach, Ready for you: the builder writes what changed, the factory runs the
		// preview (tryout.go), and mailZach waits for both
		n := readyRound(t)
		if err = set(labelStage, "zach", labelVerdict, verdict, labelNote, readyNote(n), labelReady, fmt.Sprint(n),
			labelAskWhy, why, labelWriting, now, labelReadyHead, strings.TrimSpace(t.L(labelReviewed)), labelReadyFeedback, "", labelFeedbackHold, ""); err == nil {
			msg := readyMsg(why)
			if fb != "" {
				// one message: a second would interrupt the builder writing the first
				msg += uiFeedbackPreviewNote + fb
				fb = ""
			}
			err = f.send(target, msg)
		}
	} else {
		if goesAhead {
			f.mailFeedbackGoesAhead(t)
			labels[labelFeedbackHold] = ""
		}
		note := "landing: no sign-off needed"
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
			msg := "The gate passed. " + f.landMsgFor(t)
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

// StartReview starts gate kind's fresh reviewer agent for card c in the card's own workspace and returns its
// id. A gate needs a clean worktree, so that what it reads is what lands: a dirty one gets the builder a
// message to commit first, and the gate stays queued until it stops again.
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
	labels[labelOwnReviewed] = ""
	if c.Cwd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		dirty, err := f.dirtyStatus(ctx, c.Cwd)
		head, herr := f.GitOut(ctx, c.Cwd, "rev-parse", "HEAD")
		cancel()
		switch {
		case err != nil || herr != nil:
			// not a git checkout (or git failed): review anyway; with no reviewed commit recorded,
			// its approval stops for Zach (reviewHold) instead of landing
			f.logf("%s: no clean-tree check before review: %v %v", c.Short, err, herr)
			labels[labelReviewed] = ""
		case dirty != "":
			return "", f.askToCommit(c, dirty, kind)
		default:
			labels[labelReviewed] = strings.TrimSpace(head)
			if f.ownReviewedWork(labeled(f.agent(c.ID), labels)) {
				labels[labelOwnReviewed] = labels[labelReviewed]
			}
			labels[labelDirtyAsks] = ""
		}
	}
	rid, err := f.runReviewer(c, kind, f.visibility(c.Cwd), gateTitle(kind, c.Title), "", "")
	if err != nil {
		return "", err
	}
	f.logf("started %s gate %s for %s (%s)", kind, short(rid), c.Short, trunc(c.Title, 40))
	labels[labelStage] = kind
	if err := f.setLabels(c.ID, labels); err != nil {
		return rid, err
	}
	return rid, nil
}

// dirtyAskCap is how many times a builder is asked to commit before review; then Zach decides.
const dirtyAskCap = 2

// askToCommit tells a builder whose worktree is dirty to commit before gate kind, and keeps it queued. After dirtyAskCap asks the card goes to Needs Zach.
func (f *Factory) askToCommit(c Card, dirty, kind string) error {
	asks := atoi(f.agent(c.ID).L(labelDirtyAsks)) + 1
	if asks > dirtyAskCap {
		f.logf("%s worktree still not clean after %d asks", c.Short, dirtyAskCap)
		return f.setLabels(c.ID, map[string]string{labelStage: "zach", labelAutoReview: "",
			labelNote: "worktree not clean for review", labelReviewerRetries: ""})
	}
	if err := f.setLabels(c.ID, map[string]string{labelStage: "building", labelAutoReview: kind,
		labelSince: iso(f.Now()), labelDirtyAsks: fmt.Sprint(asks), labelReviewerRetries: ""}); err != nil {
		return err
	}
	f.logf("%s: worktree not clean, asked the builder to commit before review", c.Short)
	return f.send(c.ID, "Before the next gate the worktree must be clean, so the reviewer sees exactly what lands. "+
		"Commit everything that belongs to this work on your branch (do NOT land), "+
		"then stop with a short summary (including the deploy steps as built, if any). "+noLiveChangesRule+" "+overviewReminder+" The gate starts when you stop.\n\n"+worktreeRule+"\n\ngit status --porcelain:\n"+trunc(dirty, 1500))
}

// staleReviewNote and the unverified notes have no comma: label values cannot hold one (labelArg).
// Every note of a card held because its reviewed commit could not be checked starts with
// unverifiedReviewNote; its Re-run starts the gate again.
const (
	staleReviewNote      = "changed since review: fresh gates"
	unverifiedReviewNote = "reviewed commit not verified: "
	noReviewedNote       = unverifiedReviewNote + "no reviewed commit recorded"
)

// reviewHold reports why card a may not be approved for landing or sent a land prompt yet: nil when
// its HEAD is still the commit its gates saw (factory.reviewed) and its worktree is clean, else
// the labels that move it. A moved HEAD or uncommitted work goes back to the gate (since
// cleared, so it starts once the agent is stopped); a HEAD or status that cannot be read stops the
// card for Zach, and so does a card with no reviewed commit in any repo, as cardLandBlock refuses to
// publish it. Zach's approval is no review: it is checked the same way.
func (f *Factory) reviewHold(a Agent, cwd string) map[string]string {
	zach := func(note string) map[string]string {
		return map[string]string{labelStage: "zach", labelNote: note, labelStuckFrom: "review",
			labelHeld: "", labelAutoReview: ""}
	}
	reviewed := strings.TrimSpace(a.L(labelReviewed))
	if reviewed == "" {
		return zach(noReviewedNote)
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
	stale := map[string]string{labelStage: "building", labelAutoReview: gateCheck, labelSince: "", labelHeld: "",
		labelNote: staleReviewNote}
	if head = strings.TrimSpace(head); head != reviewed {
		f.logf("%s HEAD %s is not the reviewed %s: fresh gates", short(a.ID), short(head), short(reviewed))
		return stale
	}
	// uncommitted work on the reviewed commit was not reviewed either: it is committed and reviewed first
	dirty, err := f.dirtyStatus(ctx, cwd)
	if err != nil {
		return zach(unverifiedReviewNote + "could not read worktree status: " + trunc(err.Error(), 150))
	}
	if dirty != "" {
		f.logf("%s has uncommitted work on the reviewed %s: fresh gates", short(a.ID), short(reviewed))
		return stale
	}
	return nil
}

// runReviewer starts a read-only reviewer of the given kind for c. For a landing review, why is the
// agent's note and head the commit under review.
func (f *Factory) runReviewer(c Card, kind, vis, title, why, head string) (string, error) {
	if a, ok := f.agentOK(c.ID); ok && c.Request == nil {
		r := f.cardRequest(a)
		c.Request = &r
	}
	prompt := strings.ReplaceAll(ReviewPrompt(c, kind, f.cfg.Port, vis), "<errands>", f.errandsName())
	if kind == gateCheck {
		prompt += f.approvedBehaviorPrompt(f.agent(c.ID))
	}
	if f.guarded(c.Repo) {
		prompt += publicReviewRule
	}
	if (kind == gateCheck || kind == "land") && f.uiChange(c) {
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
	return f.startReviewer(c, kind, title, prompt, nil)
}

// startReviewer starts a read-only reviewer agent of kind for c with prompt (the sub-agent rule is
// added here), with labels besides its role, card and kind.
func (f *Factory) startReviewer(c Card, kind, title, prompt string, labels map[string]string) (string, error) {
	// reviewerRunArgs: gate reviewers and landing reviewers stay on the default agent (Opus 5.5)
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
	f.mailStuckPrompts(f.agents(), now)
	f.mailParkedAsks(f.agents())
	f.mailDueReminders(f.agents(), now)
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
	// no digest: the factory sends Zach no email of its own (digest.go keeps the shipped ledger)
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
	if handled, err := f.tickArchive(a); handled || err != nil {
		return err
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
	if a.L(labelStage) == "building" && a.L(labelSource) == "board" && a.L(labelBase) == "" && !routing && !a.archived() {
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
	if moved, err := f.migrateFlow(a); moved || err != nil {
		return err
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
	case routing && a.L(labelStage) == stageRoute && (acted || a.L(labelRouted) != ""):
		if live, err := f.stillStopped(a); !live {
			return err
		}
		return f.tickPicker(a)
	case (a.L(labelAutoReview) != "" || (a.L(labelStage) == "building" && a.L(labelHeld) == "" && acted)) && (acted || since.IsZero()) && a.L(labelStage) != "landed": // Done needs no review
		if live, err := f.stillStopped(a); !live {
			return err
		}
		if a.L(labelAutoReview) == "" || isOldGate(a.L(labelAutoReview)) {
			a = labeled(a, map[string]string{labelAutoReview: gateCheck}) // a stopped builder always goes to the gate
		}
		// A landed-diff recovery must review the published HEAD even when there are no
		// unpushed commits. A verification carried from an earlier landing cannot cover it.
		if a.L(labelStage) == "building" && isGate(a.L(labelAutoReview)) && a.L(labelLandedVerified) == "1" && !readRecovery(a).LandedDiff {
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
		if a.L(labelStage) == "building" {
			// only the agent's own last reply: the builder prompt itself has a "TRACK: direct" line
			text, err := f.storedReply(a)
			if err != nil {
				if f.gate(a.ID).finished {
					// finished: StartReview refuses it, and nothing here decides on the reply
					_, err = f.StartReview(c, a.L(labelAutoReview))
					return err
				}
				return f.waitForHistory(a, a, err)
			}
			f.nameCard(a, text)
			if !gatedBefore(a) {
				guarded := f.guarded(c.Repo)
				// a public repo has no direct track: only a first pass that says it took one finishes as one,
				// and finishDirect scans what it pushed and stops for Zach. A reply there goes through the gate.
				direct := ParseTrack(text) == "direct" && (!guarded || a.L(labelFollowedUp) == "")
				if !direct && !guarded && fromInbox(a) && f.noOwnCommits(c.Cwd) {
					// an inbox card with no RESULT line and nothing unpublished did an errand; its last reply may
					// be a line after its final command, so read recent session text. Commits always get the gate.
					recent, err := storedText(f.cfg.Home, a, 60)
					if err != nil {
						return f.waitForHistory(a, a, err)
					}
					direct = ParseResult(recent) == ""
				}
				if direct {
					return f.markDirect(a, c)
				}
			}
			if held, err := f.holdStoppedBuildWait(a); held || err != nil {
				return err
			}
			if title, asks := directAsk(text); asks && ParseResult(text) == "" {
				// a builder that stopped on a question, with no finished solution: only Zach can answer it
				f.logf("%s stopped on a question for Zach", short(a.ID))
				return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelSince: iso(now), labelNote: directAskNote(title),
					labelDue: f.replyDue(a, text), labelAutoReview: ""})
			}
			labels := map[string]string{}
			setPlanWaitsOn(a, labels, text)
			if res := ParseResult(text); res != "" {
				labels[labelResult] = res
			}
			if len(labels) > 0 {
				if err := f.setLabels(a.ID, labels); err != nil {
					return err
				}
			}
		}
		_, err := f.StartReview(c, a.L(labelAutoReview))
		return err
	case (a.L(labelStage) == "landing" && a.L(labelHeld) != "") || heldBeforeBuild(a):
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
		if why := ParseReturnToBuild(reply); why != "" {
			f.logf("%s lander found big runbook drift: %s", short(a.ID), why)
			return f.returnToBuildFrom(a, gateCheck, "runbook no longer matches", "The lander checked your runbook against the live system "+
				"and found changes too big to correct: "+trunc(why, landFailMax)+"\n\nRework the proposed solution for the new state, read-only, and stop with the whole new runbook and the RESULT: line. "+overviewReminder, false)
		}
		if a.L(labelResult) == "runbook" && f.noOwnCommits(c.Cwd) {
			// a runbook-only card has no commits to verify: the lander's report is its result
			return f.finishLanding(a, c, reply)
		}
		if a.L(labelLiveCompletion) != "" {
			disposition, hold := f.assessReview(a, c.Cwd)
			if hold != nil {
				return f.setLabels(a.ID, clearHeldApproval(a, hold))
			}
			if disposition != reviewAlreadyLanded {
				return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: unverifiedReviewNote + "The factory could not verify whether the reviewed work was published.", labelStuckFrom: "review"})
			}
			return f.finishLanding(a, c, reply)
		}
		switch status, why := f.checkLanding(a, c.Cwd); status {
		case landNotOnMain:
			if _, found := ParseRemaining(reply); !found && a.L(labelLandRun) != "" {
				// its bin/land run is over but the reply has no REMAINING line, which the final report
				// ends with: a progress note written while it ran, and the agent has not reported yet.
				// One that never reports goes through the stopped-step recovery.
				return f.checkStuck(a, now)
			}
			// a push failure or rebase conflict: the card fixes its branch, then the gate again
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
		if f.Mail == nil || lineageTold(f.byID(), a) == waitNote(a) || !mailable(c.Ask) ||
			lineageEmailThread(f.byID(), a) == "" { // board waits are not emailed
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
	stageRoute: "Repo picker stopped, nothing queued",
	"building": "Builder stopped, nothing queued",
	gateCheck:  "Reviewer gone, no verdict",
	"landing":  "Landing agent stopped without landing",
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
// column replaces its gone or blocked reviewer (replaceReviewer); building and landing get a
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
	case gateCheck:
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
			WorkspaceID: a.WorkspaceID}
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

// waitNote says why a Needs Zach card waits; a card with an email thread is emailed it once per note (mail.go).
func waitNote(a Agent) string {
	if n := a.L(labelNote); n != "" {
		return n
	}
	return "waiting on Zach"
}

// directReply reports whether card a's last reply ends with TRACK: direct and its recent session text has no
// RESULT line.
func (f *Factory) directReply(a Agent) (bool, error) {
	reply, err := f.storedReply(a)
	if err != nil {
		return false, err
	}
	if ParseTrack(reply) != "direct" {
		return false, nil
	}
	text, err := storedText(f.cfg.Home, a, 60)
	return err == nil && ParseResult(text) == "", err
}

// markDirect labels card a as a direct-track card, so Zach's later replies keep it on that track
// (errandPass), then finishes it.
func (f *Factory) markDirect(a Agent, c Card) error {
	if err := f.setLabels(a.ID, map[string]string{labelTrack: "direct", labelAutoReview: "", labelStuck: ""}); err != nil {
		return err
	}
	return f.finishDirect(a, c)
}

// finishDirect closes a card the builder did in one pass (TRACK: direct): no gates,
// but its work must be on origin/main like any other landing. A last reply that asks Zach something
// waits for him instead of closing (directAsk), and his reply continues it on this track.
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
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: note, labelStuck: ""})
	}
	if a.archived() && worktreeGone(c.Cwd) {
		f.logf("%s %s", short(a.ID), directWorktreeGoneNote)
		return f.setLabels(a.ID, map[string]string{labelStage: "landed", labelNote: directWorktreeGoneNote, labelStuck: ""})
	}
	if ok, why := f.landedOnMain(c.Cwd, a.L(labelBase)); !ok {
		return f.returnDirect(a, why)
	}
	// the direct track pushes only its own work item; anything else needed the build track's reviews
	outside, err := f.ownFilesOutside(c.Cwd, "work/")
	switch {
	case err != nil:
		f.logf("%s direct card scope not checked: %v", short(a.ID), err)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: "direct card scope not verified: " + trunc(err.Error(), 150), labelStuck: ""})
	case len(outside) > 0:
		f.logf("%s direct card changed %v", short(a.ID), outside)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach",
			labelNote: "direct card changed files outside its work item without review: " + trunc(strings.Join(outside, ", "), 150), labelStuck: ""})
	}
	reply, err := f.storedReply(a)
	if err != nil {
		return f.waitForHistory(a, a, err)
	}
	f.recordLanded(a, c.Cwd)
	if title, asks := directAsk(reply); asks {
		f.logf("%s done in one pass, and asks Zach", short(a.ID))
		now := iso(f.Now())
		labels := map[string]string{labelStage: "zach", labelSince: now, labelNote: directAskNote(title),
			labelDue: f.replyDue(a, reply), labelStuck: ""}
		if a.archived() {
			labels[labelParked] = now // it shows, a reply wakes it, and mailParkedAsks emails it in its thread
		}
		return f.setLabels(a.ID, labels)
	}
	f.logf("%s done in one pass", short(a.ID))
	return f.setLabels(a.ID, map[string]string{labelStage: "landed", labelNote: "done in one pass", labelDue: "", labelStuck: ""})
}

// nameCard renames board card a once, from the TITLE line of its first plan (or direct result),
// reply being its last message. Titles given by the caller (factory.titled=given) and inbox cards keep theirs; a reply
// that is neither a proposed solution nor a direct result (a question) leaves the card for its next reply. A
// failed rename is logged and does not hold up the card.
func (f *Factory) nameCard(a Agent, reply string) {
	if a.L(labelSource) != "board" || a.L(labelTitled) != "" {
		return
	}
	if ParseTrack(reply) != "direct" && ParseResult(reply) == "" {
		return // a question: the card keeps its title for its next reply
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

// freshBase fetches main in the checkout at dir and returns origin/main, the base a new card's branch starts
// from: the checkout's own main may be behind or carry commits that never reached origin. It is "" when the
// checkout has no origin/main; a failed fetch still branches from the last fetched origin/main.
func (f *Factory) freshBase(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.Git(ctx, dir, "fetch", "-q", "origin", "main"); err != nil {
		f.logf("fetch in %s failed, branching from the last fetched origin/main: %v", dir, err)
	}
	if _, err := f.GitOut(ctx, dir, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return ""
	}
	return "origin/main"
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

const appDoneNote = "finished in Paseo"

type appDeliveryKey struct {
	id     string
	last   string
	status string
	reason string
	req    bool
	cwd    string
}

type appDeliveryEntry struct {
	key  appDeliveryKey
	done bool
	note string
}

// appDelivered reports whether a Paseo-started (app) card that would otherwise sit in Needs Zach
// has already delivered its result: either it finished cleanly with a final report (not a question
// or decision), or it errored after sending its reply email or landing its work on origin/main.
func (f *Factory) appDelivered(a Agent, fetch bool) (bool, string) {
	if !isAppCard(a) || a.L(labelStage) != "" || a.archived() || a.Status == "running" || a.Pending != "" {
		return false, ""
	}
	key := appDeliveryKey{
		id:     a.ID,
		last:   iso(a.LastActivityAt),
		status: a.Status,
		reason: a.AttentionReason,
		req:    a.RequiresAttention,
		cwd:    a.Cwd,
	}
	f.mu.Lock()
	if e, ok := f.appDeliveries[a.ID]; ok && (e.key == key || !fetch) {
		f.mu.Unlock()
		return e.done, e.note
	}
	f.mu.Unlock()
	if !fetch {
		return false, ""
	}
	done, note, ok := f.checkAppDelivered(a)
	if ok {
		f.mu.Lock()
		if f.appDeliveries == nil {
			f.appDeliveries = map[string]appDeliveryEntry{}
		}
		f.appDeliveries[a.ID] = appDeliveryEntry{key: key, done: done, note: note}
		f.mu.Unlock()
	}
	return done, note
}

func (f *Factory) checkAppDelivered(a Agent) (done bool, note string, cacheable bool) {
	switch {
	case a.Status == "idle" && a.AttentionReason != "error":
		if a.RequiresAttention && a.AttentionReason != "" && a.AttentionReason != "finished" {
			return false, "", true
		}
		reply, err := storedReplyOne(f.cfg.Home, a)
		if err != nil {
			return false, "", false
		}
		if strings.TrimSpace(reply) != "" && !replyAsksQuestion(reply) {
			return true, appDoneNote, true
		}
		return false, "", true
	case a.Status == "error" || a.AttentionReason == "error":
		evs, err := storedEvents(f.cfg.Home, a)
		if err == nil && sentReplyEmail(evs) {
			return true, appDoneNote, true
		}
		if f.appWorkLanded(a) {
			return true, appDoneNote, true
		}
		return false, "", err == nil
	}
	return false, "", true
}

// replyAsksQuestion reports whether an assistant's final message is asking for an answer or decision
// rather than reporting a completed result.
func replyAsksQuestion(reply string) bool {
	rest, qs := ParseQuestions(reply)
	if len(qs) > 0 {
		return true
	}
	if _, ov := ParseOverview(rest); ov != nil {
		var tmp Ask
		tmp.Overview = ov
		cleanOverview(&tmp)
		if tmp.Overview != nil && tmp.Overview.Decision != "" {
			return true
		}
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(rest), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return false
	}
	idx := len(lines) - 1
	for idx > 0 && optionLine.MatchString(lines[idx]) {
		idx--
	}
	last := strings.TrimRight(lines[idx], " \t*_\"'`)>]")
	return strings.HasSuffix(last, "?")
}

var (
	pdwEmailSendRe  = regexp.MustCompile(`(?m)(?:^|[;&\n]+)\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*pdw\s+call\s+[A-Za-z0-9_]+__(?:send_email|reply_to_message|reply_email)\b([^\n;&|]*)`)
	pdwHelpFlagRe   = regexp.MustCompile(`(?:^|\s)(?:--help|-h)(?:\s|$)`)
	pdwDataFlagRe   = regexp.MustCompile(`(?:^|\s)(?:--data(?:\s|=)|-d(?:\s|=))`)
	failedExitRe    = regexp.MustCompile(`(?i)(?:command exited with code|exit code:|exited with code|exit status)\s+[1-9][0-9]*`)
	failedIsErrorRe = regexp.MustCompile(`"(?:isError|is_error)"\s*:\s*true\b`)
)

func isEmailSendTool(name string) bool {
	name = strings.TrimSpace(name)
	for _, suffix := range []string{"send_email", "reply_to_message", "reply_email"} {
		if name == suffix || strings.HasSuffix(name, "__"+suffix) {
			return true
		}
	}
	return false
}

func isPdwEmailSendCmd(cmd string) bool {
	for _, m := range pdwEmailSendRe.FindAllStringSubmatch(cmd, -1) {
		args := m[1]
		if !pdwHelpFlagRe.MatchString(args) && pdwDataFlagRe.MatchString(args) {
			return true
		}
	}
	return false
}

func isFailedToolResult(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "Error:") || strings.HasPrefix(t, "error:") || strings.HasPrefix(t, "Usage: pdw") {
		return true
	}
	return failedExitRe.MatchString(t) || failedIsErrorRe.MatchString(t)
}

func sentReplyEmail(evs []Event) bool {
	results := map[string]Event{}
	for _, e := range evs {
		if e.Kind == "result" && e.ID != "" {
			results[e.ID] = e
		}
	}
	for _, e := range evs {
		if e.Kind != "tool" || e.ID == "" {
			continue
		}
		if e.Name == "Bash" {
			if !isPdwEmailSendCmd(str(e.Input, "command")) {
				continue
			}
		} else if (!isEmailSendTool(e.Name) && !isEmailSendTool(e.Native)) || len(e.Input) == 0 {
			continue
		}
		res, ok := results[e.ID]
		if !ok || res.Error || strings.TrimSpace(res.Text) == "" || isFailedToolResult(res.Text) {
			continue
		}
		return true
	}
	return false
}

func (f *Factory) appWorkLanded(a Agent) bool {
	if a.Cwd == "" || a.CreatedAt.IsZero() {
		return false
	}
	if base := a.L(labelBase); base != "" {
		if ok, _ := f.landedOnMain(a.Cwd, base); ok {
			return true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	at, hasOwn, err := f.ownLandingTime(ctx, a.Cwd, a.CreatedAt)
	return err == nil && hasOwn && !at.IsZero()
}
