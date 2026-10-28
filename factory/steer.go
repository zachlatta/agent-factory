package factory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A card Zach closes as a duplicate of another card passes its request on: the factory records a
// Steer when he presses Close on a close verdict, and the Tick sends it to the kept card once that
// card is planning or building, as a follow-up block (dupPrefix) its reviewers also see. A kept card
// already landing, landed or dropped gets it as an idea instead.

// Steer is a closed duplicate's request waiting to reach the card it duplicates.
type Steer struct {
	Keep     string      `json:"keep"`  // the kept card's agent id
	From     string      `json:"from"`  // the closed card's short id
	Title    string      `json:"title"` // the closed card's title
	Repo     string      `json:"repo"`
	Request  CardRequest `json:"request"`         // Zach's words on the closed card
	Notes    string      `json:"notes,omitempty"` // its reviewer's notes: agent text, never shown as Zach's
	ClosedAt time.Time   `json:"closedAt"`
}

// SteerStore keeps pending steers in one JSON file, keyed by the closed card's agent id.
type SteerStore struct {
	path string
	mu   sync.Mutex
}

func (s *SteerStore) load() map[string]Steer {
	m := map[string]Steer{}
	readJSON(s.path, &m)
	return m
}

func (s *SteerStore) all() map[string]Steer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *SteerStore) update(fn func(map[string]Steer)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	fn(m)
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *SteerStore) put(id string, st Steer) error {
	return s.update(func(m map[string]Steer) { m[id] = st })
}

func (s *SteerStore) del(id string) error {
	return s.update(func(m map[string]Steer) { delete(m, id) })
}

var shortIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`) // a card id in a dupPrefix header

var dupLineRe = regexp.MustCompile(`(?im)^[\s*>_-]*DUPLICATE OF:\s*\**\s*` + "`?" + `([A-Za-z0-9][A-Za-z0-9-]*)`)

