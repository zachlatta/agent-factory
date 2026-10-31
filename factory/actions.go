package factory

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Act runs one board action (POST /api/<action>).
func (f *Factory) Act(action string, b Body) (any, error) {
	switch action {
	case "new":
		return f.newCard(b)
	case "idea":
		return f.addIdea(b)
	case "scout":
		return f.startScout(b.Repo, b.Request)
	case "land-review":
		return f.startLandReview(b)
	case "answer":
		return f.answerByEmail(b)
	case "choose":
		b.Via = "" // the board's answer; email answers come through answer
		return f.choose(b)
	case "dismiss-request":
		return ok, f.dismissRequest(b.ID)
	}
	if strings.HasPrefix(b.ID, "idea-") {
		switch action {
		case "approve":
			return f.startIdea(b.ID, "zach", b.Repo, b.Agent, b.Message, b.Thinking)
		case "archive":
			return ok, f.dismissIdea(b.ID)
		}
		return nil, errors.New("ideas can only be started or dismissed")
	}
	c, found := f.card(b.ID)
	sub := false
	if !found && (action == "send" || action == "stop" || action == "archive") {
		// sub-agents and reviewers are not cards but can be messaged, stopped or archived
		if a, okA := f.agentOK(b.ID); okA {
			c, found, sub = Card{ID: a.ID, Host: a.Host, Status: a.Status, ArchivedAt: iso(a.ArchivedAt)}, true, true
		}
	}
	if !found {
		return nil, errors.New("unknown card")
	}
	if a := f.agent(c.ID); a.L(labelFinished) != "" && (action == "stage" && b.Stage != "landed" || action == "review" || action == "rerun") {
		if err := f.setLabels(c.ID, map[string]string{labelFinished: ""}); err != nil {
			return nil, err
		}
	}
	now := iso(f.Now())
	if a := f.agent(c.ID); !interruptedArchive(a) && a.L(labelStuck) != "" && (action == "stage" || action == "review" || action == "send" && !errorStuck(a)) {
		// Zach took the card on: its automatic recovery starts afresh (Re-run and Resume clear it themselves)
		if err := f.setLabels(c.ID, map[string]string{labelStuck: ""}); err != nil {
			return nil, err
		}
	}
	switch action {
	case "stage":
		if !validStage(b.Stage) {
			return nil, errors.New("bad stage")
		}
		if c.Parked != "" && b.Stage != "landed" && b.Stage != "zach" {
			// archived: no move would come for it in a working column
			return nil, errors.New("this card is parked: reply to it to wake it first")
		}
		labels := map[string]string{labelStage: b.Stage, labelSince: now}
		if b.Stage == "landed" {
			g := f.gate(c.ID)
			g.mu.Lock()
			defer g.mu.Unlock()
			labels[labelAutoReview] = ""
			labels[labelStuck] = ""
			if err := f.setLabels(c.ID, clearHeldApproval(f.agent(c.ID), labels)); err != nil {
				return nil, err
			}
			if err := f.endReviews(g, c.ID); err != nil {
				return nil, err
			}
			return ok, f.archiveAgent(c.ID)
		}
		f.reopen(c.ID)
		return ok, f.setLabels(c.ID, clearHeldApproval(f.agent(c.ID), labels))
	case "review":
		kind := b.Kind
		if kind != "plan" {
			kind = "code"
		}
		f.reopen(c.ID)
		if c.Status == "running" {
			labels := map[string]string{labelAutoReview: kind, labelSince: now}
			if f.agent(c.ID).L(labelStage) == "landed" {
				// out of Done, so the queued review starts once the agent stops
				labels[labelStage] = map[string]string{"code": "building", "plan": "plan"}[kind]
			}
			return map[string]bool{"queued": true}, f.setLabels(c.ID, labels)
		}
		_, err := f.StartReview(c, kind)
		return ok, err
	case "resume":
		if interruptedArchive(f.agent(c.ID)) {
			return ok, f.resumeArchived(c, "Resume the interrupted step. Check the current worktree before continuing.", nil)
		}
		return ok, f.resumeError(f.agent(c.ID), "Resume the interrupted step. Check the current worktree before continuing.", nil)
	case "send":
		if interruptedArchive(f.agent(c.ID)) {
			if strings.TrimSpace(b.Message) == "" && len(b.Attachments) == 0 {
				return nil, errors.New("message is empty")
			}
			return ok, f.resumeArchived(c, b.Message, b.Attachments)
		}
		if strings.TrimSpace(b.Message) == "" && len(b.Attachments) == 0 {
			return nil, errors.New("message is empty")
		}
		msg := strings.TrimSpace(b.Message)
		if msg == "" {
			msg = "(see attached)"
		}
		via := "Message from Zach via the factory board:\n\n"
		if b.Via == "email" {
			// his email reply, as a follow-up block so the card's request and reviewer prompts show it
			msg, via = f.emailFollowUp(msg), "Zach answered by email while the card waited on him:\n\n"
		}
		if !sub {
			// a review running now did not see this message or the work it asks for: from here on its
			// verdict is retired, even one a Tick is about to apply from an older list (ApplyVerdict)
			g := f.gate(c.ID)
			g.mu.Lock()
			defer g.mu.Unlock()
			g.followUp = f.Now()
		}
		if a := f.agent(c.ID); errorStuck(a) {
			if !sub {
				if err := f.retireReviews(c.ID); err != nil {
					return nil, err
				}
			}
			if err := f.resumeError(a, via+msg, b.Attachments); err != nil {
				return nil, err
			}
			f.delivered(c.ID, sub)
			return ok, nil
		}
		if a := f.agent(c.ID); a.L(labelStage) == "zach" && strings.HasPrefix(a.L(labelNote), "history unavailable") {
			// Retire the paused review before sending new work. Its file can appear
			// later, but its verdict must never decide the response to this message.
			if err := f.supersedeReviewers(c.ID); err != nil {
				return nil, err
			}
		} else if !sub {
			if err := f.retireReviews(c.ID); err != nil {
				return nil, err
			}
		}
		if c.Awaiting == "plan" {
			// Zach's answer to a build he was asked to allow: revise the plan, then plan review runs again
			msg = "Zach read your plan and wants changes:\n\n" + msg + "\n\nRevise the plan, then restate the " +
				"whole revised plan (not just the changes) and stop. " + overviewReminder + " Do not build yet."
			files, err := f.chat(c.ID, msg, b.Attachments)
			if err != nil {
				return nil, err
			}
			f.delivered(c.ID, sub)
			return map[string]any{"ok": true, "files": files}, f.setLabels(c.ID, answerLabels(c, f.agent(c.ID), now, f.guarded(c.Repo)))
		}
		if f.agent(c.ID).L(labelRouting) != "" {
			msg += "\n\nPick the repo again and end with the REPO: line."
		}
		if landingReply(f.agent(c.ID)) {
			msg += remainingRule
		}
		files, err := f.chat(c.ID, via+msg, b.Attachments)
		if err != nil {
			return nil, err
		}
		f.delivered(c.ID, sub)
		if a := f.agent(c.ID); !sub && a.L(labelStage) != "" {
			labels := answerLabels(c, a, now, f.guarded(c.Repo))
			labels[labelFinished] = ""
			labels[labelStuck] = ""
			if labels[labelAutoReview] == "code" {
				// his message is a new request: its code review is a generation of its own
				maps.Copy(labels, newCodeGeneration(a))
			}
			if err := f.setLabels(c.ID, labels); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true, "files": files}, nil
	case "approve":
		if a := f.agent(c.ID); a.L(labelRouting) != "" {
			return ok, f.pickRepo(a, b.Repo)
		}
		answers := ""
		if m := strings.TrimSpace(b.Message); m != "" {
			// Zach approved and answered the card's questions in one step
			answers = "\n\nZach's answers to your questions:\n" + m + "\n\n"
		}
		if a := f.agent(c.ID); c.Awaiting == "plan" || planPending(a) {
			notes := "(none)"
			if fix := a.L(labelPlanFix); fix != "" {
				notes = "Apply these fixes from the last plan review: " + fix
			}
			next := map[string]string{labelStage: "building", labelSince: now, labelAutoReview: "code",
				labelAwaiting: "", labelZachPlan: "approved", labelNote: "build allowed by Zach"}
			if err := f.advance(labeled(a, next), "build", buildMsg("Zach allowed this build."+answers, notes)); err != nil {
				return nil, err
			}
			return ok, f.setLabels(c.ID, next)
		}
		// Zach approves what was reviewed: a card that moved since goes back to code review, and one whose
		// reviewed commit cannot be checked does not land
		if hold := f.reviewHold(f.agent(c.ID), c.Cwd); hold != nil {
			hold = clearHeldApproval(f.agent(c.ID), hold)
			if hold[labelStage] == "zach" {
				return nil, errors.Join(errors.New(hold[labelNote]), f.setLabels(c.ID, hold))
			}
			return ok, f.setLabels(c.ID, hold)
		}
		msg := landMsg
		if a, ok := f.agentOK(c.ID); ok {
			if id, st := f.pendingDep(a); st == depWaiting {
				// its plan lands it after a card still in flight: the next Tick sends the land prompt (tickHeld).
				// A dropped or missing one is what Zach just answered, so that lands now.
				// His approval lets the factory release it even if it is parked by then (heldApprovedParked).
				return ok, f.setLabels(c.ID, map[string]string{labelStage: "landing", labelSince: now, labelHeld: id,
					labelNote: holdNote(id), labelHeldApproved: now})
			}
			msg = f.landMsgFor(a)
		}
		if f.guarded(c.Repo) {
			// scan again: the card may have committed since its review
			if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
				return nil, errors.Join(errors.New(note), f.setLabels(c.ID, map[string]string{labelNote: note}))
			}
		}
		next := clearHeldApproval(f.agent(c.ID), map[string]string{labelStage: "landing", labelSince: now,
			labelNote: "approved by Zach"})
		if err := f.sendLanding(labeled(f.agent(c.ID), next), "Zach approved this on the factory board."+answers+" "+msg); err != nil {
			return nil, err
		}
		return ok, f.setLabels(c.ID, next)
	case "arbiter":
		// Zach hands a review that did not settle to an arbiter agent, even one that ruled before
		a := f.agent(c.ID)
		kind := "code"
		if strings.HasPrefix(a.L(labelVerdict), "plan") {
			kind = "plan"
		}
		if err := f.supersedeReviewers(c.ID); err != nil {
			return nil, err
		}
		return ok, f.startArbiter(a, kind, "(Not kept here: the last review is the latest reviewer message in the builder's "+
			"conversation, or read the card's reviewers.)", false)
	case "rerun":
		f.reopen(c.ID)
		return ok, f.rerun(c)
	case "done":
		g := f.gate(c.ID)
		g.mu.Lock()
		defer g.mu.Unlock()
		if err := f.setLabels(c.ID, clearHeldApproval(f.agent(c.ID), map[string]string{labelStage: "landed",
			labelNote: "marked done by Zach", labelAutoReview: "", labelStuck: ""})); err != nil {
			return nil, err
		}
		if err := f.endReviews(g, c.ID); err != nil {
			return nil, err
		}
		if c.Parked != "" {
			return ok, f.unpark(c.ID)
		}
		return ok, nil
	case "close":
		note := "closed by Zach"
		if keep, err := f.recordSteer(f.agent(c.ID)); err != nil {
			return nil, err
		} else if keep != "" {
			note += ": duplicate of " + keep
		}
		if err := f.setLabels(c.ID, clearHeldApproval(f.agent(c.ID), map[string]string{labelNote: note})); err != nil {
			return nil, err
		}
		return ok, f.dropAgent(c.ID)
	case "archive":
		if a := f.agent(c.ID); a.L(labelHeldApproved) != "" {
			if err := f.setLabels(c.ID, clearHeldApproval(a, map[string]string{})); err != nil {
				return nil, err
			}
		}
		return ok, f.dropAgent(c.ID)
	case "stop":
		_, err := f.paseo("stop", c.ID)
		return ok, err
	}
	return nil, errors.New("unknown action")
}

