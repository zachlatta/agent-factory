package factory

import (
	"fmt"
	"regexp"
	"strings"
)

// The arbiter settles a review gate that did not settle: the rework cap or a reviewer's escalation, at
// plan or code review. It is one fresh reviewer agent (role reviewer, labelled factory.arbiter) that reads
// the builder's conversation and every review round and rules, so a disagreement between agents never
// goes to Zach. It runs once per gate per card (factory.arbitrated); a gate that fails again after its
// ruling, or a ruling it cannot give, waits on the board.

// arbiterStuckNote is the Needs Zach note of a card whose gate did not settle even with the arbiter.
const arbiterStuckNote = "reviews did not settle"

// arbiterNote is the card's note while its arbiter works.
const arbiterNote = "an agent is settling the review"

var mustFixRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*MUST[ \t]+FIX[*_]*\s*:[ \t]*(.*)$`)

// ParseMustFix joins the arbiter's MUST FIX lines into one line (";"-separated, no commas, at most
// smallFixCap runes for the label); "" when there are none. Placeholders and "none" are skipped.
func ParseMustFix(text string) string {
	var items []string
	for _, m := range mustFixRe.FindAllStringSubmatch(text, -1) {
		s := strings.Trim(strings.TrimSpace(strings.ReplaceAll(m[1], "`", "")), "*_\"' ")
		s = strings.Join(strings.Fields(s), " ")
		switch strings.ToLower(strings.TrimRight(s, ".;: ")) {
		case "", "none", "no", "n/a", "nothing":
			continue
		}
		if strings.HasPrefix(s, "<") {
			continue
		}
		items = append(items, strings.ReplaceAll(s, ",", ";"))
	}
	return trunc(strings.Join(items, "; "), smallFixCap)
}

// arbitrated reports whether card a's gate of kind already had its arbiter.
func arbitrated(a Agent, kind string) bool {
	for _, k := range strings.Fields(a.L(labelArbitrated)) {
		if k == kind {
			return true
		}
	}
	return false
}

const arbiterPersona = "You are the Arbiter: a DIFFERENT agent instance from the builder and from its reviewers. " +
	"The review of this card did not settle: the reviewer and the builder went the full number of rounds, or the " +
	"reviewer escalated. You decide, so that Zach is not asked to referee agents. Weigh each open point on its merits: " +
	"side with the reviewer where it is right, with the builder where it is right."

// ArbiterPrompt is the prompt for the arbiter of card c's gate kind ("plan" or "code"). last is the review
// that ended the rounds (the builder has not seen it); escalated says the reviewer escalated.
func ArbiterPrompt(c Card, kind string, port int, vis, last string, escalated bool) string {
	cwd := c.Cwd
	if cwd == "" {
		cwd = "."
	}
	why := "The review went the full number of rounds without passing."
	if escalated {
		why = "The reviewer escalated: it says only Zach can decide. Zach is asked before building only about " +
			"real-world side effects (money, messages to other people, production data written, deleted or moved, " +
			"hosts or services changed, hardware), and after code review he sees the finished work if he asked for it " +
			"or it is on the always-ask list. Anything else is yours to decide."
	}
	var ruling string
	if kind == "plan" {
		ruling = `Rule on the PLAN (no code exists yet):
- approve: it is good enough to build as it stands.
- changes: it is good enough to build once the fixes you list are applied. List each as one line
  MUST FIX: <a concrete fix the builder applies while building and a code reviewer can check>
  The builder builds with them and code review checks every one. Use changes only with MUST FIX lines.
- escalate: building it would have a real-world side effect (above) that needs Zach's OK first.
- close: it is a duplicate, already done, or not worth doing.
Then, right before TIER:, one line:
` + sideEffectsRule
	} else {
		ruling = `Rule on the CODE (the diff: ` + "`git -C <cwd> diff origin/main...HEAD`" + `):
- approve: it is ready as it stands.
- changes: it is ready once the fixes you list are applied. List each as one line
  MUST FIX: <a concrete fix a code reviewer can check>
  The builder applies them, then one more code review checks only those. Use changes only with MUST FIX lines.
- escalate: Zach must see it before it lands; say why in one line.
- close: it is a duplicate, already done, or not worth doing.
Then, right before TIER:, one line:
` + alwaysAskRule
	}
	ruling = strings.ReplaceAll(ruling, "<cwd>", cwd)
	return fmt.Sprintf(`%s

Card: "%s" (Paseo agent %s on %s).
Working directory: %s
Repo: %s

%s

Read the builder's whole conversation first (each review round's findings were sent to it as a message):
`+"`paseo logs %s --tail 400`"+`.

The review that ended the rounds (the builder has not seen it):
<<<
%s
>>> End of review

%s

%s

%s

%s

Rules:
- Read-only. Do not edit, commit, push, land, send email, create PDW mutations, or message the builder.
- Keep it short: one line per open point (the reviewer's view, the builder's view, your ruling), at most 8.
- `+visibilityLine(vis)+`
- `+shellRule+`
- `+scratchSafetyRule+`

`+overviewRule+`

End your final message with exactly two lines:
TIER: <low|risky|architecture>
VERDICT: <approve|changes|escalate|close>
`, arbiterPersona, c.Title, c.ID, c.Host, cwd, c.Repo, why, c.ID, trunc(last, 8000), requestSection(c.Request),
		ruling, tierRules, wholeAppRule)
}

