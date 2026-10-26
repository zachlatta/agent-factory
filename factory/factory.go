package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

type Config struct {
	Host      string // name shown on cards (this machine)
	Home      string // home dir holding ~/.paseo, ~/.claude and ~/dev
	StateDir  string // ideas.json and routes.json live here
	Port      int
	Window    time.Duration // archived agents stay in Done this long
	Poll      time.Duration
	ReworkCap int // review rounds per gate before the card goes to Zach
	IdeaWIP   int // agent-suggested cards in flight at once
	IdeaDaily int // agent-suggested cards started per day
	Location  *time.Location
	Rev       string // commit this binary was built from (shown by /api/health)
	ReadOnly  bool   // serve and refresh only: no Tick, no actions, no provider diagnostic (deploy canary)

	BoardURL     string   // where Zach opens the board, for emails; "" = "the factory board"
	DefaultRepo  string   // owner/repo for new cards, ideas and scouts with none given; errands are recorded there
	LanesFile    string   // board lane names per repo (lanes.go); default ~/.config/agent-factory/lanes.json
	Guarded      []string // public repos: scanned before landing; only Zach's own cards land without his approval (guard.go)
	PrivateRules string   // file of private scan patterns (hostnames and the like), never in a repo
	Gitleaks     string   // gitleaks binary; "" = gitleaks on PATH
	ZachEmail    string   // where cards with no email thread email Zach (mail.go); "" = they don't
	MetricHosts  []string // SSH aliases whose load the board shows besides this host (hostfleet.go)
}

func (c *Config) defaults() {
	if c.Port == 0 {
		c.Port = 7478
	}
	if c.Window == 0 {
		c.Window = 12 * time.Hour
	}
	if c.Poll == 0 {
		c.Poll = 15 * time.Second
	}
	if c.ReworkCap == 0 {
		c.ReworkCap = 3
	}
	if c.IdeaWIP == 0 {
		c.IdeaWIP = 2
	}
	if c.IdeaDaily == 0 {
		c.IdeaDaily = 5
	}
	if c.LanesFile == "" {
		c.LanesFile = filepath.Join(c.Home, ".config/agent-factory/lanes.json")
	}
	if c.Guarded == nil {
		c.Guarded = []string{"zachlatta/agent-factory"}
	}
	if c.PrivateRules == "" {
		c.PrivateRules = filepath.Join(c.Home, ".config/agent-factory/denylist")
	}
	if c.Location == nil {
		c.Location, _ = time.LoadLocation("America/New_York")
		if c.Location == nil {
			c.Location = time.Local
		}
	}
}

type Factory struct {
	cfg    Config
	p      Paseo
	ideas  *IdeaStore
	routes *RouteStore

	hostMetrics hostSampler
	fleet       *hostFleet
	// landReviews: landing reviews asked for by agents outside the card flow (landreview.go)
	landReviews *LandReviewStore
	GitHubAPI   string // base URL for repo visibility lookups
	Now         func() time.Time
	Git         func(ctx context.Context, dir string, args ...string) error           // runs `git -C dir args...`
	GitOut      func(ctx context.Context, dir string, args ...string) (string, error) // same, returns stdout
	Sleep       func(time.Duration)                                                   // waits between archive retries
	Scan        func(ctx context.Context, dir, rng string) ([]scan.Hit, error)        // secret scan of a commit range (guard.go)
	Usage       func(ctx context.Context, provider string) AgentUsage                 // remaining subscription usage per provider
	Mail        Mailer                                                                // zrl-bot mailbox for Needs Zach email; nil = none (mail.go)
	kick        chan struct{}

	mu        sync.Mutex
	list      []Agent
	cards     []Card
	updated   time.Time
	readSeq   uint64 // agent-file reads started (nextRead)
	boardSeq  uint64 // the read behind list and cards; the board never moves to an older one
	errs      map[string]string
	log       []string
	details   map[string]detailEntry // Needs Zach card id -> fetched Ask detail
	routing   map[string]bool        // pickers whose card is being started now (route.go)
	mailRetry map[string]time.Time   // card id -> no Needs Zach email before this (a send failed)
	mailProbe mailProbe              // last mailbox check, for /api/health
	once      map[string]string      // log lines written once (noteOnce)
	// archivedNow: agents the board archived, and when. Applied over every snapshot until Paseo's
	// files show the archive (or archiveMarkTTL passes), so a Refresh that read an agent file before
	// Paseo wrote the archive cannot make the card look live again.
	archivedNow map[string]time.Time

	provMu  sync.Mutex
	prov    providers // Paseo provider status, for the agent choices
	usageMu sync.Mutex
	usage   usageCache

	peerCache peerCache           // git and request reads for /api/peers
	vis       map[string]visEntry // owner/repo -> cached visibility

	// health (health.go): when the first Refresh ran and when the loop last started and finished
	hmu                          sync.Mutex
	started, tickStart, lastLoop time.Time
	refreshed                    bool
	stopping                     atomic.Bool // set when Run's context ends; Tick stops between agents
}

