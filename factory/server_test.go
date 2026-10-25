package factory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Agent factory") {
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
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelNote: "rework cap reached",
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
