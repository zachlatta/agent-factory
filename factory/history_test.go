package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stuckLogsPaseo is a fake Paseo whose `logs` hangs for a second, then fails, as a timeline read
// stuck resuming a provider would: a read that asks it shows up as a slow request and a recorded
// call (noLogsCalls), without hanging the suite.
type stuckLogsPaseo struct {
	*fakePaseo
	done chan struct{}
}

func (s *stuckLogsPaseo) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "logs" {
		s.fakePaseo.mu.Lock()
		s.fakePaseo.calls = append(s.fakePaseo.calls, args)
		s.fakePaseo.mu.Unlock()
		select {
		case <-s.done:
		case <-time.After(time.Second):
		}
		return "", fmt.Errorf("paseo logs: stuck")
	}
	return s.fakePaseo.Run(ctx, args...)
}

func newStuckLogsFactory(t *testing.T, agents ...Agent) (*Factory, *fakePaseo) {
	t.Helper()
	f, p := newTestFactory(t)
	s := &stuckLogsPaseo{fakePaseo: p, done: make(chan struct{})}
	t.Cleanup(func() { close(s.done) })
	f.p = s
	f.SetAgents(agents)
	return f, p
}

func noLogsCalls(t *testing.T, p *fakePaseo) {
	t.Helper()
	if n := p.countCalls("logs", ""); n != 0 {
		t.Errorf("made %d paseo logs calls (each loads the agent in Paseo): %v", n, p.calls)
	}
}

func archived(a Agent) Agent {
	a.Status, a.ArchivedAt = "closed", t0.Add(-time.Hour)
	return a
}

func getJSON(t *testing.T, f *Factory, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var j map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	return rec.Code, j
}

// providerAgents are archived agents of every provider with a session-file reader, each with a
// session file whose last reply is "<provider> reply".
func providerAgents(t *testing.T, home string) []Agent {
	t.Helper()
	cl := archived(card("cl", nil))
	cl.Cwd = "/w/cl"
	writeFile(t, home+"/.claude/projects/-w-cl/"+cl.SessionID+".jsonl",
		`{"type":"user","message":{"content":"go"}}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"claude reply\nTIER: low"}]}}`+"\n")
	cx := archived(card("cx", nil))
	cx.Provider, cx.SessionID = "codex", "sess-cx"
	writeFile(t, home+"/.codex/sessions/2026/01/02/rollout-x-sess-cx.jsonl",
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}`+"\n"+
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex reply"}]}}`+"\n")
	ag := archived(card("ag", nil))
	ag.Provider = "antigravity"
	ag.SessionID = antigravityFile(t, home, "conv-ag",
		`{"step_index":0,"type":"USER_INPUT","content":"<USER_REQUEST>\ngo\n</USER_REQUEST>"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","content":"antigravity reply"}`)
	mu := archived(card("mu", nil))
	mu.Provider, mu.SessionID = "muse", `plugin:{"version":1,"data":{"sessionId":"sess-mu","model":"m"}}`
	ev := func(ts int, e string) string {
		return `{"recorded_at":` + strconv.Itoa(ts) + `,"payload_type":"runtime.session","payload":{"event":` + e + `}}`
	}
	writeFile(t, home+"/.local/share/muse/sessions/2026/01/02/sess-mu/session.jsonl",
		ev(1, `{"kind":"user_prompt_display","prompt":"go"}`)+"\n"+ev(2, `{"kind":"assistant_message_committed","text":"muse reply"}`)+"\n")
	return []Agent{cl, cx, ag, mu}
}