var ok = map[string]bool{"ok": true}

// dropAgent archives agent id, the agents it replaced and its running reviewers, and forgets its
// saved picker request, if any.
func (f *Factory) dropAgent(id string) error {
	g := f.gate(id)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := f.setLabels(id, clearHeldApproval(f.agent(id), map[string]string{labelFinished: "1", labelStuck: "", labelAutoReview: ""})); err != nil {
		return err
	}
	if err := f.endReviews(g, id); err != nil {
		return err
	}

	if err := f.archiveAgent(id); err != nil {
		return err
	}
	byID := f.byID()
	f.dropLineage(byID, id)
	for _, r := range f.agents() {
		if r.L(labelRole) == "reviewer" && sameCard(byID, r.L(labelReviewOf), id) && r.Status == "running" && !r.archived() {
			if err := f.archiveAgent(r.ID); err != nil {
				f.logf("reviewer %s of dropped %s: %v", short(r.ID), short(id), err)
			}
		}
	}
	if f.agent(id).L(labelRouting) != "" {
		return f.routes.del(id)
	}
	return nil
}

// cardGate orders a card's follow-ups (Act send), review starts (StartReview) and review verdicts
// (ApplyVerdict) in this process, which do not otherwise wait for each other, and keeps the time of
// its last follow-up: a verdict from a reviewer started before it is retired, and no review starts
// until the builder has acted on a delivered one, even when the Tick doing it read its agents before
// the follow-up's labels were written. After a restart the labels alone hold.
type cardGate struct {
	mu        sync.Mutex
	followUp  time.Time // a follow-up began: earlier reviewers are retired
	delivered time.Time // that follow-up reached the builder, and its labels queue the next step
	finished  bool      // Zach finished the card: no review starts or applies until he reopens it
}

