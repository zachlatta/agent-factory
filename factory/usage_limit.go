package factory

import (
	"regexp"
	"strings"
	"time"
)

// Provider reset times. A card agent's error with a known reset resumes on the same model once,
// limitMargin after it (recover.go); other errors take the timed retries.
const limitMargin = 30 * time.Second

var relativeLimit = regexp.MustCompile(`(?i)\bresets in ([0-9]+(?:h|m|s)(?:[0-9]+(?:h|m|s))*)(?:\.|$)`)
var absoluteLimit = regexp.MustCompile(`(?i)\bresets at ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+(?:Z|[+-][0-9]{2}:[0-9]{2}))`)

func limitReset(message string, observed time.Time) (time.Time, bool) {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "individual quota reached") {
		if m := relativeLimit.FindStringSubmatch(message); m != nil {
			if d, err := time.ParseDuration(m[1]); err == nil && d > 0 {
				return observed.Add(d), true
			}
		}
	}
	if strings.Contains(lower, "api error 429") && strings.Contains(lower, "quota") {
		if m := absoluteLimit.FindStringSubmatch(message); m != nil {
			t, err := time.Parse(time.RFC3339Nano, m[1])
			return t, err == nil
		}
	}
	return time.Time{}, false
}
