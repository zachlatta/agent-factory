package factory

import (
	"regexp"
	"strings"
)

// A card lands only part of its work when its landing agent says something is left: the landing
// message asks for a REMAINING line. A card with no line, or with something left that needs Zach, waits
// in Needs Zach; one whose NEEDS FROM ZACH says nothing goes to Done, and what is left goes to an agent
// as a follow-up idea.

// remainingRule ends every landing message. It stays on one line: the log fallback (lastMessage)
// skips the whole [User] line, so the rule's own example is never read as the agent's answer.
// It lists only this card's own unfinished work: anything else held finished cards in Needs Zach.
const remainingRule = " End that summary with a line that says what is left of this card's own request: " +
	"REMAINING: none only when everything this card was asked to do is landed and live; otherwise a line " +
	"REMAINING: followed by one - line per part of it not done (a commit not pushed, a check of this change " +
	"not run, a setting this change needs that is not set). Do not list work another card owns, a problem " +
	"this change did not cause (an expired login, a test failing for reasons unrelated to it) or follow-ups: " +
	"say those in the summary, and suggest any worth doing as an idea. Run every check of this change you " +
	"can before you write that line. Start each - line with who must do it: Zach: only for a decision, " +
	"approval, login or action only he can take, else agent:. Your NEEDS FROM ZACH names every Zach: item " +
	"and says nothing only when there is none: a card that needs nothing from Zach goes to Done, and its " +
	"agent: items go to an agent as a follow-up."

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

// Notes on a landed card: noteWorkLeft waits in Needs Zach (ask.go and mail.go read it), noteForAgents
// is a Done card's.
const (
	noteWorkLeft  = "landed; still to do: "
	noteForAgents = "landed; left for agents: "
)

// landedNext is where a verified landing goes from its agent's last reply: the stage, its note, and the
// items to file as an agent's follow-up (only when it goes to Done with something left).
func landedNext(reply string) (stage, note string, followUp []string) {
	items, found := ParseRemaining(reply)
	switch {
	case !found:
		return "zach", "landed; did not say what remains", nil
	case len(items) == 0:
		return "landed", "landed", nil
	}
	if !needsNothing(reply) {
		return "zach", trunc(strings.ReplaceAll(noteWorkLeft+strings.Join(items, "; "), ",", ""), 200), nil
	}
	for i, it := range items {
		items[i] = remainingAgentRe.ReplaceAllString(it, "")
	}
	return "landed", trunc(strings.ReplaceAll(noteForAgents+strings.Join(items, "; "), ",", ""), 200), items
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

// rerunLandMsg is the landing message a re-run sends (actions.go rerun).
func rerunLandMsg(guarded bool) string {
	msg := landMsg
	if guarded {
		msg = guardedLandMsg
	}
	return "The factory is re-running your landing. " + msg + remainingRule
}

// landed moves a card whose landing is verified on from its agent's last reply (landedNext). One that
// needs nothing from Zach goes to Done, and what is left is filed as an idea for an agent.
func (f *Factory) landed(a Agent, c Card, reply string) error {
	stage, note, followUp := landedNext(reply)
	labels := map[string]string{labelStage: stage, labelNote: note, labelLandedVerified: "1"}
	switch {
	case stage == "zach":
		f.logf("%s landed with work left: %s", short(a.ID), note)
		labels[labelRemainingChecked] = "1"
	case len(followUp) > 0:
		f.logf("%s landed, needs nothing from Zach: %s", short(a.ID), note)
		f.fileFollowUp(a, c, followUp)
	default:
		f.logf("%s finished landing", short(a.ID))
	}
	return f.setLabels(a.ID, labels)
}

// landedFollowUpPrefix starts a follow-up idea's title; a card started from one files no further follow-up.
const landedFollowUpPrefix = "Follow up on landed "

// fileFollowUp files what a landed card left for agents as one idea in its repo, at its tier.
func (f *Factory) fileFollowUp(a Agent, c Card, items []string) {
	title := c.Title
	if title == "" {
		title = a.Title
	}
	if strings.HasPrefix(title, landedFollowUpPrefix) {
		f.logf("%s is a follow-up: no further follow-up filed", short(a.ID))
		return
	}
	repo := c.Repo
	if repo == "" {
		repo = a.Repo
	}
	req := "Card " + short(a.ID) + " (\"" + title + "\") landed and needs nothing from Zach. Finish what its " +
		"landing agent said is left, or say why it cannot be done:\n- " + strings.Join(items, "\n- ")
	_, err := f.addIdea(Body{Repo: repo, Tier: a.L(labelTier), By: "factory (landed " + short(a.ID) + ")",
		Title: landedFollowUpPrefix + short(a.ID) + ": " + title, Request: req,
		Summary:  "Card " + short(a.ID) + " landed with work left that needs no decision from Zach.",
		Why:      "Its landing reply said NEEDS FROM ZACH: nothing, and listed what is left.",
		DoneWhen: "Each item is done or shown not needed, and the card's landed change is unharmed."})
	if err != nil {
		f.logf("%s follow-up idea refused: %v", short(a.ID), err)
	}
}

// landedWaiting reports a landed card waiting in Needs Zach with work left whose reply was not read for
// NEEDS FROM ZACH: cards that landed before that rule, read once.
func landedWaiting(a Agent) bool {
	return a.L(labelStage) == "zach" && a.L(labelLandedVerified) == "1" && a.L(labelRemainingChecked) == "" &&
		strings.HasPrefix(a.L(labelNote), noteWorkLeft)
}

// recheckLanded reads a landedWaiting card's last reply from its session file (never Paseo: the card
// may be parked) and moves it to Done if it needs nothing from Zach; otherwise it is marked read.
func (f *Factory) recheckLanded(a Agent) error {
	reply, err := f.storedReply(a)
	if err != nil {
		f.logf("%s landed reply not read: %v", short(a.ID), err)
		return f.setLabels(a.ID, map[string]string{labelRemainingChecked: "1"})
	}
	stage, note, followUp := landedNext(reply)
	if stage != "landed" || len(followUp) == 0 {
		return f.setLabels(a.ID, map[string]string{labelRemainingChecked: "1"})
	}
	c, _ := f.card(a.ID)
	f.logf("%s landed earlier and needs nothing from Zach, to Done: %s", short(a.ID), note)
	f.fileFollowUp(a, c, followUp)
	labels := map[string]string{labelStage: "landed", labelNote: note, labelRemainingChecked: "1"}
	if a.parked() {
		labels[labelParked] = "" // Done drops the park, as the board's Done does
	}
	return f.setLabels(a.ID, labels)
}
