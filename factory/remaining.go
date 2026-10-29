package factory

import (
	"fmt"
	"regexp"
	"strings"
)

// A card lands only part of its work when its landing agent says something is left: the landing
// message asks for a REMAINING line. A card with no line, or with something left that needs Zach, waits
// in Needs Zach; one whose NEEDS FROM ZACH says nothing goes back to the same card to finish what is left
// (build, code review, land again), at most workLeftCap rounds, then waits in Needs Zach.

// remainingRule ends every landing message. Completion is read from the assistant reply,
// so examples in the user prompt cannot count as the agent's answer.
// It lists only this card's own unfinished work: anything else held finished cards in Needs Zach.
const remainingRule = " End that summary with a line that says what is left of this card's own request: " +
	"REMAINING: none only when everything this card was asked to do is landed and live; otherwise a line " +
	"REMAINING: followed by one - line per part of it not done (a commit not pushed, a check of this change " +
	"not run, a setting this change needs that is not set). Do not list work another card owns, a problem " +
	"this change did not cause (an expired login, a test failing for reasons unrelated to it) or work outside " +
	"the area of the ask: say those in the summary, and suggest any worth doing as an idea. Run every check " +
	"of this change you can before you write that line. Start each - line with who must do it: Zach: only " +
	"for a decision, approval, login or action only he can take, else agent:. Your NEEDS FROM ZACH names " +
	"every Zach: item and says nothing only when there is none: when it needs nothing from Zach, its agent: " +
	"items come back to you in this card (at most 2 rounds): you finish them, a code review runs, and you " +
	"land again."

