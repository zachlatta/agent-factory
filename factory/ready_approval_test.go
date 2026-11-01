package factory

import "testing"

func TestReadyWholeApproval(t *testing.T) {
	ask := readyAsk("")
	for _, word := range append(append([]string{}, approveWords...), landWords...) {
		for _, suffix := range []string{"", "!", ".\n\n-- \nsignature", "\n\n> quoted history", "\nSent from my phone"} {
			text := word + suffix
			if got := parseAnswer(text, ask); got.Choice != "land" || got.Message != "" {
				t.Errorf("parseAnswer(%q) = %+v", text, got)
			}
			if !exactAnswer(text, ask) {
				t.Errorf("exactAnswer(%q) = false", text)
			}
		}
	}
	for _, text := range []string{"yes but change the color", "okay\nI cannot see it", "yes?", "approve the other work", "1A"} {
		if got := parseAnswer(text, ask); got.Choice != "change" || got.Message != text {
			t.Errorf("feedback %q = %+v", text, got)
		}
		if exactAnswer(text, ask) {
			t.Errorf("feedback %q treated as exact approval", text)
		}
	}
}
