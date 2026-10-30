package factory

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Card dependencies. An approved plan may name one card this card lands after, with a line
// "Waits on: <short id>" (ParseWaitsOn; stored as factory.waits-on when the plan review passes).
// After code review passes, the land prompt is held while that card is in flight (factory.held,
// stage landing) and sent by the next Tick once it lands. If it was dropped, or the factory cannot
// find it, the card is told and goes back to building (rebuildWithoutDep): it decides itself whether
// to rebuild without it or land as is, and either way code review runs again.

// labelHeldApproved marks a held card Zach approved himself (the time). Only such a card is released
// while parked: the release's land prompt is what wakes it, after the freshness and scan gates.
const labelHeldApproved = "held-approved"

// waitsOnRe matches a "Waits on:" line, markdown bullets and bold allowed, naming one short id.
var waitsOnRe = regexp.MustCompile("(?im)^[\\s>*_-]*waits on\\s*[*_]*:[*_\\s]*`?([0-9a-f]{7})\\b")

// ParseWaitsOn returns the short id on the last "Waits on:" line of text, or "" when there is none
// or it names the card self itself.
func ParseWaitsOn(text, self string) string {
	id := lastMatch(waitsOnRe, text)
	if id == short(self) {
		return ""
	}
	return id
}

// The state of a card named in a Waits on line.
const (
	depWaiting = "waiting" // in flight: hold the landing
	depLanded  = "landed"  // Done or verified landing with follow-up work: land
	depDropped = "dropped" // archived without landing (dropped or closed): the card decides
	depMissing = "missing" // not on the board: the card decides
)

// depOf is the state of the card with short id among agents. A Done card is landed even after it is
// archived. A verified landing also stays landed after follow-up work or closure.
func depOf(agents []Agent, id string) string {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	target := resolveCardID(byID, id)
	for _, d := range agents {
		if (d.ID != target && short(d.ID) != id) || replacedPredecessor(byID, d) || d.L(labelRole) != "" {
			continue
		}
		switch {
		case d.L(labelStage) == "landed" || d.L(labelLandedVerified) == "1":
			return depLanded
		case d.gone():
			return depDropped
		}
		return depWaiting
	}
	return depMissing
}

// depState is the state of a's waits-on card id. Seen landed once (factory.waits-landed) stays
// landed, so a card that left the list after landing is not reported missing.
func depState(agents []Agent, a Agent, id string) string {
	if a.L(labelWaitsDone) != "" && a.L(labelWaitsOn) == id {
		return depLanded
	}
	return depOf(agents, id)
}

// pendingDep is a's waits-on card id when it has not landed yet (in flight, dropped or missing), or "".
func (f *Factory) pendingDep(a Agent) (string, string) {
	id := a.L(labelWaitsOn)
	if id == "" {
		return "", ""
	}
	if st := depState(f.agents(), a, id); st != depLanded {
		return id, st
	}
	return "", ""
}

// markDepLanded records that a's waits-on card has landed, once.
func (f *Factory) markDepLanded(a Agent) {
	id := a.L(labelWaitsOn)
	if id == "" || a.L(labelWaitsDone) != "" || a.archived() || depOf(f.agents(), id) != depLanded {
		return
	}
	if err := f.setLabels(a.ID, map[string]string{labelWaitsDone: "1"}); err != nil {
		f.logf("%s: recording that %s landed: %v", short(a.ID), id, err)
	}
}

// holdNote is a held card's note. Label values hold no commas (labelArg).
func holdNote(id string) string { return "waits on " + id + " to land" }

// waitFlag is the board flag for a card with a waits-on line.
func waitFlag(agents []Agent, a Agent) string {
	id := a.L(labelWaitsOn)
	switch depState(agents, a, id) {
	case depLanded:
		return "waits on " + id + " (landed)"
	case depDropped:
		return "waits on " + id + " (dropped)"
	case depMissing:
		return "waits on " + id + " (not found)"
	}
	return "waits on " + id
}

