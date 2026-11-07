package factory

import "strings"

// finishedWorkReason says why card t comes to Zach before it lands, or "" when it lands on its own.
// review is the gate's final message.
func (f *Factory) finishedWorkReason(t Agent, review string) string {
	return strings.Join(f.finishedWorkReasons(t, review), "; ")
}

// finishedWorkReasons lists finishedWorkReason's reasons: the gate's decision, or, when a repair kept the
// behaviour Zach already approved, only what the repair adds.
func (f *Factory) finishedWorkReasons(t Agent, review string) []string {
	r := ParseSignOff(review)
	if reuseBehaviorApproval(t, review) {
		r = repairAlwaysAsk(review)
	}
	switch r {
	case "none":
		return nil
	case "":
		return []string{"the gate did not say whether it needs you"}
	}
	return []string{r}
}
