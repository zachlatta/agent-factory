package factory

import (
	"encoding/base64"
	"strings"
)

// Finished-work authorization is separate from a held dependency release and from
// code approval, which must always identify the exact reviewed commit.
func (f *Factory) finishedApproval(a Agent) map[string]string {
	labels := map[string]string{}
	head := strings.TrimSpace(a.L(labelReadyHead))
	if !isFinishedNote(a.L(labelNote)) || head == "" {
		return labels
	}
	summary, _ := f.storedReply(a)
	labels[labelBehaviorApproval] = head
	labels[labelBehaviorSummary] = base64.RawURLEncoding.EncodeToString([]byte(trunc(summary, handoffTextCap)))
	labels[labelBehaviorRepair] = ""
	return labels
}

func reuseBehaviorApproval(a Agent, review string) bool {
	summary, err := base64.RawURLEncoding.DecodeString(a.L(labelBehaviorSummary))
	if err != nil || strings.TrimSpace(string(summary)) == "" {
		return false
	}
	return a.L(labelBehaviorApproval) != "" && a.L(labelBehaviorSummary) != "" && a.L(labelBehaviorRepair) == "1" && ParseApprovedBehavior(review) == "unchanged"
}
