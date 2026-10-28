package factory

import (
	"maps"
	"slices"
	"strings"
)

const maxReplaceHops = 16

// carriedFactoryLabels lists the factory.* labels copied at handoff (copyCarriedLabels, except
// labelTold which is stage-bound). Clearable labels (held, plan-fix, waits-on, waits-done,
// reviewed, landed-verified, decided) are copied once at handoff and never brought back by
// applyLineageLabels after the replacement clears them; non-clearable lineage labels
// (chainCarriedFactoryLabels) and any explicit empty value on a newer agent are read through the chain.
var carriedFactoryLabels = []string{
	labelZachThread, labelTold, labelAnswered, labelSource, labelOrigin, labelTitled,
	labelTier, labelTrack, labelBase, labelPlanRounds, labelCodeRounds, labelZachPlan,
	labelDecided, labelPlanFix, labelReviewed, labelLandedVerified, labelWaitsOn, labelWaitsDone,
	labelHeld, labelDelegated, labelArbitrated,
}

var chainCarriedFactoryLabels = []string{
	labelZachThread, labelSource, labelOrigin, labelTitled,
	labelTier, labelTrack, labelBase, labelPlanRounds, labelCodeRounds, labelZachPlan,
	labelDelegated, labelArbitrated,
}

var carriedRawLabels = []string{gmailThreadLabel}

func lookupAgentByPrefix(byID map[string]Agent, id string) (Agent, bool) {
	if id == "" {
		return Agent{}, false
	}
	if a, ok := byID[id]; ok {
		return a, true
	}
	for k, a := range byID {
		if strings.HasPrefix(k, id) {
			return a, true
		}
	}
	return Agent{}, false
}

// resolveCardID resolves a card or prefix id to the active replacement agent's full ID by
// following factory.replaced-by links and reverse factory.replaces links (bounded to maxReplaceHops).
func resolveCardID(byID map[string]Agent, id string) string {
	cur, ok := lookupAgentByPrefix(byID, id)
	if !ok {
		return id
	}
	seen := map[string]bool{cur.ID: true}
	for range maxReplaceHops {
		nextID := cur.L(labelReplacedBy)
		if nextID == "" {
			for _, a := range byID {
				if rep := a.L(labelReplaces); rep != "" && (rep == cur.ID || strings.HasPrefix(cur.ID, rep)) {
					nextID = a.ID
					break
				}
			}
		}
		next, ok := lookupAgentByPrefix(byID, nextID)
		if !ok || seen[next.ID] {
			break
		}
		seen[next.ID] = true
		cur = next
	}
	return cur.ID
}

// cardLineage returns the ordered slice of Fellow agents (oldest to newest) for id.
func cardLineage(byID map[string]Agent, id string) []Agent {
	tail, ok := lookupAgentByPrefix(byID, resolveCardID(byID, id))
	if !ok {
		return nil
	}
	rev := []Agent{tail}
	seen := map[string]bool{tail.ID: true}
	cur := tail
	for range maxReplaceHops {
		prevID := cur.L(labelReplaces)
		if prevID == "" {
			for _, a := range byID {
				if rb := a.L(labelReplacedBy); rb != "" && (rb == cur.ID || strings.HasPrefix(cur.ID, rb)) {
					prevID = a.ID
					break
				}
			}
		}
		prev, ok := lookupAgentByPrefix(byID, prevID)
		if !ok || seen[prev.ID] {
			break
		}
		seen[prev.ID] = true
		rev = append(rev, prev)
		cur = prev
	}
	slices.Reverse(rev)
	return rev
}

func sameCard(byID map[string]Agent, aID, bID string) bool {
	if aID == "" || bID == "" {
		return false
	}
	ra, rb := resolveCardID(byID, aID), resolveCardID(byID, bID)
	return ra == rb || strings.HasPrefix(ra, rb) || strings.HasPrefix(rb, ra)
}

func replacedPredecessor(byID map[string]Agent, a Agent) bool {
	return a.ID != "" && resolveCardID(byID, a.ID) != a.ID
}