// reopen lets card id's reviews run again after Zach finished it: his explicit review, re-run or
// move out of Done. Done's stamp is no message the builder must answer first, so it goes too; reviewers
// started before Done stay retired (followUp). His message reopens the card too (delivered).
func (f *Factory) reopen(id string) {
	g := f.gate(id)
	g.mu.Lock()
	if g.finished {
		g.finished, g.delivered = false, time.Time{}
	}
	g.mu.Unlock()
}

func (f *Factory) gate(id string) *cardGate {
	g, _ := f.cardGates.LoadOrStore(id, &cardGate{})
	return g.(*cardGate)
}

// delivered records that card id's follow-up reached its builder; the caller holds the card's gate.
func (f *Factory) delivered(id string, sub bool) {
	if !sub {
		g := f.gate(id)
		g.delivered, g.finished = g.followUp, false
	}
}

// endReviews ends card id's review generation when Zach finishes the card, for a caller holding its gate
// g: no review queued or started before now starts or applies (ApplyVerdict, startReview), and its
// unapplied code and plan reviewers are retired. Only his next message starts a new generation.
func (f *Factory) endReviews(g *cardGate, id string) error {
	g.followUp = f.Now()
	g.delivered, g.finished = g.followUp, true
	return f.retireReviews(id)
}

