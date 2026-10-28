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
	for name, s := range map[string]string{"landFailMsg": landFailMsg("HEAD not on origin/main"), "landDiffMsg": landDiffMsg("extra commit")} {
		if strings.Contains(s, "bin/land") {
			t.Errorf("%s names bin/land: %q", name, s)
		}
	}
}
