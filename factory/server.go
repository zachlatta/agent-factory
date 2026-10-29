package factory

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed web/board.html web/transcript.html web/host.html web/host-metrics.js web/usage.html web/usage-evidence.html
var web embed.FS

// Handler serves the board, the transcript pages and the JSON API.
func (f *Factory) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := web.ReadFile("web/" + name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if strings.HasSuffix(name, ".js") {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Write(b)
		}
	}
	// Gzip is allowlisted per route, never a blanket wrapper: a new route is served
	// plain unless it opts in here, so a future streaming route cannot stall behind it.
	mux.HandleFunc("GET /{$}", gzipHandler(page("board.html")))
	mux.HandleFunc("GET /index.html", gzipHandler(page("board.html")))
	mux.HandleFunc("GET /usage/agent/{id}", gzipHandler(page("usage-evidence.html")))
	mux.HandleFunc("GET /api/usage/evidence/{id}", gzipHandler(f.handleUsageEvidence))
	mux.HandleFunc("GET /usage", gzipHandler(page("usage.html")))
	mux.HandleFunc("GET /t/{id}", gzipHandler(page("transcript.html")))
	mux.HandleFunc("GET /api/evidence/{id}/{slot}", f.handleEvidence)
	mux.HandleFunc("GET /api/board", gzipHandler(f.handleBoard))
	mux.HandleFunc("GET /api/ideas", gzipHandler(f.handleIdeas))
	mux.HandleFunc("GET /api/peers", gzipHandler(f.handlePeers))
	mux.HandleFunc("GET /api/agents", gzipHandler(f.handleAgents))
	mux.HandleFunc("GET /api/usage", gzipHandler(f.handleUsage))
	mux.HandleFunc("GET /api/lanes", gzipHandler(f.handleLanes))
	mux.HandleFunc("GET /api/health", gzipHandler(f.handleHealth))
	mux.HandleFunc("GET /api/metrics", gzipHandler(f.handleMetrics))
	mux.HandleFunc("GET /api/timings", gzipHandler(f.handleTimings))
	mux.HandleFunc("GET /api/deploy", gzipHandler(f.handleDeploy))
	mux.HandleFunc("GET /api/unanswered", gzipHandler(f.handleUnanswered))
	mux.HandleFunc("GET /api/transcript/{id}", gzipHandler(f.handleTranscript))
	mux.HandleFunc("GET /api/transcript/{id}/image/{ref}", f.handleTranscriptImage)
	mux.HandleFunc("GET /api/logs/{id}", gzipHandler(f.handleLogs))
	mux.HandleFunc("GET /api/land-review/{by}", gzipHandler(f.handleLandReview))
	mux.HandleFunc("GET /host", page("host.html"))
	mux.HandleFunc("GET /host-metrics.js", page("host-metrics.js"))
	mux.HandleFunc("GET /api/host", f.handleHost)
	mux.HandleFunc("GET /api/hosts", f.handleHosts)
	mux.HandleFunc("POST /api/seen", f.handleSeen)
	mux.HandleFunc("POST /api/{action}", f.handleAction)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

// gzipHandler compresses h's response when the client accepts gzip. Only the routes
// wrapped in Handler are compressed; Flush is forwarded so a flushing handler streams.
func gzipHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h(gzipWriter{ResponseWriter: w, gz: gz}, r)
	}
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// Flush pushes the buffered gzip stream and the underlying response out now.
func (g gzipWriter) Flush() {
	g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *Factory) handleBoard(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	cards, updated, log := f.cards, f.updated, append([]string{}, f.log...)
	errs := map[string]string{}
	for k, v := range f.errs {
		errs[k] = v
	}
	f.mu.Unlock()
	if len(log) > 30 {
		log = log[len(log)-30:]
	}
	if cards == nil {
		cards = []Card{}
	}
	// each waiting idea's repo caps, and why auto-start is paused (ideas.go)
	ideaCaps, ideaHold, lanes := map[string]RepoIdeaCaps{}, "", f.lanes()
	for _, c := range cards {
		if c.Stage == "idea" && c.Idea != nil && c.Idea.Status == "waiting" {
			ideaCaps[c.Repo] = lanes[c.Repo].ideaCaps()
		}
	}
	if len(ideaCaps) > 0 {
		ideaHold = f.ideaHold()
	}
	writeJSON(w, 200, map[string]any{"cards": cards, "sections": f.sections(), "updated": iso(updated), "errors": errs, "stages": Stages,
		"hosts": []string{f.cfg.Host}, "log": log, "repos": f.Repos(), "tiers": TierHelp,
		"caps": map[string]int{"rework": f.cfg.ReworkCap}, "ideaCaps": ideaCaps, "ideaHold": ideaHold})
}

