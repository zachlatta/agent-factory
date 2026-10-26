package factory

import (
	"fmt"
	"regexp"
	"strings"
)

// Card dependencies. An approved plan may name one card this card lands after, with a line
// "Waits on: <short id>" (ParseWaitsOn; stored as factory.waits-on when the plan review passes).
// After code review passes, the land prompt is held while that card is in flight (factory.held,
// stage landing) and sent by the next Tick once it lands. If it was dropped, or the factory cannot
// find it, the card goes to Needs Zach with one question; his Approve lands it as reviewed.

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
	depLanded  = "landed"  // Done: land
	depDropped = "dropped" // archived without landing (dropped or closed): ask Zach
	depMissing = "missing" // not on the board: ask Zach
)

// depOf is the state of the card with short id among agents. A Done card is landed even after it is
// archived; any other archived card was dropped or closed.
func depOf(agents []Agent, id string) string {
	for _, d := range agents {
		if short(d.ID) != id || d.L(labelRole) != "" {
			continue
		}
		switch {
		case d.L(labelStage) == "landed":
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

// Notes for held cards. Label values hold no commas (labelArg).
func holdNote(id string) string { return "waits on " + id + " to land" }

const (
	depDroppedNote = ": it was dropped"
	depMissingNote = ": no such card found"
)

func depAskNote(id, state string) string {
	if state == depMissing {
		return "waits on " + id + depMissingNote
	}
	return "waits on " + id + depDroppedNote
}

// depAsk is the one question for a card whose waits-on card was dropped or cannot be found.
func depAsk(note string) *Ask {
	id := strings.TrimPrefix(note, "waits on ")
	id, _, _ = strings.Cut(id, ":")
	title, what := "The card this waits on was dropped: land anyway?",
		"was dropped or closed without landing"
	if strings.HasSuffix(note, depMissingNote) {
		title, what = "Can't find the card this waits on: land anyway?",
			"is not on the board (a wrong id, or it left the board before it landed)"
	}
	return &Ask{Title: title, From: "builder",
		Why: "The approved plan says this card lands after card " + id + ", which " + what + ". The code review " +
			"passed and the landing is held. Land it as reviewed, tell the agent what to do instead, or drop it.",
		Actions: []Action{primary("approve", "Land it anyway"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
}

func isDepAskNote(note string) bool {
	return strings.HasPrefix(note, "waits on ") &&
		(strings.HasSuffix(note, depDroppedNote) || strings.HasSuffix(note, depMissingNote))
}

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

// tickHeld moves a held landing card on: nothing while its waits-on card is in flight, the land
// prompt once it lands, Needs Zach if it was dropped or cannot be found. A held card is never
// checked as a landing (or stuck): it has not been told to land.
func (f *Factory) tickHeld(a Agent, c Card) error {
	id := a.L(labelHeld)
	st := depState(f.agents(), a, id)
	if st == depWaiting {
		return nil
	}
	if live, err := f.stillStopped(a); !live {
		return err
	}
	if st != depLanded {
		f.logf("%s held: %s %s", short(a.ID), id, st)
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelHeld: "", labelNote: depAskNote(id, st)})
	}
	if f.guarded(c.Repo) {
		// scan again: the card may have committed since its review
		if note := f.scanNote(c.Cwd, ""); !scanPassed(note) {
			return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelHeld: "", labelNote: note})
		}
	}
	if err := f.setLabels(a.ID, map[string]string{labelHeld: "", labelSince: iso(f.Now()),
		labelNote: "auto-landing: " + id + " landed"}); err != nil {
		return err
	}
	f.logf("%s released: %s landed", short(a.ID), id)
	return f.send(a.ID, fmt.Sprintf("Card %s, which this card waits on, has landed: rebase on origin/main first. "+
		"Code review passed (%s tier). %s", id, a.L(labelTier), f.landMsgFor(a)))
}
