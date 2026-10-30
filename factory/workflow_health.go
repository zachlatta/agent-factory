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
		chain := traceHeldChain(agents, byID, a)
		h.LongestChain = max(h.LongestChain, chain.depth)
		if chain.missing {
			h.MissingDependency++
		}
		if chain.cyclic {
			h.Cyclic++
		}
		if chain.decision != "" {
			h.HeldForInput++
		}
	}
	return h
}
