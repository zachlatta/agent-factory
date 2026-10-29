package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PreviewHandler serves the real board and API with synthetic inputs. root must be an
// empty directory owned by the caller. No pipeline loop runs; supported actions change only the fabricated snapshot.
// The caller removes root after closing the server.
func PreviewHandler(root string) (http.Handler, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, fmt.Errorf("preview requires an empty directory")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	home, state := filepath.Join(root, "home"), filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	repo := filepath.Join(home, "dev", "example", "board")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		return nil, err
	}
	// Freeze ages for reproducible captures, and prevent every optional process hook.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// the email dispatcher's file of requests with no answer, fabricated: Dismiss records the real
	// dismissal in the preview's own state, never in this file (unanswered.go)
	unanswered := filepath.Join(state, "sample-unanswered.json")
	sample, _ := json.Marshal(unansweredFile{Updated: iso(now), Requests: []UnansweredRequest{
		{Key: "sample-request-1", Source: "email", Thread: "sample-thread-1", Asked: iso(now.Add(-3 * time.Hour)),
			Subject: "Order more desk lamps", Why: "the agent stopped without replying"},
		{Key: "sample-request-2", Source: "voice", Thread: "sample-thread-2", Asked: iso(now.Add(-2 * time.Hour)),
			Subject: "Move the team call to Thursday", Why: "no reply in the thread"},
	}})
	if err := os.WriteFile(unanswered, sample, 0600); err != nil {
		return nil, err
	}
	f := New(Config{Host: "preview", Home: home, StateDir: state, ReadOnly: true,
		DefaultRepo: "example/board", Guarded: []string{}, Unanswered: unanswered}, previewPaseo{})
	f.Now = func() time.Time { return now }
	f.GitOut = func(context.Context, string, ...string) (string, error) {
		return "", fmt.Errorf("git disabled in preview")
	}
	f.Git = func(context.Context, string, ...string) error { return fmt.Errorf("git disabled in preview") }
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }
	f.prov.status = map[string]string{"claude": "available", "codex": "available", "antigravity": "available", "muse": "available"}
	f.prov.listed = now
	f.usage.byProvider = map[string]AgentUsage{}
	f.usage.fetched = now
	if err := f.ideas.save([]Idea{{ID: "idea-preview", Title: "Make the board easier to read", Repo: "example/board",
		Summary: "Check an idea before starting it in the selected repo.", Why: "Keep the next action clear on a phone.",
		Request: "Build a readable board layout.", DoneWhen: "Desktop and phone layouts are checked.",
		By: "preview", Tier: "low", Status: "waiting", CreatedAt: now.Add(-time.Hour)}}); err != nil {
		return nil, err
	}
	var agents []Agent
	for _, stage := range Stages {
		if stage[0] == "idea" {
			continue
		}
		id := "preview-" + stage[0]
		a := Agent{ID: id, Host: "preview", Provider: "claude", SessionID: id,
			Cwd: "/preview/example/board", Repo: "example/board", Title: stage[1] + ": check the board layout",
			Status: "idle", CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
			Labels: map[string]string{labelPrefix + labelStage: stage[0], labelPrefix + labelTier: "architecture",
				labelPrefix + labelSource: "board", labelPrefix + labelSeen: strconv.FormatInt(now.Unix(), 10)}}
		request, reply := previewRequest, previewReply
		if stage[0] == "zach" {
			// the one question before building: a reviewed plan whose build has a side effect (sideEffectStop)
			a.Title, request, reply = "Board card list loads faster", previewDecisionRequest, previewDecisionReply
			a.Labels[labelPrefix+labelAwaiting] = "plan"
			a.Labels[labelPrefix+labelVerdict] = "plan side effects"
			a.Labels[labelPrefix+labelNote] = sideEffectNote
		}
		// A real session file exercises the normal transcript, request and Ask parsing paths.
		if err := writePreviewSession(home, a.Cwd, id, request, reply); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	// finished work waiting on Zach: the Ready for you stop with Land / Change / Drop, on its own session
	ready := agents[slices.IndexFunc(agents, func(a Agent) bool { return a.ID == "preview-zach" })]
	ready.ID, ready.SessionID, ready.Title = "preview-ready", "preview-ready", "Clearer board buttons"
	if err := writePreviewSession(home, ready.Cwd, ready.ID, previewReadyRequest, previewReadyReply); err != nil {
		return nil, err
	}
	ready.Labels = maps.Clone(ready.Labels)
	delete(ready.Labels, labelPrefix+labelAwaiting)
	delete(ready.Labels, labelPrefix+labelVerdict)
	ready.Labels[labelPrefix+labelNote] = readyNote(1)
	// built after a reviewed plan and one code review, as real finished work is
	ready.Labels[labelPrefix+labelPlanRounds], ready.Labels[labelPrefix+labelCodeRounds] = "1", "1"
	agents = append(agents, ready)
	// work that landed with one thing left for Zach to try: the try ask (asktry.go), under an agent's
	// "None" decision that the board drops
	try := ready
	try.ID, try.SessionID, try.Title = "preview-try", "preview-try", "Usage page fits a phone"
	if err := writePreviewSession(home, try.Cwd, try.ID, previewTryRequest, previewTryReply); err != nil {
		return nil, err
	}
	try.Labels = maps.Clone(ready.Labels)
	try.Labels[labelPrefix+labelNote] = noteWorkLeft + "Zach: open the usage page on your phone and say whether anything is cut off"
	agents = append(agents, try)
	f.hostMetrics.read = func(string) hostReading {
		return hostReading{Metrics: hostMetrics{CPUCount: 4, Errors: map[string]string{}}}
	}
	f.fleet = nil
	f.SetAgents(agents)
	var actionMu sync.Mutex
	real := f.Handler()
	// Keep host/process inspection routes out of the preview, including future routes
	// until they are explicitly reviewed as safe. Allowed APIs use production handlers.
	allowed := map[string]bool{"/": true, "/index.html": true, "/api/board": true,
		"/api/agents": true, "/api/lanes": true, "/api/deploy": true, "/api/hosts": true, "/host-metrics.js": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			actionMu.Lock()
			defer actionMu.Unlock()
			previewAction(f, w, r)
			return
		}
		safe := allowed[r.URL.Path]
		for _, prefix := range []string{"/t/", "/api/logs/", "/api/transcript/"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				id := strings.TrimPrefix(r.URL.Path, prefix)
				_, safe = f.agentOK(id)
				if prefix == "/api/logs/" {
					_, safe = f.card(id)
				}
				break
			}
		}
		if r.Method != http.MethodGet || !safe {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "preview: navigation only; mutations and host inspection are disabled"})
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			page, _ := web.ReadFile("web/board.html")
			// Keep the real embedded board, with only its preview clock fixed and its preview mode marked:
			// the page then shows its sample-data notice (#previewNote).
			clock := fmt.Sprintf("<title>Factory preview</title><script>Date.now=()=>%d;</script>", now.UnixMilli())
			html := strings.Replace(string(page), "<title>Factory</title>", clock, 1)
			page = []byte(strings.Replace(html, `<html lang="en">`, `<html lang="en" data-mode="preview">`, 1))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(page)
			return
		}
		real.ServeHTTP(w, r)
	}), nil
}

