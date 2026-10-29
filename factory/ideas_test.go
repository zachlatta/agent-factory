package factory

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdeasAddDedupeDismiss(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "Make tests faster", By: "scout", Tier: "low"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if idea.Status != "waiting" || idea.Title != "Make tests faster" || idea.Tier != "low" {
		t.Errorf("idea = %+v", idea)
	}
	if _, err := f.Act("idea", Body{Request: "Make tests faster"}); err == nil {
		t.Error("duplicate waiting idea accepted")
	}
	if _, err := f.Act("idea", Body{Request: "  "}); err == nil {
		t.Error("empty idea accepted")
	}
	if _, err := f.Act("archive", Body{ID: idea.ID}); err != nil {
		t.Fatal(err)
	}
	if got := f.ideas.List(); got[0].Status != "dismissed" {
		t.Errorf("after dismiss: %+v", got)
	}
	_, err = f.Act("idea", Body{Request: "make TESTS faster"})
	if err == nil || !strings.Contains(err.Error(), idea.ID) || !strings.Contains(err.Error(), "dismissed") {
		t.Errorf("re-adding a dismissed idea: %v", err)
	}
}

func TestIdeasStartedTitleBlockedFor14Days(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "Cache the board"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if _, err := f.Act("approve", Body{ID: idea.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = f.Act("idea", Body{Request: "cache the board"})
	if err == nil || !strings.Contains(err.Error(), idea.ID) || !strings.Contains(err.Error(), "started") {
		t.Errorf("re-adding a started idea: %v", err)
	}
	f.Now = func() time.Time { return t0.Add(15 * 24 * time.Hour) }
	if _, err := f.Act("idea", Body{Request: "Cache the board"}); err != nil {
		t.Errorf("after 14 days: %v", err)
	}
}

func TestTickStartsIdeasWithinCaps(t *testing.T) {
	f, p := newTestFactory(t)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":2,"daily":3}}}`)
	for _, r := range []string{"one", "two", "three", "four"} {
		if _, err := f.Act("idea", Body{Request: r}); err != nil {
			t.Fatal(err)
		}
	}
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("one idea per tick, got %d", n)
	}
	// the started idea is now an agent in flight
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "plan", labelOrigin: "idea"})})
	f.Tick()
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "plan", labelOrigin: "idea"}),
		card("new-agent-2", map[string]string{labelStage: "building", labelOrigin: "idea"})})
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 2 {
		t.Fatalf("WIP cap 2: started %d", n)
	}
	// both finish; the daily cap (3) still allows one more, then stops
	f.SetAgents([]Agent{card("new-agent-1", map[string]string{labelStage: "landed", labelOrigin: "idea"}),
		card("new-agent-2", map[string]string{labelStage: "landed", labelOrigin: "idea"})})
	f.Tick()
	f.SetAgents(nil)
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 3 {
		t.Fatalf("daily cap 3: started %d", n)
	}
	// Zach can start one by hand past the caps
	var waiting string
	for _, i := range f.ideas.List() {
		if i.Status == "waiting" {
			waiting = i.ID
		}
	}
	if _, err := f.Act("approve", Body{ID: waiting}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 4 {
		t.Fatalf("manual start: %d", n)
	}
}

func TestDailyCapResetsNextDay(t *testing.T) {
	f, p := newTestFactory(t)
	setLanes(t, f, `{"example/desk":{"name":"desk","ideas":{"inFlight":5,"daily":1}}}`)
	f.Act("idea", Body{Request: "a"})
	f.Act("idea", Body{Request: "b"})
	f.Tick()
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 1 {
		t.Fatalf("started %d, want 1", n)
	}
	f.Now = func() time.Time { return t0.Add(24 * time.Hour) }
	f.Tick()
	if n := len(p.runsWithLabel(labelArg(labelOrigin, "idea"))); n != 2 {
		t.Fatalf("next day started %d, want 2", n)
	}
}

