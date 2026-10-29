package factory

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const limitNote = "agent error: resume needed"
const limitMargin = 30 * time.Second
const limitGrace = 2 * time.Minute
const limitResumeMessage = "The provider limit reset. Check the current worktree and continue the interrupted step."

// One label stores the recovery atomically. Base64 avoids Paseo's comma separator.
// Revision also protects a send from a stale snapshot within this process.
type limitRecovery struct {
	Revision int
	Stage    string
	Error    string
	Reset    time.Time
	Attempt  time.Time
	UserAt   time.Time
	Phase    string // waiting, attempted, blocked, complete
}

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

func readLimit(a Agent) (r limitRecovery) {
	b, err := base64.RawURLEncoding.DecodeString(a.L(labelLimit))
	if err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func limitFingerprint(a Agent) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(a.LastError))) }

// Caller holds limitMu through persistence and send, serializing ticks and Resume.
func (f *Factory) limitState(a Agent) limitRecovery {
	r := readLimit(a)
	if cached := f.limitStates[a.ID]; cached.Revision > r.Revision {
		r = cached
	}
	return r
}

func (f *Factory) saveLimit(a Agent, r limitRecovery, labels map[string]string) error {
	r.Revision++
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelLimit] = base64.RawURLEncoding.EncodeToString(b)
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	if f.limitStates == nil {
		f.limitStates = map[string]limitRecovery{}
	}
	f.limitStates[a.ID] = r
	return nil
}

func (f *Factory) blockLimit(a Agent, r limitRecovery) error {
	if live, err := f.stillStopped(a); !live {
		return err
	}
	r.Phase = "blocked"
	return f.saveLimit(a, r, map[string]string{labelStage: "zach", labelNote: limitNote})
}

// tickLimit consumes errored working cards before ordinary completion rules can run.
// Error text is read from the saved agent snapshot; it never loads provider logs.
func (f *Factory) tickLimit(a Agent, now time.Time) (bool, error) {
	if a.archived() || stuckWhy[a.L(labelStage)] == "" {
		return false, nil
	}
	if _, ok := f.card(a.ID); !ok {
		return false, nil
	}
	f.limitMu.Lock()
	defer f.limitMu.Unlock()
	r := f.limitState(a)
	if a.Status != "error" {
		// A running retry may still fail. Only a completed new turn ends the episode.
		if r.Phase != "" && r.Phase != "complete" && a.stopped() && a.LastUserMessageAt.After(r.UserAt) {
			r.Phase = "complete"
			r.UserAt = a.LastUserMessageAt
			return false, f.saveLimit(a, r, nil)
		}
		return false, nil
	}
	if r.Phase == "complete" && !a.LastUserMessageAt.After(r.UserAt) {
		return true, nil // an older error snapshot cannot reopen a completed episode
	}
	if r.Phase == "blocked" {
		return true, f.blockLimit(a, r)
	}
	if r.Phase == "attempted" {
		if now.Sub(r.Attempt) >= limitGrace || (now.Sub(r.Attempt) > 45*time.Second && a.LastUserMessageAt.After(r.UserAt)) {
			return true, f.blockLimit(a, r)
		}
		return true, nil // allow the send to reach the provider; do not trust a stale error
	}
	if r.Phase == "" || r.Phase == "complete" {
		observed := a.LastActivityAt
		if observed.IsZero() || observed.After(now) {
			observed = now
		}
		reset, known := limitReset(a.LastError, observed)
		r = limitRecovery{Revision: r.Revision, Stage: a.L(labelStage), Error: limitFingerprint(a), UserAt: a.LastUserMessageAt, Reset: reset.Add(limitMargin), Phase: "waiting"}
		if !known {
			return true, f.blockLimit(a, r)
		}
		if err := f.saveLimit(a, r, nil); err != nil {
			return true, err
		}
		r = f.limitState(a)
	}
	if r.Error != limitFingerprint(a) {
		return true, f.blockLimit(a, r)
	}
	if now.Before(r.Reset) {
		return true, nil
	}
	return true, f.attemptLimit(a, r, now, limitResumeMessage, nil)
}

func (f *Factory) attemptLimit(a Agent, r limitRecovery, now time.Time, message string, attachments []Attachment) error {
	live, err := f.stillStopped(a)
	if err != nil || !live {
		return err
	}
	r.Phase, r.Attempt, r.UserAt = "attempted", now, a.LastUserMessageAt
	// A crash or timeout after this write cannot cause a second automatic send.
	if err := f.saveLimit(a, r, map[string]string{labelStage: r.Stage, labelSince: iso(now), labelNote: "resuming interrupted step", labelTold: ""}); err != nil {
		return err
	}
	if _, err := f.chat(a.ID, message, attachments); err != nil {
		return errors.Join(err, f.blockLimit(a, f.limitState(a)))
	}
	return nil
}

func (f *Factory) resumeLimit(a Agent, message string, attachments []Attachment) error {
	f.limitMu.Lock()
	defer f.limitMu.Unlock()
	r := f.limitState(a)
	if a.L(labelStage) != "zach" || a.L(labelNote) != limitNote || r.Phase != "blocked" || stuckWhy[r.Stage] == "" || a.archived() {
		return errors.New("this card is not waiting for error recovery")
	}
	return f.attemptLimit(a, r, f.Now(), message, attachments)
}

func limitAsk(a Agent) *Ask {
	why := "The agent failed or its reset time could not be read. Automatic recovery has stopped. Resume makes one attempt at the interrupted step."
	if a.LastError != "" {
		why += "\n\nProvider error: " + a.LastError
	}
	return &Ask{Title: "Agent error: resume the interrupted step?", From: "builder", Why: why,
		Actions: []Action{primary("resume", "Resume"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
}
