package factory

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed web/board.html web/transcript.html web/host.html web/host-metrics.js
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
	mux.HandleFunc("GET /t/{id}", gzipHandler(page("transcript.html")))
	mux.HandleFunc("GET /api/board", gzipHandler(f.handleBoard))
	mux.HandleFunc("GET /api/ideas", gzipHandler(f.handleIdeas))
	mux.HandleFunc("GET /api/peers", gzipHandler(f.handlePeers))
	mux.HandleFunc("GET /api/agents", gzipHandler(f.handleAgents))
	mux.HandleFunc("GET /api/lanes", gzipHandler(f.handleLanes))
	mux.HandleFunc("GET /api/health", gzipHandler(f.handleHealth))
	mux.HandleFunc("GET /api/deploy", gzipHandler(f.handleDeploy))
	mux.HandleFunc("GET /api/transcript/{id}", gzipHandler(f.handleTranscript))
	mux.HandleFunc("GET /api/logs/{id}", gzipHandler(f.handleLogs))
	mux.HandleFunc("GET /api/land-review/{by}", gzipHandler(f.handleLandReview))
	mux.HandleFunc("GET /host", page("host.html"))
	mux.HandleFunc("GET /host-metrics.js", page("host-metrics.js"))
	mux.HandleFunc("GET /api/host", f.handleHost)
	mux.HandleFunc("GET /api/hosts", f.handleHosts)
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
	writeJSON(w, 200, map[string]any{"cards": cards, "updated": iso(updated), "errors": errs, "stages": Stages,
		"hosts": []string{f.cfg.Host}, "log": log, "repos": f.Repos(), "tiers": TierHelp,
		"caps": map[string]int{"ideaWip": f.cfg.IdeaWIP, "ideaDaily": f.cfg.IdeaDaily, "rework": f.cfg.ReworkCap}})
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

func (f *Factory) handleTranscript(w http.ResponseWriter, r *http.Request) {
	a, found := f.agentOK(r.PathValue("id"))
	if !found {
		writeJSON(w, 404, map[string]string{"error": "agent not on the board (open, or archived in the last 12 h)"})
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	res, err := ReadTranscript(f.cfg.Home, TranscriptSource{Provider: a.Provider, SessionID: a.SessionID, Cwd: a.Cwd}, after)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if res.Fallback {
		text, err := f.paseo("logs", a.ID, "--tail", "2000")
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		// the summary log has no stable line numbers: send all of it each time
		res.Events, res.Next = parseLogs(text), 0
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
		"provider": res.Provider, "stages": Stages, "parent": parent, "card": nil,
		"agent": map[string]any{"id": a.ID, "host": a.Host, "title": a.Title, "status": a.Status,
			"archivedAt": iso(a.ArchivedAt), "cwd": a.Cwd, "repo": a.Repo, "branch": a.Branch, "model": a.Model,
			"createdAt": iso(a.CreatedAt), "lastActivityAt": iso(a.LastActivityAt), "labels": a.Labels}}
	if isCard {
		out["card"] = c
	}
	writeJSON(w, 200, out)
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
	text, err := f.lastText(id, 30)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"text": text})
}

var actions = map[string]bool{"new": true, "idea": true, "scout": true, "stage": true, "review": true, "send": true,
	"approve": true, "recommend": true, "rerun": true, "stop": true, "archive": true, "done": true, "close": true,
	"land-review": true, "answer": true}

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
	f.poke()
	writeJSON(w, 200, res)
}

// handleLandReview reports an agent's latest landing review (running, or done with verdict and findings).
func (f *Factory) handleLandReview(w http.ResponseWriter, r *http.Request) {
	rv, ok := f.landReviews.Get(r.PathValue("by"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no landing review for this agent"})
		return
	}
	writeJSON(w, 200, rv)
}
