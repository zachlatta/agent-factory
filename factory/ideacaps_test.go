package factory

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setLanes writes a lanes.json for the test factory.
func setLanes(t *testing.T, f *Factory, lanes string) {
	t.Helper()
	f.cfg.LanesFile = filepath.Join(t.TempDir(), "lanes.json")
	if err := os.WriteFile(f.cfg.LanesFile, []byte(lanes), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startedIdeas lists the ideas the factory started, in start order.
func startedIdeas(f *Factory) []Idea {
	var out []Idea
	for _, i := range f.ideas.List() {
		if i.Status == "started" {
			out = append(out, i)
		}
	}
	return out
}

func addIdeaAt(t *testing.T, f *Factory, at time.Time, b Body) Idea {
	t.Helper()
	now := f.Now
	f.Now = func() time.Time { return at }
	defer func() { f.Now = now }()
	i, err := f.addIdea(b)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestIdeasStartByPriorityThenAge(t *testing.T) {
	f, _ := newTestFactory(t)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":5,"daily":5}}}`)
	low := addIdeaAt(t, f, t0.Add(-3*time.Hour), Body{Request: "low one", Priority: "low"})
	old := addIdeaAt(t, f, t0.Add(-2*time.Hour), Body{Request: "old normal"})
	young := addIdeaAt(t, f, t0.Add(-time.Hour), Body{Request: "young normal", Priority: "normal"})
	high := addIdeaAt(t, f, t0.Add(-time.Minute), Body{Request: "high one", Priority: "HIGH"})
	if old.Priority != "normal" || high.Priority != "high" {
		t.Fatalf("priorities: old %q high %q", old.Priority, high.Priority)
	}
	// the board lists the line in start order, numbered per repo
	var got []string
	for _, c := range BuildBoard(nil, f.ideas.List(), t0) {
		got = append(got, c.ID)
		if c.Idea.Priority == "" {
			t.Errorf("%s: no priority on the board", c.ID)
		}
	}
	if want := []string{high.ID, old.ID, young.ID, low.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("board order = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for n, want := range []string{high.ID, old.ID, young.ID, low.ID} {
		if err := f.tickIdeas(); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, i := range startedIdeas(f) {
			if !seen[i.ID] {
				got = append(got, i.ID)
				seen[i.ID] = true
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("tick %d started %v, want %s", n, got, want)
		}
	}
}

func TestIdeaStartOrderIsPriorityThenAge(t *testing.T) {
	ideas := []Idea{
		{ID: "low", Priority: "low", CreatedAt: t0.Add(-3 * time.Hour)},
		{ID: "old", CreatedAt: t0.Add(-2 * time.Hour)},
		{ID: "young", Priority: "normal", CreatedAt: t0.Add(-time.Hour)},
		{ID: "high", Priority: "high", CreatedAt: t0},
	}
	ideaOrder(ideas)
	var got []string
	for _, i := range ideas {
		got = append(got, i.ID)
	}
	if strings.Join(got, ",") != "high,old,young,low" {
		t.Fatalf("order = %v", got)
	}
}

func TestIdeaCapsArePerRepo(t *testing.T) {
	f, p := newTestFactory(t)
	setLanes(t, f, `{"example/a":{"name":"a","ideas":{"inFlight":1}},"example/off":{"name":"off","ideas":{"inFlight":0}},"example/b":{"name":"b"}}`)
	a1 := addIdeaAt(t, f, t0.Add(-4*time.Hour), Body{Request: "a one", Repo: "example/a"})
	addIdeaAt(t, f, t0.Add(-3*time.Hour), Body{Request: "a two", Repo: "example/a"})
	addIdeaAt(t, f, t0.Add(-2*time.Hour), Body{Request: "never", Repo: "example/off", Priority: "high"})
	addIdeaAt(t, f, t0.Add(-time.Hour), Body{Request: "b one", Repo: "example/b"})
	for range 5 {
		if err := f.tickIdeas(); err != nil {
			t.Fatal(err)
		}
	}
	repos := map[string]int{}
	for _, i := range startedIdeas(f) {
		repos[i.Repo]++
	}
	if repos["example/a"] != 1 || repos["example/b"] != 1 || repos["example/off"] != 0 {
		t.Fatalf("started per repo = %v", repos)
	}
	// a's card finishes: its slot frees, while b (default caps) is unaffected
	var a1card string
	for _, i := range startedIdeas(f) {
		if i.ID == a1.ID {
			a1card = i.Card
		}
	}
	f.SetAgents([]Agent{card(a1card, map[string]string{labelStage: "landed", labelOrigin: "idea"})})
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 3 {
		t.Fatalf("after a's card landed: %d idea cards started, want 3", n)
	}
}

func TestLaneIdeaCapsDefaults(t *testing.T) {
	zero, three := 0, 3
	for _, c := range []struct {
		lane Lane
		want RepoIdeaCaps
	}{
		{Lane{}, RepoIdeaCaps{InFlight: 2, Daily: 5}},
		{Lane{Ideas: &IdeaCaps{}}, RepoIdeaCaps{InFlight: 2, Daily: 5}},
		{Lane{Ideas: &IdeaCaps{InFlight: &three}}, RepoIdeaCaps{InFlight: 3, Daily: 5}},
		{Lane{Ideas: &IdeaCaps{Daily: &zero}}, RepoIdeaCaps{InFlight: 2, Daily: 0}},
	} {
		if got := c.lane.ideaCaps(); got != c.want {
			t.Errorf("%+v: caps = %+v, want %+v", c.lane.Ideas, got, c.want)
		}
	}
	var l Lane
	if err := json.Unmarshal([]byte(`{"name":"x","ideas":{"inFlight":0,"daily":-1}}`), &l); err != nil {
		t.Fatal(err)
	}
	if got := l.ideaCaps(); got != (RepoIdeaCaps{}) {
		t.Errorf("0 and negative caps = %+v, want never", got)
	}
}

func usageAt(pct int) AgentUsage {
	in3d := t0.Add(72 * time.Hour).UTC().Format(time.RFC3339)
	return AgentUsage{Summary: "x", Windows: []UsageWindow{{Label: "7d", RemainingPct: pct, ResetsAt: in3d}, {Label: "5h", RemainingPct: 90}}}
}

func TestAutoStartHoldsAtLowWeeklyUsage(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.waitProviderStatus("claude"); err != nil { // the suggestion ranks listed providers
		t.Fatal(err)
	}
	if _, err := f.addIdea(Body{Request: "an idea"}); err != nil {
		t.Fatal(err)
	}
	all := func(pct int) map[string]AgentUsage {
		return map[string]AgentUsage{"claude": usageAt(pct), "codex": usageAt(pct), "antigravity": usageAt(pct), "muse": usageAt(pct)}
	}
	seedUsage(f, all(20))
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 0 {
		t.Fatalf("started %d at 20%% weekly usage left", n)
	}
	if h := f.ideaHold(); !strings.Contains(h, "20% of its weekly usage") {
		t.Errorf("hold = %q", h)
	}
	// the hold checks the agent the start would use, which is the usage-balanced suggestion
	low := all(90)
	low["claude"] = usageAt(10)
	seedUsage(f, low)
	if f.ideaHold() != "" {
		t.Errorf("held although the suggested agent (%s) has usage left", f.suggestedAgent())
	}
	seedUsage(f, all(21))
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("started %d at 21%%, want 1", n)
	}
	// Start now is not held
	seedUsage(f, all(5))
	i, _ := f.addIdea(Body{Request: "by hand"})
	if _, err := f.Act("approve", Body{ID: i.ID}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 2 {
		t.Fatalf("manual start while held: %d", n)
	}
}

func TestAutoStartRunsWithUnknownUsage(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.addIdea(Body{Request: "an idea"}); err != nil {
		t.Fatal(err)
	}
	if h := f.ideaHold(); h != "" {
		t.Fatalf("unknown usage held: %q", h)
	}
	if err := f.tickIdeas(); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("started %d with unknown usage", n)
	}
}

func TestIdeaAPIPriorityAndSchedule(t *testing.T) {
	f, _ := newTestFactory(t)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/idea", strings.NewReader(body))
		req.Header.Set("X-Factory", "1")
		rec := httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"request":"x","priority":"urgent"}`); rec.Code < 400 || !strings.Contains(rec.Body.String(), "priority") {
		t.Fatalf("bad priority: %d %s", rec.Code, rec.Body)
	}
	rec := post(`{"request":"weekly fixes","priority":"low","schedule":"Example self improve","by":"ignored"}`)
	if rec.Code != 200 {
		t.Fatalf("schedule idea: %d %s", rec.Code, rec.Body)
	}
	var i Idea
	for _, x := range f.ideas.List() {
		if x.Request == "weekly fixes" {
			i = x
		}
	}
	if i.Schedule != "Example self improve" || i.By != "schedule Example self improve" || i.Priority != "low" {
		t.Fatalf("idea = %+v", i)
	}
}

func TestBoardSendsIdeaCapsAndHold(t *testing.T) {
	f, _ := newTestFactory(t)
	setLanes(t, f, `{"example/off":{"name":"off","ideas":{"daily":0}}}`)
	f.addIdea(Body{Request: "one", Repo: "example/off"})
	f.addIdea(Body{Request: "two", Repo: "example/desk"})
	f.Refresh()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var b struct {
		IdeaCaps map[string]RepoIdeaCaps `json:"ideaCaps"`
		IdeaHold string                  `json:"ideaHold"`
		Caps     map[string]int          `json:"caps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.IdeaCaps["example/off"] != (RepoIdeaCaps{InFlight: 2, Daily: 0}) || b.IdeaCaps["example/desk"] != (RepoIdeaCaps{InFlight: 2, Daily: 5}) {
		t.Errorf("ideaCaps = %+v", b.IdeaCaps)
	}
	if _, ok := b.Caps["ideaWip"]; ok {
		t.Errorf("global idea caps still sent: %v", b.Caps)
	}
	seedUsage(f, map[string]AgentUsage{"claude": usageAt(3), "codex": usageAt(3), "antigravity": usageAt(3), "muse": usageAt(3)})
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	json.Unmarshal(rec.Body.Bytes(), &b)
	if !strings.Contains(b.IdeaHold, "3% of its weekly usage") {
		t.Errorf("ideaHold = %q", b.IdeaHold)
	}
}