func lineagePredecessorIDs(byID map[string]Agent, id string) []string {
	lin := cardLineage(byID, id)
	if len(lin) <= 1 {
		return nil
	}
	out := make([]string, 0, len(lin)-1)
	for _, a := range lin[:len(lin)-1] {
		out = append(out, a.ID)
	}
	return out
}

func copyCarriedLabels(old Agent) map[string]string {
	out := map[string]string{}
	for _, k := range carriedFactoryLabels {
		if k != labelTold && old.L(k) != "" {
			out[labelPrefix+k] = old.L(k)
		}
	}
	for _, k := range carriedRawLabels {
		if v := old.Labels[k]; v != "" {
			out[k] = v
		}
	}
	return out
}

func applyLineageLabels(agents []Agent) []Agent {
	if !slices.ContainsFunc(agents, func(a Agent) bool {
		return a.L(labelReplaces) != "" || a.L(labelReplacedBy) != ""
	}) {
		return agents
	}
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	out := slices.Clone(agents)
	for i := range out {
		a := &out[i]
		lin := cardLineage(byID, a.ID)
		if len(lin) <= 1 || lin[len(lin)-1].ID != a.ID {
			continue
		}
		var cloned bool
		put := func(rawKey, val string) {
			if !cloned {
				a.Labels = maps.Clone(a.Labels)
				if a.Labels == nil {
					a.Labels = map[string]string{}
				}
				cloned = true
			}
			a.Labels[rawKey] = val
		}
		inherit := func(rawKey string) {
			if _, exists := a.Labels[rawKey]; exists {
				return
			}
			for j := len(lin) - 2; j >= 0; j-- {
				if v, exists := lin[j].Labels[rawKey]; exists {
					if v != "" {
						put(rawKey, v)
					}
					return
				}
			}
		}
		for _, k := range chainCarriedFactoryLabels {
			inherit(labelPrefix + k)
		}
		for _, k := range carriedRawLabels {
			inherit(k)
		}
		var answered []string
		for _, x := range lin {
			for _, tok := range strings.Fields(x.L(labelAnswered)) {
				if !slices.Contains(answered, tok) {
					answered = append(answered, tok)
				}
			}
		}
		if joined := strings.Join(answered, " "); joined != "" && a.L(labelAnswered) != joined {
			put(labelPrefix+labelAnswered, joined)
		}
		byID[a.ID] = *a
	}
	return out
}

func lineageRaw(byID map[string]Agent, a Agent, rawKey string) string {
	if v, exists := a.Labels[rawKey]; exists {
		return v
	}
	lin := cardLineage(byID, a.ID)
	for i := len(lin) - 1; i >= 0; i-- {
		if v, exists := lin[i].Labels[rawKey]; exists {
			return v
		}
	}
	return ""
}

func lineageLabel(byID map[string]Agent, a Agent, key string) string {
	return lineageRaw(byID, a, labelPrefix+key)
}

func lineageEmailThread(byID map[string]Agent, a Agent) string {
	return strings.TrimSpace(lineageRaw(byID, a, gmailThreadLabel))
}

func lineageHasThread(byID map[string]Agent, a Agent, thread string) bool {
	return thread != "" && (emailThread(a) == thread || a.L(labelZachThread) == thread ||
		slices.ContainsFunc(cardLineage(byID, a.ID), func(x Agent) bool {
			return emailThread(x) == thread || x.L(labelZachThread) == thread
		}))
}

func lineageHasAnswered(byID map[string]Agent, a Agent, msgID string) bool {
	return msgID != "" && (slices.Contains(strings.Fields(a.L(labelAnswered)), msgID) ||
		slices.ContainsFunc(cardLineage(byID, a.ID), func(x Agent) bool {
			return slices.Contains(strings.Fields(x.L(labelAnswered)), msgID)
		}))
}

func lineageTold(byID map[string]Agent, a Agent) string { return lineageLabel(byID, a, labelTold) }

func (f *Factory) byID() map[string]Agent {
	agents := f.agents()
	m := make(map[string]Agent, len(agents))
	for _, a := range agents {
		m[a.ID] = a
	}
	return m
}

func (f *Factory) cardAgentOK(id string) (Agent, bool) {
	if id == "" {
		return Agent{}, false
	}
	return f.agentOK(resolveCardID(f.byID(), id))
}
