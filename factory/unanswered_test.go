package factory

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// writeUnanswered writes the dispatcher's file of flagged requests for f.
func writeUnanswered(t *testing.T, f *Factory, reqs ...UnansweredRequest) {
	t.Helper()
	f.cfg.Unanswered = filepath.Join(t.TempDir(), "unanswered.json")
	b, _ := json.Marshal(unansweredFile{Updated: t0.Format("2006-01-02T15:04:05Z"), Requests: reqs})
	if err := os.WriteFile(f.cfg.Unanswered, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

var (
	batteryReq = UnansweredRequest{Key: "m1", Source: "email", Thread: "t1", Asked: "2026-10-04T13:00:00Z", Subject: "Order batteries", Why: "the agent stopped without replying"}
	reportReq  = UnansweredRequest{Key: "voice:rec-2", Source: "voice", Thread: "t2", Asked: "2026-10-04T12:00:00Z", Subject: "Rename the weekly report", Why: "no reply in the thread"}
)

func TestUnansweredRequestsFillNoAnswer(t *testing.T) {
	f, _ := newTestFactory(t)
	writeUnanswered(t, f, batteryReq, reportReq)
	s := f.sections()
	if len(s.NoAnswer) != 2 || s.NoAnswer[0].Key != "voice:rec-2" || s.NoAnswer[1].Title != "Order batteries" {
		t.Fatalf("noAnswer = %+v", s.NoAnswer)
	}
	if s.NoAnswer[0].ID != "" || s.NoAnswer[0].State != "voice memo, no answer yet: no reply in the thread" ||
		s.NoAnswer[1].State != "no answer yet: the agent stopped without replying" {
		t.Errorf("lines = %+v", s.NoAnswer)
	}
}

func TestUnansweredMissingOrBadFileShowsNothing(t *testing.T) {
	f, _ := newTestFactory(t)
	if got := f.sections().NoAnswer; len(got) != 0 {
		t.Errorf("no file set: %v", got)
	}
	f.cfg.Unanswered = filepath.Join(t.TempDir(), "missing.json")
	if got := f.sections().NoAnswer; len(got) != 0 {
		t.Errorf("missing file: %v", got)
	}
	os.WriteFile(f.cfg.Unanswered, []byte("{not json"), 0o644)
	if got := f.sections().NoAnswer; len(got) != 0 {
		t.Errorf("bad file: %v", got)
	}
}

func TestDismissRequestHidesItAndNeverWritesTheDispatchersFile(t *testing.T) {
	f, _ := newTestFactory(t)
	writeUnanswered(t, f, batteryReq, reportReq)
	before, _ := os.ReadFile(f.cfg.Unanswered)
	if _, err := f.Act("dismiss-request", Body{ID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.unanswered(); len(got) != 1 || got[0].Key != "voice:rec-2" {
		t.Errorf("after dismiss = %+v", got)
	}
	after, _ := os.ReadFile(f.cfg.Unanswered)
	if string(before) != string(after) {
		t.Error("the dispatcher's file changed")
	}
	// the dispatcher rewrites its file on its next sweep: the dismissal holds
	writeUnanswered(t, f, batteryReq, reportReq)
	if got := f.unanswered(); len(got) != 1 {
		t.Errorf("dismissal lost after a rewrite: %+v", got)
	}
	if _, err := f.Act("dismiss-request", Body{ID: "nope"}); err == nil {
		t.Error("dismissed an unknown request")
	}
}

func TestUnansweredEndpoint(t *testing.T) {
	f, _ := newTestFactory(t)
	writeUnanswered(t, f, batteryReq)
	w := httptest.NewRecorder()
	f.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/unanswered", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"subject":"Order batteries"`) {
		t.Errorf("GET /api/unanswered = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dismiss-request", strings.NewReader(`{"id":"m1"}`))
	r.Header.Set("X-Factory", "1")
	f.Handler().ServeHTTP(w, r)
	if w.Code != 200 || len(f.unanswered()) != 0 {
		t.Errorf("POST /api/dismiss-request = %d %s", w.Code, w.Body.String())
	}
}

// noAnswerKeys is each line of a No answer yet list as key, card, title and state, for comparing lists.
func noAnswerKeys(ls []SectionLine) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Key+"|"+l.ID+"|"+l.Title+"|"+l.State)
	}
	sort.Strings(out)
	return out
}

// A request dismissed on the board leaves the board's No answer yet list and the daily digest alike, and
// stays dismissed after a factory restart, the dispatcher's next rewrite, and past a month while the
// dispatcher still lists it. The card that ended without a reply is not a second line for the same request.
func TestDismissedRequestLeavesBoardAndDigestAlike(t *testing.T) {
	f, m, now := digestFactory(t, 17, 0)
	path := filepath.Join(t.TempDir(), "unanswered.json")
	f.cfg.Unanswered = path
	rewrite := func(reqs ...UnansweredRequest) {
		t.Helper()
		b, _ := json.Marshal(unansweredFile{Updated: now.Format(time.RFC3339), Requests: reqs})
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(batteryReq, reportReq)
	ended := now.Add(-time.Hour).Format(time.RFC3339)
	noReply := Card{ID: "e2", Short: "e2", Title: "Order batteries", Stage: "landed", Source: "email",
		Timing: &TimingView{NoReply: true, Stages: []StageSpan{{Stage: "landed", Start: ended}}}}
	withCard := func(g *Factory) {
		g.mu.Lock()
		g.cards = append(append([]Card{}, g.cards...), noReply)
		g.mu.Unlock()
		g.recordShipped()
	}
	withCard(f)
	same := func(g *Factory, when string) Sections {
		t.Helper()
		d, err := g.readDigest()
		if err != nil {
			t.Fatal(err)
		}
		board, digest := g.sections(), g.digestSections(d, g.Now())
		if b, dg := noAnswerKeys(board.NoAnswer), noAnswerKeys(digest.NoAnswer); !slices.Equal(b, dg) {
			t.Errorf("%s: board and digest differ:\nboard  %v\ndigest %v", when, b, dg)
		}
		for _, l := range board.NoAnswer {
			if l.Key == "m1" || l.ID == "e2" {
				t.Errorf("%s: the dismissed request is listed: %+v", when, l)
			}
		}
		if !slices.ContainsFunc(board.NoAnswer, func(l SectionLine) bool { return l.Key == reportReq.Key }) {
			t.Errorf("%s: the other request is gone: %+v", when, board.NoAnswer)
		}
		return board
	}
	// Dismiss as the board sends it
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dismiss-request", strings.NewReader(`{"id":"m1"}`))
	r.Header.Set("X-Factory", "1")
	f.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("dismiss = %d %s", w.Code, w.Body.String())
	}
	same(f, "after Dismiss")

	// a restart reads the factory's own store
	g := New(f.cfg, f.p)
	g.Mail, g.Now = m, f.Now
	g.SetAgents(f.agents())
	withCard(g)
	same(g, "after a restart")
	// the dispatcher rewrites its file, still listing the request, for over a month
	*now = now.Add(40 * 24 * time.Hour)
	rewrite(batteryReq, reportReq)
	if _, err := g.Act("dismiss-request", Body{ID: reportReq.Key}); err != nil {
		t.Fatal(err) // a later dismissal prunes the store
	}
	rewrite(batteryReq, reportReq, UnansweredRequest{Key: "m3", Source: "email", Asked: now.Format(time.RFC3339), Subject: "Book the room"})
	if got := noAnswerKeys(g.sections().NoAnswer); !slices.Equal(got, []string{"m3||Book the room|no answer yet"}) {
		t.Errorf("a month later = %v, want only the new request", got)
	}
	// the evening's digest says the same as the board
	*now = time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, now.Location())
	g.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %d", len(d))
	}
	text := d[0].Args["text"].(string)
	if !strings.Contains(text, "Book the room") || strings.Contains(text, "Order batteries") || strings.Contains(text, "Rename the weekly report") {
		t.Errorf("digest:\n%s", text)
	}
}

