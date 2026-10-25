package factory

import (
	"fmt"
	"strings"
	"testing"
)

// Landing instructions for ordinary repos name no landing script: the agent reads how the repo
// lands work. Guarded repos keep their own message (guard_test.go).
func TestLandingTextNamesNoScript(t *testing.T) {
	f := &Factory{}
	texts := map[string]string{
		"landMsg":       landMsg,
		"landMsgFor":    f.landMsgFor(card("c", map[string]string{})),
		"newCardPrompt": fmt.Sprintf(newCardPrompt, "req", "example/desk", "", "", "", ""),
	}
	for name, s := range texts {
		if strings.Contains(s, "bin/land") {
			t.Errorf("%s names bin/land: %q", name, s)
		}
		if !strings.Contains(s, "AGENTS.md") {
			t.Errorf("%s does not point at AGENTS.md", name)
		}
	}
	ask := BuildBoard([]Agent{card("c", map[string]string{labelStage: "zach", labelNote: "landing not verified"})}, nil, t0)[0].Ask
	if ask == nil {
		t.Fatal("no ask for landing not verified")
	}
	for _, a := range ask.Actions {
		if strings.Contains(a.Hint, "bin/land") || strings.Contains(a.Label, "bin/land") {
			t.Errorf("landing-not-verified action names bin/land: %+v", a)
		}
	}
}
