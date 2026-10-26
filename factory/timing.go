package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Card timing: when Zach asked, each column a card entered, its review rounds, when its work
// landed and when the reply reached Zach. Kept in StateDir/timings.json so a restart keeps it.
//
// Stage changes are read from the board the factory already builds (setAgents), so they cost no
// paseo call: only reads setAgents accepted count. Review rounds, the landing commit and the
// factory's own emails are recorded where they happen. Only factory cards are timed: a factory.stage
// label, no factory.role (reviewers, Scouts) and no factory.routing (repo pickers). A card first seen
// past its start (in flight when timing began, or created while the factory was down) is partial:
// its history is incomplete, so metrics leave it out.

const (
	timingKeep      = 30 * 24 * time.Hour // entries this long past their last event are dropped
	timingFreshCard = 2 * time.Minute     // a card first seen in plan this soon after it was created is whole
	replyWait       = 5 * time.Minute     // the reply check runs this long after Done; replies count up to Done + this
	replyNearMail   = 2 * time.Minute     // a SENT message this close to a factory email is that email
	replyTries      = 3                   // failed thread reads before the reply check gives up
	replyPerTick    = 3                   // thread reads per Tick at most
	askedWait       = 10 * time.Minute    // an email card whose first prompt is unread this long uses its creation
)

// TimingEvent: the card entered Stage at At.
type TimingEvent struct {
	Stage string    `json:"stage"`
	At    time.Time `json:"at"`
}