// retireReviews retires card id's unapplied code and plan reviewers, like supersedeReviewers, before
// a message gives the card new work: their late verdicts must not pass it. A landing review is left
// alone: it answers bin/land, which checks the commit it lands itself.
func (f *Factory) retireReviews(id string) error {
	byID := f.byID()
	for _, r := range f.agents() {
		if r.L(labelRole) != "reviewer" || !sameCard(byID, r.L(labelReviewOf), id) || r.L(labelApplied) != "" ||
			r.L(labelReviewKind) == "land" {
			continue
		}
		labels := retireLabels("superseded", f.Now())
		if err := f.setLabels(r.ID, labels); err != nil {
			return err
		}
		if r.archived() {
			continue
		}
		r = withRetireLabels(r, labels)
		if err := f.archiveRetired(r); err != nil {
			f.logf("reviewer %s superseded but not archived (the reap tries again): %v", short(r.ID), err)
		}
	}
	return nil
}

// planPending reports whether a board card's plan has not passed review or Zach yet. Its rework-cap
// or escalation cards then mean "approve the plan", and Zach's replies go back to plan review.
// A card in Done (say, a direct-track errand) has no plan pending.
func planPending(a Agent) bool {
	v, st := a.L(labelVerdict), a.L(labelStage)
	return a.L(labelSource) == "board" && (st == "plan" || st == "plan-review" || st == "zach") && a.L(labelZachPlan) != "approved" && a.L(labelCodeRounds) == "" &&
		!strings.HasPrefix(v, "plan approve") && !strings.HasPrefix(v, "plan nits") && !strings.HasPrefix(v, "plan fixes") && !strings.HasPrefix(v, "code")
}

