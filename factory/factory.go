package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	StateDir  string // ideas.json, routes.json and steers.json live here
	Port      int
	Window    time.Duration // archived agents stay in Done this long
	Poll      time.Duration
	ReworkCap int           // review rounds per gate before the card goes to Zach
	IdleTTL   time.Duration // a Needs Zach card idle this long is parked (reap.go)
	IdleCap   int           // more live idle factory agents than this parks the longest idle (reap.go)
	Location  *time.Location
	Rev       string // commit this binary was built from (shown by /api/health)
	ReadOnly  bool   // serve and refresh only: no Tick, no actions, no provider diagnostic (deploy canary)
	DryRun    bool   // serve real data; actions answer but only log their paseo and git writes (dryrun.go)

	// NoStageHandoff resumes the card's session at every stage instead of starting a fresh agent (stagehandoff.go).
	NoStageHandoff bool

	BoardURL     string   // where Zach opens the board, for emails; "" = "the factory board"
	DefaultRepo  string   // owner/repo for new cards, ideas and scouts with none given; errands are recorded there
	LanesFile    string   // board lane names per repo (lanes.go); default ~/.config/agent-factory/lanes.json
	Guarded      []string // public repos: public-repo review rule, then scanned before landing (guard.go)
	PrivateRules string   // file of private scan patterns (hostnames and the like), never in a repo
	Gitleaks     string   // gitleaks binary; "" = gitleaks on PATH
	ZachEmail    string   // where cards with no email thread email Zach (mail.go); "" = they don't
	DigestTo     string   // where the daily digest goes (digest.go); "" = no digest, never ZachEmail
	MetricHosts  []string // SSH aliases whose load the board shows besides this host (hostfleet.go)
	PreviewPorts string   // "lo-hi": the ports previews run and are published on (tryout.go)
	PreviewMax   int      // previews running at once
	Unanswered   string   // the email dispatcher's file of requests with no answer (unanswered.go); "" = none
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
	if c.IdleTTL == 0 {
		c.IdleTTL = defaultIdleTTL
	}
	if c.IdleCap == 0 {
		c.IdleCap = defaultIdleCap
	}
	if c.PreviewPorts == "" {
		c.PreviewPorts = "7700-7799"
	}
	if c.PreviewMax == 0 {
		c.PreviewMax = 8
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
	quietMu    sync.Mutex
	quiet      quietState
	recoveryMu sync.Mutex              // serializes recovery ticks and explicit Resume (recover.go)
	recoveries map[string]recovery     // latest persisted recovery across stale snapshots
	handoffMu  sync.Mutex              // guards handoffs
	handoffs   map[string]handoffState // latest persisted hand-off step across stale snapshots (handoff.go)
	panelMu    sync.Mutex              // guards panels and lenses (panel.go)
	panels     map[string]string       // card id -> its open panel round ("" closed), ahead of stale snapshots
	lenses     map[string]lensResult   // panel reviewer id -> its recorded result, ahead of stale snapshots
	cardGates  sync.Map                // card ID -> *cardGate: its follow-ups and review verdicts (actions.go)

	cfg        Config
	p          Paseo
	ideas      *IdeaStore
	routes     *RouteStore
	replies    *replyStore
	ReplyCheck func(context.Context, ReplyContext) (string, error)
	steers     *SteerStore   // closed duplicates' requests on their way to the kept card (steer.go)
	dismissed  *DismissStore // flagged requests Zach dismissed (unanswered.go)

	hostMetrics hostSampler
	fleet       *hostFleet
	// landReviews: landing reviews asked for by agents outside the card flow (landreview.go)
	landReviews *LandReviewStore
	timings     *TimingStore // card timings (timing.go)
	GitHubAPI   string       // base URL for repo visibility lookups
	Now         func() time.Time
	Git         func(ctx context.Context, dir string, args ...string) error           // runs `git -C dir args...`
	GitOut      func(ctx context.Context, dir string, args ...string) (string, error) // same, returns stdout
	Sleep       func(time.Duration)                                                   // waits between archive retries
	Scan        func(ctx context.Context, dir, rng string) ([]scan.Hit, error)        // secret scan of a commit range (guard.go)
	Usage       func(ctx context.Context, provider string) AgentUsage                 // remaining subscription usage per provider
	Mail        Mailer                                                                // zrl-bot mailbox for Needs Zach email; nil = none (mail.go)
	kick        chan struct{}
	hotKick     chan struct{} // the agent watcher marked agents hot (watch.go): a hot pass runs

	mu            sync.Mutex
	list          []Agent
	cards         []Card
	updated       time.Time
	readSeq       uint64 // agent-file reads started (nextRead)
	boardSeq      uint64 // the read behind list and cards; the board never moves to an older one
	errs          map[string]string
	log           []string
	details       map[string]detailEntry      // Needs Zach card id -> fetched Ask detail
	appDeliveries map[string]appDeliveryEntry // app card id -> cached delivery check (policy.go)
	routing       map[string]bool             // pickers whose card is being started now (route.go)
	mailRetry     map[string]time.Time        // card id -> no Needs Zach email before this (a send failed)
	mailProbe     mailProbe                   // last mailbox check, for /api/health
	once          map[string]string           // log lines written once (noteOnce)
	// archivedNow: agents the board archived, and when. Applied over every snapshot until Paseo's
	// files show the archive (or archiveMarkTTL passes), so a Refresh that read an agent file before
	// Paseo wrote the archive cannot make the card look live again.
	archivedNow map[string]time.Time
	parkedNow   map[string]string    // agents the reap parked: their factory.parked value, applied like archivedNow
	lastReap    *ReapReport          // the last reap that changed anything, for /api/health
	retireTries map[string]retireTry // retired reviewers' failed archive tries in this process (retire.go)

	provMu     sync.Mutex
	prov       providers // Paseo provider status, for the agent choices
	usageMu    sync.Mutex
	usage      usageCache
	tokenMu    sync.Mutex
	tokenCache tokenUsageCache
	turnCache  transcriptTurnCache

	peerCache peerCache           // git and request reads for /api/peers
	vis       map[string]visEntry // owner/repo -> cached visibility

	// health (health.go): when the first Refresh ran and when the loop last started and finished
	hmu                          sync.Mutex
	started, tickStart, lastLoop time.Time
	refreshed                    bool
	stopping                     atomic.Bool // set when Run's context ends; Tick stops between agents

	calls    callLog         // paseo calls in the last callWindow, for /api/health (paseocalls.go)
	helper   *PaseoHelper    // the Paseo helper, when the factory runs one (paseohelper.go)
	permitAt time.Time       // last `permit ls` that succeeded (addPending)
	hot      map[string]bool // agents whose file changed in a way the rules act on (watch.go)
	archiveQ *[]string       // during a Tick: reviewer and scout archives, run after the moves

	previews *PreviewStore            // previews.json (tryout.go)
	pv       previews                 // running previews
	runner   PreviewRunner            // host work of previews; nil runs none
	GoEnv    func() map[string]string // the host's Go cache paths, for previews

	noAnswer     []NoAnswerSource // unanswered requests that are not cards (sections.go AddNoAnswer)
	denied       map[string]bool  // permission prompts the factory already answered (blocked.go denyPrompts)
	digestRetry  time.Time        // a failed daily digest is not tried again before this (digest.go)
	digestStatus string           // the daily digest's last state, for /api/health (digest.go)
	digestMu     sync.Mutex       // digest.json writes (digest.go)
}

