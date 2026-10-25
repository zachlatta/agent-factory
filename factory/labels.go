package factory

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
	labelTold       = "told"         // inbox cards: the Needs Zach note already emailed to Zach
	labelRouting    = "routing"      // a repo picker: its card starts in the repo it names (route.go)
	labelRouted     = "routed"       // a picker's new card id ("starting" while it is being started)
	labelCandidates = "candidates"   // repos a picker could not choose between, comma-separated
	labelDecided    = "zach-decided" // Zach ruled on the open review points (recommend): later reviews treat them as settled
	labelReviewed   = "reviewed"     // the HEAD the last code review saw: landing must put exactly that on main
	labelDirtyAsks  = "dirty-asks"   // times the builder was asked to commit before code review
	labelLandReview = "land-review"  // agents outside the card flow: last landing review, <verdict>@<sha>
)

// labelArg is the `paseo --label` value that sets factory label k to v.
func labelArg(k, v string) string { return labelPrefix + k + "=" + v }

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