// depHoldMsg tells a held builder about the reviewer's UI feedback, and to wait.
func depHoldMsg(id, fb string) string {
	return "Code review passed. Do not land yet: the approved plan says this card lands after card " + id +
		", and the factory sends you the land prompt once it lands. Do not poll for it, change anything or reply " +
		"at length now. The reviewer tested the UI and left this non-blocking feedback; when you land, end your " +
		"summary with it under 'UI feedback:'.\n\n" + fb
}

// tickHeld moves a held landing card on. It sends the land prompt once its dependency landed, or
// sends the card back to building if the dependency was dropped or is missing.
// A held card is never checked as a landing (or stuck): it has not been told to land.
func (f *Factory) tickHeld(a Agent, c Card) error {
	id := a.L(labelHeld)
	if id == "" {
		id = a.L(labelWaitsOn)
	}
	st := depState(f.agents(), a, id)
	if st == depWaiting {
		return nil
	}
	if live, err := f.heldStopped(a); !live {
		return err
	}
	if st != depLanded {
		return f.rebuildWithoutDep(a, id, st)
	}
	if hold := f.reviewHold(a, c.Cwd); hold != nil {
		// the land prompt is for the reviewed commit only
		if a.archived() && hold[labelStage] != "zach" {
			// a parked card cannot wait archived in a working column for its fresh review: it stops for
			// Zach, whose Re-run starts that review (stuck-from review)
			hold = map[string]string{labelStage: "zach", labelNote: parkedStaleNote, labelStuckFrom: "review",
				labelHeld: "", labelAutoReview: ""}
		}
		return f.setLabels(a.ID, clearHeldApproval(a, hold))
	}
	if f.guarded(c.Repo) {
		// scan again: the card may have committed since its review
		if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
			return f.setLabels(a.ID, clearHeldApproval(a, map[string]string{labelStage: "zach", labelHeld: "", labelNote: note}))
		}
	}
	labels := clearHeldApproval(a, map[string]string{labelHeld: "", labelSince: iso(f.Now()), labelTold: "",
		labelWaitsDone: "1", labelNote: "auto-landing: " + id + " landed"})
	if a.L(labelStage) == "zach" {
		labels[labelStage] = "landing"
	}
	if heldApprovedParked(a) {
		// the git and scan checks above take time: confirm the approval still stands before waking it
		if live, err := f.parkedHoldCurrent(a); !live {
			return err
		}
	}
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	f.logf("%s released: %s landed", short(a.ID), id)
	return f.sendLanding(labeled(a, labels), fmt.Sprintf("Card %s, which this card waits on, has landed: rebase on origin/main first. "+
		"Code review passed (%s tier). %s", id, a.L(labelTier), f.landMsgFor(a)))
}

// parkedStaleNote is the note of a parked held card whose HEAD moved since its review.
const parkedStaleNote = unverifiedReviewNote + "HEAD changed since review"

// heldApprovedParked reports whether a is a parked held card Zach approved: the one archived card the
// factory moves on by itself, since its release's land prompt (or a stop for Zach) is what he asked for.
func heldApprovedParked(a Agent) bool {
	return a.parked() && a.L(labelStage) == "landing" && a.L(labelHeld) != "" && a.L(labelHeldApproved) != ""
}

// heldStopped is stillStopped for a held card, except that a parked one Zach approved may be moved on
// while archived, once parkedHoldCurrent confirms the approval still stands.
func (f *Factory) heldStopped(a Agent) (bool, error) {
	if !heldApprovedParked(a) {
		return f.stillStopped(a)
	}
	return f.parkedHoldCurrent(a)
}

