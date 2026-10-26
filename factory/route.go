package factory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Repo picking: a new card or a Scout (Suggest ideas) can start without a repo; the factory finds
// the best one, and asks Zach only when it truly can't tell.
//
// A New card with repo "auto" starts a read-only picker card (factory.routing) in ~/dev. When it
// stops, its last reply names a repo (REPO: owner/name) and the factory starts the real card there
// with Zach's saved request, then archives the picker. "REPO: unsure", an unknown repo or no line
// sends the picker to Needs Zach (factory.awaiting=repo) with buttons for its candidates.

const autoRepo = "auto"

const routedNote = "started in " // + repo: a picker whose card started

// Route is a picker's saved New card request, kept until the real card starts.
type Route struct {
	Request   string    `json:"request"`
	Title     string    `json:"title"`
	Thread    string    `json:"thread,omitempty"`
	Origin    string    `json:"origin,omitempty"`
	Tier      string    `json:"tier,omitempty"`
	Agent     string    `json:"agent,omitempty"` // AgentChoice id for the real card ("" = default)
	Scout     bool      `json:"scout,omitempty"` // start a Scout (Suggest ideas) with Request as guidance, not a card
	CreatedAt time.Time `json:"createdAt"`
}

// RouteStore keeps routes in one JSON file, keyed by picker agent id.
type RouteStore struct {
	path string
	mu   sync.Mutex
}

func (s *RouteStore) load() map[string]Route {
	m := map[string]Route{}
	readJSON(s.path, &m)
	return m
}

func (s *RouteStore) update(fn func(map[string]Route)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	fn(m)
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *RouteStore) get(id string) (Route, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.load()[id]
	return r, ok
}

func (s *RouteStore) put(id string, r Route) error {
	return s.update(func(m map[string]Route) { m[id] = r })
}

func (s *RouteStore) del(id string) error {
	return s.update(func(m map[string]Route) { delete(m, id) })
}

var (
	repoLineRe = regexp.MustCompile(`(?im)^[\s*>_-]*REPO:\s*\**\s*` + "`?" + `([A-Za-z0-9._/-]+)`)
	candLineRe = regexp.MustCompile(`(?im)^[\s*>_-]*CANDIDATES:\s*(.+)$`)
)

// ParseRepo reads a picker's reply: the repo named on its last REPO line if it is one of repos, and
// the candidates (from the CANDIDATES line, known repos only) for Zach when it is not.
func ParseRepo(text string, repos []string) (repo string, candidates []string) {
	known := func(s string) string {
		s = strings.Trim(strings.TrimSpace(s), "`*.,;")
		for _, r := range repos {
			if strings.EqualFold(r, s) {
				return r
			}
		}
		return ""
	}
	if m := repoLineRe.FindAllStringSubmatch(text, -1); len(m) > 0 {
		repo = known(m[len(m)-1][1])
	}
	if m := candLineRe.FindAllStringSubmatch(text, -1); len(m) > 0 {
		for _, s := range strings.FieldsFunc(m[len(m)-1][1], func(r rune) bool { return r == ',' || r == ' ' }) {
			if r := known(s); r != "" && !slices.Contains(candidates, r) {
				candidates = append(candidates, r)
			}
		}
	}
	return repo, candidates
}

const pickerPrompt = `You pick the repo for a new card on Zach's agent factory board. Read-only: do not edit,
commit, check out or land anything, and do not message anyone. Another agent does the work; you only
choose where it starts.

Zach's request:
%s

Candidate repos (checkouts under %s):
%s

Pick the repo where this work would land. Read each likely repo's README and AGENTS.md, its recent
` + "`git log`" + `, and grep it for names in the request. Check past agent sessions for earlier work on
the same thing (PDW: ` + "`pdw readme agent-sessions`" + `). Errands that change no code (a question, research,
a report or an email to Zach, a PDW mutation) go to <default repo>, where such work is recorded.
Be quick: a few minutes at most.

End your reply with a short reason, then one last line in exactly one of these forms:
REPO: <owner/name from the list>
or, only if you truly cannot tell, a one-sentence question for Zach and then:
CANDIDATES: <owner/name>, <owner/name>
REPO: unsure
`

// readmeLine is the first line of a repo's README that says something (a heading or a sentence).
func readmeLine(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		return ""
	}
	var parts []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.TrimLeft(l, "# "))
		if l == "" || strings.HasPrefix(l, "[!") || strings.HasPrefix(l, "<") {
			continue
		}
		parts = append(parts, l)
		if len(parts) == 2 {
			break
		}
	}
	return trunc(strings.Join(parts, ": "), 200)
}

// scoutPickRequest is what a picker reads for a Scout: where the ideas would land, not the errand repo.
const scoutPickRequest = `Suggest ideas: a read-only Scout will look for up to 3 improvements to make. Pick the repo
those improvements would change. Zach's guidance for the Scout:
`

