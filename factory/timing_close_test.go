package factory

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitingCard is a board card asked once: it entered Needs Zach at t0 and is there now.
func waitingCard(t *testing.T, id string, labels map[string]string) (*Factory, *fakePaseo, Agent) {
	t.Helper()
	a := fresh(id)
	f, p := newTestFactory(t, a)
	a = moved(a, "zach", "")
	for k, v := range labels {
		a.Labels[labelPrefix+k] = v
	}
	f.SetAgents([]Agent{a})
	return f, p, a
}

func clockAt(f *Factory, at time.Time) { f.Now = func() time.Time { return at } }

func TestAsksCountsEachStayInNeedsZach(t *testing.T) {
	ct := CardTiming{}
	if ct.asks() != 0 {
		t.Errorf("no events = %d asks", ct.asks())
	}
	ct.Events = []TimingEvent{{Stage: "zach", At: t0}, {Stage: "building", At: t0.Add(time.Minute)},
		{Stage: "zach", At: t0.Add(2 * time.Minute)}, {Stage: "landed", At: t0.Add(3 * time.Minute)}}
	if ct.asks() != 2 {
		t.Errorf("asks = %d, want 2 (a first event in Needs Zach counts)", ct.asks())
	}
}

func TestAsksAndClosedAtReachBoardAndExport(t *testing.T) {
	f, _, a := waitingCard(t, "c1", nil)
	closeAt := t0.Add(time.Hour)
	clockAt(f, closeAt)
	if _, err := f.Act("close", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	a.ArchivedAt = closeAt
	f.SetAgents([]Agent{a}) // the read a board action does after it
	f.mu.Lock()
	v := f.cards[0].Timing
	f.mu.Unlock()
	if v == nil || v.Asks != 1 || v.ClosedAt != iso(closeAt) {
		t.Fatalf("board timing = %+v", v)
	}
	rr := httptest.NewRecorder()
	f.handleTimings(rr, httptest.NewRequest("GET", "/api/timings", nil))
	var got struct {
		Cards []struct {
			Asks     int       `json:"asks"`
			ClosedAt time.Time `json:"closedAt"`
			Title    *string   `json:"title"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 1 || got.Cards[0].Asks != 1 || !got.Cards[0].ClosedAt.Equal(closeAt) || got.Cards[0].Title != nil {
		t.Fatalf("export = %s", rr.Body.String())
	}
}

// A parked card closed later is Done at the close, not at the park, even once it has left the board.
func TestParkedCardClosedLaterEndsAtTheClose(t *testing.T) {
	for _, action := range []string{"close", "done", "stage"} {
		t.Run(action, func(t *testing.T) {
			parkAt := t0.Add(30 * time.Minute)
			a := moved(fresh("c1"), "zach", "")
			f, _ := newTestFactory(t, a)
			clockAt(f, parkAt)
			a.Labels[labelPrefix+labelParked] = iso(parkAt)
			a.ArchivedAt = parkAt
			f.SetAgents([]Agent{a})

			closeAt := parkAt.Add(51 * time.Minute)
			clockAt(f, closeAt)
			if _, err := f.Act(action, Body{ID: "c1", Stage: "landed"}); err != nil {
				t.Fatal(err)
			}
			ct := timing(t, f, "c1")
			if l := ct.last(); l.Stage != "landed" || !l.At.Equal(closeAt) || !ct.ClosedAt.Equal(closeAt) {
				t.Fatalf("after close = %+v", ct)
			}
			if got := strings.Join(stages(ct), " "); got != "zach landed" {
				t.Fatalf("stages = %s", got)
			}
			// the unparked file drops off the next read, which must change nothing
			clockAt(f, closeAt.Add(time.Hour))
			f.SetAgents(nil)
			if got := timing(t, f, "c1"); !got.doneAt().Equal(closeAt) || !got.ClosedAt.Equal(closeAt) {
				t.Fatalf("after leaving the board = %+v", got)
			}
		})
	}
}

// Every way Zach closes a card stamps it; the board's next read adds nothing.
func TestCloseActionsStampClosedAt(t *testing.T) {
	for _, tc := range []struct {
		action string
		body   Body
	}{
		{"close", Body{ID: "c1"}},
		{"done", Body{ID: "c1"}},
		{"archive", Body{ID: "c1"}},
		{"stage", Body{ID: "c1", Stage: "landed"}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f, _, a := waitingCard(t, "c1", nil)
			closeAt := t0.Add(time.Hour)
			clockAt(f, closeAt)
			if _, err := f.Act(tc.action, tc.body); err != nil {
				t.Fatal(err)
			}
			a.ArchivedAt = closeAt
			f.SetAgents([]Agent{a})
			ct := timing(t, f, a.ID)
			if !ct.ClosedAt.Equal(closeAt) || !ct.doneAt().Equal(closeAt) || ct.CloseVersion != closeVersion {
				t.Fatalf("timing = %+v", ct)
			}
			if n := strings.Count(strings.Join(stages(ct), " "), "landed"); n != 1 {
				t.Fatalf("stages = %v", stages(ct))
			}
		})
	}
}

// A board read that started before a close and is stored after it still shows the card in Needs
// Zach. It is no reopening: the close time stays and no ask is added.
func TestReadStartedBeforeACloseDoesNotUndoIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		body   Body
		labels map[string]string
	}{
		{"done", "done", Body{ID: "c1"}, nil},
		{"parked done", "done", Body{ID: "c1"}, map[string]string{labelParked: iso(t0)}},
		{"close", "close", Body{ID: "c1"}, nil},
		{"archive", "archive", Body{ID: "c1"}, nil},
		{"drag to Done", "stage", Body{ID: "c1", Stage: "landed"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, a := waitingCard(t, "c1", tc.labels)
			before := f.nextRead()
			closeAt := t0.Add(time.Hour)
			clockAt(f, closeAt)
			if _, err := f.Act(tc.action, tc.body); err != nil {
				t.Fatal(err)
			}
			clockAt(f, closeAt.Add(time.Second))
			if !f.setAgents([]Agent{a}, before, false) {
				t.Fatal("the board did not store the older read: this test no longer covers the race")
			}
			check := func(when string) {
				t.Helper()
				ct := timing(t, f, "c1")
				if ct.asks() != 1 || !ct.ClosedAt.Equal(closeAt) || !ct.doneAt().Equal(closeAt) {
					t.Fatalf("%s: asks = %d, timing = %+v", when, ct.asks(), ct)
				}
				if got := strings.Join(stages(ct), " "); got != "building zach landed" {
					t.Fatalf("%s: stages = %s", when, got)
				}
			}
			check("after the older read")
			clockAt(f, closeAt.Add(time.Minute))
			f.SetAgents([]Agent{moved(a, "landed", "")}) // the action's own reload
			check("after the newer read")
		})
	}
}

// A failed close records nothing.
func TestFailedCloseIsNotStamped(t *testing.T) {
	f, p, _ := waitingCard(t, "c1", nil)
	p.stuck["c1"] = true
	p.status["c1"] = "running"
	clockAt(f, t0.Add(time.Hour))
	if _, err := f.Act("archive", Body{ID: "c1"}); err == nil {
		t.Fatal("archive of a stuck agent succeeded")
	}
	if ct := timing(t, f, "c1"); !ct.ClosedAt.IsZero() {
		t.Fatalf("closed = %v", ct.ClosedAt)
	}
}

// Tidying a card already in Done is no close: its Done time and closedAt stay.
func TestClosingACardInDoneChangesNothing(t *testing.T) {
	doneAt := t0.Add(time.Minute)
	a := fresh("c1")
	f, _ := newTestFactory(t, a)
	clockAt(f, doneAt)
	f.SetAgents([]Agent{moved(a, "landed", "")})
	for _, tc := range []struct {
		action string
		body   Body
	}{{"archive", Body{ID: "c1"}}, {"done", Body{ID: "c1"}}, {"stage", Body{ID: "c1", Stage: "landed"}}} {
		clockAt(f, t0.Add(time.Hour))
		if _, err := f.Act(tc.action, tc.body); err != nil {
			t.Fatal(err)
		}
		ct := timing(t, f, "c1")
		if !ct.ClosedAt.IsZero() || !ct.doneAt().Equal(doneAt) {
			t.Fatalf("%s changed it: %+v", tc.action, ct)
		}
	}
}

func TestReopeningClearsClosedAt(t *testing.T) {
	f, _, a := waitingCard(t, "c1", nil)
	clockAt(f, t0.Add(time.Hour))
	if _, err := f.Act("done", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if timing(t, f, "c1").ClosedAt.IsZero() {
		t.Fatal("not stamped")
	}
	clockAt(f, t0.Add(2*time.Hour))
	f.SetAgents([]Agent{moved(a, "building", iso(f.Now()))})
	if ct := timing(t, f, "c1"); !ct.ClosedAt.IsZero() || ct.last().Stage != "building" {
		t.Fatalf("after reopening = %+v", ct)
	}
}

// New records need no backfill, so a new card causes no long read of the agent files.
func TestNewRecordsAreAlreadyCloseMigrated(t *testing.T) {
	f, _ := newTestFactory(t, fresh("c1"))
	if got := timing(t, f, "c1").CloseVersion; got != closeVersion {
		t.Errorf("CloseVersion = %d", got)
	}
	if f.timings.closeBackfillDue() {
		t.Error("a new record asks for the backfill")
	}
}

func TestObserveKeepsTheCardTitlePrivately(t *testing.T) {
	a := fresh("c1")
	a.Title = "Fix the printer"
	f, _ := newTestFactory(t, a)
	if got := timing(t, f, "c1").Title; got != "Fix the printer" {
		t.Errorf("title = %q", got)
	}
}

// closedFile writes an agent file as Paseo keeps it.
func closedFile(t *testing.T, f *Factory, id, title, note string, archived, updated time.Time) {
	t.Helper()
	path := filepath.Join(f.cfg.Home, ".paseo/agents/test", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"id": id, "title": title, "lastStatus": "closed", "lastActivityAt": iso(updated),
		"archivedAt": iso(archived), "updatedAt": iso(updated), "labels": map[string]string{labelPrefix + labelNote: note}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// oldRecord is a retained record from before close times: Needs Zach for stay, then Done.
func oldRecord(stay time.Duration, done time.Time) *CardTiming {
	return &CardTiming{Version: timingVersion, Source: "board", LandedAt: done.Add(-stay), LandingChecked: true,
		Events: []TimingEvent{{Stage: "zach", At: done.Add(-stay)}, {Stage: "landed", At: done}}}
}

func TestBackfillClosedAt(t *testing.T) {
	parkAt := t0
	updated := parkAt.Add(51 * time.Minute)
	for _, tc := range []struct {
		name      string
		note      string
		stay      time.Duration
		archived  time.Time
		updated   time.Time
		noFile    bool
		wantDone  time.Time
		wantClose time.Time
	}{
		{name: "parked then dropped", note: "closed by Zach", stay: time.Hour, archived: parkAt, updated: updated, wantDone: updated, wantClose: updated},
		{name: "parked then dropped as a duplicate", note: "closed by Zach: duplicate of other", stay: time.Hour, archived: parkAt, updated: updated, wantDone: updated, wantClose: updated},
		{name: "parked then marked done", note: "marked done by Zach", stay: capMinIdle, archived: parkAt, updated: updated, wantDone: updated, wantClose: updated},
		{name: "closed at once", note: "closed by Zach", stay: 15 * time.Minute, archived: parkAt, updated: updated, wantDone: parkAt, wantClose: parkAt},
		{name: "not archived at Done", note: "marked done by Zach", stay: time.Hour, archived: parkAt.Add(5 * time.Minute), updated: updated, wantDone: parkAt, wantClose: parkAt},
		{name: "file not written later", note: "closed by Zach", stay: time.Hour, archived: parkAt, updated: parkAt, wantDone: parkAt, wantClose: parkAt},
		{name: "written in the future", note: "closed by Zach", stay: time.Hour, archived: parkAt, updated: parkAt.Add(48 * time.Hour), wantDone: parkAt, wantClose: parkAt},
		{name: "no note from Zach", note: "landed", stay: time.Hour, archived: parkAt, updated: updated, wantDone: parkAt},
		{name: "no file", noFile: true, stay: time.Hour, wantDone: parkAt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			now := parkAt.Add(24 * time.Hour)
			clockAt(f, now)
			closedFile(t, f, "other", "Another card", "", time.Time{}, parkAt)
			f.timings.cards["c1"] = oldRecord(tc.stay, parkAt)
			if !tc.noFile {
				closedFile(t, f, "c1", "Fix the printer", tc.note, tc.archived, tc.updated)
			}
			f.reconcileTimings()
			ct := timing(t, f, "c1")
			if !ct.doneAt().Equal(tc.wantDone) || !ct.ClosedAt.Equal(tc.wantClose) {
				t.Fatalf("done %v closed %v", ct.doneAt(), ct.ClosedAt)
			}
			if !ct.LandedAt.Equal(parkAt.Add(-tc.stay)) {
				t.Errorf("verified landing moved to %v", ct.LandedAt)
			}
			if ct.CloseVersion != closeVersion {
				t.Errorf("CloseVersion = %d", ct.CloseVersion)
			}
			if want := "Fix the printer"; !tc.noFile && ct.Title != want {
				t.Errorf("title = %q", ct.Title)
			}
		})
	}
}

// The correction runs once: its marker survives a restart, so later writes to a file change nothing.
func TestBackfillRunsOnceAcrossARestart(t *testing.T) {
	f, _ := newTestFactory(t)
	clockAt(f, t0.Add(24*time.Hour))
	f.timings.cards["c1"] = oldRecord(time.Hour, t0)
	first := t0.Add(time.Hour)
	closedFile(t, f, "c1", "Fix the printer", "closed by Zach", t0, first)
	f.timings.mu.Lock()
	f.timings.dirty = true
	f.timings.mu.Unlock()
	f.reconcileTimings()
	if got := timing(t, f, "c1").ClosedAt; !got.Equal(first) {
		t.Fatalf("closed = %v", got)
	}
	closedFile(t, f, "c1", "Fix the printer", "closed by Zach", t0, first.Add(time.Hour))
	f.timings = newTimingStore(f.timings.path, f.Now, f.cfg.Location)
	if f.timings.closeBackfillDue() {
		t.Fatal("still due after a restart")
	}
	f.reconcileTimings()
	if got := timing(t, f, "c1"); !got.ClosedAt.Equal(first) || !got.doneAt().Equal(first) {
		t.Fatalf("second run moved it: %+v", got)
	}
}

// With no agent file read at all the records stay due, since that is a failed read, not lost evidence.
func TestBackfillWaitsWhenNoAgentFilesAreRead(t *testing.T) {
	f, _ := newTestFactory(t)
	clockAt(f, t0.Add(24*time.Hour))
	f.timings.cards["c1"] = oldRecord(time.Hour, t0)
	f.reconcileTimings()
	if got := timing(t, f, "c1").CloseVersion; got != 0 {
		t.Errorf("CloseVersion = %d with no agent files", got)
	}
}

func TestBackfillNeverRunsReadOnly(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.ReadOnly = true
	clockAt(f, t0.Add(24*time.Hour))
	f.timings.cards["c1"] = oldRecord(time.Hour, t0)
	closedFile(t, f, "c1", "Fix the printer", "closed by Zach", t0, t0.Add(time.Hour))
	f.reconcileTimings()
	if got := timing(t, f, "c1"); !got.ClosedAt.IsZero() || got.CloseVersion != 0 || !got.doneAt().Equal(t0) {
		t.Fatalf("read-only run changed it: %+v", got)
	}
}

// asked is a record with n stays in Needs Zach, Done at done.
func asked(title string, n int, done time.Time) *CardTiming {
	ct := &CardTiming{Version: timingVersion, CloseVersion: closeVersion, Source: "board", Title: title}
	for i := range n {
		at := done.Add(time.Duration(i-2*n) * time.Minute)
		ct.Events = append(ct.Events, TimingEvent{Stage: "zach", At: at}, TimingEvent{Stage: "building", At: at.Add(30 * time.Second)})
	}
	ct.Events = append(ct.Events, TimingEvent{Stage: "landed", At: done})
	return ct
}

func askedLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "Asked you 3 or more times") {
			return l
		}
	}
	return ""
}

// The line lists finished cards asked 3 or more times, most asked first, wherever they went: a dropped
// card and one off the board count; 2 asks and an open card do not.
func TestDigestListsCardsAskedOften(t *testing.T) {
	f, m, now := digestFactory(t, 18, 0)
	earlier := now.Add(-time.Hour)
	f.timings.cards["printer"] = asked("Move the server", 3, earlier)
	f.timings.cards["server"] = asked("Fix the printer", 4, earlier)
	f.timings.cards["twice"] = asked("Asked twice", 2, earlier)
	open := asked("Still open", 5, earlier)
	open.Events = open.Events[:len(open.Events)-1]
	f.timings.cards["open"] = open
	f.timings.cards["yesterday"] = asked("Finished yesterday", 5, now.Add(-24*time.Hour))
	f.timings.cards["untitled"] = asked("", 3, earlier)
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %d", len(d))
	}
	want := "Asked you 3 or more times: Fix the printer (4), (untitled) (3), Move the server (3)"
	if got := askedLine(d[0].Args["text"].(string)); got != want {
		t.Errorf("line = %q\nwant  %q", got, want)
	}
	if !strings.Contains(d[0].Args["subject"].(string), "3 asked 3+ times") {
		t.Errorf("subject = %q", d[0].Args["subject"])
	}
	if html := d[0].Args["html"].(string); !strings.Contains(html, "Asked you 3 or more times: Fix the printer (4)") {
		t.Errorf("html lacks the line: %s", html)
	}
}

func TestDigestAskedLineEscapesTitles(t *testing.T) {
	f, m, now := digestFactory(t, 18, 0)
	f.timings.cards["c9"] = asked("Tom & <b>Jerry</b>", 3, now.Add(-time.Hour))
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %d", len(d))
	}
	if html := d[0].Args["html"].(string); strings.Contains(html, "<b>Jerry") || !strings.Contains(html, "&amp;") {
		t.Errorf("html = %s", html)
	}
}

// The line alone is enough to send a digest, so a quiet day does not lose it.
func TestDigestAskedLineAloneSends(t *testing.T) {
	f, _ := newTestFactory(t)
	loc, _ := time.LoadLocation("America/New_York")
	f.cfg.Location = loc
	e, ok := f.digestEmail(Sections{ManyAsks: []AskedCard{{Title: "Fix the printer", Asks: 3}}})
	if !ok || e.Subject != digestSubject+"1 asked 3+ times" || askedLine(e.Text) != "Asked you 3 or more times: Fix the printer (3)" {
		t.Fatalf("ok %v email %+v", ok, e)
	}
}

// A digest sent after 6 PM covers what finished before the send: tonight's lists a card Done between
// 6 PM and the send, and tomorrow's does not list it again. A card Done after the send waits for tomorrow.
func TestDigestLateSendListsEachCardOnce(t *testing.T) {
	f, m, now := digestFactory(t, 19, 30)
	f.timings.cards["before-send"] = asked("Done before the send", 3, now.Add(-30*time.Minute))
	f.tickDigest()
	f.timings.cards["after-send"] = asked("Done after the send", 3, now.Add(time.Minute))
	*now = now.Add(24 * time.Hour)
	f.tickDigest()
	d := digests(m)
	if len(d) != 2 {
		t.Fatalf("digests = %d", len(d))
	}
	if got := askedLine(d[0].Args["text"].(string)); got != "Asked you 3 or more times: Done before the send (3)" {
		t.Errorf("tonight = %q", got)
	}
	if got := askedLine(d[1].Args["text"].(string)); got != "Asked you 3 or more times: Done after the send (3)" {
		t.Errorf("tomorrow = %q", got)
	}
}

// A send that surely failed restores the cutoff, so the retry lists the same cards and no later digest
// lists them again.
func TestDigestFailedSendKeepsTheAskedCards(t *testing.T) {
	f, m, now := digestFactory(t, 18, 0)
	f.timings.cards["c9"] = asked("Fix the printer", 3, now.Add(-time.Minute))
	m.errs["send_email"] = fmt.Errorf("mailbox down")
	f.tickDigest()
	if d, err := f.readDigest(); err != nil || !d.AsksCutoff.IsZero() || d.Sent != "" {
		t.Fatalf("after a failed send: %+v %v", d, err)
	}
	delete(m.errs, "send_email")
	*now = now.Add(mailRetryAfter + time.Second)
	f.tickDigest()
	d := digests(m)
	if len(d) == 0 || askedLine(d[len(d)-1].Args["text"].(string)) != "Asked you 3 or more times: Fix the printer (3)" {
		t.Fatalf("the retry lost the card: %+v", d)
	}
	if st, err := f.readDigest(); err != nil || !st.AsksCutoff.Equal(*now) {
		t.Fatalf("after the retry: %+v %v", st, err)
	}
	sent := len(d)
	*now = now.Add(24 * time.Hour)
	f.tickDigest()
	d = digests(m)
	if len(d) != sent+1 || askedLine(d[len(d)-1].Args["text"].(string)) != "" {
		t.Fatalf("listed again: %+v", d)
	}
}