// errandPass reports whether card a is doing a one-pass errand: on its first pass (in plan, with no
// plan written or reviewed yet) or marked direct with no plan review since. Zach's reply keeps it in
// plan, so its TRACK: direct ending (or an inbox card's missing TIER line) still finishes it there.
func errandPass(a Agent) bool {
	if a.L(labelPlanRounds) != "" {
		return false
	}
	if a.L(labelTrack) == "direct" {
		return true
	}
	return a.L(labelStage) == "plan" && a.L(labelCodeRounds) == "" && a.L(labelVerdict) == ""
}

// labelCodeRoundsFrom is the code round count when Zach's latest follow-up started a new code review
// generation: the rework cap counts only the rounds after it, so his new request gets its own bounded
// review while automatic rework inside a generation still stops at the cap.
const labelCodeRoundsFrom = "code-rounds-from"

// newCodeGeneration is the labels that start card a's new code review generation after Zach's
// follow-up: the cap counts from its current round, the code arbiter may rule again and reviewer
// replacements start fresh. Round numbers, plan state and approvals are kept.
func newCodeGeneration(a Agent) map[string]string {
	labels := map[string]string{labelCodeRoundsFrom: a.L(labelCodeRounds), labelReviewerRetries: ""}
	if arbitrated(a, "code") {
		var keep []string
		for _, k := range strings.Fields(a.L(labelArbitrated)) {
			if k != "code" {
				keep = append(keep, k)
			}
		}
		labels[labelArbitrated] = strings.Join(keep, " ")
	}
	return labels
}

// replyLabels places a factory card after Zach replies to it, so its next move is queued once the
// agent answers: a landing card stays in Landing (the landing check runs when it stops), a plan that
// has not passed goes back to plan review, an errand stays in plan, anything else gets a code
// review. A guarded (public) repo has no direct track, so its errands get a code review too. Nothing
// is left in Building with no move queued.
// answerLabels is where Zach's message (No, Change or an answer) moves card c: a plan he was asked to
// allow goes back to planning, anything else to the step replyLabels picks. The board preview
// simulates the same move.
func answerLabels(c Card, a Agent, now string, guarded bool) map[string]string {
	if c.Awaiting == "plan" {
		return map[string]string{labelStage: "plan", labelAwaiting: "", labelPlanFix: "", labelSince: now,
			labelNote: "revising plan for Zach", labelDecided: "1"}
	}
	labels := replyLabels(a, now, guarded)
	if a.L(labelStage) == "zach" {
		labels[labelDecided] = "1" // his answer to a stop: later reviews treat it as settled
	}
	return labels
}

func replyLabels(a Agent, now string, guarded bool) map[string]string {
	switch {
	case a.L(labelRouting) != "":
		// a repo picker answers with a new REPO line, which the Tick routes again
		return map[string]string{labelStage: "plan", labelSince: now, labelAwaiting: "", labelNote: "picking a repo"}
	case a.L(labelStage) == "landing" || landingReply(a):
		return map[string]string{labelStage: "landing", labelSince: now, labelAutoReview: ""}
	case planPending(a):
		return map[string]string{labelStage: "plan", labelSince: now, labelNote: "revising plan for Zach"}
	case !guarded && errandPass(a):
		return map[string]string{labelStage: "plan", labelSince: now, labelAutoReview: "", labelNote: "answering Zach"}
	}
	return map[string]string{labelStage: "building", labelSince: now, labelAutoReview: "code"}
}

