package factory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// IdeaStore keeps suggested cards in one JSON file (ideas are not Paseo agents until started).
type IdeaStore struct {
	path string
	mu   sync.Mutex
}

func (s *IdeaStore) List() []Idea {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *IdeaStore) load() []Idea {
	var ideas []Idea
	readJSON(s.path, &ideas)
	return ideas
}

func (s *IdeaStore) save(ideas []Idea) error {
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, err := json.MarshalIndent(ideas, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// update loads, lets fn change the list, and saves it, under the lock.
func (s *IdeaStore) update(fn func([]Idea) ([]Idea, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ideas, err := fn(s.load())
	if err != nil {
		return err
	}
	return s.save(ideas)
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "idea-" + hex.EncodeToString(b)
}

// ideaRepeatWindow is how long a started idea's title stays blocked (dismissed titles stay blocked).
const ideaRepeatWindow = 14 * 24 * time.Hour

func (f *Factory) addIdea(b Body) (Idea, error) {
	req := strings.TrimSpace(b.Request)
	if req == "" {
		return Idea{}, errors.New("request is empty")
	}
	title := b.Title
	if title == "" {
		title = strings.SplitN(req, "\n", 2)[0]
	}
	idea := Idea{ID: newID(), Title: truncWords(strings.TrimSpace(title), 70), Request: trunc(req, 6000),
		Summary: trunc(strings.TrimSpace(b.Summary), 600), Why: trunc(b.Why, 2000), DoneWhen: trunc(b.DoneWhen, 1000), Repo: b.Repo, By: trunc(b.By, 80), CreatedAt: f.Now(), Status: "waiting"}
	if idea.Repo == "" {
		idea.Repo = f.cfg.DefaultRepo
	}
	if idea.By == "" {
		idea.By = "agent"
	}
	if tierRank(b.Tier) >= 0 {
		idea.Tier = b.Tier
	}
	if err := checkAgentID(b.Agent); err != nil {
		return Idea{}, err
	}
	if b.Thinking != "" {
		a, ok := agentByID(b.Agent)
		if !ok {
			return Idea{}, errors.New("choose an agent before setting idea thinking")
		}
		if _, err := a.withThinking(b.Thinking); err != nil {
			return Idea{}, err
		}
	}
	idea.Agent, idea.Thinking = b.Agent, b.Thinking
	err := f.ideas.update(func(ideas []Idea) ([]Idea, error) {
		for _, i := range ideas {
			if !strings.EqualFold(i.Title, idea.Title) {
				continue
			}
			switch i.Status {
			case "waiting":
				return nil, fmt.Errorf("idea %s with that title is already waiting", i.ID)
			case "dismissed":
				return nil, fmt.Errorf("idea %s with that title was dismissed", i.ID)
			case "started":
				at := i.StartedAt
				if at.IsZero() {
					at = i.CreatedAt
				}
				if idea.CreatedAt.Sub(at) < ideaRepeatWindow {
					return nil, fmt.Errorf("idea %s with that title was started %s (card %s)", i.ID,
						at.Format("2006-01-02"), i.Card)
				}
			}
		}
		return append(ideas, idea), nil
	})
	if err != nil {
		return Idea{}, err
	}
	f.logf("idea %s from %s: %s", idea.ID, idea.By, idea.Title)
	return idea, nil
}

// startIdea turns a waiting idea into a planning card. When Zach starts it, agent (his pick in the
// panel) wins, else the idea's suggested agent. The factory's own starts (by "auto") clear the
// idea's suggested agent so newCard uses the usage-balanced auto-suggestion.
// note is Zach's own changes typed before starting; it follows the brief and overrides it.
func (f *Factory) startIdea(id, by, agent, note, thinking string) (map[string]string, error) {
	var res map[string]string
	note = trunc(strings.TrimSpace(note), 4000)
	err := f.ideas.update(func(ideas []Idea) ([]Idea, error) {
		for n := range ideas {
			i := &ideas[n]
			if i.ID != id || i.Status != "waiting" {
				continue
			}
			req := i.Request
			if i.Why != "" {
				req += fmt.Sprintf("\n\nWhy (from %s): %s", i.By, i.Why)
			}
			if note != "" {
				req += "\n\nZach's changes before starting (his own words; where they differ from the brief above, his win):\n" + note
			}
			switch {
			case by == "auto":
				agent, thinking = "", ""
			case agent == "":
				agent = i.Agent
				if thinking == "" {
					thinking = i.Thinking
				}
			}
			r, err := f.newCard(Body{Request: req, Repo: i.Repo, Title: i.Title, Origin: "idea", Tier: i.Tier, Agent: agent, Thinking: thinking})
			if err != nil {
				return nil, err
			}
			i.Status, i.Card, i.StartedAt, i.StartedBy, i.Note = "started", r["id"], f.Now(), by, note
			res = r
			return ideas, nil
		}
		return nil, errors.New("idea not found")
	})
	return res, err
}

func (f *Factory) dismissIdea(id string) error {
	return f.ideas.update(func(ideas []Idea) ([]Idea, error) {
		for n := range ideas {
			if ideas[n].ID == id {
				ideas[n].Status = "dismissed"
				return ideas, nil
			}
		}
		return nil, errors.New("idea not found")
	})
}

// replaceCard updates any started idea pointing at oldID to point at its replacement newID.
func (s *IdeaStore) replaceCard(oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	return s.update(func(ideas []Idea) ([]Idea, error) {
		for n := range ideas {
			if ideas[n].Card != "" && (ideas[n].Card == oldID || strings.HasPrefix(oldID, ideas[n].Card)) {
				ideas[n].Card = newID
			}
		}
		return ideas, nil
	})
}

// tickIdeas starts the oldest waiting idea if the in-flight and daily caps allow.
func (f *Factory) tickIdeas() error {
	ideas := f.ideas.List()
	var waiting []Idea
	for _, i := range ideas {
		// a public repo's ideas wait for Zach's Start (guard.go)
		if i.Status == "waiting" && !f.guarded(i.Repo) {
			waiting = append(waiting, i)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	now := f.Now().In(f.cfg.Location)
	day := now.Format("2006-01-02")
	agents := map[string]Agent{}
	for _, a := range f.agents() {
		agents[a.ID] = a
	}
	startedToday, inFlight := 0, 0
	for _, i := range ideas {
		if i.Status != "started" {
			continue
		}
		if i.StartedBy == "auto" && i.StartedAt.In(f.cfg.Location).Format("2006-01-02") == day {
			startedToday++
		}
		a, ok := agents[resolveCardID(agents, i.Card)]
		switch {
		case !ok && now.Sub(i.StartedAt) < 10*time.Minute: // just started, not on disk yet
			inFlight++
		case ok && a.Status != "error" && !a.archived() && Derive(a) != "landed" && Derive(a) != "zach":
			inFlight++
		}
	}
	if inFlight >= f.cfg.IdeaWIP || startedToday >= f.cfg.IdeaDaily {
		return nil
	}
	sort.Slice(waiting, func(a, b int) bool { return waiting[a].CreatedAt.Before(waiting[b].CreatedAt) })
	if _, err := f.startIdea(waiting[0].ID, "auto", "", "", ""); err != nil {
		return err
	}
	f.logf("started idea %s (%d/%d in flight, %d/%d today)", waiting[0].ID, inFlight+1, f.cfg.IdeaWIP,
		startedToday+1, f.cfg.IdeaDaily)
	return nil
}

const scoutPrompt = `You are the Scout for Zach's agent factory. Find up to %d improvements worth making in
%s (checkout: %s; read its AGENTS.md and README first). Good sources: bugs and TODOs in the code,
recent failures and Zach's corrections in past agent sessions about this repo (PDW:
` + "`pdw readme agent-sessions`" + `), slow or flaky paths, docs that disagree with the code.

Rules:
- Read-only. Do not edit, commit or land anything. Do not message anyone.
- The checkout may be behind and is shared: run ` + "`git -C %[3]s fetch -q origin`" + ` and read the latest
  code at origin/main (` + "`git show origin/main:<path>`" + `, or a scratch worktree under /tmp). Never
  change, pull or check out anything in the checkout itself.
- Only ideas with a concrete payoff you can name; no busywork, no style churn. Fewer is better.
- Check each idea is not already done, waiting, started or dismissed: ` + "`curl -s localhost:%d/api/ideas`" + `
  (all ideas, newest first; a dismissed title is rejected) and ` + "`curl -s localhost:%d/api/peers?repo=" + "<repo>`" + `
  (cards in flight: what each is doing and the files it changes).
- Each idea is self-contained: what to change, where, why, and how to tell it worked.
- Zach reviews ideas on the board, often from his phone. Write for him first:
  - title: the outcome, at most 60 characters ("Dropped cards stay dropped", not a function name).
  - summary: 1-2 plain sentences: what changes and what he gets. No file paths, function names
    or line numbers.
  - why: the evidence (what you saw, where, when).
  - doneWhen: how anyone can tell it worked.
  - request: the technical brief for the builder (files, functions, tests). Paths go here.

%s

%s

Submit each idea:
curl -s -X POST localhost:%d/api/idea -H 'X-Factory: 1' -H 'Content-Type: application/json' \
  -d '{"repo":"%s","by":"scout","tier":"<low|risky|architecture>","title":"<outcome>","summary":"<plain words>","why":"<evidence>","doneWhen":"<check>","request":"<technical brief>"}'

Then stop with a one-line list of what you submitted.
`

// startScout starts a read-only agent that suggests ideas for repo through POST /api/idea.
func (f *Factory) startScout(repo, guidance string) (map[string]string, error) {
	if repo == "" {
		repo = f.cfg.DefaultRepo
	}
	if repo == autoRepo {
		if strings.TrimSpace(guidance) == "" {
			return nil, errors.New("Suggest ideas with Repo Auto needs a prompt: pick a repo, or say what ideas you want")
		}
		return f.startPicker(Route{Request: guidance, Title: truncWords("Scout: "+strings.SplitN(strings.TrimSpace(guidance), "\n", 2)[0], 70), Scout: true})
	}
	cwd := filepath.Join(f.cfg.Home, "dev", repo)
	prompt := strings.ReplaceAll(fmt.Sprintf(scoutPrompt, 3, repo, cwd, f.cfg.Port, f.cfg.Port, tierRules, wholeChangeRule,
		f.cfg.Port, repo), "<repo>", repo)
	title := "Scout: ideas for " + repo
	if strings.TrimSpace(guidance) != "" {
		prompt += "\nGuidance:\n" + guidance + "\n"
	}
	out, err := f.paseo(append(reviewerRunArgs(), "--cwd", cwd, "--title", truncWords(title, 70),
		"--label", labelArg(labelRole, "scout"), "--label", labelArg(labelSource, "board"), "--background", "--quiet", prompt)...)
	if err != nil {
		return nil, err
	}
	id := lastLine(out)
	f.logf("scout %s started for %s", short(id), repo)
	return map[string]string{"id": id}, nil
}
