package factory

import (
	"regexp"
	"strings"
)

// Required actions after landing keep their action choices. Optional tries close through landedNext.

var (
	// an item or sentence that asks Zach to try the change
	tryVerb = regexp.MustCompile(`(?i)^(?:please\s+)?(?:try|test|tap|click|press|see|watch|look)\b`)
	// a first sentence that says nothing is needed: "None.", "Nothing for you to decide yet."
	saysNothing = regexp.MustCompile(`(?i)^[\s*_]*(?:none|nothing|n/?a|no action|no decision)\b[^.!?\n]*[.!?]*\s*`)
)

// askAction is a required post-landing action.
const askAction = "action"

// kindLabels is each kind's label, for an ask that sets none of its own.
var kindLabels = map[string]string{
	askReady:    "Ready to land",
	askDecision: "Decision needed",
	askQuestion: "The agent asked you",
	askProblem:  "Something needs a look",
	askAction:   "Work landed. Do this:",
}

// workLeftAsk is the ask for a landed card with work left: an action ask for the Zach: items, else the
// agents' items, which their agent already stopped with (send it back, or mark it done).
func workLeftAsk(left string) *Ask {
	var zach, agents []string
	for _, it := range strings.Split(left, "; ") {
		if it = strings.TrimSpace(it); it == "" {
			continue
		}
		if remainingZachRe.MatchString(it) {
			zach = append(zach, strings.TrimSpace(remainingZachRe.ReplaceAllString(it, "")))
		} else {
			agents = append(agents, strings.TrimSpace(remainingAgentRe.ReplaceAllString(it, "")))
		}
	}
	if len(zach) > 0 {
		ask := &Ask{From: "builder", landed: true, zachItem: strings.Join(zach, "; ")}
		makeAction(ask, ask.zachItem)
		ask.Why = "The agent landed its change and needs you to do this: " + ask.zachItem + "."
		if len(agents) > 0 {
			ask.Why += " Also left for an agent: " + strings.Join(agents, "; ") + "."
		}
		return ask
	}
	rest := strings.Join(agents, "; ")
	ask := &Ask{Label: "Work landed. Part is left:", landed: true, From: "builder",
		Title:   "The agent stopped with work left",
		Why:     "It landed part of this card and stopped with this left: " + rest + ". Send it back to finish it, or mark the card done.",
		Actions: []Action{primary("finish", "Send it back to finish it"), act("done", "Mark done"), act("close", "Drop it")}}
	ask.Choices = []Choice{
		{ID: "finish", Label: "Send it back to finish it", Primary: true, action: "send", says: "Finish what is left of this card: " + rest + "."},
		{ID: "done", Label: "Mark done", action: "done"},
		{ID: "drop", Label: "Drop it", action: "close"},
	}
	return ask
}

// makeAction gives required actions their completion and reply choices.
func makeAction(ask *Ask, item string) {
	ask.Kind = askAction
	works, broken, skip := "Done it, mark done", "Something's wrong", "Skip it"
	ask.Label, ask.Title = "Work landed. Do this:", "Do this: "+trunc(item, 140)
	ask.Actions = []Action{primary("done", works), reply(broken, "What went wrong?"), act("done", skip)}
	ask.Choices = []Choice{
		{ID: "works", Label: works, Primary: true, action: "done"},
		{ID: "broken", Label: broken, Text: true, Hint: "What went wrong? The agent gets it.", action: "send"},
		{ID: "skip", Label: skip, action: "done"},
	}
}

// refineAsk applies what the agent's reply says (its overview and questions, read after askFor) to
// ask: the headline never says "None", and a
// decision with QUESTION blocks says so. Run it wherever an ask meets its reply (board, email, choose).
func refineAsk(ask *Ask) {
	if ask == nil {
		return
	}
	cleanOverview(ask)
	if ov := ask.Overview; ask.Kind == askAction && ov != nil && ov.Needs != "" {
		ask.Title = "Do it, then mark it done"
	}
	if (ask.Kind == askDecision || ask.Kind == askQuestion) && len(ask.Questions) > 0 &&
		(ask.Label == kindLabels[askDecision] || ask.Label == kindLabels[askQuestion]) {
		ask.Label = "Answer the agent's questions"
	}
}

// cleanOverview drops a DECISION or NEEDS FROM ZACH line that says nothing is needed, on a card that is
// asking for something. The rest of a "None. …" decision moves to the overview unless the overview already
// contains it; an empty NEEDS falls back to the item left for Zach. It copies the overview: the cached one stays as the agent wrote it.
func cleanOverview(ask *Ask) {
	if ask.Overview == nil {
		return
	}
	ov := *ask.Overview
	if loc := saysNothing.FindStringIndex(ov.Decision); loc != nil {
		if rest := strings.TrimSpace(ov.Decision[loc[1]:]); rest != "" && !overviewContains(ov.Overview, rest) {
			ov.Overview = strings.TrimSpace(rest + " " + ov.Overview)
		}
		ov.Decision = ""
	}
	if loc := saysNothing.FindStringIndex(ov.Needs); loc != nil {
		ov.Needs = strings.TrimSpace(ov.Needs[loc[1]:])
	}
	if ov.Needs == "" {
		ov.Needs = ask.zachItem
	}
	ask.Overview = &ov
}

func overviewContains(overview, rest string) bool {
	stem := strings.TrimSpace(strings.TrimRight(rest, ".!?"))
	return stem != "" && strings.Contains(strings.ToLower(overview), strings.ToLower(stem))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// replyAsk is askFor with the agent's reply read now and applied (refineAsk), so an answer meets the
// same buttons the board and the email show.
func (f *Factory) replyAsk(c Card, a Agent) *Ask {
	ask := askFor(c, a)
	if ask == nil {
		return nil
	}
	var qs []Question
	_, qs, ask.Overview = f.askDetail(c, ask)
	ask.Questions = askQuestions(ask, qs)
	refineAsk(ask)
	return ask
}
