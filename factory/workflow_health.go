package factory

// WorkflowHealth describes work waiting in the pipeline independently of process
// liveness. Counts exclude finished, dropped and replaced cards, but include parked
// cards. Dependency counts describe held cards, including transitive blockers.
type WorkflowHealth struct {
	Active            int `json:"active"`
	Held              int `json:"held"`
	NeedsInput        int `json:"needsInput"`
	HeldForInput      int `json:"heldForInput"`
	LongestChain      int `json:"longestChain"`      // dependency edges from a held card
	Cyclic            int `json:"cyclic"`            // held cards whose chain contains a cycle
	MissingDependency int `json:"missingDependency"` // held cards with a missing or dropped blocker
}

func workflowHealth(agents []Agent) WorkflowHealth {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	var h WorkflowHealth
	for _, a := range agents {
		if a.L(labelStage) == "" || a.L(labelRole) != "" || a.gone() ||
			a.L(labelStage) == "landed" || replacedPredecessor(byID, a) {
			continue
		}
		h.Active++
		if Derive(a) == "zach" {
			h.NeedsInput++
		}
		if a.L(labelHeld) == "" {
			continue
		}
		h.Held++
		seen := map[string]bool{a.ID: true}
		cur, depth := a, 0
		needsInput := false
		for {
			id := cur.L(labelHeld)
			if id == "" {
				id = cur.L(labelWaitsOn)
			}
			if id == "" {
				break
			}
			state := depState(agents, cur, id)
			if state == depLanded {
				break
			}
			depth++
			h.LongestChain = max(h.LongestChain, depth)
			if state == depMissing || state == depDropped {
				h.MissingDependency++
				break
			}
			target := resolveCardID(byID, id)
			if seen[target] {
				h.Cyclic++
				break
			}
			seen[target] = true
			cur = byID[target]
			if Derive(cur) == "zach" {
				needsInput = true
			}
		}
		if needsInput {
			h.HeldForInput++
		}
	}
	return h
}