func TestScoutGuidance(t *testing.T) {
	for _, guidance := range []string{"", "Find ways to speed up tests.\nPrioritize flaky cases and report 50% gains."} {
		t.Run(guidance, func(t *testing.T) {
			f, p := newTestFactory(t)
			raw, err := json.Marshal(map[string]string{
				"repo": "example/desk", "request": guidance, "focus": "legacy/path",
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/api/scout", strings.NewReader(string(raw)))
			req.Header.Set("X-Factory", "1")
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("scout: %d %s", rec.Code, rec.Body)
			}
			runs := p.runsWithLabel(labelArg(labelRole, "scout"))
			if len(runs) != 1 {
				t.Fatalf("scout runs = %v", runs)
			}
			prompt := runs[0][len(runs[0])-1]
			if guidance != "" && !strings.Contains(prompt, guidance) {
				t.Error("scout prompt lost guidance")
			}
			if guidance == "" && strings.Contains(prompt, "Guidance:") {
				t.Error("empty guidance added a prompt section")
			}
			if strings.Contains(strings.Join(runs[0], " "), "legacy/path") || strings.Contains(prompt, "Only look at") {
				t.Error("legacy focus restricted the scout")
			}
			if strings.Contains(prompt, "%!") || !strings.Contains(prompt, "localhost:7478/api/idea") ||
				!strings.Contains(prompt, "fetch -q origin") || !strings.Contains(prompt, "localhost:7478/api/ideas") {
				t.Errorf("bad scout prompt:\n%s", prompt)
			}
		})
	}
}

func TestIdeaKeepsSummaryAndDoneWhen(t *testing.T) {
	f, _ := newTestFactory(t)
	i, err := f.Act("idea", Body{Request: "In foo.go change bar", Summary: "Board loads twice as fast",
		DoneWhen: "page loads in under 1 s", Why: "it is slow"})
	if err != nil {
		t.Fatal(err)
	}
	idea := i.(Idea)
	if idea.Summary != "Board loads twice as fast" || idea.DoneWhen != "page loads in under 1 s" {
		t.Errorf("idea = %+v", idea)
	}
}

func TestIdeaTitleCutAtWord(t *testing.T) {
	f, _ := newTestFactory(t)
	long := "skill_search: merge partial and body hits from a full candidate pool, then apply the limit"
	i, err := f.Act("idea", Body{Request: "x", Title: long})
	if err != nil {
		t.Fatal(err)
	}
	got := i.(Idea).Title
	if got != "skill_search: merge partial and body hits from a full candidate…" {
		t.Errorf("title = %q", got)
	}
	if got := truncWords("short title", 70); got != "short title" {
		t.Errorf("short = %q", got)
	}
	if got := truncWords("abcdefghij", 5); got != "abcd…" {
		t.Errorf("one long word = %q", got)
	}
}

func TestScoutAsksForSummaryAndDoneWhen(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("scout", Body{Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelRole, "scout"))
	prompt := runs[0][len(runs[0])-1]
	for _, want := range []string{`"summary":`, `"doneWhen":`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("scout prompt lacks %s", want)
		}
	}
}

func TestStartIdeaWithZachsChanges(t *testing.T) {
	f, p := newTestFactory(t)
	i, _ := f.addIdea(Body{Request: "Cache the board in memory", Title: "cache", Why: "slow"})
	if _, err := f.Act("approve", Body{ID: i.ID, Message: "  Use a 5s TTL, not forever.  "}); err != nil {
		t.Fatal(err)
	}
	r := p.runsWithLabel(labelArg(labelOrigin, "idea"))
	if len(r) != 1 {
		t.Fatalf("runs = %v", r)
	}
	prompt := r[0][len(r[0])-1]
	brief := strings.Index(prompt, "Cache the board in memory")
	note := strings.Index(prompt, "Zach's changes before starting (his own words; where they differ from the brief above, his win):\nUse a 5s TTL, not forever.")
	if brief < 0 || note < brief {
		t.Errorf("prompt lacks the marked note after the brief:\n%s", prompt)
	}
	got := f.ideas.List()[0]
	if got.Note != "Use a 5s TTL, not forever." {
		t.Errorf("idea note = %q", got.Note)
	}
	f.SetAgents([]Agent{card(got.Card, map[string]string{labelStage: "plan", labelOrigin: "idea"})})
	if c, _ := f.card(got.Card); c.Idea == nil || c.Idea.Note != got.Note {
		t.Errorf("started card idea = %+v", c.Idea)
	}

	// no text, or only whitespace: today's request, no note
	for n, msg := range []string{"", " \n\t "} {
		j, _ := f.addIdea(Body{Request: "Plain idea " + string(rune('a'+n)), Title: "plain " + string(rune('a'+n))})
		if _, err := f.Act("approve", Body{ID: j.ID, Message: msg}); err != nil {
			t.Fatal(err)
		}
		r = p.runsWithLabel(labelArg(labelOrigin, "idea"))
		if prompt := r[len(r)-1][len(r[len(r)-1])-1]; strings.Contains(prompt, "Zach's changes before starting") {
			t.Errorf("message %q added a note:\n%s", msg, prompt)
		}
		for _, k := range f.ideas.List() {
			if k.ID == j.ID && k.Note != "" {
				t.Errorf("message %q stored note %q", msg, k.Note)
			}
		}
	}
}
