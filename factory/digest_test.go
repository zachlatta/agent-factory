package factory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// digestFactory is a factory on a mailbox, with the clock at h:m Eastern on 2026-10-28 and one card of
// each kind: waiting on Zach, building, stuck, and shipped today.
func digestFactory(t *testing.T, h, m int) (*Factory, *fakeMail, *time.Time) {
	t.Helper()
	ready := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1)})
	ready.Title = "Board groups cards by repo"
	building := card("c2", map[string]string{labelStage: "building"})
	building.Title = "Retries after a usage limit 3fa9c1e"
	stuck := card("c3", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	stuck.Title = "Nightly restore drill"
	f, _, mail := mailFactory(t, ready, building, stuck)
	loc, _ := time.LoadLocation("America/New_York")
	f.cfg.Location = loc
	now := time.Date(2026, 10, 28, h, m, 0, 0, loc)
	f.Now = func() time.Time { return now }
	f.AddNoAnswer(func(time.Time) []SectionLine {
		return []SectionLine{{Title: "Order more batteries", State: "no answer yet"}}
	})
	return f, mail, &now
}

func digests(m *fakeMail) []mailCall {
	var out []mailCall
	for _, s := range m.sends() {
		if s.Tool == "send_email" && strings.HasPrefix(s.Args["subject"].(string), "Factory today") {
			out = append(out, s)
		}
	}
	return out
}

// One digest a day at 6 PM Zach's time: not before, once after, never twice across restarts.
func TestDigestOncePerDayAtSixPM(t *testing.T) {
	f, m, now := digestFactory(t, 17, 59)
	f.tickDigest()
	if n := len(digests(m)); n != 0 {
		t.Fatalf("sent %d before 6 PM", n)
	}
	*now = now.Add(time.Minute)
	f.tickDigest()
	f.tickDigest()
	if n := len(digests(m)); n != 1 {
		t.Fatalf("sent %d at 6 PM, want 1", n)
	}
	// a restart reads the state file and does not send it again
	g := New(f.cfg, f.p)
	g.Mail, g.Now = m, f.Now
	g.SetAgents(f.agents())
	g.tickDigest()
	if n := len(digests(m)); n != 1 {
		t.Errorf("sent %d after a restart, want 1", n)
	}
	// the next day sends again at 6 PM, not after midnight
	*now = now.Add(7 * time.Hour) // 01:00 the next day
	g.tickDigest()
	if n := len(digests(m)); n != 1 {
		t.Errorf("sent %d after midnight, want still 1", n)
	}
	*now = now.Add(17 * time.Hour) // 18:00 the next day
	g.tickDigest()
	if n := len(digests(m)); n != 2 {
		t.Errorf("sent %d the next evening, want 2", n)
	}
}

// A factory that was down at 6 PM still sends that evening.
func TestDigestLateStartSendsThatEvening(t *testing.T) {
	f, m, _ := digestFactory(t, 21, 30)
	f.tickDigest()
	if n := len(digests(m)); n != 1 {
		t.Errorf("sent %d at 9:30 PM, want 1", n)
	}
}

// The digest lists what waits on him, what shipped, what is in progress, what is stuck and what got
// no answer, in plain words with a link per card, to his address, and says it once in the subject.
func TestDigestContent(t *testing.T) {
	f, m, _ := digestFactory(t, 18, 0)
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %+v", d)
	}
	text, html := d[0].Args["text"].(string), d[0].Args["html"].(string)
	if to := fmt.Sprint(d[0].Args["to"]); to != "["+testZach+"]" {
		t.Errorf("to = %v", to)
	}
	if subj := d[0].Args["subject"].(string); subj != "Factory today: 1 for you, 1 in progress, 1 stuck, 1 no answer" {
		t.Errorf("subject = %q", subj)
	}
	order := []string{"Waiting on you", "Board groups cards by repo", "In progress", "Retries after a usage limit",
		"Stuck", "Nightly restore drill", "No answer yet", "Order more batteries"}
	last := -1
	for _, w := range order {
		i := strings.Index(text, w)
		if i < 0 || i < last {
			t.Errorf("text lacks %q in order:\n%s", w, text)
		}
		last = i
	}
	if strings.Contains(text, "3fa9c1e") {
		t.Errorf("a hash reached the digest:\n%s", text)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if !strings.Contains(text, "https://board.example/#"+id) || !strings.Contains(html, `href="https://board.example/#`+id+`"`) {
			t.Errorf("no link to %s:\ntext %s\nhtml %s", id, text, html)
		}
	}
	if strings.Count(text, "https://board.example/") < 4 { // the cards and the board at the foot
		t.Errorf("text lacks the board link:\n%s", text)
	}
}

