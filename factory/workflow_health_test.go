package factory

import "testing"

func TestWorkflowHealthDependencyChains(t *testing.T) {
	root := card("aaa0001", map[string]string{labelStage: "zach"})
	middle := card("bbb0002", map[string]string{labelStage: "landing", labelHeld: "aaa0001", labelWaitsOn: "aaa0001"})
	leaf := card("ccc0003", map[string]string{labelStage: "landing", labelHeld: "bbb0002", labelWaitsOn: "bbb0002"})
	f, _ := newTestFactory(t, root, middle, leaf)
	h := f.Health().Workflow
	if h.Active != 3 || h.Held != 2 || h.NeedsInput != 1 || h.HeldForInput != 2 || h.LongestChain != 2 {
		t.Fatalf("workflow = %+v", h)
	}
	// Completing the blocker clears the transitive human wait immediately.
	root.Labels[labelPrefix+labelStage] = "landed"
	f, _ = newTestFactory(t, root, middle, leaf)
	h = f.Health().Workflow
	if h.Active != 2 || h.NeedsInput != 0 || h.HeldForInput != 0 {
		t.Fatalf("completed blocker: %+v", h)
	}
}

func TestWorkflowHealthCyclesAndMissingDependencies(t *testing.T) {
	a := card("aaa0001", map[string]string{labelStage: "landing", labelHeld: "bbb0002", labelWaitsOn: "bbb0002"})
	b := card("bbb0002", map[string]string{labelStage: "landing", labelHeld: "aaa0001", labelWaitsOn: "aaa0001"})
	c := card("ccc0003", map[string]string{labelStage: "landing", labelHeld: "ddd0004", labelWaitsOn: "ddd0004"})
	f, _ := newTestFactory(t, a, b, c)
	h := f.Health().Workflow
	if h.Cyclic != 2 || h.MissingDependency != 1 || h.Held != 3 || h.HeldForInput != 0 {
		t.Fatalf("workflow = %+v", h)
	}
}

func TestWorkflowHealthContinuesPastInputBlocker(t *testing.T) {
	root := card("aaa0001", map[string]string{labelStage: "building"})
	middle := card("bbb0002", map[string]string{labelStage: "zach", labelHeld: "aaa0001", labelWaitsOn: "aaa0001"})
	leaf := card("ccc0003", map[string]string{labelStage: "landing", labelHeld: "bbb0002", labelWaitsOn: "bbb0002"})
	h := workflowHealth([]Agent{root, middle, leaf})
	if h.HeldForInput != 1 || h.LongestChain != 2 {
		t.Fatalf("workflow = %+v", h)
	}
}

func TestWorkflowHealthParkedReplacedAndFinishedCards(t *testing.T) {
	old := card("aaa0001", map[string]string{labelStage: "zach", labelReplacedBy: "bbb0002"})
	current := card("bbb0002", map[string]string{labelStage: "zach", labelReplaces: "aaa0001", labelParked: "1"})
	current.ArchivedAt = t0
	held := card("ccc0003", map[string]string{labelStage: "landing", labelHeld: "aaa0001", labelWaitsOn: "aaa0001"})
	done := card("ddd0004", map[string]string{labelStage: "landed"})
	dropped := card("eee0005", map[string]string{labelStage: "building"})
	dropped.ArchivedAt = t0
	f, _ := newTestFactory(t, old, current, held, done, dropped, reviewer("r1", "ccc0003", "code"))
	f.noteRefreshed()
	code, health := health(t, f)
	if code != 200 || !health.OK {
		t.Fatalf("a workflow hold must not fail process health: %d %+v", code, health)
	}
	h := health.Workflow
	if h.Active != 2 || h.NeedsInput != 1 || h.Held != 1 || h.HeldForInput != 1 || h.LongestChain != 1 {
		t.Fatalf("workflow = %+v", h)
	}
}