// handleIdeas lists every idea (all statuses), newest first, with the request cut short.
func (f *Factory) handleIdeas(w http.ResponseWriter, r *http.Request) {
	ideas := f.ideas.List()
	if ideas == nil {
		ideas = []Idea{}
	}
	sort.SliceStable(ideas, func(a, b int) bool { return ideas[a].CreatedAt.After(ideas[b].CreatedAt) })
	for n := range ideas {
		ideas[n].Request = trunc(ideas[n].Request, 300)
	}
	writeJSON(w, 200, map[string]any{"ideas": ideas})
}

// handlePeers is the brief of other cards for agent ?for= (text; &format=json for JSON).
func (f *Factory) handlePeers(w http.ResponseWriter, r *http.Request) {
	p := f.Peers(r.URL.Query().Get("for"), r.URL.Query().Get("repo"))
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, 200, p)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, p.Text(f.cfg.Location))
}

// transcriptAgent is the agent whose transcript /api/transcript/{id} shows: the card's current agent
// when id names one it replaced.
func (f *Factory) transcriptAgent(id string) (Agent, map[string]Agent, bool) {
	a, found := f.agentOK(id)
	byID := f.byID()
	if !found || (a.L(labelRole) == "" && replacedPredecessor(byID, a)) {
		if ca, ok := f.cardAgentOK(id); ok {
			a, found = ca, true
		}
	}
	return a, byID, found
}

// lineage is the agents a card's transcript is stitched from, oldest first and a last (or just a).
func lineage(byID map[string]Agent, a Agent) []Agent {
	if a.L(labelRole) == "" {
		if lin := cardLineage(byID, a.ID); len(lin) > 1 {
			return lin
		}
	}
	return []Agent{a}
}

// lineagePrefix is the history of the agents a card's agent replaced, oldest first, each followed by
// a "Switched from" note. Its events carry Prev (how many agents back) and image refs ending in
// .<Prev>, so the image route reads them from that agent's own session file.
func (f *Factory) lineagePrefix(lin []Agent) []Event {
	var out []Event
	for i := 0; i < len(lin)-1; i++ {
		prev, next, hops := lin[i], lin[i+1], len(lin)-1-i
		src := TranscriptSource{Provider: prev.Provider, SessionID: prev.SessionID, Cwd: prev.Cwd}
		if pr, err := ReadTranscriptPage(f.cfg.Home, src, Page{Refs: true}); err == nil && !pr.Fallback {
			for _, e := range pr.Events {
				e.Prev = hops
				eachImage([]Event{e}, func(_, _ int, im *Image) {
					if im.Ref != "" {
						im.Ref += "." + strconv.Itoa(hops)
					}
				})
				out = append(out, e)
			}
		}
		out = append(out, Event{Kind: "system", Prev: hops, Ts: iso(next.CreatedAt),
			Text: fmt.Sprintf("Switched from %s (%s) to %s (%s)", short(prev.ID), orNone(prev.Model, prev.Provider), short(next.ID), orNone(next.Model, next.Provider))})
	}
	return out
}

