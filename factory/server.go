package factory

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed web/board.html web/transcript.html
var web embed.FS

// Handler serves the board, the transcript pages and the JSON API.
func (f *Factory) Handler() http.Handler {
	mux := http.NewServeMux()
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := web.ReadFile("web/" + name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", page("board.html"))
	mux.HandleFunc("GET /index.html", page("board.html"))
	mux.HandleFunc("GET /t/{id}", page("transcript.html"))
	mux.HandleFunc("GET /api/board", f.handleBoard)
	mux.HandleFunc("GET /api/ideas", f.handleIdeas)
	mux.HandleFunc("GET /api/peers", f.handlePeers)
	mux.HandleFunc("GET /api/agents", f.handleAgents)
	mux.HandleFunc("GET /api/lanes", f.handleLanes)
	mux.HandleFunc("GET /api/health", f.handleHealth)
	mux.HandleFunc("GET /api/deploy", f.handleDeploy)
	mux.HandleFunc("GET /api/transcript/{id}", f.handleTranscript)
	mux.HandleFunc("GET /api/logs/{id}", f.handleLogs)
	mux.HandleFunc("GET /api/land-review/{by}", f.handleLandReview)
	mux.HandleFunc("POST /api/{action}", f.handleAction)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
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
		all := parseLogs(text)
		res.Events, res.Next = all[min(after, len(all)):], len(all)
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
	out := map[string]any{"events": res.Events, "next": res.Next, "fallback": res.Fallback, "provider": res.Provider,
		"stages": Stages, "parent": parent, "card": nil,
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
	"land-review": true}

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
		writeJSON(w, 400, map[string]string{"error": err.Error()})
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
