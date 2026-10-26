package factory

import (
	"regexp"
	"strings"
)

// Overview is the three lines an agent writes for Zach at the top of a message where it stops: the
// decision, what the work is, and what it needs from him. The board shows it on top of a Needs Zach
// card and the email leads with it; with none, both show the agent's message as before.
type Overview struct {
	Decision string `json:"decision,omitempty"`
	Overview string `json:"overview,omitempty"`
	Needs    string `json:"needs,omitempty"`
}

// overviewRule teaches agents the block ParseOverview reads. Every message where an agent stops carries
// it: the agent cannot tell which of them the factory puts in front of Zach.
const overviewRule = `Start every message where you stop (plan, build or rework summary, review, brief, handoff,
result) with three lines for Zach. His board shows them on top of the card when it waits on him, and
he reads them on his phone, so use plain words:
DECISION: <what Zach decides if this reaches him, one line>
OVERVIEW: <what it changes, why, and the main risk; at most 3 short sentences>
NEEDS FROM ZACH: <approve, choose or answer, and what happens after>
Then the rest of the message. QUESTION: blocks stay as they are, and TIER:, VERDICT: and TRACK:
lines stay last.`

// overviewReminder goes in each factory message that asks an agent for a reply it stops on.
const overviewReminder = "Start that reply with the DECISION:, OVERVIEW: and NEEDS FROM ZACH: lines for Zach."

var overviewKey = regexp.MustCompile(`^\s*\*{0,2}(DECISION|OVERVIEW|NEEDS FROM ZACH|NEEDS):\*{0,2}\s*(.*?)\s*$`)

const overviewCap = 2000 // a safety cap only: an overview is shown whole

// ParseOverview takes the first overview block (2 or more of its keys, upper case, at the start of a
// line) out of text and returns the rest and the overview, or text unchanged and nil. A key's text runs
// on to a blank line or the next key; the first block wins, so a quoted one later in the reply is kept as text.
func ParseOverview(text string) (string, *Overview) {
	lines := strings.Split(text, "\n")
	for i := range lines {
		if !overviewKey.MatchString(lines[i]) {
			continue
		}
		fields := map[string][]string{}
		key, end := "", i
		for j := i; j < len(lines); j++ {
			if m := overviewKey.FindStringSubmatch(lines[j]); m != nil {
				k := m[1]
				if k == "NEEDS FROM ZACH" {
					k = "NEEDS"
				}
				if _, seen := fields[k]; seen {
					break
				}
				key, fields[k], end = k, []string{m[2]}, j+1
				continue
			}
			if strings.TrimSpace(lines[j]) == "" {
				// a blank line ends the block, unless a key not yet given follows it
				n := j
				for n < len(lines) && strings.TrimSpace(lines[n]) == "" {
					n++
				}
				if m := overviewKey.FindStringSubmatch(safeLine(lines, n)); m != nil && !hasKey(fields, m[1]) {
					j = n - 1
					continue
				}
				break
			}
			if questionLine.MatchString(lines[j]) {
				break
			}
			fields[key] = append(fields[key], strings.TrimRight(lines[j], " \t"))
			end = j + 1
		}
		if len(fields) < 2 {
			continue
		}
		get := func(k string) string { return trunc(strings.TrimSpace(strings.Join(fields[k], "\n")), overviewCap) }
		ov := &Overview{Decision: get("DECISION"), Overview: get("OVERVIEW"), Needs: get("NEEDS")}
		rest := append(append([]string{}, lines[:i]...), lines[end:]...)
		return strings.TrimSpace(strings.Join(rest, "\n")), ov
	}
	return strings.TrimSpace(text), nil
}

func safeLine(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

func hasKey(fields map[string][]string, k string) bool {
	if k == "NEEDS FROM ZACH" {
		k = "NEEDS"
	}
	_, ok := fields[k]
	return ok
}

// parseReply splits an agent's reply into the text shown as Detail, its QUESTION: blocks and its overview.
// Questions come out first, so a question right under NEEDS FROM ZACH stays a question.
func parseReply(text string) (string, []Question, *Overview) {
	rest, qs := ParseQuestions(text)
	rest, ov := ParseOverview(rest)
	return rest, qs, ov
}
