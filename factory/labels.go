package factory

import "strings"

// labelPrefix namespaces the factory's labels on Paseo agents (factory.<key>).
const labelPrefix = "factory."

// Factory label keys, without labelPrefix.
const (
	labelStage      = "stage"
	labelSince      = "since"
	labelAutoReview = "auto-review"
	labelTier       = "tier"
	labelAwaiting   = "awaiting"
	labelZachPlan   = "zach-plan"
	labelVerdict    = "verdict"
	labelNote       = "note"
	labelPlanRounds = "plan-rounds"
	labelCodeRounds = "code-rounds"
	labelApplied    = "applied"
	labelRole       = "role"
	labelReviewOf   = "review-of"
	labelReviewKind = "review-kind"
	labelOrigin     = "origin"
	labelSource     = "source"
	labelAgent      = "agent"        // the AgentChoice id that plans and builds the card
	labelBase       = "base"         // the worktree's HEAD when the card was first seen: its own commits come after
	labelStuckFrom  = "stuck-from"   // the column a stuck card left for Needs Zach
	labelTold       = "told"         // the Needs Zach note already emailed to Zach (mail.go)
	labelZachThread = "zach-thread"  // cards with no gmail-thread: the zrl-bot thread the factory emails Zach in
	labelAnswered   = "answered"     // the last email reply from Zach applied to the card (answer.go)
	labelWriting    = "writing"      // when the builder was asked to write Zach a summary: his email waits for it
	labelRouting    = "routing"      // a repo picker: its card starts in the repo it names (route.go)
	labelRouted     = "routed"       // a picker's new card id ("starting" while it is being started)
	labelCandidates = "candidates"   // repos a picker could not choose between, space-separated
	labelDecided    = "zach-decided" // Zach ruled on the open review points (recommend): later reviews treat them as settled
	labelReviewed   = "reviewed"     // the HEAD the last code review saw: landing must put exactly that on main
	labelDirtyAsks  = "dirty-asks"   // times the builder was asked to commit before code review
	labelLandReview = "land-review"  // agents outside the card flow: last landing review, <verdict>@<sha>
	labelSeen       = "seen"         // unix seconds Zach last opened the card on the board or its transcript
	labelTitled     = "titled"       // the card's title is settled: given (by the caller), planner (renamed) or kept
	labelParked     = "parked"       // archived by the idle reap with its column kept; a send resumes it (reap.go)
	labelWaitsOn    = "waits-on"     // short id of the card this one lands after, from its approved plan (deps.go)
	labelWaitsDone  = "waits-landed" // the waits-on card was seen landed: it stays landed after it leaves the list
	labelHeld       = "held"         // in landing: the land prompt waits for this card to land (deps.go)
	labelPlanFix    = "plan-fix"     // small blocking fixes from the final plan-review round: code review checks them
	labelNoVerdict  = "no-verdict"   // the current review round already re-ran once after a reviewer gave no verdict
	labelTrack      = "track"        // "direct": the card did its errand in one pass; Zach's replies keep it on that track
)

// labelLandedVerified records that commits reached main and passed the reviewed-change check.
// It survives follow-up work and closure.
const labelLandedVerified = "landed-verified"

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