// Shipped today shows each shipped card on one line with its link.
func TestDigestShippedToday(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	shipped := []Card{{ID: "d1", Title: "Phone board", Stage: "landed", Source: "board",
		Timing: &TimingView{LandedAt: "2026-10-28T14:00:00Z", Stages: []StageSpan{{Stage: "landed", Start: "2026-10-28T14:00:00Z"}}}}}
	s := BuildSections(shipped, time.Date(2026, 10, 28, 18, 0, 0, 0, loc), loc)
	f, _ := newTestFactory(t)
	f.cfg.BoardURL, f.cfg.Location = "https://board.example/", loc
	e, ok := f.digestEmail(s)
	if !ok || !strings.Contains(e.Subject, "1 shipped") || !strings.Contains(e.Text, "Shipped today") ||
		!strings.Contains(e.Text, "Phone board") || !strings.Contains(e.Text, "https://board.example/#d1") {
		t.Errorf("digest = %+v", e)
	}
}

// A day with nothing to report sends nothing, and still counts as that day's digest.
func TestDigestSkipsAnEmptyDay(t *testing.T) {
	f, p := newTestFactory(t)
	m := &fakeMail{threads: map[string][]threadMsg{}, errs: map[string]error{}}
	f.Mail, f.cfg.ZachEmail = m, testZach
	loc, _ := time.LoadLocation("America/New_York")
	f.cfg.Location = loc
	f.Now = func() time.Time { return time.Date(2026, 10, 28, 18, 5, 0, 0, loc) }
	f.tickDigest()
	if len(m.sends()) != 0 {
		t.Errorf("an empty day sent %+v", m.sends())
	}
	if d, _ := f.digestState(); d != "2026-10-28" {
		t.Errorf("state = %q", d)
	}
	_ = p
}

// A failed send is tried again after a pause; a dry run or a read-only factory never sends.
func TestDigestRetryAndPassive(t *testing.T) {
	f, m, now := digestFactory(t, 18, 0)
	m.errs["send_email"] = errors.New("mailbox down")
	f.tickDigest()
	if d, _ := f.digestState(); d != "" {
		t.Errorf("a failed send was recorded as sent: %q", d)
	}
	delete(m.errs, "send_email")
	tried := len(digests(m)) // the failed try
	f.tickDigest()
	if n := len(digests(m)); n != tried {
		t.Errorf("retried at once: %d", n-tried)
	}
	*now = now.Add(mailRetryAfter + time.Second)
	f.tickDigest()
	if n := len(digests(m)); n != tried+1 {
		t.Errorf("not retried after the pause: %d", n-tried)
	}
	g, mg, _ := digestFactory(t, 18, 0)
	g.cfg.DryRun = true
	g.tickDigest()
	if n := len(digests(mg)); n != 0 {
		t.Errorf("a dry run sent the digest")
	}
}

// The digest can go to its own address (FACTORY_DIGEST_TO), else to Zach's.
func TestDigestAddress(t *testing.T) {
	f, m, _ := digestFactory(t, 18, 0)
	f.cfg.DigestTo = "digest@example.com"
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 || fmt.Sprint(d[0].Args["to"]) != "[digest@example.com]" {
		t.Errorf("digest = %+v", d)
	}
}

// A card shipped this morning is still in the evening's digest after it has left the board.
func TestDigestKeepsShippedCardsThatLeftTheBoard(t *testing.T) {
	f, m, now := digestFactory(t, 9, 0)
	loc := f.cfg.Location
	at := now.Add(-time.Minute).Format(time.RFC3339)
	done := Card{ID: "d1", Short: "d1", Title: "Phone board", Stage: "landed", Source: "board",
		Timing: &TimingView{LandedAt: at, Stages: []StageSpan{{Stage: "landed", Start: at}}}}
	f.mu.Lock()
	board := f.cards
	f.cards = append(append([]Card{}, board...), done)
	f.mu.Unlock()
	if got := sectionIDs(f.sections().Shipped); len(got) != 1 || got[0] != "d1" {
		t.Fatalf("shipped this morning = %v", got)
	}
	f.recordShipped()
	// by 6 PM it has left the board
	f.mu.Lock()
	f.cards = board
	f.mu.Unlock()
	*now = time.Date(2026, 10, 28, 18, 0, 0, 0, loc)
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 || !strings.Contains(d[0].Args["text"].(string), "Phone board") || !strings.Contains(d[0].Args["subject"].(string), "1 shipped") {
		t.Fatalf("digest = %+v", d)
	}
	// the next day starts a new ledger
	*now = now.Add(24 * time.Hour)
	f.recordShipped()
	if got := f.sections().Shipped; len(got) != 0 {
		t.Errorf("yesterday's shipped cards are still listed: %+v", got)
	}
}
