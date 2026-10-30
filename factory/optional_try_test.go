package factory

import (
	"strings"
	"testing"
)

const optionalTryReport = "DECISION: None.\nOVERVIEW: Live; observation awaits a future event.\nNEEDS FROM ZACH: Tap Mute thread once and reply\n\nREMAINING:\n- Zach: tap Mute thread once and reply"

func TestOptionalLandedTry(t *testing.T) {
	for _, rounds := range []int{0, workLeftCap} {
		stage, note, _ := landedNext(optionalTryReport, rounds)
		if stage != "landed" || !strings.Contains(note, "Try it if you like: tap Mute thread once and reply") {
			t.Errorf("got %s %s", stage, note)
		}
	}
	if !landedNothingRemains(optionalTryReport) {
		t.Error("recovery rejected optional try")
	}
	for _, item := range []string{"run the backfill script on prod", "use the new token to set the secret", "check production and restart the worker", "verify the data and delete old records", "approve the Stripe payout", "log in to X", "run the migration on prod", "try to approve it", "try to sign in", "try to decide", "try to choose", "try to confirm", "try to pay", "try to buy", "try to install", "try to enable", "try to rotate", "try to migrate", "try to deploy"} {
		report := strings.ReplaceAll(optionalTryReport, "tap Mute thread once and reply", item)
		if stage, _, _ := landedNext(report, 0); stage != "zach" {
			t.Errorf("%q: %s", item, stage)
		}
	}
	for _, report := range []string{
		optionalTryReport + "\n- agent: verify the next event",
		optionalTryReport + "\n\nQUESTION: Continue?\n- Yes\n- No",
		strings.Replace(optionalTryReport, "DECISION: None.", "DECISION: Choose a setting.", 1),
	} {
		if stage, _, _ := landedNext(report, 0); stage != "zach" {
			t.Errorf("unsafe completion: %s", report)
		}
	}
}

func TestOptionalTryPromptContract(t *testing.T) {
	if !strings.Contains(remainingRule, "future live event") {
		t.Error("missing optional observation rule")
	}
	if !strings.Contains(overviewRule, "after landing") {
		t.Error("missing post-landing scope")
	}
}

func TestPreLandingTryKeepsApprovalChoices(t *testing.T) {
	ask := everyAsk()["ready for you"]
	if ask == nil {
		t.Fatal("missing ready fixture")
	}
	kind, label := ask.Kind, ask.Label
	ids := strings.Join(choiceIDs(ask), ",")
	ask.Overview = &Overview{Decision: "Try the preview?", Needs: "Tap the button once and reply"}
	refineAsk(ask)
	if ask.Kind != kind || ask.Label != label || strings.Join(choiceIDs(ask), ",") != ids || ask.Overview.Needs == "" {
		t.Fatalf("pre-landing ask changed: %+v", ask)
	}
}