const archiveMarkTTL = 10 * time.Minute

func New(cfg Config, p Paseo) *Factory {
	cfg.defaults()
	f := &Factory{cfg: cfg, p: p, ideas: &IdeaStore{path: filepath.Join(cfg.StateDir, "ideas.json")},
		routes: &RouteStore{path: filepath.Join(cfg.StateDir, "routes.json")}, routing: map[string]bool{},
		replies:  &replyStore{path: filepath.Join(cfg.StateDir, "reply-clarifications.json")},
		steers:   &SteerStore{path: filepath.Join(cfg.StateDir, "steers.json")},
		previews: &PreviewStore{path: filepath.Join(cfg.StateDir, "previews.json")},
		pv:       previews{procs: map[string]PreviewProc{}, busy: map[string]bool{}},
		runner:   hostRunner{}, GoEnv: runGoEnv,
		dismissed:   &DismissStore{path: filepath.Join(cfg.StateDir, "dismissed-requests.json")},
		landReviews: &LandReviewStore{path: filepath.Join(cfg.StateDir, "land-reviews.json")},
		GitHubAPI:   "https://api.github.com", vis: map[string]visEntry{},
		Now: time.Now, Git: runGit, GitOut: runGitOut, Sleep: time.Sleep, kick: make(chan struct{}, 1), hotKick: make(chan struct{}, 1), hot: map[string]bool{}, errs: map[string]string{},
		details: map[string]detailEntry{}, archivedNow: map[string]time.Time{}, parkedNow: map[string]string{}, retireTries: map[string]retireTry{}, mailRetry: map[string]time.Time{},
		once: map[string]string{}, denied: map[string]bool{}}
	f.helper, _ = p.(*PaseoHelper)
	if cfg.DryRun {
		f.p = &dryRunPaseo{inner: p, logf: f.logf}
		f.Git, f.GitOut = f.dryRunGit(runGit, runGitOut)
	}
	f.noAnswer = append(f.noAnswer, f.noAnswerLines)
	f.timings = newTimingStore(filepath.Join(cfg.StateDir, "timings.json"), func() time.Time { return f.Now() }, f.cfg.Location)
	f.timings.readOnly = cfg.ReadOnly
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
// session files; without it the board uses only cached detail (board actions must stay fast).
func (f *Factory) setAgents(agents []Agent, seq uint64, fetch bool) bool {
	agents = applyLineageLabels(f.withArchiveMarks(agents))
	cards := BuildBoard(agents, f.ideas.List(), f.Now())
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	for i := range cards {
		a, ok := byID[cards[i].ID]
		if !ok || !isAppCard(a) {
			continue
		}
		if fetch {
			if req := f.cardRequest(a); req.Base != "" || len(req.FollowUps) > 0 {
				cards[i].Request = &req
			}
		}
		if cards[i].Stage == "zach" {
			if done, why := f.appDelivered(a, fetch); done {
				cards[i].Stage, cards[i].Ask, cards[i].Unread, cards[i].Note = "landed", nil, false, why
				cards[i].Flags = slices.DeleteFunc(cards[i].Flags, func(fl string) bool {
					return fl == "error" || strings.HasPrefix(fl, "needs attention")
				})
				if !a.archived() && !slices.Contains(cards[i].Flags, "not archived") {
					cards[i].Flags = append(cards[i].Flags, "not archived")
				}
			}
		}
	}
	f.fillAsks(cards, agents, fetch)
	for _, c := range cards {
		if c.Ask != nil {
			f.fillReady(c.Ask, c.ID, c.Ask.Report)
		}
	}
	asked := map[string]CardRequest{} // first prompts of email and voice cards not yet timed (timing.go)
	for _, a := range f.timings.needsAsked(agents) {
		asked[a.ID] = f.cardRequest(a)
	}
	f.mu.Lock()
	if seq < f.boardSeq {
		f.mu.Unlock()
		return false
	}
	// timings change only on an accepted read, so an older read cannot step a card back
	now := f.Now()
	changed := f.timings.observe(cards, agents, seq, now, asked)
	f.timings.fillTiming(cards, now)
	f.list, f.cards, f.updated, f.boardSeq = agents, cards, now, seq
	f.mu.Unlock()
	if changed {
		if err := f.timings.save(); err != nil {
			f.logf("timings not saved: %v", err)
		}
	}
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
			delete(f.parkedNow, id)
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
			delete(f.parkedNow, out[i].ID)
		default:
			out[i].ArchivedAt = at
			if v, ok := f.parkedNow[out[i].ID]; ok {
				out[i].Labels = maps.Clone(out[i].Labels)
				out[i].Labels[labelPrefix+labelParked] = v
			}
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

// unmarkArchived drops the board's archive mark for id (a parked card Zach just woke): the snapshot
// shows it live again, and the next Refresh reads Paseo's unarchive.
func (f *Factory) unmarkArchived(id string) {
	f.clearPark(id, true)
}

// unpark drops parked agent id for good: it stays archived, without the parked label, so it is
// Done like any archived card.
func (f *Factory) unpark(id string) error {
	if err := f.setLabels(id, map[string]string{labelParked: ""}); err != nil {
		return err
	}
	f.clearPark(id, false)
	return nil
}

// clearPark removes id's parked label and park mark from the snapshot (and its archive, if live).
func (f *Factory) clearPark(id string, live bool) {
	f.mu.Lock()
	delete(f.parkedNow, id)
	if live {
		delete(f.archivedNow, id)
	}
	list, seq := slices.Clone(f.list), f.boardSeq
	for i := range list {
		if list[i].ID == id {
			if live {
				list[i].ArchivedAt = time.Time{}
			}
			list[i].Labels = maps.Clone(list[i].Labels)
			delete(list[i].Labels, labelPrefix+labelParked)
		}
	}
	f.mu.Unlock()
	f.setAgents(list, seq, false) // like markArchived: a newer read wins, and shows Paseo's unarchive
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

// Run polls Paseo and applies the rules until ctx ends. A POST kicks an early refresh; the agent
// watcher (watch.go) kicks a hot pass for the agents whose files changed, and the loop also wakes
// when a rule's grace window ends. When ctx ends, a Tick in progress finishes the agent it is on and
// skips the rest; Run then returns.
func (f *Factory) Run(ctx context.Context) {
	t := time.NewTicker(f.cfg.Poll)
	defer t.Stop()
	stop := context.AfterFunc(ctx, func() { f.stopping.Store(true) })
	defer stop()
	if f.helper != nil {
		defer f.helper.Close()
	}
	defer f.startWatch()()
	full := true
	for {
		f.loopStarted()
		switch {
		case full:
			f.Refresh()
			if !f.passive() {
				f.Tick()
				f.Refresh()
				f.providerStatus("claude") // keeps the provider list fresh (re-read every providerListTTL)
				f.providerUsages()         // same for agent usage: warmed here, never on the request path
			}
		case f.passive():
			f.mu.Lock()
			clear(f.hot) // no rules run: the reload shows the change
			f.mu.Unlock()
			f.reload()
		default:
			f.hotPass()
		}
		f.loopDone()
		var grace <-chan time.Time
		due, ids := graceDue(f.agents(), f.Now())
		if !due.IsZero() {
			grace = time.After(due.Sub(f.Now()))
		}
		select {
		case <-ctx.Done():
			f.stopping.Store(true)
			return
		case <-t.C:
			full = true
		case <-f.kick:
			full = true
		case <-f.hotKick:
			full = false
		case <-grace:
			f.mu.Lock()
			for _, id := range ids {
				f.hot[id] = true
			}
			f.mu.Unlock()
			full = false
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

// agentOK looks up a single agent by exact or prefix ID without following replacement links,
// so write primitives (setLabels, send, archiveChecked) only ever act on the exact agent named.
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
		for _, prev := range c.Replaces {
			if prev == id || strings.HasPrefix(prev, id) {
				return c, true
			}
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
	if len(args) > 0 && args[0] == "run" && !f.cfg.DryRun {
		if len(args) < 2 {
			return "", fmt.Errorf("missing agent prompt")
		}
		var err error
		args, err = f.quietLaunch(args)
		if err != nil {
			return "", err
		}
	}
	return f.rawPaseo(args...)
}

func (f *Factory) rawPaseo(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	var path string
	out, err := f.p.Run(withCallPath(ctx, &path), args...)
	f.calls.add(f.Now(), args, time.Since(start), path)
	return out, err
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
	if err := f.quietBeforeSend(id); err != nil {
		return err
	}
	args := []string{"send", "--no-wait"}
	for _, p := range images {
		args = append(args, "--image", p)
	}
	if _, err := f.paseo(append(args, id, msg)...); err != nil {
		return err
	}
	f.resume(id)
	return nil
}

// resume clears the parked state of agent id after a send, which unarchived it in Paseo. Every
// message to an agent (board reply, email answer, verdict) goes through send, so all of them wake a
// parked card. A label that fails to clear is cleared by the next reap (reap.go).
func (f *Factory) resume(id string) {
	f.mu.Lock()
	_, marked := f.parkedNow[id]
	f.mu.Unlock()
	if a, ok := f.agentOK(id); !marked && (!ok || a.L(labelParked) == "") {
		return
	}
	id = f.agent(id).ID
	if id == "" {
		return
	}
	if err := f.setLabels(id, map[string]string{labelParked: ""}); err != nil {
		f.logf("%s: clearing parked: %v", short(id), err)
	}
	f.logf("%s resumed from parked", short(id))
	f.unmarkArchived(id)
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

// repoDir returns the local git checkout directory for repo (owner/name): ~/dev/<owner>/<name>
// when its .git exists, else the checkout under ~/dev/*/* whose origin remote matches repo.
func (f *Factory) repoDir(repo string) string {
	def := filepath.Join(f.cfg.Home, "dev", repo)
	if _, err := os.Stat(filepath.Join(def, ".git")); err == nil {
		return def
	}
	owner, _, _ := strings.Cut(repo, "/")
	dirs, _ := filepath.Glob(filepath.Join(f.cfg.Home, "dev", "*", "*", ".git"))
	match := ""
	for _, d := range dirs {
		dir := filepath.Dir(d)
		r := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
		if strings.Contains(r, "archive") || strings.Contains(r, "-wt-") {
			continue
		}
		if !strings.EqualFold(originRepo(gitConfigPath(dir)), repo) {
			continue
		}
		if strings.EqualFold(filepath.Base(filepath.Dir(dir)), owner) {
			return dir
		}
		if match == "" {
			match = dir
		}
	}
	if match != "" {
		return match
	}
	return def
}

// knownRepos lists Repos() plus each checkout's origin remote owner/repo when it differs from the folder path.
func (f *Factory) knownRepos() []string {
	out := f.Repos()
	for _, r := range out {
		orig := originRepo(gitConfigPath(filepath.Join(f.cfg.Home, "dev", r)))
		if orig != "" && !slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, orig) }) {
			out = append(out, orig)
		}
	}
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
type CLI struct{ Bin, ArchiveBin string }

func (c CLI) Run(ctx context.Context, args ...string) (string, error) {
	markCall(ctx, callCLI)
	bin := c.Bin
	if bin == "" {
		bin = "paseo"
	}
	if len(args) > 0 && args[0] == "archive" {
		if c.ArchiveBin != "" {
			bin = c.ArchiveBin
		}
		resolved, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("archive CLI: %w", err)
		}
		bin, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		key, checked := archiveVersionOK(bin)
		if !checked {
			version, err := runCLI(ctx, bin, "--version")
			if err != nil {
				return "", err
			}
			if !fixedArchiveVersion(version) {
				return "", fmt.Errorf("archive requires Paseo CLI >= 0.13.0 (stable); selected executable reports %q", strings.TrimSpace(version))
			}
			archiveVersions.Store(bin, key)
		}
		out, err := runCLI(ctx, bin, args...)
		if err != nil {
			archiveVersions.Delete(bin) // the next archive checks the version again
		}
		return out, err
	}
	return runCLI(ctx, bin, args...)
}

// archiveVersions: archive executables whose version check passed, by path, with the size and
// modification time they had then. A replaced executable is checked again before its next archive.
var archiveVersions sync.Map

// archiveVersionOK reports whether bin passed the version check as the file it is now, and the
// key to store when it passes.
func archiveVersionOK(bin string) (string, bool) {
	st, err := os.Stat(bin)
	if err != nil {
		return "", false
	}
	key := fmt.Sprint(st.Size(), " ", st.ModTime().UnixNano())
	v, ok := archiveVersions.Load(bin)
	return key, ok && v == key
}

func runCLI(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	// started from inside a Paseo agent? don't let the CLI think we are that agent
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PASEO_AGENT_ID=") && !strings.HasPrefix(kv, "PASEO_AGENT_CWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("paseo command: %w", ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		verb := strings.Join(args[:min(2, len(args))], " ")
		return "", fmt.Errorf("paseo %s: %s", verb, trunc(msg, 400))
	}
	return stdout.String(), nil
}