func (f *Factory) handleTranscript(w http.ResponseWriter, r *http.Request) {
	a, byID, found := f.transcriptAgent(r.PathValue("id"))
	if !found {
		writeJSON(w, 404, map[string]string{"error": "agent not on the board (open, or archived in the last 12 h)"})
		return
	}
	// tail=1: the newest page; before=N: the page before line N; after=N (from=N): new lines to poll
	q := r.URL.Query()
	num := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return max(n, 0) }
	p := Page{After: num("after"), Before: num("before"), From: num("from"), Tail: q.Get("tail") == "1",
		Budget: TranscriptBudget, Refs: true}
	res, err := ReadTranscriptPage(f.cfg.Home, TranscriptSource{Provider: a.Provider, SessionID: a.SessionID, Cwd: a.Cwd}, p)
	unavailable := ""
	if err != nil || res.Fallback {
		// no readable session file: say why, never ask Paseo (`paseo logs` would load the agent)
		res = Transcript{Events: []Event{}, Fallback: true, Replace: true, Provider: a.Provider}
		if _, err := storedEvents(f.cfg.Home, a); err != nil {
			unavailable = err.Error()
		}
	}
	// the agents this card's agent replaced come before its first line: only on the newest-first page
	// that reaches it, never on a poll (the page keeps them, so a poll would add another copy)
	if lin := lineage(byID, a); p.back() && !res.Older && len(lin) > 1 {
		if prefix := f.lineagePrefix(lin); len(prefix) > 0 {
			res.Events = append(prefix, res.Events...)
			res.Fallback = false
			unavailable = ""
		}
	}
	c, isCard := f.card(a.ID)
	var parent map[string]string
	f.mu.Lock()
	for _, x := range f.cards {
		for _, k := range x.Children {
			if k.ID == a.ID {
				parent = map[string]string{"id": x.ID, "title": x.Title}
			}
		}
	}
	f.mu.Unlock()
	out := map[string]any{"events": res.Events, "next": res.Next, "fallback": res.Fallback, "replace": res.Replace,
		"unavailable": unavailable, "before": res.Before, "older": res.Older, "more": res.More,
		"provider": res.Provider, "stages": Stages, "parent": parent, "card": nil,
		"agent": map[string]any{"id": a.ID, "host": a.Host, "title": a.Title, "status": a.Status,
			"archivedAt": iso(a.ArchivedAt), "cwd": a.Cwd, "repo": a.Repo, "branch": a.Branch, "model": a.Model,
			"createdAt": iso(a.CreatedAt), "lastActivityAt": iso(a.LastActivityAt), "labels": a.Labels}}
	if isCard {
		out["card"] = c
	}
	writeJSON(w, 200, out)
}

// handleTranscriptImage serves an image a transcript page refers to (Image.Ref), read from the
// session file of an agent on the board, so a page fetches images only when it shows them.
func (f *Factory) handleTranscriptImage(w http.ResponseWriter, r *http.Request) {
	a, byID, found := f.transcriptAgent(r.PathValue("id"))
	if !found {
		writeJSON(w, 404, map[string]string{"error": "agent not on the board"})
		return
	}
	// line.k is an image of a's own session; line.k.p one of the agent p back in its card's lineage
	ref, hops := r.PathValue("ref"), 0
	if m := lineageRef.FindStringSubmatch(ref); m != nil {
		ref = m[1]
		hops, _ = strconv.Atoi(m[2])
	}
	if lin := lineage(byID, a); hops < len(lin) {
		a = lin[len(lin)-1-hops]
	} else {
		writeJSON(w, 404, map[string]string{"error": errNoImage.Error()})
		return
	}
	media, data, err := TranscriptImage(f.cfg.Home, TranscriptSource{Provider: a.Provider, SessionID: a.SessionID, Cwd: a.Cwd}, ref)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400") // a written line never changes
	w.Write(data)
}