// rerun restarts the step a stuck card left: its column when it is marked stuck there (factory.stuck),
// else the column it left for Needs Zach (factory.stuck-from). Its automatic retries start afresh.
func (f *Factory) rerun(c Card) error {
	if interruptedArchive(f.agent(c.ID)) {
		return f.resumeArchived(c, "Re-run the interrupted step. Check the current worktree and complete this step before its review.", nil)
	}

	a := f.agent(c.ID)
	if a.L(labelRouting) != "" {
		return f.routeFromReply(a) // a picker's step is reading its REPO line, not a plan review
	}
	from := a.L(labelStuckFrom)
	if a.L(labelStuck) != "" {
		from = a.L(labelStage)
	}
	if from == "" {
		return errors.New("this card is not stuck")
	}
	if err := f.updateRecovery(a, func(r *recovery) { r.Nudges, r.NudgeStage = 0, "" },
		map[string]string{labelStuck: "", labelReviewerRetries: ""}); err != nil {
		return err
	}
	return f.rerunStep(c, from)
}

// rerunStep re-runs the step of column from for card c: its review, or its landing.
func (f *Factory) rerunStep(c Card, from string) error {
	switch from {
	case "landing":
		if hold := f.reviewHold(f.agent(c.ID), c.Cwd); hold != nil {
			hold = clearHeldApproval(f.agent(c.ID), hold)
			if hold[labelStage] == "zach" {
				return errors.Join(errors.New(hold[labelNote]), f.setLabels(c.ID, hold))
			}
			return f.setLabels(c.ID, hold)
		}
		guarded := f.guarded(c.Repo)
		if guarded {
			if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
				return errors.Join(errors.New(note), f.setLabels(c.ID, map[string]string{labelNote: note}))
			}
		}
		next := map[string]string{labelStage: "landing", labelSince: iso(f.Now()), labelNote: "landing re-run"}
		if err := f.sendLanding(labeled(f.agent(c.ID), next), rerunLandMsg(guarded)); err != nil {
			return err
		}
		return f.setLabels(c.ID, next)
	case "plan", "plan-review", "building", "review":
		kind := "code"
		if from == "plan" || from == "plan-review" {
			kind = "plan"
		}
		if err := f.supersedeReviewers(c.ID); err != nil {
			return err
		}
		if c.Status == "running" {
			return f.setLabels(c.ID, map[string]string{labelStage: from, labelAutoReview: kind, labelSince: iso(f.Now())})
		}
		_, err := f.StartReview(c, kind)
		return err
	}
	return errors.New("this card is not stuck")
}

// fromEmail reports whether a new-card request came from an email or voice-memo thread.
func fromEmail(b Body) bool {
	return strings.TrimSpace(b.Thread) != "" || b.Origin == "email" || b.Origin == "voice"
}