// startPicker starts the repo picker for a New card or Scout with repo "auto" and saves r for it.
func (f *Factory) startPicker(r Route) (map[string]string, error) {
	dev := filepath.Join(f.cfg.Home, "dev")
	var list []string
	for _, r := range f.Repos() {
		line := "- " + r
		if d := readmeLine(filepath.Join(dev, r)); d != "" {
			line += ": " + d
		}
		list = append(list, line)
	}
	def := f.cfg.DefaultRepo
	if def == "" {
		def = "the default repo"
	}
	req := r.Request
	if r.Scout {
		req = scoutPickRequest + req
	}
	prompt := fmt.Sprintf(strings.ReplaceAll(pickerPrompt, "<default repo>", def), req, dev, strings.Join(list, "\n"))
	out, err := f.paseo("run", "--provider", ReviewerModel, "--thinking", "low", "--mode", "bypassPermissions",
		"--cwd", dev, "--title", r.Title,
		"--label", labelArg(labelSource, "board"), "--label", labelArg(labelStage, "plan"),
		"--label", labelArg(labelRouting, "1"), "--label", labelArg(labelSince, iso(f.Now())),
		"--background", "--quiet", prompt)
	if err != nil {
		return nil, err
	}
	id := lastLine(out)
	r.CreatedAt = f.Now()
	if err := f.routes.put(id, r); err != nil {
		return nil, fmt.Errorf("picker %s started, but saving its request failed: %v", short(id), err)
	}
	f.logf("new card %s: picking a repo for %s", short(id), r.Title)
	f.poke()
	return map[string]string{"id": id}, nil
}

// routeFromReply acts on a stopped picker's reply: start the card in the repo it named, or ask Zach.
func (f *Factory) routeFromReply(a Agent) error {
	text, err := f.lastReply(a)
	if err != nil {
		return err
	}
	repo, cands := ParseRepo(text, f.Repos())
	if repo != "" {
		return f.startRouted(a, repo)
	}
	f.logf("%s could not pick a repo: asking Zach", short(a.ID))
	return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "pick a repo",
		labelCandidates: strings.Join(cands, " "), labelSince: iso(f.Now())})
}

// startRouted starts picker a's real card in repo, then archives the picker. The picker is claimed
// first (in memory and with factory.routed=starting), so a Tick and a click cannot start it twice;
// its saved request is deleted only once the new card exists.
func (f *Factory) startRouted(a Agent, repo string) error {
	f.mu.Lock()
	if f.routing[a.ID] {
		f.mu.Unlock()
		return errors.New("this card is already starting")
	}
	f.routing[a.ID] = true
	f.mu.Unlock()
	release := func() {
		f.mu.Lock()
		delete(f.routing, a.ID)
		f.mu.Unlock()
	}
	now := iso(f.Now())
	failed := func(why error) error {
		release()
		f.logf("%s routing to %s failed: %v", short(a.ID), repo, why)
		if err := f.setLabels(a.ID, map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "routing failed",
			labelRouted: "", labelSince: now}); err != nil {
			return err
		}
		return why
	}
	r, ok := f.routes.get(a.ID)
	if !ok {
		return failed(errors.New("its request was not saved: start a new card"))
	}
	if err := f.setLabels(a.ID, map[string]string{labelRouted: "starting"}); err != nil {
		release()
		return err
	}
	var res map[string]string
	var err error
	if r.Scout {
		res, err = f.startScout(repo, r.Request)
	} else {
		res, err = f.newCard(Body{Request: r.Request, Repo: repo, Title: r.Title, Thread: r.Thread, Origin: r.Origin, Tier: r.Tier, Agent: r.Agent})
	}
	if err != nil {
		return failed(err)
	}
	f.logf("%s routed to %s: %s", short(a.ID), repo, short(res["id"]))
	return f.finishRouted(a.ID, res["id"], repo)
}

// finishRouted records the new card on the picker, archives the picker and drops its saved request.
// A failed step is retried by the next Tick (factory.routed holds the new card's id).
func (f *Factory) finishRouted(id, card, repo string) error {
	if err := f.setLabels(id, map[string]string{labelRouted: card, labelAwaiting: "", labelNote: routedNote + repo}); err != nil {
		return err
	}
	if err := f.archiveAgent(id); err != nil {
		return err
	}
	return f.routes.del(id)
}

// tickPicker is the Tick rule for a stopped picker in Plan.
func (f *Factory) tickPicker(a Agent) error {
	f.mu.Lock()
	busy := f.routing[a.ID]
	f.mu.Unlock()
	switch routed := a.L(labelRouted); {
	case busy:
		return nil
	case routed == "starting":
		// claimed, but the factory stopped before the new card's id was recorded
		f.logf("%s routing interrupted: asking Zach", short(a.ID))
		return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelAwaiting: "repo", labelNote: "routing interrupted",
			labelSince: iso(f.Now())})
	case routed != "":
		return f.finishRouted(a.ID, routed, strings.TrimPrefix(a.L(labelNote), routedNote))
	}
	return f.routeFromReply(a)
}

// pickRepo is Zach's answer to a picker in Needs Zach (POST /api/approve with a repo).
func (f *Factory) pickRepo(a Agent, repo string) error {
	if repo == "" || !slices.Contains(f.Repos(), repo) {
		return errors.New("pick one of the repos")
	}
	if r := a.L(labelRouted); r != "" && a.L(labelNote) != "routing interrupted" {
		return errors.New("this card already started in a repo")
	}
	return f.startRouted(a, repo)
}