// The board's summary of an archived agent comes from its session file, for every provider.
func TestLogsEndpointReadsSessionFiles(t *testing.T) {
	f, p := newStuckLogsFactory(t)
	agents := providerAgents(t, f.cfg.Home)
	f.SetAgents(agents)
	want := map[string]string{"cl": "[User] go\n[Thought] hmm\nclaude reply\nTIER: low", "cx": "[User] go\ncodex reply",
		"ag": "[User] go\nantigravity reply", "mu": "[User] go\nmuse reply"}
	for _, a := range agents {
		code, j := getJSON(t, f, "/api/logs/"+a.ID)
		if code != 200 || j["text"] != want[a.ID] {
			t.Errorf("%s (%s): %d %v", a.ID, a.Provider, code, j)
		}
		reply, err := f.storedReply(a)
		if err != nil || !strings.HasPrefix(reply, a.Provider+" reply") {
			t.Errorf("%s reply = %q, %v", a.Provider, reply, err)
		}
	}
	noLogsCalls(t, p)
}

// With no readable session file, the summary and the transcript say why at once, and never ask Paseo.
func TestMissingHistoryIsExplicitAndImmediate(t *testing.T) {
	f, p := newStuckLogsFactory(t)
	home := f.cfg.Home
	noID := archived(card("noid", nil))
	noID.SessionID = ""
	other := archived(card("other", nil))
	other.Provider = "gemini-cli"
	missing := archived(card("missing", nil))
	empty := archived(card("empty", nil))
	writeFile(t, home+"/.claude/projects/-w-empty/"+empty.SessionID+".jsonl", `{"type":"summary"}`+"\n")
	unreadable := archived(card("unreadable", nil))
	path := home + "/.claude/projects/-w-unreadable/" + unreadable.SessionID + ".jsonl"
	writeFile(t, path, `{"type":"user","message":{"content":"go"}}`+"\n")
	os.Chmod(path, 0)
	_, rootReads := os.ReadFile(path)
	f.SetAgents([]Agent{noID, other, missing, empty, unreadable})
	reasons := map[string]string{"noid": "has no stored session", "other": `no session-file reader for provider "gemini-cli"`,
		"missing": "no claude session file", "empty": "no claude session file", "unreadable": "reading claude session"}
	for id, reason := range reasons {
		if id == "unreadable" && rootReads == nil {
			continue // root reads any file
		}
		start := time.Now()
		code, j := getJSON(t, f, "/api/logs/"+id)
		msg, _ := j["error"].(string)
		if code != 404 || !strings.HasPrefix(msg, "history unavailable: ") || !strings.Contains(msg, reason) {
			t.Errorf("logs %s: %d %v", id, code, j)
		}
		code, j = getJSON(t, f, "/api/transcript/"+id)
		msg, _ = j["unavailable"].(string)
		if code != 200 || j["fallback"] != true || len(j["events"].([]any)) != 0 || !strings.Contains(msg, reason) {
			t.Errorf("transcript %s: %d %v", id, code, j)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("%s took %v", id, d)
		}
	}
	if code, _ := getJSON(t, f, "/api/logs/nosuch"); code != 404 {
		t.Errorf("unknown agent: %d", code)
	}
	noLogsCalls(t, p)
}

// Switching cards fast: concurrent summary reads each get their own agent's text.
func TestLogsEndpointConcurrentCardSwitches(t *testing.T) {
	f, p := newStuckLogsFactory(t)
	agents := providerAgents(t, f.cfg.Home)
	f.SetAgents(agents)
	var wg sync.WaitGroup
	for n := 0; n < 40; n++ {
		a := agents[n%len(agents)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs/"+a.ID, nil))
			if !strings.Contains(rec.Body.String(), a.Provider+" reply") {
				t.Errorf("%s got %s", a.ID, rec.Body)
			}
		}()
	}
	wg.Wait()
	noLogsCalls(t, p)
}