// Each request's line says why it has no answer, in the dispatcher's plain words, on the board and in
// the digest alike; a request with no reason keeps the plain state. Keys and threads are never shown.
func TestUnansweredLinesShowTheirReason(t *testing.T) {
	f, m, _ := digestFactory(t, 18, 0)
	old := UnansweredRequest{Key: "msg-old-7", Source: "email", Thread: "thread-old-7", Asked: "2026-10-01T13:00:00Z",
		Subject: "Renew the domain", Why: "from before the watchdog started"}
	bare := UnansweredRequest{Key: "msg-bare-8", Source: "email", Thread: "thread-bare-8", Asked: "2026-10-02T13:00:00Z", Subject: "Book the room"}
	writeUnanswered(t, f, old, reportReq, bare)
	want := map[string]string{
		"Renew the domain":         "no answer yet: from before the watchdog started",
		"Rename the weekly report": "voice memo, no answer yet: no reply in the thread",
		"Book the room":            "no answer yet",
	}
	for _, l := range f.sections().NoAnswer {
		if w, ok := want[l.Title]; ok && l.State != w {
			t.Errorf("%s: state %q, want %q", l.Title, l.State, w)
		}
	}
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %d", len(d))
	}
	text, html := d[0].Args["text"].(string), d[0].Args["html"].(string)
	for title, state := range want {
		if !strings.Contains(text, title+": "+state) {
			t.Errorf("digest lacks %q:\n%s", title+": "+state, text)
		}
	}
	for _, secret := range []string{"msg-old-7", "thread-old-7", "msg-bare-8", "thread-bare-8", "voice:rec-2"} {
		if strings.Contains(text, secret) || strings.Contains(html, secret) {
			t.Errorf("digest shows %q:\n%s", secret, text)
		}
	}
}