// TimingRound is one review round: started with the reviewer, ended when its verdict was applied.
type TimingRound struct {
	Kind    string    `json:"kind"`
	N       int       `json:"n"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Verdict string    `json:"verdict"`
}

// FactoryMail is an email the factory itself sent for a card (a Needs Zach note or an answer's
// confirmation): never the reply to Zach.
type FactoryMail struct {
	ID string    `json:"id,omitempty"`
	At time.Time `json:"at"`
}

// CardTiming is one card's recorded times.
type CardTiming struct {
	Repo          string          `json:"repo,omitempty"`
	Sessions      []TimingSession `json:"sessions,omitempty"`
	RequestedAt   time.Time       `json:"requestedAt,omitzero"`
	RequestedFrom string          `json:"requestedFrom,omitempty"` // email-date, voice-recorded or created
	Source        string          `json:"source"`
	Tier          string          `json:"tier,omitempty"`
	Thread        string          `json:"thread,omitempty"` // email and voice cards: where the reply goes
	Partial       bool            `json:"partial,omitempty"`
	Events        []TimingEvent   `json:"events"`
	Rounds        []TimingRound   `json:"rounds,omitempty"`
	LandedAt      time.Time       `json:"landedAt,omitzero"` // the last landing's commit time
	RepliedAt     time.Time       `json:"repliedAt,omitzero"`
	ReplyChecked  bool            `json:"replyChecked,omitempty"`
	ReplyTries    int             `json:"replyTries,omitempty"`
	ReplyRetry    time.Time       `json:"replyRetry,omitzero"`
	Mails         []FactoryMail   `json:"mails,omitempty"`
}

func (t CardTiming) last() TimingEvent {
	if len(t.Events) == 0 {
		return TimingEvent{}
	}
	return t.Events[len(t.Events)-1]
}

// doneAt is when the card reached Done, or zero.
func (t CardTiming) doneAt() time.Time {
	if l := t.last(); l.Stage == "landed" {
		return l.At
	}
	return time.Time{}
}

// zach is the time spent in Needs Zach up to now (or Done).
func (t CardTiming) zach(now time.Time) time.Duration {
	var d time.Duration
	for i, e := range t.Events {
		if e.Stage != "zach" {
			continue
		}
		end := now
		if i+1 < len(t.Events) {
			end = t.Events[i+1].At
		}
		if end.After(e.At) {
			d += end.Sub(e.At)
		}
	}
	return d
}

// end is when the card's work reached Zach: the reply, else the landing; zero if neither.
func (t CardTiming) end() time.Time {
	if !t.RepliedAt.IsZero() {
		return t.RepliedAt
	}
	return t.LandedAt
}

// TimingStore keeps CardTimings in one JSON file, keyed by card id.
type TimingStore struct {
	path  string
	now   func() time.Time
	loc   *time.Location
	mu    sync.Mutex
	cards map[string]*CardTiming
	dirty bool
	// readOnly: a deploy canary shares the live factory's StateDir, so it keeps changes in memory
	readOnly bool
	// highestSeq is the newest board read observed, in memory only: read numbers start again at 0
	// on every run (Factory.readSeq), so a saved one would hold back every read after a restart.
	highestSeq uint64
}

func newTimingStore(path string, now func() time.Time, loc *time.Location) *TimingStore {
	s := &TimingStore{path: path, now: now, loc: loc, cards: map[string]*CardTiming{}}
	readJSON(path, &s.cards)
	if s.cards == nil {
		s.cards = map[string]*CardTiming{}
	}
	return s
}

// get returns a copy of card id's timing.
func (s *TimingStore) get(id string) (CardTiming, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.cards[id]
	if !ok {
		return CardTiming{}, false
	}
	return t.copy(), true
}

func (t *CardTiming) copy() CardTiming {
	c := *t
	c.Events = append([]TimingEvent(nil), t.Events...)
	c.Rounds = append([]TimingRound(nil), t.Rounds...)
	c.Mails = append([]FactoryMail(nil), t.Mails...)
	c.Sessions = append([]TimingSession(nil), t.Sessions...)
	return c
}

// all returns a copy of every timing.
func (s *TimingStore) all() map[string]CardTiming {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]CardTiming, len(s.cards))
	for id, t := range s.cards {
		out[id] = t.copy()
	}
	return out
}

// update changes card id's timing (if it is timed) and saves it.
func (s *TimingStore) update(id string, fn func(*CardTiming)) {
	s.mu.Lock()
	t, ok := s.cards[id]
	if ok {
		fn(t)
		s.dirty = true
	}
	s.mu.Unlock()
	if ok {
		s.save()
	}
}

// save writes the file if anything changed, dropping entries long past their last event.
func (s *TimingStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty || s.readOnly {
		return nil
	}
	now := s.now()
	for id, t := range s.cards {
		if l := t.last(); !l.At.IsZero() && now.Sub(l.At) > timingKeep {
			delete(s.cards, id)
		}
	}
	b, err := json.Marshal(s.cards)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// timed reports whether agent a is a card the factory times.
func timed(a Agent) bool {
	return a.L(labelStage) != "" && a.L(labelRole) == "" && a.L(labelRouting) == ""
}

// needsAsked lists the email and voice cards whose request time is not known yet: their first
// prompt must be read (cardRequest) before observe.
func (s *TimingStore) needsAsked(agents []Agent) []Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Agent
	for _, a := range agents {
		if !timed(a) || !fromInbox(a) || a.gone() {
			continue
		}
		if t, ok := s.cards[a.ID]; !ok || t.RequestedAt.IsZero() {
			out = append(out, a)
		}
	}
	return out
}

// observe records the columns of the cards on an accepted board read (number seq). A read older
// than one already observed changes nothing; the same read again adds nothing, since an event is
// added only when a card's column changed. asked holds the first prompts of email and voice cards
// (needsAsked). It reports whether anything changed; save writes it.
func (s *TimingStore) observe(cards []Card, agents []Agent, seq uint64, now time.Time, asked map[string]CardRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.highestSeq {
		return false
	}
	s.highestSeq = seq
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	changed := false
	owners := cardOwners(agents, nil)
	for _, c := range cards {
		a, ok := byID[c.ID]
		if !ok || !timed(a) {
			continue
		}
		t := s.cards[c.ID]
		if t == nil {
			if a.gone() || c.Stage == "landed" {
				continue // finished before timing saw it: nothing to time
			}
			t = &CardTiming{Source: timingSource(a)}
			if c.Stage == "plan" && now.Sub(a.CreatedAt) <= timingFreshCard {
				t.Events = []TimingEvent{{Stage: "plan", At: a.CreatedAt}}
			} else {
				t.Partial = true
				t.Events = []TimingEvent{{Stage: c.Stage, At: movedAt(a, time.Time{}, now)}}
			}
			s.cards[c.ID] = t
			changed = true
		} else if l := t.last(); l.Stage != c.Stage {
			at := movedAt(a, l.At, now)
			if c.Stage == "landed" && a.gone() && a.ArchivedAt.After(l.At) && !a.ArchivedAt.After(now) {
				at = a.ArchivedAt
			}
			t.Events = append(t.Events, TimingEvent{Stage: c.Stage, At: at})
			changed = true
		}
		if c.Tier != "" && c.Tier != t.Tier {
			t.Tier, changed = c.Tier, true
		}
		if a.Repo != "" && a.Repo != t.Repo {
			t.Repo, changed = a.Repo, true
		}
		if t.observeSessions(c, agents, byID, owners, now) {
			changed = true
		}
		if th := emailThread(a); th != "" && th != t.Thread {
			t.Thread, changed = th, true
		}
		if t.RequestedAt.IsZero() {
			if at, from := requestedAt(a, asked, s.loc, now); !at.IsZero() {
				t.RequestedAt, t.RequestedFrom, changed = at, from, true
			}
		}
	}
	if changed {
		s.dirty = true
	}
	return changed
}

// movedAt is when agent a entered its column: its factory.since when that is after the last event
// (and not in the future), else now. Moves to Needs Zach and Done set no since: they get now.
func movedAt(a Agent, last, now time.Time) time.Time {
	if since := parseTime(a.L(labelSince)); since.After(last) && !since.After(now) {
		return since
	}
	return now
}

// timingSource groups cards for metrics: idea, else the factory.source label (board, email, voice).
func timingSource(a Agent) string {
	if a.L(labelOrigin) == "idea" {
		return "idea"
	}
	if s := a.L(labelSource); s != "" {
		return s
	}
	return source(a)
}

// requestedAt is when Zach asked for card a: an email card's Date header, a voice card's recording
// time, else when the card was created. Email and voice cards wait for their first prompt (asked);
// one that has none yet gives zero (for askedWait), and one whose line does not parse falls back to creation.
func requestedAt(a Agent, asked map[string]CardRequest, loc *time.Location, now time.Time) (time.Time, string) {
	if !fromInbox(a) {
		return a.CreatedAt, "created"
	}
	r, ok := asked[a.ID]
	if !ok || !r.Read {
		if now.Sub(a.CreatedAt) > askedWait {
			return a.CreatedAt, "created" // no first prompt to read (no session file)
		}
		return time.Time{}, ""
	}
	switch r.AskedKind {
	case "email-date":
		if t, err := mail.ParseDate(r.Asked); err == nil {
			return t.UTC(), r.AskedKind
		}
	case "voice-recorded":
		if t, ok := parseRecorded(r.Asked, a.CreatedAt, loc); ok {
			return t, r.AskedKind
		}
	}
	return a.CreatedAt, "created"
}

// The email dispatcher's first prompt carries when Zach asked: "- Date: <RFC 5322>" for an email,
// "- Recorded: Mon Jan 2, 3:04 PM (Zone), N seconds" for a voice memo.
const (
	dateLine     = "- Date: "
	recordedLine = "- Recorded: "
)

// askedOf finds the first Date or Recorded line in a first prompt: its kind and value.
func askedOf(prompt string) (kind, value string) {
	for _, l := range strings.Split(prompt, "\n") {
		if v, ok := strings.CutPrefix(l, dateLine); ok {
			return "email-date", strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, recordedLine); ok {
			return "voice-recorded", strings.TrimSpace(v)
		}
	}
	return "", ""
}

// parseRecorded reads a Recorded value. It has no year: the year is the one that puts it at or
// before the card's creation (plus a day for clock skew).
func parseRecorded(v string, created time.Time, loc *time.Location) (time.Time, bool) {
	when, rest, ok := strings.Cut(v, " (")
	if !ok {
		return time.Time{}, false
	}
	zone, _, ok := strings.Cut(rest, ")")
	if !ok {
		return time.Time{}, false
	}
	if l, err := time.LoadLocation(zone); err == nil {
		loc = l
	}
	t, err := time.ParseInLocation("Mon Jan 2, 3:04 PM", when, loc)
	if err != nil {
		return time.Time{}, false
	}
	year := created.In(loc).Year()
	t = time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if t.After(created.Add(24 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t.UTC(), true
}

// TimingView is a card's timing on the board: times are ISO, durations seconds.
type TimingView struct {
	RequestedAt   string      `json:"requestedAt,omitempty"`
	RequestedFrom string      `json:"requestedFrom,omitempty"`
	Stages        []StageSpan `json:"stages"`
	Rounds        []RoundView `json:"rounds,omitempty"`
	ZachSecs      int         `json:"zachSecs"`
	LandedAt      string      `json:"landedAt,omitempty"`
	RepliedAt     string      `json:"repliedAt,omitempty"`
	TotalSecs     int         `json:"totalSecs,omitempty"` // asked to reply (else landing, Done or now)
	Partial       bool        `json:"partial,omitempty"`
}

// StageSpan is one stay in a column; Done has no length.
type StageSpan struct {
	Stage string `json:"stage"`
	Start string `json:"start"`
	Secs  int    `json:"secs"`
}

type RoundView struct {
	Kind    string `json:"kind"`
	N       int    `json:"n"`
	Secs    int    `json:"secs"`
	Verdict string `json:"verdict"`
}

func secs(d time.Duration) int { return int(d.Round(time.Second) / time.Second) }

// view is t as the board shows it at now.
func (t CardTiming) view(now time.Time) *TimingView {
	v := &TimingView{RequestedAt: iso(t.RequestedAt), RequestedFrom: t.RequestedFrom, ZachSecs: secs(t.zach(now)),
		LandedAt: iso(t.LandedAt), RepliedAt: iso(t.RepliedAt), Partial: t.Partial, Stages: []StageSpan{}}
	for i, e := range t.Events {
		sp := StageSpan{Stage: e.Stage, Start: iso(e.At)}
		if e.Stage != "landed" {
			end := now
			if i+1 < len(t.Events) {
				end = t.Events[i+1].At
			}
			sp.Secs = max(0, secs(end.Sub(e.At)))
		}
		v.Stages = append(v.Stages, sp)
	}
	for _, r := range t.Rounds {
		v.Rounds = append(v.Rounds, RoundView{Kind: r.Kind, N: r.N, Secs: max(0, secs(r.End.Sub(r.Start))), Verdict: r.Verdict})
	}
	if !t.RequestedAt.IsZero() {
		end := t.end()
		if end.IsZero() {
			end = t.doneAt()
		}
		if end.IsZero() {
			end = now
		}
		v.TotalSecs = max(0, secs(end.Sub(t.RequestedAt)))
	}
	return v
}

// fillTiming puts each timed card's timing on the board.
func (s *TimingStore) fillTiming(cards []Card, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range cards {
		if t, ok := s.cards[cards[i].ID]; ok {
			cards[i].Timing = t.view(now)
		}
	}
}

// recordRound records a review round of card id.
func (f *Factory) recordRound(id, kind string, n int, start time.Time, verdict string) {
	end := f.Now()
	f.timings.update(id, func(t *CardTiming) {
		t.Rounds = append(t.Rounds, TimingRound{Kind: kind, N: n, Start: start, End: end, Verdict: verdict})
	})
}

// recordLanded records that card a's work landed: the commit time of its HEAD, else now. A card
// that lands again (part of its work first) keeps the last landing.
func (f *Factory) recordLanded(a Agent, cwd string) {
	at := f.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out, err := f.GitOut(ctx, cwd, "log", "-1", "--format=%cI", "HEAD")
	cancel()
	if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(out)); err == nil && perr == nil {
		at = t.UTC()
	} else {
		f.logf("%s landing time from the clock: no commit time (%v)", short(a.ID), err)
	}
	f.timings.update(a.ID, func(t *CardTiming) { t.LandedAt = at })
}

// sendMail sends one email for card id through the mailbox and records it as the factory's own, so
// the reply check never takes it for the reply to Zach. Every factory email goes through here.
func (f *Factory) sendMail(ctx context.Context, id, tool string, args map[string]any) (json.RawMessage, error) {
	out, err := f.Mail.Call(ctx, tool, args)
	if err != nil {
		return out, err
	}
	var res struct {
		ID        string `json:"id"`
		MessageID string `json:"message_id"`
	}
	json.Unmarshal(out, &res)
	m := FactoryMail{ID: res.ID, At: f.Now()}
	if m.ID == "" {
		m.ID = res.MessageID
	}
	f.timings.update(id, func(t *CardTiming) { t.Mails = append(t.Mails, m) })
	return out, nil
}

// checkReplies finds when the reply reached Zach for email and voice cards that landed: one thread
// read per card, replyWait after Done, at most replyPerTick a Tick.
func (f *Factory) checkReplies(now time.Time) {
	if f.Mail == nil || f.cfg.DryRun {
		return
	}
	n := 0
	for id, t := range f.timings.all() {
		if n >= replyPerTick || f.stopping.Load() {
			return
		}
		done := t.doneAt()
		if t.ReplyChecked || t.Thread == "" || t.LandedAt.IsZero() || done.IsZero() || now.Sub(done) < replyWait ||
			now.Before(t.ReplyRetry) || (t.Source != "email" && t.Source != "voice") {
			continue
		}
		n++
		msgs, err := f.readThread(t.Thread)
		if err != nil {
			f.logf("%s reply time not read: %v", short(id), err)
			f.timings.update(id, func(t *CardTiming) {
				t.ReplyTries++
				t.ReplyRetry = now.Add(replyWait)
				t.ReplyChecked = t.ReplyTries >= replyTries
			})
			continue
		}
		at := replyTime(t, msgs)
		f.timings.update(id, func(t *CardTiming) { t.RepliedAt, t.ReplyChecked = at, true })
	}
}

func (f *Factory) readThread(thread string) ([]threadMsg, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := f.Mail.Call(ctx, "get_thread", map[string]any{"thread_id": thread, "max_body_chars": 1})
	if err != nil {
		return nil, err
	}
	var th struct {
		Messages []threadMsg `json:"messages"`
	}
	if err := json.Unmarshal(out, &th); err != nil {
		return nil, fmt.Errorf("get_thread: %v", err)
	}
	return th.Messages, nil
}

// replyTime is the card's reply to Zach: the last message the mailbox sent after he asked and by
// Done + replyWait that is not one of the factory's own emails; zero if none. A one-pass answer is
// sent before its work item lands, so it is not bounded by the landing.
func replyTime(t CardTiming, msgs []threadMsg) time.Time {
	limit := t.doneAt().Add(replyWait)
	var out time.Time
	for _, m := range msgs {
		if !m.sent() {
			continue
		}
		d, err := mail.ParseDate(m.Date)
		if err != nil || d.After(limit) || (!t.RequestedAt.IsZero() && !d.After(t.RequestedAt)) || t.factoryMail(m.ID, d) {
			continue
		}
		if d.After(out) {
			out = d.UTC()
		}
	}
	return out
}

func (t CardTiming) factoryMail(id string, at time.Time) bool {
	for _, m := range t.Mails {
		if (m.ID != "" && m.ID == id) || (at.Sub(m.At) < replyNearMail && m.At.Sub(at) < replyNearMail) {
			return true
		}
	}
	return false
}
