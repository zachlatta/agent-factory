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
		wantNeeds := "Reply yes or land to approve, ask for a change, or drop it. " + readyApprovalText
		if c.Ask.Overview.Needs != wantNeeds {
			t.Errorf("needs = %q, want %q", c.Ask.Overview.Needs, wantNeeds)
		}
		if len(c.Ask.Ready.Changed) != 3 || !strings.Contains(c.Ask.Report, "CHANGED: Labels are clearer.") {
			t.Errorf("change summary lost: ready=%+v report=%q", c.Ask.Ready, c.Ask.Report)
		}
	}
}

func TestReadyPreviewRefreshPreservesBuilderNeeds(t *testing.T) {
	const needs = "Try the preview, then reply land."
	f, _ := newTestFactory(t)
	cached := &Overview{Decision: "Use clearer buttons?", Needs: needs}
	for _, state := range []string{"", previewStarting, previewReady, previewFailed, previewStarting, previewReady} {
		if state != "" {
			preview := Preview{Card: "c1", State: state}
			if state == previewReady {
				preview.URL = "https://preview.example/"
			}
			if err := f.previews.put(preview); err != nil {
				t.Fatal(err)
			}
		}
		// Each board refresh creates an ask using the same cached overview.
		ask := readyAsk("")
		ask.Overview = cached
		f.fillReady(ask, "c1", "")
		wantTitle, wantNeeds := readyTitle, needs
		if state == "" || state == previewFailed {
			wantTitle = "Use clearer buttons?"
			wantNeeds = "Reply yes or land to approve, ask for a change, or drop it. " + readyApprovalText
		}
		if ask.Title != wantTitle || ask.Overview.Needs != wantNeeds {
			t.Errorf("state %q: title=%q needs=%q; want title=%q needs=%q", state, ask.Title, ask.Overview.Needs, wantTitle, wantNeeds)
		}
		if cached.Needs != needs {
			t.Errorf("state %q: cached needs changed to %q", state, cached.Needs)
		}
	}
}
