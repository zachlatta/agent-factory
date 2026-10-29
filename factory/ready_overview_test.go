package factory

import (
	"strings"
	"testing"
)

// The ready prompt puts CHANGED directly after NEEDS; those lines belong to the
// change summary, never the instruction the person must act on.
func TestReadyChangesDoNotBecomeTheNeedsLine(t *testing.T) {
	const needs = "Try it, then land or change."
	const reply = "DECISION: Land the change?\nOVERVIEW: The change is ready.\n" +
		"NEEDS FROM ZACH: " + needs + "\n" +
		"CHANGED: Labels are clearer.\nCHANGED: Actions fit the question.\nCHANGED: The result is easier to read.\n"
	f, p := newTestFactory(t)
	a := card("c1", map[string]string{labelStage: "zach", labelNote: readyPrefix})
	storeLog(t, f, p, a.ID, "[User] finish the change\n"+reply)
	for range 2 { // both the fresh reply and the cached board must keep the summary
		f.SetAgents([]Agent{a})
		c, ok := f.card(a.ID)
		if !ok || c.Ask == nil || c.Ask.Overview == nil || c.Ask.Ready == nil {
			t.Fatalf("missing ready ask: %+v", c.Ask)
		}
		if c.Ask.Overview.Needs != needs {
			t.Errorf("needs = %q, want %q", c.Ask.Overview.Needs, needs)
		}
		if len(c.Ask.Ready.Changed) != 3 || !strings.Contains(c.Ask.Report, "CHANGED: Labels are clearer.") {
			t.Errorf("change summary lost: ready=%+v report=%q", c.Ask.Ready, c.Ask.Report)
		}
	}
}