// A Needs Zach card that stopped long ago is shown and emailed from its session file; with none,
// the card says the history is unavailable instead of loading the agent.
func TestAskDetailAndEmailNeverAskPaseo(t *testing.T) {
	f, p := newStuckLogsFactory(t)
	// idle cards Paseo may have unloaded since (a restart): reading them must not load them again
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"})
	storeLog(t, f, p, "c1", "[User] go\nI added --dry-run.")
	gone := card("c2", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land"})
	f.SetAgents([]Agent{c, gone})
	if got, _ := f.card("c1"); got.Ask == nil || got.Ask.Detail != "I added --dry-run." {
		t.Errorf("detail = %+v", got.Ask)
	}
	got, _ := f.card("c2")
	if got.Ask == nil || !strings.Contains(got.Ask.Detail, "history unavailable: no claude session file") {
		t.Errorf("missing detail = %+v", got.Ask)
	}
	if text, _, _ := f.askDetail(got, got.Ask); !strings.Contains(text, "history unavailable") {
		t.Errorf("email detail = %q", text)
	}
	noLogsCalls(t, p)
}

// Close can come days after the review: the duplicate line is read from session files, and a
// reviewer with no file falls back to the card's own line without asking Paseo.
func TestCloseReadsSessionFilesOnly(t *testing.T) {
	f, p := dupFactory(t, "building", "running", nil)
	s := &stuckLogsPaseo{fakePaseo: p, done: make(chan struct{})}
	defer close(s.done)
	f.p = s
	closeDup(t, f)
	if st := f.steers.all()[dupID]; st.Keep != keepID || !strings.Contains(st.Notes, "not scouts or pickers") {
		t.Errorf("steer from the reviewer's file: %+v", st)
	}
	noLogsCalls(t, p)

	f, p = dupFactory(t, "building", "running", nil)
	s = &stuckLogsPaseo{fakePaseo: p, done: make(chan struct{})}
	defer close(s.done)
	f.p = s
	os.Remove(f.cfg.Home + "/.claude/projects/-test/" + testSession("rev1") + ".jsonl")
	storeLog(t, f, p, dupID, "Card keep123 already does this.\nDUPLICATE OF: keep123")
	closeDup(t, f)
	if st := f.steers.all()[dupID]; st.Keep != keepID {
		t.Errorf("steer from the card's own file: %+v", st)
	}
	noLogsCalls(t, p)
}

// When a plan review ends, the builder (idle all review, maybe unloaded) is read from its file.
func TestPlanReviewReadsBuilderFromSessionFile(t *testing.T) {
	f, p := newTestFactory(t, card("c1", map[string]string{labelStage: "plan-review", labelSource: "board"}),
		reviewer("r1", "c1", "plan"))
	storeLog(t, f, p, "r1", "TIER: low\nVERDICT: approve")
	storeLog(t, f, p, "c1", "plan: replace the storage layer\nWaits on: keep123\nTIER: architecture")
	delete(p.logs, "c1")
	f.ApplyVerdict(f.agent("r1"))
	if l := p.labels("c1"); l[labelTier] != "architecture" {
		t.Errorf("builder tier not read from its file: %v", l)
	}
	if p.called("logs", "c1") {
		t.Errorf("asked Paseo for the builder: %v", p.calls)
	}
}

// Retry on a stuck picker reads its file; with none it fails at once, saying why.
func TestRetryStuckPickerReadsSessionFileOnly(t *testing.T) {
	a := picker("pk", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "plan"})
	f, p := newStuckLogsFactory(t, a)
	withRepos(t, f, "example/desk")
	f.routes.put("pk", Route{Request: "q", Title: "q"})
	if _, err := f.Act("rerun", Body{ID: "pk"}); err == nil || !strings.Contains(err.Error(), "history unavailable") {
		t.Errorf("rerun without a file: %v", err)
	}
	storeLog(t, f, p, "pk", "[User] x\nREPO: example/desk")
	if _, err := f.Act("rerun", Body{ID: "pk"}); err != nil {
		t.Fatal(err)
	}
	if !p.archived["pk"] {
		t.Error("rerun did not route the picker")
	}
	noLogsCalls(t, p)
}
