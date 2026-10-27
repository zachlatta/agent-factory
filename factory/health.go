package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// hungAfter: a loop (Refresh, Tick, Refresh) that has not finished, or not started, for this long
// means the factory is hung. Each paseo call is bounded at 90 s, so a busy Tick stays well under it.
const hungAfter = 15 * time.Minute

// Health is GET /api/health, read by the deployer (cmd/factory-deploy).
//   - Live: the process answers and its loop is not hung. The deployer's watchdog acts on this.
//   - OK: Live and the first Refresh has run. A new deploy must report OK with its own Rev.
//
// A Refresh error (Paseo down, ~/.paseo unreadable) shows in RefreshErr and on the board; it
// leaves Live and OK alone, so a Paseo outage never restarts or rolls back the factory.
type Health struct {
	Rev            string      `json:"rev"`
	Started        string      `json:"started,omitempty"`
	Refreshed      bool        `json:"refreshed"`
	RefreshErr     string      `json:"refreshErr,omitempty"`
	TickRunningFor string      `json:"tickRunningFor,omitempty"`
	LastLoopDone   string      `json:"lastLoopDone,omitempty"`
	Mail           string      `json:"mail,omitempty"` // last mailbox check: "ok" or its error; never fails the check (mail.go)
	Live           bool        `json:"live"`
	OK             bool        `json:"ok"`
	Agents         AgentCounts `json:"agents"`
	PaseoCalls     PaseoCalls  `json:"paseoCalls"` // paseo CLI calls in the last 10 minutes (paseocalls.go)
}

// AgentCounts is how many of this host's Paseo agents are loaded, for the idle reap (reap.go).
// Live: not archived and not closed (closed sessions hold no process). Idle: live and not running.
type AgentCounts struct {
	Live     int            `json:"live"`
	Running  int            `json:"running"`
	Idle     int            `json:"idle"`
	Parked   int            `json:"parked"`
	ByKind   map[string]int `json:"byKind"` // live agents: card, reviewer, scout, picker, other
	IdleTTL  string         `json:"idleTTL"`
	IdleCap  int            `json:"idleCap"`
	Cards    []CardAgents   `json:"cards"` // cards with a live agent, or parked; longest idle first
	LastReap *ReapReport    `json:"lastReap,omitempty"`
}

// CardAgents counts one card's live agents: its own and its reviewers and sub-agents.
type CardAgents struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Stage   string `json:"stage"`
	Live    int    `json:"live"`
	Idle    int    `json:"idle"`
	IdleMin int    `json:"idleMin"` // the card's own agent, while it is not running
	Parked  bool   `json:"parked,omitempty"`
}

// shortDuration is d without zero trailing units: 3h, 90m, 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func agentKind(a Agent) string {
	switch {
	case a.L(labelRole) != "":
		return a.L(labelRole)
	case a.L(labelRouting) != "":
		return "picker"
	case a.L(labelStage) != "":
		return "card"
	}
	return "other"
}

func (f *Factory) agentCounts() AgentCounts {
	now := f.Now()
	agents := f.agents()
	f.mu.Lock()
	last := f.lastReap
	f.mu.Unlock()
	n := AgentCounts{ByKind: map[string]int{}, IdleTTL: shortDuration(f.cfg.IdleTTL), IdleCap: f.cfg.IdleCap,
		Cards: []CardAgents{}, LastReap: last}
	cards := map[string]*CardAgents{}
	var order []string
	byID := f.byID()
	for _, a := range agents {
		if a.L(labelStage) == "" || a.L(labelRole) != "" || a.gone() || replacedPredecessor(byID, a) {
			continue
		}
		c := &CardAgents{ID: a.ID, Title: a.Title, Stage: Derive(a), Parked: a.parked()}
		if !a.archived() && a.Status != "running" {
			c.IdleMin = int(idleSince(a, now).Minutes())
		}
		cards[a.ID] = c
		order = append(order, a.ID)
	}
	// Quiet anchors are grouping boundaries: each card still owns its own live agent.
	owners := cardOwners(agents, quietBoundaries(agents, nil))
	for _, a := range agents {
		if a.parked() {
			n.Parked++
		}
		if a.archived() || a.Status == "closed" {
			continue
		}
		n.Live++
		n.ByKind[agentKind(a)]++
		running := a.Status == "running"
		if running {
			n.Running++
		} else {
			n.Idle++
		}
		owner := a.ID
		if o, ok := owners[a.ID]; ok {
			owner = o
		}
		if c := cards[owner]; c != nil {
			c.Live++
			if !running {
				c.Idle++
			}
		}
	}
	for _, id := range order {
		if c := cards[id]; c.Live > 0 || c.Parked {
			n.Cards = append(n.Cards, *c)
		}
	}
	sort.SliceStable(n.Cards, func(i, j int) bool { return n.Cards[i].IdleMin > n.Cards[j].IdleMin })
	return n
}

func (f *Factory) noteRefreshed() {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	if !f.refreshed {
		f.refreshed, f.started = true, f.Now()
	}
}

func (f *Factory) loopStarted() {
	f.hmu.Lock()
	f.tickStart = f.Now()
	f.hmu.Unlock()
}

func (f *Factory) loopDone() {
	f.hmu.Lock()
	f.lastLoop = f.Now()
	f.hmu.Unlock()
}

func (f *Factory) Health() Health {
	now := f.Now()
	f.hmu.Lock()
	started, refreshed, tickStart, lastLoop := f.started, f.refreshed, f.tickStart, f.lastLoop
	f.hmu.Unlock()
	f.mu.Lock()
	refreshErr := f.errs[f.cfg.Host]
	f.mu.Unlock()

	f.mu.Lock()
	mailStatus := f.mailProbe.status
	f.mu.Unlock()
	h := Health{Rev: f.cfg.Rev, Refreshed: refreshed, RefreshErr: refreshErr, Live: true, Mail: mailStatus}
	if !started.IsZero() {
		h.Started = iso(started)
	}
	if !lastLoop.IsZero() {
		h.LastLoopDone = iso(lastLoop)
	}
	ref := started
	if lastLoop.After(ref) {
		ref = lastLoop
	}
	if tickStart.After(lastLoop) { // a loop is running now
		h.TickRunningFor = now.Sub(tickStart).Round(time.Second).String()
		ref = tickStart
	}
	if !ref.IsZero() && now.Sub(ref) >= hungAfter {
		h.Live = false
	}
	h.OK = h.Live && refreshed
	h.Agents = f.agentCounts()
	h.PaseoCalls = f.calls.report(now)
	return h
}

func (f *Factory) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := f.Health()
	code := http.StatusOK
	if !h.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

// handleDeploy serves the deployer's last result (deploy.json in the state dir) beside the
// running rev, for the board's banner.
func (f *Factory) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var state any
	if b, err := os.ReadFile(filepath.Join(f.cfg.StateDir, "deploy.json")); err == nil {
		json.Unmarshal(b, &state)
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": f.cfg.Rev, "state": state})
}

// Serve runs the pipeline and serves h on ln until ctx ends. Then it stops the pipeline between
// agents, lets requests in flight finish (up to drain), and returns once both are done.
func (f *Factory) Serve(ctx context.Context, ln net.Listener, h http.Handler, drain time.Duration) error {
	f.Refresh()
	srv := &http.Server{Handler: h}
	ran := make(chan struct{})
	go func() { f.Run(ctx); close(ran) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := srv.Shutdown(sctx)
	<-ran
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