// startArbiter starts the arbiter for card t's gate kind and marks the gate as arbitrated.
func (f *Factory) startArbiter(t Agent, kind, last string, escalated bool) error {
	c, ok := f.card(t.ID)
	if !ok {
		c = Card{ID: t.ID, Short: short(t.ID), Title: t.Title, Host: t.Host, Cwd: t.Cwd, Repo: t.Repo,
			WorkspaceID: t.WorkspaceID, Tier: t.L(labelTier)}
	}
	if c.Request == nil {
		r := f.cardRequest(t)
		c.Request = &r
	}
	vis := f.visibility(c.Cwd)
	prompt := strings.ReplaceAll(ArbiterPrompt(c, kind, f.cfg.Port, vis, last, escalated), "<errands>", f.errandsName())
	if f.guarded(c.Repo) {
		prompt += publicReviewRule
	}
	rid, err := f.startReviewer(c, kind, "Arbiter ("+kind+"): "+trunc(c.Title, 60), prompt, labelArbiter)
	if err != nil {
		return err
	}
	stage := "review"
	if kind == "plan" {
		stage = "plan-review"
	}
	done := strings.TrimSpace(t.L(labelArbitrated) + " " + kind)
	if arbitrated(t, kind) {
		done = t.L(labelArbitrated)
	}
	f.logf("started %s arbiter %s for %s", kind, short(rid), short(t.ID))
	return f.setLabels(t.ID, map[string]string{labelStage: stage, labelArbitrated: done, labelNote: arbiterNote,
		labelSince: iso(f.Now()), labelAutoReview: "", labelNoVerdict: ""})
}

// arbiterFixMsg sends the builder the arbiter's code fixes for one more round.
func arbiterFixMsg(notes string) string {
	return "Code review did not settle, so another agent (the arbiter) ruled on it. Apply each MUST FIX item in its " +
		"ruling below, and nothing else; then commit on your branch, leave the worktree clean, do NOT land, and stop " +
		"with a short summary of how you applied each one (including the deploy steps as built, if any). One more code " +
		"review checks them. " + noLiveChangesRule + " " + overviewReminder + "\n\n" + worktreeRule + "\n\nThe arbiter's ruling:\n" + notes
}

// applyArbiter moves card t after its arbiter ruled. labels already holds the round's labels (tier and
// the like); verdict and notes are the arbiter's.
func (f *Factory) applyArbiter(t Agent, kind, verdict, notes, tier, builderPlan, builderReply string, labels map[string]string) error {
	now := iso(f.Now())
	fixes := ParseMustFix(notes)
	ruled := kind + " arbiter " + verdict
	set := func(kv ...string) error {
		for i := 0; i+1 < len(kv); i += 2 {
			labels[kv[i]] = kv[i+1]
		}
		return f.setLabels(t.ID, labels)
	}
	switch {
	case verdict == "close":
		return set(labelStage, "zach", labelVerdict, "close", labelNote, "reviewer suggests closing")
	case kind == "plan" && (verdict == "approve" || verdict == "nits" || verdict == "changes" && fixes != ""):
		passed := "plan approve" // a passed plan's verdict (planPending)
		if fixes != "" {
			passed = "plan fixes"
		}
		return f.passPlan(t, labels, passed, tier, notes, builderPlan, builderReply, fixes,
			"The plan review did not settle, so another agent (the arbiter) ruled on it: build it. Its ruling is in Reviewer notes below.")
	case kind == "plan" && verdict == "escalate":
		return f.sideEffectStop(t, labels, notes)
	case kind == "code" && (verdict == "approve" || verdict == "nits"):
		return f.passCode(t, labels, ruled, tier, notes, "")
	case kind == "code" && verdict == "changes" && fixes != "":
		if err := set(labelStage, "building", labelVerdict, ruled, labelSince, now, labelAutoReview, "code",
			labelNote, "applying the arbiter's fixes", labelPlanFix, fixes); err != nil {
			return err
		}
		return f.send(t.ID, arbiterFixMsg(notes))
	case kind == "code" && verdict == "escalate":
		why := "the agent that settled its review wants you to decide"
		if r := ParseAlwaysAsk(notes); r != "" && r != "none" {
			why += " (" + r + ")"
		}
		return f.passCode(t, labels, ruled, tier, notes, why)
	}
	return set(labelStage, "zach", labelVerdict, ruled, labelNote, arbiterStuckNote, labelStuckFrom, "")
}