// previewAction deliberately does not call Act: it cannot launch agents, run Git,
// send messages, or approve a real card. ReadOnly stays enabled as a second guard. Dismiss on a
// sample request writes only the preview's own dismissal store.
func previewAction(f *Factory, w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/")
	supported := map[string]bool{"approve": true, "send": true, "stage": true, "close": true, "archive": true, "done": true, "stop": true, "seen": true, "choose": true,
		"dismiss-request": true}
	if !supported[action] || r.Header.Get("X-Factory") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "preview: only sample-card actions are simulated"})
		return
	}
	var b Body
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid preview action"})
		return
	}
	if action == "dismiss-request" {
		// a sample request: the real dismissal, recorded in the preview's own state directory
		if err := f.dismissRequest(b.ID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "preview: " + err.Error()})
			return
		}
		f.logf("PREVIEW: dismissed a fabricated request; nothing runs")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "simulated": true})
		return
	}
	agents := slices.Clone(f.agents())
	idx := slices.IndexFunc(agents, func(a Agent) bool { return a.ID == b.ID })
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not a fabricated card"})
		return
	}
	a := &agents[idx]
	if action == "choose" {
		// the board's shared buttons (choose.go): simulate the action the card's button runs
		c, _ := f.card(a.ID)
		ask := f.replyAsk(c, *a)
		var ch *Choice
		if ask != nil {
			ch = choiceByID(ask, b.Choice)
		}
		if ch == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "preview: that button is not on this card"})
			return
		}
		action = ch.action
		if action == "approve" && a.L(labelAwaiting) == "" {
			b.Stage = "landing" // Land on finished work starts landing
		}
	}
	a.Labels = maps.Clone(a.Labels)
	switch action {
	case "approve":
		a.Labels[labelPrefix+labelStage] = "building"
		if b.Stage == "landing" {
			a.Labels[labelPrefix+labelStage] = "landing"
		}
		delete(a.Labels, labelPrefix+labelAwaiting)
	case "send":
		// No, Change or an answer: the card moves where the real send moves it (answerLabels)
		c, _ := f.card(a.ID)
		for k, v := range answerLabels(c, *a, iso(f.Now()), false) {
			a.Labels[labelPrefix+k] = v
		}
	case "stage":
		if !validStage(b.Stage) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown stage"})
			return
		}
		a.Labels[labelPrefix+labelStage] = b.Stage
	case "close", "archive", "done":
		a.ArchivedAt = f.Now()
		a.Status = "closed"
		a.Labels[labelPrefix+labelStage] = "landed"
	case "stop":
		a.Status = "idle"
	case "seen":
		a.Labels[labelPrefix+labelSeen] = strconv.FormatInt(f.Now().Unix(), 10)
	}
	f.SetAgents(agents)
	f.logf("PREVIEW: simulated %s on a fabricated card; nothing runs", action)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "simulated": true})
}

