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

// landedBeforeError checks whether an errored card already pushed its reviewed work to origin/main
// and wrote a final report saying nothing remains. When verified, it moves the card to Done and
// keeps the error in factory.note instead of retrying, handing off, or marking the card stuck.
func (f *Factory) landedBeforeError(a Agent, c Card) (bool, error) {
	cwd := c.Cwd
	if cwd == "" {
		cwd = a.Cwd
	}
	if cwd == "" || a.L(labelHeld) != "" || a.L(labelVerdict) == "code changes" {
		return false, nil
	}
	repo := c.Repo
	if repo == "" {
		repo = a.Repo
	}
	reviewed := strings.TrimSpace(a.L(labelReviewed))
	if reviewed == "" && (f.guarded(repo) || (a.L(labelStage) != "landing" && readRecovery(a).Stage != "landing" && a.L(labelLandedVerified) != "1")) {
		return false, nil
	}
	if f.landReviewPending(a) || f.landRunning(a) {
		return false, nil
	}
	reply, err := replyBeforeError(f.cfg.Home, a)
	if err != nil || !landedNothingRemains(reply) {
		return false, nil
	}
	if f.alreadyLandChecked(a) {
		return false, nil
	}
	if live, err := f.stillStopped(a); !live {
		return true, err
	}
	if status, _ := f.checkLanding(a, cwd); status != landVerified {
		f.noteLandChecked(a)
		return false, nil
	}
	if live, err := f.stillStopped(a); !live {
		return true, err
	}
	f.recordLanded(a, cwd)
	return true, f.landedError(a, c, reply, errorLandedSuffix(a))
}

// replyBeforeError reads agent a's last assistant reply from its own session file, dropping only
// trailing unanswered retry prompts at the end of the session. It never walks predecessor sessions
// or skips past a newer assistant reply or tool event.
func replyBeforeError(home string, a Agent) (string, error) {
	evs, err := storedEvents(home, a)
	if err != nil {
		return "", err
	}
	for len(evs) > 0 && evs[len(evs)-1].Kind == "user" && isErrorRetryPrompt(evs[len(evs)-1].Text) {
		evs = evs[:len(evs)-1]
	}
	var last []string
	for _, e := range evs {
		switch e.Kind {
		case "user", "tool", "result":
			last = nil
		case "assistant":
			last = append(last, e.Text)
		}
	}
	return stripTrailer(strings.Join(last, "\n\n")), nil
}

func isErrorRetryPrompt(text string) bool {
	t := strings.TrimSpace(text)
	return t == limitResumeMessage || t == errorRetryMessage
}

func errorLandedSuffix(a Agent) string {
	s := strings.Join(strings.Fields(a.LastError), " ")
	s = strings.ReplaceAll(s, ",", ";")
	if s == "" {
		return legacyLimitNote
	}
	return s
}

func landCheckKey(a Agent) string {
	return errorFingerprint(a) + "@" + iso(a.LastActivityAt)
}

func (f *Factory) alreadyLandChecked(a Agent) bool {
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	return f.recoveryState(a).LandChecked == landCheckKey(a)
}

func (f *Factory) noteLandChecked(a Agent) {
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	r := f.recoveryState(a)
	r.LandChecked = landCheckKey(a)
	if f.recoveries == nil {
		f.recoveries = map[string]recovery{}
	}
	f.recoveries[a.ID] = r
}
