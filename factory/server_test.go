package factory

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServer(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "zach"}))
	h := f.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var b struct {
		Cards  []Card          `json:"cards"`
		Stages [][3]string     `json:"stages"`
		Tiers  json.RawMessage `json:"tiers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != 200 {
		t.Fatalf("board: %d %s", rec.Code, rec.Body)
	}
	if len(b.Cards) != 1 || b.Stages[0][0] != "idea" || len(b.Tiers) == 0 {
		t.Errorf("board = %+v", b)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<h1>Factory</h1>") {
		t.Errorf("index: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/t/c1", nil))
	if rec.Code != 200 {
		t.Errorf("transcript page: %d", rec.Code)
	}

	// POSTs need the X-Factory header (blocks cross-site form posts)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/idea", strings.NewReader(`{"request":"x"}`)))
	if rec.Code != 403 {
		t.Errorf("no header: %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/api/idea", strings.NewReader(`{"request":"Try it","by":"test"}`))
	req.Header.Set("X-Factory", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"waiting"`) {
		t.Errorf("idea: %d %s", rec.Code, rec.Body)
	}

	// GET /api/ideas lists every status, newest first, request truncated, no header needed
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	long, _ := f.Act("idea", Body{Title: "Long one", Request: strings.Repeat("x", 500)})
	f.Act("archive", Body{ID: long.(Idea).ID})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/ideas", nil))
	var il struct {
		Ideas []Idea `json:"ideas"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &il); err != nil || rec.Code != 200 {
		t.Fatalf("ideas: %d %s", rec.Code, rec.Body)
	}
	if len(il.Ideas) != 2 || il.Ideas[0].Title != "Long one" || il.Ideas[0].Status != "dismissed" ||
		len(il.Ideas[0].Request) != 300 || il.Ideas[1].Title != "Try it" {
		t.Errorf("ideas = %+v", il.Ideas)
	}
	if got := f.ideas.List(); len(got[1].Request) != 500 {
		t.Errorf("stored request changed: %d", len(got[1].Request))
	}

	// GET /api/logs/<idea id> carries the whole idea, request uncut, for the idea panel
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs/"+long.(Idea).ID, nil))
	var lg struct {
		Idea Idea `json:"idea"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lg); err != nil || rec.Code != 200 || len(lg.Idea.Request) != 500 {
		t.Errorf("idea logs: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/approve", strings.NewReader(`{"id":"nope"}`))
	req.Header.Set("X-Factory", "1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("unknown card: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("404: %d", rec.Code)
	}
}

// every action a Needs Zach button posts is routed (rerun was missing: its button got a 404)
func TestServerRoutesEveryAskAction(t *testing.T) {
	f, p := newDiskFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
		labelVerdict: "code changes", labelPlanRounds: "1", labelCodeRounds: "3"}),
		card("c2", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "review"}))
	h := f.Handler()
	for _, tc := range []struct{ action, id string }{{"recommend", "c1"}, {"rerun", "c2"}} {
		req := httptest.NewRequest("POST", "/api/"+tc.action, strings.NewReader(`{"id":"`+tc.id+`"}`))
		req.Header.Set("X-Factory", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: %d %s", tc.action, rec.Code, rec.Body)
		}
	}
	if p.labels("c1")[labelDecided] != "1" || len(p.runsWithLabel(labelArg(labelReviewOf, "c2"))) != 1 {
		t.Errorf("calls = %v", p.calls)
	}
}

// On a phone a text field left focused brings the keyboard back (tab resumed, window
// refocused), so both pages drop focus when the page hides or the keyboard closes.
func TestPagesReleaseKeyboard(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", nil))
	h := f.Handler()
	for _, path := range []string{"/", "/t/c1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "function releaseKeyboard(") {
			t.Errorf("%s: %d, no releaseKeyboard hook", path, rec.Code)
		}
	}
}