// newCard starts a planning agent in a fresh worktree of the chosen repo, or, for repo "auto", a
// picker that chooses the repo first (route.go).
func (f *Factory) newCard(b Body) (map[string]string, error) {
	req := strings.TrimSpace(b.Request)
	if req == "" {
		return nil, errors.New("request is empty")
	}
	repo := b.Repo
	if repo == "" {
		repo = f.cfg.DefaultRepo
	}
	if repo == "" {
		return nil, errors.New("no repo given, and no default repo (FACTORY_DEFAULT_REPO) is set")
	}
	title := b.Title
	if title == "" {
		title = strings.SplitN(req, "\n", 2)[0]
	}
	title = truncWords(strings.TrimSpace(title), 70)
	agentID := b.Agent
	if agentID == "" && fromEmail(b) {
		agentID = defaultAgent
	}
	agent, err := f.resolveAgent(agentID)
	if err != nil {
		return nil, err
	}
	agent, err = agent.withThinking(b.Thinking)
	if err != nil {
		return nil, err
	}
	source, extra, err := inboxIntake(b)
	if err != nil {
		return nil, err
	}
	if repo == autoRepo {
		if source != "board" {
			return nil, errors.New("an email or voice card names its repo")
		}
		r := Route{Request: req, Title: title, Titled: b.Title != "", Thread: strings.TrimSpace(b.Thread), Origin: b.Origin, Agent: agent.ID, Thinking: agent.Thinking}
		if tierRank(b.Tier) >= 0 {
			r.Tier = b.Tier
		}
		return f.startPicker(r)
	}
	repoDir := f.repoDir(repo)
	args := append(agent.runArgs(), "--new-workspace", "worktree", "--cwd", repoDir, "--title", title,
		"--label", labelArg(labelSource, source), "--label", labelArg(labelStage, "plan"), "--label", labelArg(labelArchiveWatch, "1"), "--label", labelArg(labelSince, iso(f.Now())),
		"--label", labelArg(labelAgent, agent.ID))
	args = append(args, extra...)
	if branch := f.freeBranch(repoDir, branchSlug(title)); branch != "" {
		args = append(args, "--new-branch", branch)
	}
	if b.Origin == "idea" {
		args = append(args, "--label", labelArg(labelOrigin, "idea"))
	}
	if tierRank(b.Tier) >= 0 {
		args = append(args, "--label", labelArg(labelTier, b.Tier))
	}
	if b.Title != "" {
		// the caller named the card (an idea, the email dispatcher): the planner does not rename it
		args = append(args, "--label", labelArg(labelTitled, "given"))
	}
	prompt := fmt.Sprintf(strings.ReplaceAll(newCardPrompt, "<errands>", f.errandsName()), req, repo, fellowPeersRule(f.cfg.Port, f.Peers("", repo).Text(f.cfg.Location)),
		tierRules, wholeChangeRule, questionRule)
	prompt += ideaRule(f.cfg.Port, "fellow <first 7 characters of $PASEO_AGENT_ID>", "")
	prompt += f.delegateRule(false)
	if f.guarded(repo) {
		prompt += publicCardNote
	}
	if t := strings.TrimSpace(b.Thread); t != "" {
		args = append(args, "--label", gmailThreadLabel+"="+t)
		prompt += newCardEmailNote(t)
	}
	if in := strings.TrimSpace(b.Inbox); in != "" {
		prompt += "\n" + in + "\n"
	}
	args = append(args, "--background", "--quiet", prompt)
	out, err := f.paseo(args...)
	if err != nil {
		return nil, err
	}
	id := lastLine(out)
	f.logf("new card %s: %s", short(id), title)
	f.poke()
	return map[string]string{"id": id}, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	n := strings.TrimLeft(trunc(unsafeName.ReplaceAllString(filepath.Base(name), "_"), 80), ".")
	if n == "" {
		return "file"
	}
	return n
}

// chat sends Zach's message. Every attachment is saved under ~/.paseo/factory-uploads/<id8>/ and
// named in the message; images under 5 MB also go inline (paseo send --image).
func (f *Factory) chat(id, msg string, atts []Attachment) ([]string, error) {
	var files, images []string
	for _, a := range atts {
		data, err := base64.StdEncoding.DecodeString(a.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", a.Name, err)
		}
		if len(data) > 25<<20 {
			return nil, fmt.Errorf("%s is over 25 MB", a.Name)
		}
		dir := f.uploadDir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, f.Now().Format("20060102-150405")+"-"+safeName(a.Name))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		files = append(files, path)
		if strings.HasPrefix(a.Type, "image/") && len(data) < 5<<20 {
			images = append(images, path)
		}
	}
	if len(files) > 0 {
		msg += "\n\nAttached files (saved on " + f.cfg.Host + "):\n- " + strings.Join(files, "\n- ")
	}
	return files, f.send(id, msg, images...)
}

// landingReply resumes verification or completion reporting without queuing a code review.
func landingReply(a Agent) bool {
	note := a.L(labelNote)
	return note == "landed; did not say what remains"
}