// writePreviewSession writes agent id's Claude session file: Zach's request, then the agent's reply.
func writePreviewSession(home, cwd, id, request, reply string) error {
	dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(cwd))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var data []byte
	for _, m := range []map[string]any{
		{"type": "user", "message": map[string]any{"role": "user", "content": boardHeader + "\n" + request + "\n\nWork in this repo (example/board)."}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": reply}}}},
	} {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		data = append(append(data, line...), '\n')
	}
	return os.WriteFile(filepath.Join(dir, id+".jsonl"), data, 0600)
}

const previewRequest = "Make the board easy to read on a phone: one column, and every card I need to answer at the top."

const previewTryRequest = "Make the usage page fit a phone screen."

// previewTryReply is a landing that leaves Zach one thing to try, under a decision line that says nothing.
const previewTryReply = `DECISION: None. The usage page is live.
OVERVIEW: The usage page now fits a phone screen.
NEEDS FROM ZACH: Open the usage page on your phone and say whether anything is cut off.

Landed.

REMAINING:
- Zach: open the usage page on your phone and say whether anything is cut off`

const previewReadyRequest = "Make the board's answer buttons clearer on a phone, so one tap does what it says."

// previewReadyReply is finished work: built and reviewed, waiting for Zach to try it and land it.
const previewReadyReply = `DECISION: Land the clearer board buttons?
OVERVIEW: Each card's buttons now say what they do, and one tap answers. It is built and reviewed; nothing is live yet.
NEEDS FROM ZACH: Try it, then land it, ask for a change or drop it.

CHANGED: Every card you answer shows its buttons in one row you can reach with your thumb.
CHANGED: A tap answers at once, with no separate Submit.
CHANGED: Drop asks for a second tap before it closes a card.

- A long button label could wrap onto two lines on a small phone.

QUESTION: Should Drop also ask for a reason?
- No, one extra tap is enough (recommended)
- Yes, ask for a short reason

TIER: architecture`

// previewReply is a plan in progress on the other stages' cards: nothing is asked of Zach.
const previewReply = `DECISION: Nothing for you yet.
OVERVIEW: The board shows one column on a phone, with the cards waiting on you at the top. Long titles wrap instead of scrolling sideways.
NEEDS FROM ZACH: Nothing now. You see the finished work with a preview before it lands.

The plan changes the board page only, and is checked at phone and desktop widths on sample cards.

BUILD SIDE EFFECTS: none
TIER: architecture`

const previewDecisionRequest = "Make the board's card list load faster on a phone."

// previewDecisionReply is a side-effect decision: allow the build to do one thing outside its worktree.
const previewDecisionReply = `DECISION: Allow this build to restart the test board service once?
OVERVIEW: Building it restarts the test board (never the live one) to measure how fast the new card list loads. The test board is down for about ten seconds.
NEEDS FROM ZACH: Approve to build, or say what to change.

Measuring on a copy instead avoids the restart, but takes longer to set up.`

// previewPaseo never starts a process. Session reads normally satisfy logs; the
// fallback stays synthetic so changes to transcript discovery cannot reach Paseo.
type previewPaseo struct{}

func (previewPaseo) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "logs" {
		return previewReply, nil
	}
	return "", fmt.Errorf("Paseo disabled in preview")
}