var (
	remainingRe     = regexp.MustCompile(`(?i)^[ \t>*_#-]*REMAINING[*_]*\s*:[*_]*[ \t]*(.*)$`)
	remainingItemRe = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+(.*\S)\s*$`)
	remainingNoneRe = regexp.MustCompile(`(?i)^[*_]*none[*_.]*$`)
	// an item for Zach or for an agent, as remainingRule asks
	remainingZachRe  = regexp.MustCompile(`(?i)^[*_]*zach[*_]*\s*:`)
	remainingAgentRe = regexp.MustCompile(`(?i)^[*_]*agent[*_]*\s*:[*_]*\s*`)
	// NEEDS FROM ZACH that asks for nothing: it starts with nothing, and the rest names no ask
	needsNothingRe = regexp.MustCompile(`(?i)^(nothing|none|n/?a|no action)\b`)
	needsAskRe     = regexp.MustCompile(`(?i)\b(you|your|zach|please|approve|log ?in|reply|decide|choose|confirm)\b`)
)

// ParseRemaining reads the last REMAINING line of a landing agent's reply: found is false when
// there is none (or it lists nothing); items is empty for "none", else the text on the line and the
// list lines right after it.
func ParseRemaining(text string) (items []string, found bool) {
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if remainingRe.MatchString(l) {
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	first := strings.TrimSpace(remainingRe.FindStringSubmatch(lines[at])[1])
	if remainingNoneRe.MatchString(first) {
		return nil, true
	}
	if first != "" {
		items = append(items, first)
	}
	for _, l := range lines[at+1:] {
		m := remainingItemRe.FindStringSubmatch(l)
		if m == nil {
			break
		}
		items = append(items, m[1])
	}
	return items, len(items) > 0
}

// noteWorkLeft is the note of a landed card waiting in Needs Zach with work left (ask.go and mail.go read it).
const noteWorkLeft = "landed; still to do: "

// workLeftCap is the most rounds a landed card is sent back to finish its own agent work before it
// waits on the board.
const workLeftCap = 2

// landedNext is where a verified landing goes from its agent's last reply, after rounds work-left rounds:
// the stage and its note, and the items to send back to the same card (only for a work-left round).
// A card goes back only when its reply needs nothing from Zach; any ask for him keeps it in Needs Zach.
func landedNext(reply string, rounds int) (stage, note string, again []string) {
	items, found := ParseRemaining(reply)
	switch {
	case !found:
		return "zach", "landed; did not say what remains", nil
	case len(items) == 0:
		return "landed", "landed", nil
	}
	if needsNothing(reply) {
		for i, it := range items {
			items[i] = remainingAgentRe.ReplaceAllString(it, "")
		}
		if rounds < workLeftCap {
			return "building", fmt.Sprintf("finishing work left (round %d of %d)", rounds+1, workLeftCap), items
		}
	}
	return "zach", trunc(strings.ReplaceAll(noteWorkLeft+strings.Join(items, "; "), ",", ""), 200), nil
}

// needsNothing reports whether reply asks nothing of Zach: its NEEDS FROM ZACH says nothing and names no
// ask, it has no QUESTION block, and no REMAINING item is his. A reply with no overview asks.
func needsNothing(reply string) bool {
	rest, qs := ParseQuestions(reply)
	if len(qs) > 0 {
		return false
	}
	_, ov := ParseOverview(rest)
	if ov == nil {
		return false
	}
	needs := strings.TrimSpace(strings.Trim(ov.Needs, "*_ \t"))
	m := needsNothingRe.FindStringIndex(needs)
	if m == nil || needsAskRe.MatchString(needs[m[1]:]) {
		return false
	}
	items, _ := ParseRemaining(reply)
	for _, it := range items {
		if remainingZachRe.MatchString(it) {
			return false
		}
	}
	return true
}

// landedNothingRemains reports whether reply says nothing remains (REMAINING: none), asks no
// QUESTION block, and when an overview block or NEEDS FROM ZACH line is present, asks nothing of
// Zach (needsNothing).
func landedNothingRemains(reply string) bool {
	stage, _, _ := landedNext(reply, 0)
	if stage != "landed" {
		return false
	}
	rest, qs := ParseQuestions(reply)
	if len(qs) > 0 {
		return false
	}
	if _, ov := ParseOverview(rest); ov != nil {
		return needsNothing(reply)
	}
	for _, l := range strings.Split(rest, "\n") {
		if m := overviewKey.FindStringSubmatch(l); m != nil && (m[1] == "NEEDS FROM ZACH" || m[1] == "NEEDS") {
			return needsNothing("DECISION: None.\n" + l + "\n" + rest)
		}
	}
	return true
}

// rerunLandMsg is the landing message a re-run sends (actions.go rerun).
func rerunLandMsg(guarded bool) string {
	msg := landMsg
	if guarded {
		msg = guardedLandMsg
	}
	return "The factory is re-running your landing. " + msg + remainingRule
}

// workLeftMsg sends a landed card back to finish the agent work its REMAINING list named. It stays on
// one line, like the landing message, so a log fallback never reads the rule as the agent's answer.
func workLeftMsg(items []string, round int) string {
	return fmt.Sprintf("Your landing is verified, but this card is not done: your REMAINING list has work for "+
		"an agent, and it belongs to this card (round %d of %d). Do it now in this worktree: %s. Commit any "+
		"change on your branch and stop with a short summary; do not land: a code review runs on new commits, "+
		"then the factory tells you to land again. If an item turns out to need Zach, mark it Zach: and ask "+
		"in NEEDS FROM ZACH.", round, workLeftCap, strings.Join(items, "; ")) + remainingRule
}

// landed moves a card whose landing is verified on from its agent's last reply (landedNext). Agent work
// left goes back to the same card, at most workLeftCap rounds; then, or with anything for Zach, the card
// waits in Needs Zach.
func (f *Factory) landed(a Agent, c Card, reply string) error {
	return f.landedError(a, c, reply, "")
}

// landedError moves a card whose landing is verified on from its agent's last reply, sharing the
// label builder with landed. When errNote is non-empty and the card moves to Done (stage "landed"),
// it keeps the error in factory.note and completes error recovery together in one update.
func (f *Factory) landedError(a Agent, c Card, reply, errNote string) error {
	rounds := atoi(a.L(labelWorkLeftRounds))
	stage, note, again := landedNext(reply, rounds)
	labels := map[string]string{labelStage: stage, labelNote: note, labelLandedVerified: "1",
		labelAutoReview: "", labelRemainingAsk: "", labelStuck: ""}
	if _, found := ParseRemaining(reply); !found {
		labels[labelRemainingAsk] = a.L(labelRemainingAsk)
	}
	if errNote != "" && stage == "landed" {
		note = trunc(strings.ReplaceAll(note+"; "+errNote, ",", ";"), 200)
		labels[labelNote] = note
		f.logf("%s finished landing before error: %s", short(a.ID), note)
		return f.updateRecovery(a, func(r *recovery) {
			r.Phase, r.UserAt = "complete", a.LastUserMessageAt
			r.FromReset, r.ResetUsed, r.Timed, r.Manual = false, false, 0, false
		}, labels)
	}
	switch {
	case len(again) > 0:
		f.logf("%s landed with agent work left, sent back: %s", short(a.ID), strings.Join(again, "; "))
		labels[labelAutoReview], labels[labelSince] = "code", iso(f.Now())
		labels[labelWorkLeftRounds] = fmt.Sprint(rounds + 1)
		if err := f.setLabels(a.ID, labels); err != nil {
			return err
		}
		return f.advance(labeled(a, labels), "work left after landing", workLeftMsg(again, rounds+1))
	case stage == "zach":
		f.logf("%s landed with work left: %s", short(a.ID), note)
	default:
		f.logf("%s finished landing", short(a.ID))
	}
	return f.setLabels(a.ID, labels)
}

// sendLanding starts a new landing attempt with a fresh completion clarification allowance.
func (f *Factory) sendLanding(a Agent, msg string) error {
	if err := f.setLabels(a.ID, map[string]string{labelRemainingAsk: ""}); err != nil {
		return err
	}
	return f.advance(labeled(a, map[string]string{labelRemainingAsk: ""}), "landing", msg)
}

// finishLanding asks once for an omitted completion line before involving a person.
func (f *Factory) finishLanding(a Agent, c Card, reply string) error {
	if _, found := ParseRemaining(reply); !found && a.L(labelRemainingAsk) == "" {
		if err := f.setLabels(a.ID, map[string]string{labelStage: "landing", labelAutoReview: "", labelSince: iso(f.Now()),
			labelLandedVerified: "1", labelRemainingAsk: "1", labelNote: "asking what remains"}); err != nil {
			return err
		}
		return f.send(a.ID, "Your landing was verified, but your reply omitted the completion line. Report what remains of this card's request."+remainingRule)
	}
	return f.landed(a, c, reply)
}