// Gzip covers only the allowlisted GET routes; anything else (actions today, a future
// streaming route) is served plain unless it opts in.
func TestGzipAllowlist(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "plan"}))
	h := f.Handler()
	get := func(path string, gz bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/", "/api/board", "/api/agents"} {
		rec := get(path, true)
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: code %d, encoding %q", path, rec.Code, rec.Header().Get("Content-Encoding"))
			continue
		}
		r, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		plain, err := io.ReadAll(r)
		if err != nil || len(plain) == 0 {
			t.Errorf("%s: gzip round-trip failed (err %v)", path, err)
		} else if path == "/api/agents" { // its body changes as background refreshes land
			if !strings.Contains(string(plain), `"agents"`) {
				t.Errorf("%s: decoded body = %s", path, plain)
			}
		} else if string(plain) != get(path, false).Body.String() {
			t.Errorf("%s: gzip round-trip mismatch", path)
		}
		if rec.Body.Len() >= len(plain) {
			t.Errorf("%s: gzipped %d bytes, plain %d; want smaller", path, rec.Body.Len(), len(plain))
		}
	}
	// No Accept-Encoding, no gzip; POST actions are never gzipped.
	if rec := get("/", false); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("plain GET / encoded %q", rec.Header().Get("Content-Encoding"))
	}
	req := httptest.NewRequest("POST", "/api/idea", strings.NewReader(`{"request":"x","by":"test"}`))
	req.Header.Set("X-Factory", "1")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("POST /api/idea encoded %q", rec.Header().Get("Content-Encoding"))
	}
}

// A handler behind the gzip writer that flushes must stream: the first chunk reaches
// the client before the handler returns. Guards any future streaming route that opts in.
func TestGzipFlushStreams(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(gzipHandler(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "second")
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding %q", resp.Header.Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 5)
		n, _ := io.ReadFull(gz, buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if s != "first" {
			t.Errorf("first chunk = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk did not stream before the handler returned")
	}
	close(release)
	rest, _ := io.ReadAll(gz)
	if string(rest) != "second" {
		t.Errorf("rest = %q", rest)
	}
}

func benchFactory(b *testing.B, cards int) *Factory {
	b.Helper()
	p := &fakePaseo{logs: map[string]string{}, archived: map[string]bool{}, status: map[string]string{},
		busy: map[string]int{}, providers: map[string]string{}, fixed: map[string]bool{},
		cwd: map[string]string{}, unknown: map[string]bool{}}
	f := New(Config{Host: "host1", Home: b.TempDir(), StateDir: b.TempDir(), DefaultRepo: "example/desk"}, p)
	f.Now = func() time.Time { return t0 }
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }
	var agents []Agent
	for i := range cards {
		agents = append(agents, card(fmt.Sprintf("c%d", i), map[string]string{labelStage: "plan"}))
	}
	f.SetAgents(agents)
	return f
}

func BenchmarkHandleBoard(b *testing.B) {
	f := benchFactory(b, 200)
	h := f.Handler()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
		if rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

func BenchmarkHandleAgents(b *testing.B) {
	f := benchFactory(b, 0)
	if _, err := f.resolveAgent("opus"); err != nil { // warm the provider cache, as Run does
		b.Fatal(err)
	}
	h := f.Handler()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/agents", nil))
		if rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

// POST /api/seen marks an unread card seen with a factory.seen label, rebuilds the board so the
// next GET shows it read, and never kicks the loop. A card that is not unread costs no paseo call.
func TestServerSeen(t *testing.T) {
	unread := card("c1", map[string]string{labelStage: "zach"})
	unread.RequiresAttention, unread.AttentionReason, unread.LastUserMessageAt = true, "finished", t0.Add(-10*time.Minute)
	read := card("c2", map[string]string{labelStage: "zach"})
	f, p := newDiskFactory(t, unread, read)
	h := f.Handler()
	seen := func(id string, header bool) int {
		req := httptest.NewRequest("POST", "/api/seen", strings.NewReader(`{"id":"`+id+`"}`))
		if header {
			req.Header.Set("X-Factory", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if !boardCard(t, f, "c1").Unread {
		t.Fatal("c1 should start unread")
	}
	if code := seen("c1", false); code != 403 {
		t.Errorf("no X-Factory header: %d, want 403", code)
	}
	if code := seen("nope", true); code != 404 {
		t.Errorf("unknown card: %d, want 404", code)
	}
	if code := seen("c1", true); code != 200 {
		t.Fatalf("seen c1: %d", code)
	}
	if got := p.labels("c1")[labelSeen]; got != strconv.FormatInt(t0.Unix(), 10) {
		t.Errorf("factory.seen = %q, want %d (calls %v)", got, t0.Unix(), p.calls)
	}
	if boardCard(t, f, "c1").Unread {
		t.Error("c1 still unread after seen")
	}
	if len(f.kick) != 0 {
		t.Error("seen kicked the loop")
	}
	n := len(p.calls)
	if code := seen("c2", true); code != 200 || len(p.calls) != n {
		t.Errorf("seen on a read card: %d, calls %v", code, p.calls[n:])
	}
	f.cfg.ReadOnly = true
	if code := seen("c1", true); code != 403 || len(p.calls) != n {
		t.Errorf("read-only: %d, calls %v", code, p.calls[n:])
	}
}
