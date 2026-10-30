package factory

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// diskPaseo is the fake paseo plus Paseo's agent files: `agent update` writes the labels to the
// agent's file, as Paseo does before the CLI returns, and `permit ls` can be held on a channel.
type diskPaseo struct {
	*fakePaseo
	home        string
	permitStart chan struct{} // when non-nil, `permit ls` signals here, then waits for permitGo
	permitGo    chan struct{}
	mu          sync.Mutex
}

func (d *diskPaseo) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "permit" && d.permitStart != nil {
		d.permitStart <- struct{}{}
		<-d.permitGo
	}
	out, err := d.fakePaseo.Run(ctx, args...)
	if err == nil && len(args) > 2 && args[0] == "agent" && args[1] == "update" {
		d.mu.Lock()
		defer d.mu.Unlock()
		path := agentFile(d.home, args[2])
		var raw map[string]any
		b, _ := os.ReadFile(path)
		json.Unmarshal(b, &raw)
		labels, _ := raw["labels"].(map[string]any)
		for i := 3; i+1 < len(args); i++ {
			if args[i] == "--label" {
				k, v, _ := strings.Cut(args[i+1], "=")
				labels[k] = v
			}
		}
		b, _ = json.Marshal(raw)
		os.WriteFile(path, b, 0o644)
	}
	return out, err
}

func agentFile(home, id string) string { return filepath.Join(home, ".paseo/agents", id, id+".json") }

// writeAgents saves agents as Paseo agent files under f's home, so a reload reads them back.
func writeAgents(t *testing.T, f *Factory, agents ...Agent) {
	t.Helper()
	for _, a := range agents {
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		b, _ := json.Marshal(map[string]any{"id": a.ID, "title": a.Title, "cwd": a.Cwd, "lastStatus": a.Status,
			"createdAt": iso(a.CreatedAt), "lastActivityAt": iso(a.LastActivityAt), "labels": labels,
			"requiresAttention": a.RequiresAttention, "attentionReason": a.AttentionReason,
			"lastUserMessageAt": iso(a.LastUserMessageAt), "provider": a.Provider,
			"persistence": map[string]string{"sessionId": a.SessionID}})
		writeFile(t, agentFile(f.cfg.Home, a.ID), string(b))
	}
}

// newDiskFactory is a test factory whose agents live in files and whose paseo updates them.
func newDiskFactory(t *testing.T, agents ...Agent) (*Factory, *diskPaseo) {
	t.Helper()
	f, p := newTestFactory(t, agents...)
	d := &diskPaseo{fakePaseo: p, home: f.cfg.Home}
	f.p = d
	writeAgents(t, f, agents...)
	return f, d
}

func boardCard(t *testing.T, f *Factory, id string) Card {
	t.Helper()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var b struct{ Cards []Card }
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Cards {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("card %s not on the board", id)
	return Card{}
}

func post(t *testing.T, f *Factory, action, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/"+action, strings.NewReader(body))
	req.Header.Set("X-Factory", "1")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", action, rec.Code, rec.Body)
	}
}

func zachCard() Agent {
	return card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelVerdict: "code approve",
		labelNote: "review passed: approve to land", labelPlanRounds: "1", labelCodeRounds: "1", labelReviewed: reviewedSHA})
}

// A button press shows on the next board read, not after the factory's next loop.
func TestActionShowsOnBoardAtOnce(t *testing.T) {
	f, _ := newDiskFactory(t, zachCard())
	headIs(f, reviewedSHA, nil)
	if c := boardCard(t, f, "c1"); c.Stage != "zach" {
		t.Fatalf("before: stage %s", c.Stage)
	}
	post(t, f, "approve", `{"id":"c1"}`)
	if c := boardCard(t, f, "c1"); c.Stage != "landing" {
		t.Errorf("after approve: stage %s, want landing", c.Stage)
	}
}