const archiveMarkTTL = 10 * time.Minute

func New(cfg Config, p Paseo) *Factory {
	cfg.defaults()
	f := &Factory{cfg: cfg, p: p, ideas: &IdeaStore{path: filepath.Join(cfg.StateDir, "ideas.json")},
		routes: &RouteStore{path: filepath.Join(cfg.StateDir, "routes.json")}, routing: map[string]bool{},
		landReviews: &LandReviewStore{path: filepath.Join(cfg.StateDir, "land-reviews.json")},
		GitHubAPI:   "https://api.github.com", vis: map[string]visEntry{},
		Now: time.Now, Git: runGit, GitOut: runGitOut, Sleep: time.Sleep, kick: make(chan struct{}, 1), errs: map[string]string{},
		details: map[string]detailEntry{}, archivedNow: map[string]time.Time{}, mailRetry: map[string]time.Time{},
		once: map[string]string{}}
	f.Scan = f.runScan
	f.Usage = f.readProviderUsage
	f.fleet = newHostFleet(cfg.MetricHosts, cfg.Host)
	return f
}

// SetAgents replaces the factory's view of Paseo and rebuilds the board, as a fresh read.
func (f *Factory) SetAgents(agents []Agent) {
	f.setAgents(agents, f.nextRead(), true)
}

// nextRead numbers a read of Paseo's agent files. Take it just before the read: the board only
// ever moves to a newer read (setAgents), so a slow Refresh cannot undo a board action's reload.
func (f *Factory) nextRead() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readSeq++
	return f.readSeq
}

// setAgents stores agents (read number seq) and their board unless a newer read is already
// stored, and reports whether it stored. fetch lets fillAsks read new Needs Zach detail with
// `paseo logs`; without it the board uses only the cached detail (board actions must stay fast).
func (f *Factory) setAgents(agents []Agent, seq uint64, fetch bool) bool {
	agents = f.withArchiveMarks(agents)
	cards := BuildBoard(agents, f.ideas.List(), f.Now())
	f.fillAsks(cards, agents, fetch)
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq < f.boardSeq {
		return false
	}
	f.list, f.cards, f.updated, f.boardSeq = agents, cards, f.Now(), seq
	return true
}

// withArchiveMarks returns agents with the board's recent archives applied. It copies the slice
// before changing it: the caller's list (and a Tick ranging over the old one) stays untouched.
func (f *Factory) withArchiveMarks(agents []Agent) []Agent {
	now := f.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, at := range f.archivedNow {
		if now.Sub(at) > archiveMarkTTL {
			delete(f.archivedNow, id)
		}
	}
	if len(f.archivedNow) == 0 {
		return agents
	}
	out := slices.Clone(agents)
	for i := range out {
		at, ok := f.archivedNow[out[i].ID]
		switch {
		case !ok:
		case out[i].archived() && !out[i].ArchivedAt.Equal(at):
			delete(f.archivedNow, out[i].ID) // Paseo's own archive time: the files have caught up
		default:
			out[i].ArchivedAt = at
		}
	}
	return out
}