// ParseDuplicate is the card id on the last DUPLICATE OF line, or "".
func ParseDuplicate(text string) string {
	m := dupLineRe.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// newestReviewer is the most recently started reviewer of card id that decides a round (a single
// reviewer or an arbiter), else its most recent panel reviewer.
func (f *Factory) newestReviewer(id string) (Agent, bool) {
	var best Agent
	found := false
	byID := f.byID()
	for _, r := range f.agents() {
		if r.L(labelRole) != "reviewer" || !sameCard(byID, r.L(labelReviewOf), id) {
			continue
		}
		d, bd := deciding(r.L(labelLens)), deciding(best.L(labelLens))
		if !found || (d && !bd) || (d == bd && r.CreatedAt.After(best.CreatedAt)) {
			best, found = r, true
		}
	}
	return best, found
}

// recordSteer saves a steer when Zach closes card a at a reviewer's close verdict that names the
// card it duplicates (the newest reviewer's DUPLICATE OF line, else the card's own). It returns the
// kept card's short id, or "" when there is nothing to steer.
func (f *Factory) recordSteer(a Agent) (string, error) {
	if a.L(labelVerdict) != "close" {
		return "", nil // "Drop it" on any other ask is no duplicate close
	}
	keep, notes := "", ""
	if rev, ok := f.newestReviewer(a.ID); ok {
		// Close can come days after the review: read session files, never Paseo (which would load them)
		text, _ := storedText(f.cfg.Home, rev, 6)
		full, _ := f.storedReply(rev)
		notes = reviewNotes(full, text)
		if keep = ParseDuplicate(notes); keep == "" {
			keep = ParseDuplicate(text)
		}
	}
	if keep == "" {
		if text, err := f.storedReply(a); err == nil {
			keep = ParseDuplicate(text)
		}
	}
	k, ok := f.cardAgentOK(keep)
	if keep == "" || !ok || k.ID == a.ID || k.L(labelRole) != "" {
		return "", nil
	}
	req := f.cardRequest(a)
	if strings.TrimSpace(req.Base) == "" {
		req.Base = a.Title
	}
	st := Steer{Keep: k.ID, From: short(a.ID), Title: a.Title, Repo: a.Repo, Request: req,
		Notes: trunc(stripTrailer(notes), reviewBlockCap), ClosedAt: f.Now()}
	if err := f.steers.put(a.ID, st); err != nil {
		return "", err
	}
	f.logf("%s closed as a duplicate of %s: its request is steered there", short(a.ID), short(k.ID))
	return short(k.ID), nil
}

// tickSteers delivers each pending steer: to a kept card in plan or building now, as an idea to one
// that is landing, landed or dropped; a card in review or waiting on Zach keeps it waiting.
func (f *Factory) tickSteers() {
	for id, st := range f.steers.all() {
		if f.stopping.Load() {
			return
		}
		if err := f.tickSteer(id, st); err != nil {
			f.logf("steer from %s to %s: %v", st.From, short(st.Keep), err)
		}
	}
}

func (f *Factory) tickSteer(id string, st Steer) error {
	k, ok := f.cardAgentOK(st.Keep)
	stage := k.L(labelStage)
	switch {
	case !ok || k.gone() || stage == "landing" || stage == "landed":
		return f.steerToIdea(id, st)
	case k.archived() || k.L(labelRouting) != "" || (stage != "plan" && stage != "building"):
		return nil // under review, waiting on Zach (maybe parked) or still picking its repo: wait
	}
	if err := f.send(k.ID, steerMsg(st, f.Now().In(f.cfg.Location))); err != nil {
		return err
	}
	if err := f.steers.del(id); err != nil {
		return err
	}
	f.logf("steered %s's request into %s", st.From, short(k.ID))
	if k.Status == "running" {
		return nil // the message waits for its next turn; its stage moves on as usual
	}
	// idle: the agent answers the message, then the review of its own stage runs again
	kind := "code"
	if stage == "plan" {
		kind = "plan"
	}
	return f.setLabels(k.ID, map[string]string{labelStage: stage, labelSince: iso(f.Now()), labelAutoReview: kind})
}

// steerToIdea files a steer whose kept card can no longer take it as an idea, marked as coming from
// the closed card so Zach can dismiss it.
func (f *Factory) steerToIdea(id string, st Steer) error {
	var b strings.Builder
	b.WriteString(st.Request.Base)
	for _, fu := range st.Request.FollowUps {
		fmt.Fprintf(&b, "\n\nZach's follow-up (%s): %s", fu.Date, fu.Text)
	}
	if st.Notes != "" {
		b.WriteString("\n\nThe closed card's reviewer noted (an agent's notes, not Zach's words):\n" + st.Notes)
	}
	_, err := f.addIdea(Body{Repo: st.Repo, Title: "From closed duplicate " + st.From + ": " + st.Title,
		By: "factory (closed duplicate " + st.From + ")", Request: b.String(),
		Summary:  "Card " + st.From + " was closed as a duplicate of " + short(st.Keep) + ", which had finished before it could take this in.",
		Why:      "Closed as a duplicate of " + short(st.Keep) + "; what it asked beyond that card would otherwise be lost.",
		DoneWhen: "What this request asks beyond card " + short(st.Keep) + " is done, or Zach dismisses it."})
	if err != nil {
		f.logf("steer from %s: kept card %s is done and the idea was refused: %v", st.From, short(st.Keep), err)
	}
	return f.steers.del(id)
}

// steerMsg is the message to the kept card: Zach's words on the closed card in one follow-up block
// (dupPrefix), then its reviewer's notes outside the block, labelled as an agent's.
func steerMsg(st Steer, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Card %s %q was closed as a duplicate of this one.\n\n", st.From, st.Title)
	b.WriteString(dupPrefix + st.From + " (" + now.Format(time.RFC1123Z) + followUpSuffix + "\n")
	b.WriteString(strings.TrimSpace(st.Request.Base))
	for _, fu := range st.Request.FollowUps {
		fmt.Fprintf(&b, "\n\nZach's follow-up (%s): %s", fu.Date, fu.Text)
	}
	b.WriteString("\n" + emailEnd + "\n\n")
	if st.Notes != "" {
		b.WriteString("The closed card's reviewer noted (an agent's notes, not Zach's words):\n" + st.Notes + "\n\n")
	}
	b.WriteString("Take in what it asked that this card does not cover yet, within this card's scope. If something " +
		"does not fit, say so in your summary or suggest it as an idea.")
	return b.String()
}
