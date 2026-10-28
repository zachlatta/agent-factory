package factory

import "strings"

// labelPrefix namespaces the factory's labels on Paseo agents (factory.<key>).
const labelPrefix = "factory."

// Factory label keys, without labelPrefix.
const (
	labelQuiet      = "quiet" // anchor identity used to repair detached factory roots
	labelStage      = "stage"
	labelSince      = "since"
	labelAutoReview = "auto-review"
	labelTier       = "tier"
	labelAwaiting   = "awaiting"
	labelZachPlan   = "zach-plan" // "approved": Zach allowed a plan whose build has side effects
	labelVerdict    = "verdict"
	labelNote       = "note"
	labelPlanRounds = "plan-rounds"
	labelCodeRounds = "code-rounds"
	labelApplied    = "applied"
	labelRole       = "role"
	labelReviewOf   = "review-of"
	labelReviewKind = "review-kind"
	labelLens       = "lens"      // a code-review panel reviewer's focus: correctness, architecture, ux or arbiter (panel.go)
	labelPanel      = "panel"     // reviewer: the code-review round its panel belongs to; card: that round while its panel is open
	labelLensTier   = "lens-tier" // a panel reviewer's TIER line, for the merged decision
	labelOrigin     = "origin"
	labelSource     = "source"
	labelAgent      = "agent"        // the AgentChoice id that plans and builds the card
	labelBase       = "base"         // the worktree's HEAD when the card was first seen: its own commits come after
	labelStuckFrom  = "stuck-from"   // the column a card paused for Needs Zach left (history unavailable)
	labelTold       = "told"         // the Needs Zach note already emailed to Zach (mail.go)
	labelZachThread = "zach-thread"  // cards with no gmail-thread: the zrl-bot thread the factory emails Zach in
	labelAnswered   = "answered"     // the last email reply from Zach applied to the card (answer.go)
	labelWriting    = "writing"      // when the builder was asked to write Zach a summary: his email waits for it
	labelRouting    = "routing"      // a repo picker: its card starts in the repo it names (route.go)
	labelRouted     = "routed"       // a picker's new card id ("starting" while it is being started)
	labelCandidates = "candidates"   // repos a picker could not choose between, space-separated
	labelDecided    = "zach-decided" // Zach answered or asked for changes on this card: later reviews treat his choices as settled
	labelReviewed   = "reviewed"     // the HEAD the last code review saw: landing must put exactly that on main
	labelDirtyAsks  = "dirty-asks"   // times the builder was asked to commit before code review
	labelLandReview = "land-review"  // agents outside the card flow: last landing review, <verdict>@<sha>
	labelSeen       = "seen"         // unix seconds Zach last opened the card on the board or its transcript
	labelTitled     = "titled"       // the card's title is settled: given (by the caller), planner (renamed) or kept
	labelParked     = "parked"       // archived by the idle reap with its column kept; a send resumes it (reap.go)
	labelWaitsOn    = "waits-on"     // short id of the card this one lands after, from its approved plan (deps.go)
	labelWaitsDone  = "waits-landed" // the waits-on card was seen landed: it stays landed after it leaves the list
	labelHeld       = "held"         // in landing: the land prompt waits for this card to land (deps.go)
	labelReplaces   = "replaces"     // predecessor card agent id replaced by this agent (replace.go)
	labelReplacedBy = "replaced-by"  // replacement card agent id that took over this card (replace.go)
	labelPlanFix    = "plan-fix"     // small blocking fixes from the final plan-review round: code review checks them
	labelTrack      = "track"        // "direct": the card did its errand in one pass; Zach's replies keep it on that track
	labelDelegated  = "delegated"    // an email or voice card got the sub-agent section from the factory (inbox.go)
	labelArbiter    = "arbiter"      // on a reviewer: it is its card's arbiter (arbiter.go)
	labelArbitrated = "arbitrated"   // the gates ("plan", "code"; space-separated) whose arbiter already ruled
	labelAskWhy     = "ask-why"      // why a card whose code review passed comes to Zach before it lands (alwaysask.go)
	labelReady      = "ready"        // Ready for you stops so far: each gets its own note and email (ready.go)
)

// labelCodeOK is the HEAD a completed code review (or arbiter) passed, set where the card moves on
// (passCode) and cleared when any review starts. bin/land's landing review reuses only that approval.
const labelCodeOK = "code-approved"

// Automatic recovery (recover.go).
const (
	labelRecovery        = "recovery"         // error retries, stopped-card steps and landing returns
	labelStuck           = "stuck"            // why a card stays in its column after its recovery ran out
	labelReviewerRetries = "reviewer-retries" // replacement reviewers started in the current review round
)

// legacyLimitLabel held the error recovery state before factory.recovery; it is read once, then cleared.
const legacyLimitLabel = "limit-recovery"

// labelLandedVerified records that commits reached main and passed the reviewed-change check.
// It survives later work on the card and closure.
const labelLandedVerified = "landed-verified"

// labelWorkLeftRounds counts the times a landed card was sent back to finish its own agent work
// (remaining.go). It is carried across handoffs and never reset, so the cap holds.
const labelWorkLeftRounds = "work-left-rounds"

// labelArg is the `paseo --label` value that sets factory label k to v.
// labelArg is a factory label for `paseo --label`. Paseo reads --label as comma-separated k=v pairs,
// so a comma in a value would split it and fail the whole update: commas become semicolons.
func labelArg(k, v string) string { return labelPrefix + k + "=" + strings.ReplaceAll(v, ",", ";") }

// roundsLabel is the label counting review rounds of kind ("plan" or "code").
func roundsLabel(kind string) string {
	switch kind {
	case "plan":
		return labelPlanRounds
	case "code":
		return labelCodeRounds
	}
	return kind + "-rounds"
}

// labelRemainingAsk marks the one completion clarification for the current landing.
const labelRemainingAsk = "remaining-asked"