// markArchived records that the board archived agent id and rebuilds the board at once, so the
// next Tick does not act on a snapshot taken before the archive. It rebuilds the stored list under
// that list's own read number; if a newer reload stores in between, this store is refused, and the
// mark in archivedNow still applies to that newer board and every later one.
func (f *Factory) markArchived(id string) {
	f.mu.Lock()
	f.archivedNow[id] = f.Now()
	list, seq := f.list, f.boardSeq
	f.mu.Unlock()
	f.setAgents(list, seq, false)
}

// Refresh reloads agents from disk, with pending permissions and new Needs Zach detail. Those
// take seconds of paseo calls; if a board action reloaded meanwhile, Refresh reads the files again
// and stores that with what it fetched, so the action's change stays on the board.
func (f *Factory) Refresh() {
	seq := f.nextRead()
	agents, err := LoadAgents(f.cfg.Home, f.cfg.Host, f.cfg.Window, f.Now())
	f.mu.Lock()
	if err != nil {
		f.errs[f.cfg.Host] = err.Error()
	} else {
		delete(f.errs, f.cfg.Host)
	}
	f.mu.Unlock()
	if err == nil {
		f.addPending(agents)
		if !f.setAgents(agents, seq, true) {
			pending := map[string]string{}
			for _, a := range agents {
				pending[a.ID] = a.Pending
			}
			if stored, err := f.load(pending); !stored {
				// a newer board stays; these permissions show after the next loop
				f.logf("refresh gave up: board actions reloaded during it (re-read error: %v)", err)
			}
		}
	}
	f.noteRefreshed()
}

// reload re-reads the agent files and rebuilds the board with no paseo calls: pending permissions
// carry over from the current list and Needs Zach detail comes from the cache. A board action
// calls it so its change shows at once instead of after the next loop.
func (f *Factory) reload() error {
	pending := map[string]string{}
	for _, a := range f.agents() {
		pending[a.ID] = a.Pending
	}
	_, err := f.load(pending)
	return err
}

// load reads the agent files, applies pending, and stores the board without fetching detail. It
// reports whether it stored: false when the read failed or a newer read is already on the board.
func (f *Factory) load(pending map[string]string) (bool, error) {
	seq := f.nextRead()
	agents, err := LoadAgents(f.cfg.Home, f.cfg.Host, f.cfg.Window, f.Now())
	if err != nil {
		return false, err
	}
	for i := range agents {
		agents[i].Pending = pending[agents[i].ID]
	}
	return f.setAgents(agents, seq, false), nil
}

// Run polls Paseo and applies the rules until ctx ends. A POST kicks an early refresh. When ctx
// ends, a Tick in progress finishes the agent it is on and skips the rest; Run then returns.
func (f *Factory) Run(ctx context.Context) {
	t := time.NewTicker(f.cfg.Poll)
	defer t.Stop()
	stop := context.AfterFunc(ctx, func() { f.stopping.Store(true) })
	defer stop()
	for {
		f.loopStarted()
		f.Refresh()
		if !f.cfg.ReadOnly {
			f.Tick()
			f.Refresh()
			f.providerStatus("claude") // keeps the provider list fresh (re-read every providerListTTL)
			f.providerUsages()         // same for agent usage: warmed here, never on the request path
		}
		f.loopDone()
		select {
		case <-ctx.Done():
			f.stopping.Store(true)
			return
		case <-t.C:
		case <-f.kick:
		}
	}
}

func (f *Factory) poke() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

func (f *Factory) agents() []Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list
}

func (f *Factory) agentOK(id string) (Agent, bool) {
	if id == "" {
		return Agent{}, false
	}
	for _, a := range f.agents() {
		if a.ID == id || strings.HasPrefix(a.ID, id) {
			return a, true
		}
	}
	return Agent{}, false
}

func (f *Factory) agent(id string) Agent { a, _ := f.agentOK(id); return a }

