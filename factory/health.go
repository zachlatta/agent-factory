package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	Rev            string `json:"rev"`
	Started        string `json:"started,omitempty"`
	Refreshed      bool   `json:"refreshed"`
	RefreshErr     string `json:"refreshErr,omitempty"`
	TickRunningFor string `json:"tickRunningFor,omitempty"`
	LastLoopDone   string `json:"lastLoopDone,omitempty"`
	Live           bool   `json:"live"`
	OK             bool   `json:"ok"`
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

	h := Health{Rev: f.cfg.Rev, Refreshed: refreshed, RefreshErr: refreshErr, Live: true}
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
