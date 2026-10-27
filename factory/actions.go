package factory

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	}
	if strings.HasPrefix(b.ID, "idea-") {
		switch action {
		case "approve":
			return f.startIdea(b.ID, "zach", b.Agent, b.Message, b.Thinking)
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
	now := iso(f.Now())
	switch action {
	case "stage":
		if !validStage(b.Stage) {
			return nil, errors.New("bad stage")
		}
		if c.Parked != "" && b.Stage != "landed" && b.Stage != "zach" {
			// archived: no move would come for it in a working column
			return nil, errors.New("this card is parked: reply to it to wake it first")
		}
		if b.Stage == "landed" && c.ArchivedAt == "" { // archiveAgent drops a parked card's park
			if err := f.archiveAgent(c.ID); err != nil {
				return nil, err
			}
		}
		return ok, f.setLabels(c.ID, map[string]string{labelStage: b.Stage, labelSince: now})
	case "review":
		kind := b.Kind
		if kind != "plan" {
			kind = "code"
		}
		if c.Status == "running" {
			return map[string]bool{"queued": true}, f.setLabels(c.ID, map[string]string{labelAutoReview: kind, labelSince: now})
		}
		_, err := f.StartReview(c, kind)
		return ok, err
	case "resume":
		return ok, f.resumeLimit(f.agent(c.ID), "Resume the interrupted step. Check the current worktree before continuing.", nil)
	case "send":
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
		if a := f.agent(c.ID); a.L(labelNote) == limitNote {
			return ok, f.resumeLimit(a, via+msg, b.Attachments)
		}
		if c.Awaiting == "plan" {
			// Zach's answer to an architecture plan: revise it, then plan review runs again
			msg = "Zach read your plan and wants changes:\n\n" + msg + "\n\nRevise the plan, then restate the " +
				"whole revised plan (not just the changes) and stop. " + overviewReminder + " Do not build yet."
			files, err := f.chat(c.ID, msg, b.Attachments)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "files": files}, f.setLabels(c.ID, map[string]string{
				labelStage: "plan", labelAwaiting: "", labelPlanFix: "", labelSince: now, labelNote: "revising plan for Zach"})
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
		if a := f.agent(c.ID); !sub && c.ArchivedAt == "" && a.L(labelStage) != "" {
			if err := f.setLabels(c.ID, replyLabels(a, now, f.guarded(c.Repo))); err != nil {
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
			if err := f.send(c.ID, buildMsg("Zach approved your plan."+answers, notes)); err != nil {
				return nil, err
			}
			return ok, f.setLabels(c.ID, map[string]string{labelStage: "building", labelSince: now, labelAutoReview: "code",
				labelAwaiting: "", labelZachPlan: "approved", labelNote: "plan approved by Zach"})
		}
		msg := landMsg
		if a, ok := f.agentOK(c.ID); ok {
			if id, st := f.pendingDep(a); st == depWaiting {
				// its plan lands it after a card still in flight: the next Tick sends the land prompt (tickHeld).
				// A dropped or missing one is what Zach just answered, so that lands now.
				return ok, f.setLabels(c.ID, map[string]string{labelStage: "landing", labelSince: now, labelHeld: id,
					labelNote: holdNote(id)})
			}
			msg = f.landMsgFor(a)
		}
		if f.guarded(c.Repo) {
			// scan again: the card may have committed since its review
			if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
				return nil, errors.Join(errors.New(note), f.setLabels(c.ID, map[string]string{labelNote: note}))
			}
		}
		if err := f.sendLanding(c.ID, "Zach approved this on the factory board."+answers+" "+msg); err != nil {
			return nil, err
		}
		return ok, f.setLabels(c.ID, map[string]string{labelStage: "landing", labelSince: now, labelNote: "approved by Zach"})
	case "recommend":
		answers := ""
		if m := strings.TrimSpace(b.Message); m != "" {
			answers = "\n\nZach's answers:\n" + m + "\n\n"
		}
		// Zach accepts the builder's brief (rework cap or escalation): apply it, then code review as usual
		if planPending(f.agent(c.ID)) {
			if err := f.send(c.ID, "Zach accepts the recommendations in your brief."+answers+" Revise the plan to match them, then "+
				"build it as revised. "+noLiveChangesRule+" "+worktreeRule+" Commit on your branch, do NOT land, and stop with a short summary (including the deploy steps as built, if any). "+overviewReminder+" A code reviewer "+
				"checks it next, and the factory lands it once the review passes."); err != nil {
				return nil, err
			}
			return ok, f.setLabels(c.ID, map[string]string{labelStage: "building", labelSince: now, labelAutoReview: "code",
				labelAwaiting: "", labelZachPlan: "approved", labelDecided: "1", labelNote: "building with Zach's choices"})
		}
		if err := f.send(c.ID, "Zach accepts the recommendations in your brief."+answers+" Apply them, commit on your branch, do NOT "+
			"land, and stop with a short summary (including the deploy steps as built, if any). "+noLiveChangesRule+" "+worktreeRule+" "+overviewReminder+" A code reviewer checks it next, and the factory lands it once the "+
			"review passes."); err != nil {
			return nil, err
		}
		return ok, f.setLabels(c.ID, map[string]string{labelStage: "building", labelSince: now, labelAutoReview: "code",
			labelDecided: "1", labelNote: "applying Zach's choices"})
	case "rerun":
		return ok, f.rerun(c)
	case "done":
		if err := f.setLabels(c.ID, map[string]string{labelStage: "landed", labelNote: "marked done by Zach"}); err != nil {
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
		if err := f.setLabels(c.ID, map[string]string{labelNote: note}); err != nil {
			return nil, err
		}
		return ok, f.dropAgent(c.ID)
	case "archive":
		return ok, f.dropAgent(c.ID)
	case "stop":
		_, err := f.paseo("stop", c.ID)
		return ok, err
	}
	return nil, errors.New("unknown action")
}

var ok = map[string]bool{"ok": true}

// dropAgent archives agent id and its running reviewers, and forgets its saved picker request, if any.
func (f *Factory) dropAgent(id string) error {
	if err := f.archiveAgent(id); err != nil {
		return err
	}
	for _, r := range f.agents() {
		if r.L(labelRole) == "reviewer" && r.L(labelReviewOf) == id && r.Status == "running" && !r.archived() {
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

// replyLabels places a factory card after Zach replies to it, so its next move is queued once the
// agent answers: a landing card stays in Landing (the landing check runs when it stops), a plan that
// has not passed goes back to plan review, an errand stays in plan, anything else gets a code
// review. A guarded (public) repo has no direct track, so its errands get a code review too. Nothing
// is left in Building with no move queued.
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

// rerun restarts the step a stuck card left (factory.stuck-from): its review, or its landing.
func (f *Factory) rerun(c Card) error {
	if a := f.agent(c.ID); a.L(labelRouting) != "" {
		return f.routeFromReply(a) // a picker's step is reading its REPO line, not a plan review
	}
	switch from := f.agent(c.ID).L(labelStuckFrom); from {
	case "landing":
		guarded := f.guarded(c.Repo)
		if guarded {
			if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
				return errors.Join(errors.New(note), f.setLabels(c.ID, map[string]string{labelNote: note}))
			}
		}
		if err := f.sendLanding(c.ID, rerunLandMsg(guarded)); err != nil {
			return err
		}
		return f.setLabels(c.ID, map[string]string{labelStage: "landing", labelSince: iso(f.Now()), labelNote: "landing re-run by Zach"})
	case "plan", "plan-review", "building", "review":
		kind := "code"
		if from == "plan" || from == "plan-review" {
			kind = "plan"
		}
		if err := f.supersedeReviewers(c.ID); err != nil {
			return err
		}
		if c.Status == "running" {
			labels := map[string]string{labelStage: from, labelAutoReview: kind, labelSince: iso(f.Now())}
			if f.agent(c.ID).L(labelNoVerdict) != "" {
				labels[labelNoVerdict] = ""
			}
			return f.setLabels(c.ID, labels)
		}
		if f.agent(c.ID).L(labelNoVerdict) != "" {
			if err := f.setLabels(c.ID, map[string]string{labelNoVerdict: ""}); err != nil {
				return err
			}
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
	if repo == autoRepo {
		r := Route{Request: req, Title: title, Titled: b.Title != "", Thread: strings.TrimSpace(b.Thread), Origin: b.Origin, Agent: agent.ID, Thinking: agent.Thinking}
		if tierRank(b.Tier) >= 0 {
			r.Tier = b.Tier
		}
		return f.startPicker(r)
	}
	repoDir := filepath.Join(f.cfg.Home, "dev", repo)
	args := append(agent.runArgs(), "--new-workspace", "worktree", "--cwd", repoDir, "--title", title,
		"--label", labelArg(labelSource, "board"), "--label", labelArg(labelStage, "plan"), "--label", labelArg(labelSince, iso(f.Now())),
		"--label", labelArg(labelAgent, agent.ID))
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
		tierRules, wholeAppRule, questionRule)
	prompt += ideaRule(f.cfg.Port, "fellow <first 7 characters of $PASEO_AGENT_ID>", "")
	prompt += f.delegateRule(false)
	if f.guarded(repo) {
		prompt += publicCardNote
	}
	if t := strings.TrimSpace(b.Thread); t != "" {
		args = append(args, "--label", gmailThreadLabel+"="+t)
		prompt += newCardEmailNote(t)
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
	return note == "landing not verified" || note == "landed; did not say what remains"
}