func (f *Factory) card(id string) (Card, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		return Card{}, false
	}
	for _, c := range f.cards {
		if c.ID == id || strings.HasPrefix(c.ID, id) {
			return c, true
		}
	}
	return Card{}, false
}

func (f *Factory) logf(format string, args ...any) {
	line := f.Now().In(f.cfg.Location).Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	fmt.Println(line)
	f.mu.Lock()
	f.log = append(f.log, line)
	if len(f.log) > 200 {
		f.log = f.log[len(f.log)-200:]
	}
	f.mu.Unlock()
}

func (f *Factory) paseo(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return f.p.Run(ctx, args...)
}

func (f *Factory) setLabels(id string, labels map[string]string) error {
	args := []string{"agent", "update", id}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", labelArg(k, labels[k]))
	}
	_, err := f.paseo(args...)
	return err
}

func (f *Factory) send(id, msg string, images ...string) error {
	args := []string{"send", "--no-wait"}
	for _, p := range images {
		args = append(args, "--image", p)
	}
	_, err := f.paseo(append(args, id, msg)...)
	return err
}

// archiveAgent archives agent id and marks the snapshot archived. `paseo archive` refuses a running
// agent, so only then is it stopped first (an idle agent costs one paseo call, not two); stop can
// return before the run has wound down, so a "currently running" archive is retried for a few seconds.
func (f *Factory) archiveAgent(id string) error {
	_, err := f.paseo("archive", id)
	if err != nil && strings.Contains(err.Error(), "currently running") {
		// Paseo refuses a running agent without touching it: stop it, then retry while it winds down
		if _, err := f.paseo("stop", id); err != nil {
			return err
		}
		for try := 1; ; try++ {
			_, err = f.paseo("archive", id)
			if err == nil || try == archiveTries || !strings.Contains(err.Error(), "currently running") {
				break
			}
			f.Sleep(time.Second)
		}
	}
	if err != nil {
		return err
	}
	f.markArchived(id)
	return nil
}

const archiveTries = 5

// liveState asks Paseo (not the snapshot, which can be a poll old) whether agent id is archived
// or running.
func (f *Factory) liveState(id string) (archived, running bool, err error) {
	out, err := f.paseo("inspect", id, "--json")
	if err != nil {
		return false, false, err
	}
	var s struct {
		Status   string
		Archived bool
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return false, false, fmt.Errorf("paseo inspect %s: %v", short(id), err)
	}
	return s.Archived, s.Status == "running", nil
}

func (f *Factory) lastText(id string, n int) (string, error) {
	return f.paseo("logs", id, "--tail", fmt.Sprint(n), "--filter", "text")
}

// Repos lists git checkouts under ~/dev/<owner>/<repo> that a new card can start in, the default first.
func (f *Factory) Repos() []string {
	dirs, _ := filepath.Glob(filepath.Join(f.cfg.Home, "dev", "*", "*", ".git"))
	var out []string
	for _, d := range dirs {
		r := filepath.Base(filepath.Dir(filepath.Dir(d))) + "/" + filepath.Base(filepath.Dir(d))
		if strings.Contains(r, "archive") || strings.Contains(r, "-wt-") {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == f.cfg.DefaultRepo && out[j] != f.cfg.DefaultRepo })
	return out
}

// runGit runs `git -C dir args...`; a failure carries git's stderr.
func runGit(ctx context.Context, dir string, args ...string) error {
	_, err := runGitOut(ctx, dir, args...)
	return err
}

func runGitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, trunc(msg, 300))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.String(), nil
}

// CLI runs the real paseo binary against the local daemon.
type CLI struct{ Bin string }

func (c CLI) Run(ctx context.Context, args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "paseo"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// started from inside a Paseo agent? don't let the CLI think we are that agent
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PASEO_AGENT_ID=") && !strings.HasPrefix(kv, "PASEO_AGENT_CWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		verb := strings.Join(args[:min(2, len(args))], " ")
		return "", fmt.Errorf("paseo %s: %s", verb, trunc(msg, 400))
	}
	return stdout.String(), nil
}