func (f *Factory) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(id, "idea-") {
		for _, i := range f.ideas.List() {
			if i.ID == id {
				tier := i.Tier
				if tier == "" {
					tier = "none"
				}
				why := i.Why
				if why == "" {
					why = "(not given)"
				}
				// idea: the whole record, for the board's idea panel (its request is not on /api/board)
				writeJSON(w, 200, map[string]any{"text": i.Request + "\n\nWhy: " + why + "\n\nSuggested by " + i.By +
					" " + iso(i.CreatedAt) + "; tier guess: " + tier, "idea": i})
				return
			}
		}
		writeJSON(w, 404, map[string]string{"error": "no such idea"})
		return
	}
	a, found := f.agentOK(id)
	byID := f.byID()
	if !found || (a.L(labelRole) == "" && replacedPredecessor(byID, a)) {
		if ca, ok := f.cardAgentOK(id); ok {
			a, found = ca, true
		}
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "agent not on the board (open, or archived in the last 12 h)"})
		return
	}
	// the session file only: `paseo logs` would load the agent (resuming it even when archived)
	if a.L(labelRole) == "" {
		if lin := cardLineage(byID, a.ID); len(lin) > 1 {
			var parts []string
			var lastErr error
			for i, x := range lin {
				if i > 0 {
					parts = append(parts, fmt.Sprintf("--- Switched from %s (%s) to %s (%s) ---",
						short(lin[i-1].ID), orNone(lin[i-1].Model, lin[i-1].Provider), short(x.ID), orNone(x.Model, x.Provider)))
				}
				t, err := storedText(f.cfg.Home, x, 30)
				if err != nil {
					lastErr = err
					continue
				}
				if strings.TrimSpace(t) != "" {
					parts = append(parts, t)
				}
			}
			if len(parts) > 0 {
				writeJSON(w, 200, map[string]any{"text": strings.Join(parts, "\n"), "request": f.cardRequest(a)})
				return
			}
			if lastErr != nil {
				writeJSON(w, 404, map[string]string{"error": lastErr.Error()})
				return
			}
		}
	}
	text, err := storedText(f.cfg.Home, a, 30)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	// the card's own request, so its detail opens with Zach's words (opening a card only, never the list)
	writeJSON(w, 200, map[string]any{"text": text, "request": f.cardRequest(a)})
}

var actions = map[string]bool{"new": true, "idea": true, "scout": true, "stage": true, "review": true, "send": true,
	"approve": true, "arbiter": true, "rerun": true, "resume": true, "stop": true, "archive": true, "done": true, "close": true,
	"land-review": true, "answer": true, "choose": true, "dismiss-request": true}

func (f *Factory) handleAction(w http.ResponseWriter, r *http.Request) {
	// a custom header blocks cross-site form posts (browsers can't add one without CORS)
	if r.Header.Get("X-Factory") != "1" {
		writeJSON(w, 403, map[string]string{"error": "missing X-Factory header"})
		return
	}
	if f.cfg.ReadOnly {
		writeJSON(w, 403, map[string]string{"error": "read-only factory (deploy canary)"})
		return
	}
	action := r.PathValue("action")
	if !actions[action] {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	var b Body
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
	}
	res, err := f.Act(action, b)
	if err != nil {
		f.logf("action %s %s failed: %v", action, short(b.ID), err)
		code := 400
		switch {
		case errors.Is(err, ErrNoCard):
			code = 404 // answer: the dispatcher falls back to its own handling
		case errors.Is(err, ErrNotWaiting), errors.Is(err, ErrSharedThread):
			code = 409
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	f.logf("action %s %s ok", action, short(b.ID))
	// rebuild the board now (no paseo calls), so the next GET shows the change; the loop still runs early
	if err := f.reload(); err != nil {
		f.logf("board reload after %s failed: %v", action, err)
	}
	f.poke()
	writeJSON(w, 200, res)
}

// handleSeen marks a card seen: Zach opened it on the board or its transcript. Only an unread card
// gets the factory.seen label, so a panel opened on the phone costs at most one paseo call per
// finished turn. The board is rebuilt with no paseo calls, and the loop is not kicked: no rule
// depends on the mark.
func (f *Factory) handleSeen(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Factory") != "1" {
		writeJSON(w, 403, map[string]string{"error": "missing X-Factory header"})
		return
	}
	if f.cfg.ReadOnly {
		writeJSON(w, 403, map[string]string{"error": "read-only factory (deploy canary)"})
		return
	}
	var b Body
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	c, found := f.card(b.ID)
	if !found {
		writeJSON(w, 404, map[string]string{"error": "unknown card"})
		return
	}
	if !c.Unread {
		writeJSON(w, 200, ok)
		return
	}
	if err := f.setLabels(c.ID, map[string]string{labelSeen: strconv.FormatInt(f.Now().Unix(), 10)}); err != nil {
		f.logf("seen %s failed: %v", short(c.ID), err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := f.reload(); err != nil {
		f.logf("board reload after seen failed: %v", err)
	}
	writeJSON(w, 200, ok)
}

// handleLandReview reports an agent's latest landing review (running, or done with verdict and findings).
func (f *Factory) handleLandReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := f.landReviewView(r.PathValue("by"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no landing review for this agent"})
		return
	}
	writeJSON(w, 200, rv)
}
