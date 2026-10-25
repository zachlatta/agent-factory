package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func health(t *testing.T, f *Factory) (int, Health) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("health body %q: %v", rec.Body, err)
	}
	return rec.Code, h
}

func TestHealthBeforeFirstRefresh(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.Rev = "abc123"
	code, h := health(t, f)
	if code != 503 || h.OK || !h.Live || h.Refreshed || h.Rev != "abc123" {
		t.Errorf("before Refresh: %d %+v, want 503, live, not ok", code, h)
	}
}

func TestHealthOKAfterRefresh(t *testing.T) {
	f, _ := newTestFactory(t)
	f.Refresh()
	code, h := health(t, f)
	if code != 200 || !h.OK || !h.Live || !h.Refreshed {
		t.Errorf("after Refresh: %d %+v, want 200 ok", code, h)
	}
}

// A Paseo outage shows on the board; it must not read as the factory being down, or the
// deployer's watchdog would restart the factory (and page Zach) over a Paseo problem.
func TestHealthRefreshErrorStaysLive(t *testing.T) {
	f, _ := newTestFactory(t)
	f.Refresh()
	f.mu.Lock()
	f.errs[f.cfg.Host] = "open ~/.paseo/agents: permission denied" // what a failed LoadAgents leaves
	f.mu.Unlock()
	code, h := health(t, f)
	if h.RefreshErr == "" {
		t.Errorf("refreshErr not reported: %+v", h)
	}
	if code != 200 || !h.OK || !h.Live {
		t.Errorf("refresh error: %d %+v, want 200 live ok with refreshErr", code, h)
	}
}

func TestHealthLongTick(t *testing.T) {
	f, _ := newTestFactory(t)
	now := t0
	f.Now = func() time.Time { return now }
	f.Refresh()
	f.loopStarted()
	now = t0.Add(14 * time.Minute) // a busy tick: many 90 s paseo calls
	if _, h := health(t, f); !h.Live || h.TickRunningFor == "" {
		t.Errorf("14 min tick: %+v, want live", h)
	}
	now = t0.Add(16 * time.Minute) // hung
	if code, h := health(t, f); h.Live || h.OK || code != 503 {
		t.Errorf("16 min tick: %d %+v, want not live", code, h)
	}
	f.loopDone()
	if _, h := health(t, f); !h.Live {
		t.Errorf("after the loop finished: %+v, want live", h)
	}
	now = now.Add(16 * time.Minute) // no loop for 16 min: the Run goroutine is gone
	if _, h := health(t, f); h.Live {
		t.Errorf("no loop for 16 min: %+v, want not live", h)
	}
}

func TestTickStopsBetweenAgents(t *testing.T) {
	scout := func(id string) Agent { return card(id, map[string]string{labelRole: "scout"}) }
	f, p := newTestFactory(t, scout("s1"), scout("s2"))
	f.Tick()
	if len(p.calls) == 0 {
		t.Fatal("finished scouts made no paseo call; the test needs a card that acts")
	}
	f, p = newTestFactory(t, scout("s1"), scout("s2"))
	f.stopping.Store(true)
	f.Tick()
	if len(p.calls) != 0 {
		t.Errorf("stopping Tick called paseo: %v", p.calls)
	}
}

func TestReadOnlyRunCallsNoPaseo(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review"}))
	f.cfg.ReadOnly = true
	f.cfg.Poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	f.Run(ctx)
	if len(p.calls) != 0 {
		t.Errorf("read-only Run called paseo: %v", p.calls)
	}
	if _, h := health(t, f); !h.Live || h.LastLoopDone == "" {
		t.Errorf("read-only health: %+v, want live with a loop done", h)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/close", strings.NewReader(`{"id":"c1"}`))
	r.Header.Set("X-Factory", "1")
	f.Handler().ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Errorf("read-only POST: %d, want 403", rec.Code)
	}
}

func TestRunReturnsAfterCancel(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.Poll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !f.stopping.Load() {
		t.Error("stopping not set")
	}
}

// Serve lets a request already in flight finish before it returns.
func TestServeDrainsInFlightRequests(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.Poll = time.Hour
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Write([]byte("done"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.Serve(ctx, ln, h, 5*time.Second) }()
	got := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b := make([]byte, 16)
		n, _ := resp.Body.Read(b)
		got <- string(b[:n])
	}()
	<-entered
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned (%v) while a request was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if s := <-got; s != "done" {
		t.Errorf("in-flight request got %q", s)
	}
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the request finished")
	}
}

func TestDeployStatus(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.Rev = "def5678"
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/deploy", nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
			t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
		}
		return m
	}
	if m := get(); m["running"] != "def5678" || m["state"] != nil {
		t.Errorf("no deploy.json: %v", m)
	}
	os.WriteFile(filepath.Join(f.cfg.StateDir, "deploy.json"), []byte(`{"failed":{"rev":"abc1234","stage":"test"}}`), 0o644)
	m := get()
	st, _ := m["state"].(map[string]any)
	if fl, _ := st["failed"].(map[string]any); fl["rev"] != "abc1234" {
		t.Errorf("deploy state: %v", m)
	}
}