// A loop Refresh that read the files before a press and stores after it must not put the old
// column back, and its fetched permissions still reach the board.
func TestStaleRefreshKeepsActionOnBoard(t *testing.T) {
	c := zachCard()
	c.Status = "running" // so Refresh reads permissions
	f, p := newDiskFactory(t, c)
	headIs(f, reviewedSHA, nil)
	p.permits = `[{"agentId":"c1","name":"Bash","description":"ls"}]`
	p.permitStart, p.permitGo = make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { f.Refresh(); close(done) }()
	<-p.permitStart // Refresh has read the files: c1 is in Needs Zach there
	if _, err := f.Act("approve", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.reload(); err != nil {
		t.Fatal(err)
	}
	close(p.permitGo)
	<-done
	a := f.agent("c1")
	if a.L(labelStage) != "landing" {
		t.Errorf("stale refresh moved c1 back to %s", a.L(labelStage))
	}
	if a.Pending != "Bash: ls" {
		t.Errorf("pending = %q, want the refresh's permissions", a.Pending)
	}
}

func TestOlderReadIsNotStored(t *testing.T) {
	f, _ := newTestFactory(t, card("c1", map[string]string{labelStage: "plan"}))
	old := f.nextRead()
	f.SetAgents([]Agent{card("c1", map[string]string{labelStage: "building"})})
	if f.setAgents([]Agent{card("c1", map[string]string{labelStage: "plan"})}, old, true) {
		t.Error("older read stored over a newer one")
	}
	if st := f.agent("c1").L(labelStage); st != "building" {
		t.Errorf("stage = %s", st)
	}
	// markArchived rebuilds the stored list under its own read number: it stores
	f.markArchived("c1")
	if !f.agent("c1").archived() {
		t.Error("markArchived did not store")
	}
}

// reload makes no paseo calls: no permissions read, no Needs Zach detail fetched, and it does not
// count as the loop's first refresh for /api/health.
func TestReloadMakesNoPaseoCalls(t *testing.T) {
	c := zachCard()
	c.Status = "running"
	f, p := newDiskFactory(t, c)
	f.mu.Lock()
	f.list[0].Pending = "Bash: ls"
	f.mu.Unlock()
	writeAgents(t, f, func() Agent { c.LastActivityAt = t0; return c }()) // new activity: detail key is stale
	before := p.countCalls("", "")
	if err := f.reload(); err != nil {
		t.Fatal(err)
	}
	if n := p.countCalls("", "") - before; n != 0 {
		t.Errorf("reload made %d paseo calls: %v", n, p.calls[before:])
	}
	if a := f.agent("c1"); a.Pending != "Bash: ls" || !a.LastActivityAt.Equal(t0) {
		t.Errorf("after reload: pending %q, last activity %v", a.Pending, a.LastActivityAt)
	}
	f.hmu.Lock()
	refreshed := f.refreshed
	f.hmu.Unlock()
	if refreshed {
		t.Error("reload marked the factory refreshed")
	}
	// archiving from a board action makes no logs call either
	before = p.countCalls("", "")
	if err := f.archiveAgent("c1"); err != nil {
		t.Fatal(err)
	}
	for _, call := range p.calls[before:] {
		if call[0] == "logs" {
			t.Errorf("archive fetched detail: %v", call)
		}
	}
}

// Without fetch, a Needs Zach card keeps its cached detail even under a stale key, and the cache
// is left for the loop to refresh.
func TestReloadUsesCachedDetail(t *testing.T) {
	c := zachCard()
	f, p := newDiskFactory(t)
	storeLog(t, f, p.fakePaseo, "c1", "Old detail")
	f.SetAgents([]Agent{c})
	if got, _ := f.card("c1"); got.Ask == nil || !strings.Contains(got.Ask.Detail, "Old detail") {
		t.Fatalf("cached detail not fetched: %+v", got.Ask)
	}
	storeLog(t, f, p.fakePaseo, "c1", "New detail")
	c.LastActivityAt = t0
	writeAgents(t, f, c)
	if err := f.reload(); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.card("c1"); !strings.Contains(got.Ask.Detail, "Old detail") {
		t.Errorf("reload detail = %q, want the cached one", got.Ask.Detail)
	}
	f.mu.Lock()
	key := f.details["c1"].key
	f.mu.Unlock()
	if key.last == iso(t0) {
		t.Error("reload rewrote the detail cache")
	}
	f.Refresh()
	if got, _ := f.card("c1"); !strings.Contains(got.Ask.Detail, "New detail") {
		t.Errorf("loop refresh detail = %q, want the new one", got.Ask.Detail)
	}
}

func TestArchiveIdleAgentSkipsStop(t *testing.T) {
	f, p := newTestFactory(t, card("c1", nil))
	if err := f.archiveAgent("c1"); err != nil {
		t.Fatal(err)
	}
	if v := p.verbs("c1"); !slices.Equal(v, []string{"inspect", "archive", "inspect"}) {
		t.Errorf("paseo calls on c1 = %v, want inspect, archive, inspect without stop", v)
	}
}
