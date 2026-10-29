package factory

import (
	"sort"
	"strings"
	"time"
)

// Sections is the board as Zach reads it, one list per question he has: what waits on him (ForYou),
// what the factory is working on, what is stuck, what shipped today, which requests got no answer,
// and today's routine runs (shown only behind a filter). The board and the daily digest both read
// it, so they never disagree. "Today" is the local day in Config.Location.
type Sections struct {
	ForYou   []SectionLine `json:"forYou"`
	Working  []SectionLine `json:"working"`
	Stuck    []SectionLine `json:"stuck"`
	Shipped  []SectionLine `json:"shipped"`
	NoAnswer []SectionLine `json:"noAnswer"`
	Routine  []SectionLine `json:"routine"`
}

// SectionLine is one card (or request) in plain words: its title and a short state.
type SectionLine struct {
	ID    string `json:"id,omitempty"`  // the card; "" for a request that never became one
	Key   string `json:"key,omitempty"` // a request that never became a card: its key, to dismiss it (unanswered.go)
	Title string `json:"title"`
	State string `json:"state"`
	At    string `json:"at,omitempty"` // RFC 3339: when it shipped, or its last activity
}

// NoAnswerSource lists requests with no answer that are not cards (AddNoAnswer).
type NoAnswerSource func(now time.Time) []SectionLine

// AddNoAnswer adds a source of unanswered requests to Sections.NoAnswer.
func (f *Factory) AddNoAnswer(src NoAnswerSource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noAnswer = append(f.noAnswer, src)
}

// boardCards is the board as last built.
func (f *Factory) boardCards() []Card {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cards
}

// sections is BuildSections on the current board, plus the unanswered requests of every source.
func (f *Factory) sections() Sections {
	now := f.Now()
	f.mu.Lock()
	cards, srcs := f.cards, append([]NoAnswerSource{}, f.noAnswer...)
	f.mu.Unlock()
	s := BuildSections(cards, now, f.cfg.Location)
	// cards finished today that have left the board
	s.Shipped = mergeLines(s.Shipped, f.finishedToday(now))
	for _, src := range srcs {
		s.NoAnswer = append(s.NoAnswer, src(now)...)
	}
	return s
}

var workingState = map[string]string{"plan": "planning", "plan-review": "plan being checked", "building": "building",
	"review": "being reviewed", "landing": "shipping"}

// BuildSections sorts cards into Sections. Ideas and Scouts are left out; routine runs are listed
// only when they started today.
func BuildSections(cards []Card, now time.Time, loc *time.Location) Sections {
	if loc == nil {
		loc = time.UTC
	}
	s := Sections{ForYou: []SectionLine{}, Working: []SectionLine{}, Stuck: []SectionLine{}, Shipped: []SectionLine{},
		NoAnswer: []SectionLine{}, Routine: []SectionLine{}}
	titles := map[string]string{}
	for _, c := range cards {
		titles[c.Short] = c.Title
		titles[c.ID] = c.Title
	}
	for _, c := range cards {
		line := SectionLine{ID: c.ID, Title: c.Title, At: c.LastActivityAt}
		switch {
		case c.Stage == "idea" || c.Source == "scout":
		case c.Tier == "routine" || c.Source == "schedule":
			if sameDay(parseTime(c.CreatedAt), now, loc) {
				line.At = c.CreatedAt
				line.State = workingState[c.Stage]
				if c.Stage == "landed" {
					line.State = "done"
				}
				s.Routine = append(s.Routine, line)
			}
		case c.Stage == "zach" && c.Ask != nil && c.Ask.Kind != askProblem:
			line.State = c.Ask.Title
			if ov := c.Ask.Overview; ov != nil && ov.Decision != "" {
				line.State = ov.Decision
			}
			s.ForYou = append(s.ForYou, line)
		case c.Stage == "zach":
			line.State = stuckState(c)
			s.Stuck = append(s.Stuck, line)
		case c.Stage == "landed":
			done := doneAt(c)
			if !sameDay(done, now, loc) {
				continue
			}
			if l, ok := finishedLine(c, done); ok {
				s.Shipped = append(s.Shipped, l)
			}
		default:
			if st := stuckFlag(c); st != "" {
				line.State = st
				s.Stuck = append(s.Stuck, line)
				continue
			}
			line.State = workingState[c.Stage]
			if c.WaitsOn != "" && c.Stage != "landed" {
				if t := lookupTitle(titles, c.WaitsOn); t != "" {
					line.State = "waiting on " + t
				}
			}
			s.Working = append(s.Working, line)
		}
	}
	sortNewest(s.Shipped)
	sortNewest(s.Routine)
	return s
}

