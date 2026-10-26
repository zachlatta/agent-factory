package factory

import (
	"regexp"
	"strings"
)

// A card lands only part of its work when its landing agent says something is left: the landing
// message asks for a REMAINING line, and a card with anything left, or no line, waits in Needs Zach
// instead of going to Done.

// remainingRule ends every landing message. It stays on one line: the log fallback (lastMessage)
// skips the whole [User] line, so the rule's own example is never read as the agent's answer.
const remainingRule = " End that summary with a line that says what is left: REMAINING: none only when " +
	"everything this card was asked to do is landed and live; otherwise a line REMAINING: followed by one " +
	"- line per thing not done (a commit not pushed, a check not run, a setting someone still has to change)."

var (
	remainingRe     = regexp.MustCompile(`(?i)^[ \t>*_#-]*REMAINING[*_]*\s*:[*_]*[ \t]*(.*)$`)
	remainingItemRe = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+(.*\S)\s*$`)
	remainingNoneRe = regexp.MustCompile(`(?i)^[*_]*none[*_.]*$`)
)

// ParseRemaining reads the last REMAINING line of a landing agent's reply: found is false when
// there is none (or it lists nothing); items is empty for "none", else the text on the line and the
// list lines right after it.
func ParseRemaining(text string) (items []string, found bool) {
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if remainingRe.MatchString(l) {
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	first := strings.TrimSpace(remainingRe.FindStringSubmatch(lines[at])[1])
	if remainingNoneRe.MatchString(first) {
		return nil, true
	}
	if first != "" {
		items = append(items, first)
	}
	for _, l := range lines[at+1:] {
		m := remainingItemRe.FindStringSubmatch(l)
		if m == nil {
			break
		}
		items = append(items, m[1])
	}
	return items, len(items) > 0
}

// remainingNote is the Needs Zach note for a landed card from its agent's last reply, or "" when
// nothing is left. Commas are dropped: Paseo splits label values on them.
func remainingNote(reply string) string {
	items, found := ParseRemaining(reply)
	switch {
	case !found:
		return "landed; did not say what remains"
	case len(items) == 0:
		return ""
	}
	note := "landed; still to do: " + strings.Join(items, "; ")
	return trunc(strings.ReplaceAll(note, ",", ""), 200)
}

// rerunLandMsg is the landing message a re-run sends (actions.go rerun).
func rerunLandMsg(guarded bool) string {
	msg := landMsg
	if guarded {
		msg = guardedLandMsg
	}
	return "The factory is re-running your landing. " + msg + remainingRule
}