// parkedHoldCurrent re-reads a parked held card's state just before the factory moves it, because a
// Drop, Close, Done or move after the tick's snapshot leaves the card archived and the stale snapshot
// looks the same. It holds only while the current board snapshot, the card's Paseo agent file and
// Paseo itself all still show the same approved, parked, archived hold, and the agent is not running.
// Anything else moves nothing; a failed read is an error, so the next Tick tries again.
func (f *Factory) parkedHoldCurrent(a Agent) (bool, error) {
	same := func(l func(string) string) bool {
		return l(labelParked) != "" && l(labelStage) == "landing" && l(labelHeld) == a.L(labelHeld) &&
			l(labelHeldApproved) != "" && l(labelHeldApproved) == a.L(labelHeldApproved)
	}
	if cur, ok := f.agentOK(a.ID); !ok || !cur.parked() || !same(cur.L) {
		f.logf("%s approved hold changed since the snapshot, no move", short(a.ID))
		return false, nil
	}
	labels, err := f.agentFileLabels(a.ID)
	if err != nil {
		return false, err
	}
	if !same(func(k string) string { return labels[labelPrefix+k] }) {
		f.logf("%s approved hold cancelled since the snapshot, no move", short(a.ID))
		return false, nil
	}
	archived, running, err := f.liveState(a.ID)
	if err != nil {
		return false, err
	}
	if !archived || running {
		// woken (or working) since: the usual stopped-card rules take it from here
		return false, nil
	}
	return true, nil
}

// agentFileLabels reads agent id's labels from its Paseo agent file, which Paseo writes before a
// label update returns: fresher than the board's snapshot.
func (f *Factory) agentFileLabels(id string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(f.cfg.Home, ".paseo/agents", "*", id+".json"))
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("agent file for %s: found %d", short(id), len(files))
	}
	var d struct {
		ID     string
		Labels map[string]string
	}
	if err := readJSON(files[0], &d); err != nil {
		return nil, fmt.Errorf("agent file for %s: %v", short(id), err)
	}
	if d.ID != id {
		return nil, fmt.Errorf("agent file for %s names %s", short(id), short(d.ID))
	}
	return d.Labels, nil
}

// clearHeldApproval adds the clearing of Zach's held approval to labels, when a has one: it covers one
// release only.
func clearHeldApproval(a Agent, labels map[string]string) map[string]string {
	if a.L(labelHeldApproved) != "" {
		labels[labelHeldApproved] = ""
	}
	return labels
}

// heldChain describes the unresolved chain, including its first Needs Zach card.
type heldChain struct {
	decision string
	depth    int
	missing  bool
	cyclic   bool
}

// traceHeldChain follows the held dependency before the planned dependency, just
// as tickHeld does when deciding whether a hold can be released.
func traceHeldChain(agents []Agent, byID map[string]Agent, a Agent) heldChain {
	var chain heldChain
	seen := map[string]bool{a.ID: true}
	for {
		id := a.L(labelHeld)
		if id == "" {
			id = a.L(labelWaitsOn)
		}
		if id == "" {
			return chain
		}
		state := depState(agents, a, id)
		if state == depLanded {
			return chain
		}
		chain.depth++
		if state == depMissing || state == depDropped {
			chain.missing = true
			return chain
		}
		target := resolveCardID(byID, id)
		if seen[target] {
			chain.cyclic = true
			return chain
		}
		seen[target] = true
		a = byID[target]
		if chain.decision == "" && Derive(a) == "zach" {
			chain.decision = target
		}
	}
}

// decorateDecisionWaits derives display metadata only. Dependency release and asks
// continue to use the stored stage and labels.
func decorateDecisionWaits(cards []Card, agents []Agent) {
	byID := map[string]Agent{}
	visible := map[string]int{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	for i := range cards {
		visible[cards[i].ID] = i
		cards[i].DecisionBlocker = ""
		cards[i].DecisionWait = ""
		cards[i].WaitingCount = 0
	}
	for i := range cards {
		a, ok := byID[cards[i].ID]
		if !ok || a.L(labelStage) != "landing" || a.L(labelHeld) == "" || a.gone() || replacedPredecessor(byID, a) {
			continue
		}
		target := traceHeldChain(agents, byID, a).decision
		if j, shown := visible[target]; target != "" && shown && cards[j].Stage == "zach" {
			cards[i].DecisionBlocker = target
			cards[i].DecisionWait = fmt.Sprintf("Waiting for your answer on ‘%s’. This will continue automatically after that work finishes.", cards[j].Title)
			cards[j].WaitingCount++
		}
	}
}