// finishedLine is the Shipped line of a card that reached Done at done, and whether it is listed: a
// landing (shipped, even when its email reply is missing) or a reply (answered). A dropped card and one
// done with neither are not. noReply is a metric only: the email dispatcher's watchdog flags a request
// with no answer (unanswered.go), so No answer yet has one source, which Dismiss clears on the board
// and in the digest alike.
func finishedLine(c Card, done time.Time) (SectionLine, bool) {
	line := SectionLine{ID: c.ID, Title: c.Title, At: done.Format(time.RFC3339)}
	switch {
	case dropped(c) || c.Timing == nil:
		return line, false
	case c.Timing.LandedAt != "":
		line.State = "shipped"
	case c.Timing.RepliedAt != "" && !c.Timing.NoReply:
		line.State = "answered"
	default:
		return line, false
	}
	return line, true
}

// Finished is the Shipped lines of the board's Done cards, whatever the day: the daily digest's ledger
// records them so each is listed once (digest.go).
func Finished(cards []Card) []SectionLine {
	var shipped []SectionLine
	for _, c := range cards {
		if c.Stage != "landed" || c.Source == "scout" || c.Tier == "routine" || c.Source == "schedule" {
			continue
		}
		done := doneAt(c)
		if done.IsZero() {
			continue
		}
		if l, ok := finishedLine(c, done); ok {
			shipped = append(shipped, l)
		}
	}
	return shipped
}

func sortNewest(ls []SectionLine) {
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].At > ls[j].At })
}

// sameDay reports whether t falls on now's local day in loc.
func sameDay(t, now time.Time, loc *time.Location) bool {
	if t.IsZero() {
		return false
	}
	y1, m1, d1 := t.In(loc).Date()
	y2, m2, d2 := now.In(loc).Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// doneAt is when card c reached Done: the start of its last Done stay, else its archive time.
func doneAt(c Card) time.Time {
	if c.Timing != nil {
		for i := len(c.Timing.Stages) - 1; i >= 0; i-- {
			if c.Timing.Stages[i].Stage == "landed" {
				return parseTime(c.Timing.Stages[i].Start)
			}
		}
	}
	return parseTime(c.ArchivedAt)
}

// dropped reports whether Zach (or a reviewer he agreed with) dropped or closed card c.
func dropped(c Card) bool { return strings.HasPrefix(c.Note, "closed by Zach") }

// stuckState says in plain words why a Needs Zach card that asks no decision is waiting.
func stuckState(c Card) string {
	if st := stuckFlag(c); st != "" {
		return st
	}
	if c.Ask != nil {
		return c.Ask.Title
	}
	return "waiting"
}

// stuckFlag is a working card's stall or error, in plain words, or "".
func stuckFlag(c Card) string {
	for _, fl := range c.Flags {
		switch {
		case strings.HasPrefix(fl, "stalled "):
			return "no activity for " + strings.TrimPrefix(fl, "stalled ")
		case fl == "error":
			return "stopped with an error"
		case strings.HasPrefix(fl, stuckFlagPrefix): // automatic recovery gave up (recover.go)
			return fl
		}
	}
	return ""
}

// lookupTitle finds the title of the card a waits-on line names by its short or full id.
func lookupTitle(titles map[string]string, id string) string {
	if t, ok := titles[id]; ok {
		return t
	}
	for k, t := range titles {
		if len(k) > len(id) && strings.HasPrefix(k, id) {
			return t
		}
	}
	return ""
}
