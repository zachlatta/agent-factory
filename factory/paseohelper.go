package factory

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// PaseoHelper serves the factory's paseo calls through one long-lived Node process
// (paseohelper.mjs) connected to the daemon, instead of starting the paseo CLI for each call.
//   - It serves `inspect <id> [--json]`, `agent update <id> [--name n] [--label k=v]...`,
//     `send --no-wait [--image p]... <id> <message>` and `archive <id> [--force]`. Every other
//     call, and any other shape of these, runs the CLI unchanged.
//   - The helper uses the client code of the paseo CLI on PATH and serves only while the daemon
//     reports that CLI's version. A changed CLI package restarts it.
//   - When the helper cannot serve a call (not started, wrong version, stopped, crashed, or no
//     answer within helperTimeout, which kills it), the call runs the CLI. An unexpected helper
//     exit is logged and restarted in place at once; if starting fails or it exits again within
//     helperRetry, the next try waits helperRetry while the CLI serves.
//   - On Linux, each helper start writes 1000 to /proc/<pid>/oom_score_adj so the kernel kills
//     the helper before the board under memory pressure; a failed write is logged.
//   - A send runs the CLI only when it surely never reached the daemon: the helper was not
//     running, the request could not be written, or the helper answered that it was not
//     connected. A send that may have gone out returns an error, so no message arrives twice.
//   - An error the daemon or the command's own checks gave (not found, currently running, an
//     invalid label) is returned as the CLI would print it, without running the CLI.
//   - Archive goes through the helper only when its version passes fixedArchiveVersion;
//     otherwise the CLI (and its archive executable and version check) serves it.
type PaseoHelper struct {
	cli     Paseo                                 // the fallback: CLI
	dir     string                                // where the script is written
	command func(script, cliDir string) *exec.Cmd // starts the helper
	findCLI func() (string, error)                // the @getpaseo/cli package directory
	oomPath func(pid int) string                  // /proc/<pid>/oom_score_adj on Linux
	logf    func(format string, args ...any)
	now     func() time.Time
	timeout time.Duration // per call
	retry   time.Duration // between starts after the helper stopped twice or failed to start

	mu        sync.Mutex
	proc      *helperProc
	startDone chan struct{}
	closed    bool
	lastStop  time.Time
	nextTry   time.Time
	status    HelperStatus
}

// HelperStatus is the helper's state for /api/health (paseoCalls.helper).
type HelperStatus struct {
	State     string `json:"state"` // off (not started yet), starting, running, down, closed
	CLI       string `json:"cli,omitempty"`
	Daemon    string `json:"daemon,omitempty"`
	Started   string `json:"started,omitempty"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"lastError,omitempty"`
}

const (
	helperTimeout = 20 * time.Second
	helperRetry   = 30 * time.Second
)

//go:embed paseohelper.mjs
var helperScript []byte

// NewPaseoHelper serves calls through the helper, with cli as the fallback. The script is written
// to dir (the state directory).
func NewPaseoHelper(cli CLI, dir string) *PaseoHelper {
	return &PaseoHelper{cli: cli, dir: dir, now: time.Now, timeout: helperTimeout, retry: helperRetry,
		status: HelperStatus{State: "off"},
		command: func(script, cliDir string) *exec.Cmd {
			return exec.Command("node", script, cliDir)
		},
		findCLI: func() (string, error) { return cliPackage(cli.Bin) },
		oomPath: func(pid int) string {
			if runtime.GOOS != "linux" {
				return ""
			}
			return fmt.Sprintf("/proc/%d/oom_score_adj", pid)
		}}
}

func (h *PaseoHelper) note(format string, args ...any) {
	if h.logf != nil {
		h.logf(format, args...)
	}
}

func (h *PaseoHelper) setOOMScore(pid int) {
	if h.oomPath == nil {
		return
	}
	path := h.oomPath(pid)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte("1000\n"), 0o600); err != nil {
		h.note("setting Paseo helper OOM score (%s): %v", path, err)
	}
}

// cliPackage finds the @getpaseo/cli package of the paseo executable bin (default: paseo on PATH).
func cliPackage(bin string) (string, error) {
	if bin == "" {
		bin = "paseo"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	if path, err = filepath.EvalSymlinks(path); err != nil {
		return "", err
	}
	dir := filepath.Dir(filepath.Dir(path))
	var pkg struct{ Name string }
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err == nil {
		err = json.Unmarshal(b, &pkg)
	}
	if err != nil || pkg.Name != "@getpaseo/cli" {
		return "", fmt.Errorf("%s is not in an @getpaseo/cli package", path)
	}
	return dir, nil
}

// helperReq is one request line (paseohelper.mjs).
type helperReq struct {
	ID     int64    `json:"id"`
	Op     string   `json:"op"`
	Agent  string   `json:"agent"`
	JSON   bool     `json:"json,omitempty"`
	Name   *string  `json:"name,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Text   string   `json:"text,omitempty"`
	Images []string `json:"images,omitempty"`
	Force  bool     `json:"force,omitempty"`
}

type helperReply struct {
	ID     int64  `json:"id"`
	OK     bool   `json:"ok"`
	Out    string `json:"out"`
	Error  string `json:"error"`
	Unsent bool   `json:"unsent"`
	Lost   bool   `json:"lost"`
}

// helperRequest parses args into a helper request; ok is false for calls the CLI serves.
func helperRequest(args []string) (helperReq, bool) {
	flag := func(a string) bool { return strings.HasPrefix(a, "-") }
	switch {
	case len(args) >= 2 && args[0] == "inspect" && !flag(args[1]) && args[1] != "":
		if len(args) == 2 {
			return helperReq{Op: "inspect", Agent: args[1]}, true
		}
		if len(args) == 3 && args[2] == "--json" {
			return helperReq{Op: "inspect", Agent: args[1], JSON: true}, true
		}
	case len(args) >= 5 && args[0] == "agent" && args[1] == "update" && !flag(args[2]) && args[2] != "" && len(args)%2 == 1:
		r := helperReq{Op: "update", Agent: args[2]}
		for i := 3; i < len(args); i += 2 {
			switch args[i] {
			case "--name":
				if r.Name != nil {
					return helperReq{}, false
				}
				r.Name = &args[i+1]
			case "--label":
				r.Labels = append(r.Labels, args[i+1])
			default:
				return helperReq{}, false
			}
		}
		return r, true
	case len(args) >= 4 && args[0] == "send" && args[1] == "--no-wait":
		r := helperReq{Op: "send"}
		i := 2
		for ; i+1 < len(args) && args[i] == "--image"; i += 2 {
			r.Images = append(r.Images, args[i+1])
		}
		if i+2 != len(args) || flag(args[i]) || args[i] == "" || flag(args[i+1]) {
			return helperReq{}, false
		}
		r.Agent, r.Text = args[i], args[i+1]
		return r, true
	case len(args) >= 2 && args[0] == "archive" && !flag(args[1]) && args[1] != "":
		if len(args) == 2 {
			return helperReq{Op: "archive", Agent: args[1]}, true
		}
		if len(args) == 3 && args[2] == "--force" {
			return helperReq{Op: "archive", Agent: args[1], Force: true}, true
		}
	}
	return helperReq{}, false
}

func (h *PaseoHelper) Run(ctx context.Context, args ...string) (string, error) {
	req, ok := helperRequest(args)
	if !ok {
		return h.cli.Run(ctx, args...)
	}
	p := h.ready()
	if p == nil || (req.Op == "archive" && !fixedArchiveVersion(p.version)) {
		return h.cli.Run(ctx, args...)
	}
	verb := strings.Join(args[:min(2, len(args))], " ")
	rep, written, err := h.call(ctx, p, req)
	if err != nil && !written {
		<-p.done
		if p = h.ready(); p != nil && (req.Op != "archive" || fixedArchiveVersion(p.version)) {
			rep, written, err = h.call(ctx, p, req)
		}
	}
	switch {
	case err == nil && rep.OK:
		markCall(ctx, callHelper)
		return rep.Out, nil
	case err == nil && rep.Unsent:
		return h.cli.Run(ctx, args...)
	case err == nil && !rep.Lost:
		markCall(ctx, callHelper)
		return "", fmt.Errorf("paseo %s: %s", verb, trunc(strings.TrimSpace(rep.Error), 400))
	case err == nil: // the connection dropped during the call
		err = errors.New(strings.TrimSpace(rep.Error))
	case ctx.Err() != nil && req.Op != "send":
		return "", fmt.Errorf("paseo command: %w", ctx.Err())
	}
	if req.Op == "send" && written {
		markCall(ctx, callHelper)
		return "", fmt.Errorf("paseo %s: the Paseo helper failed after taking the message, which may have been sent, so it is not sent again: %v", verb, err)
	}
	return h.cli.Run(ctx, args...)
}

// call sends req to p and waits for its answer. written reports whether the request was written
// to the helper (so it may have reached the daemon). No answer within h.timeout kills the helper.
func (h *PaseoHelper) call(ctx context.Context, p *helperProc, req helperReq) (rep helperReply, written bool, err error) {
	ch := make(chan helperReply, 1)
	p.mu.Lock()
	if p.dead != nil {
		p.mu.Unlock()
		return rep, false, p.dead
	}
	p.next++
	req.ID = p.next
	p.pending[req.ID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
	}()
	line, _ := json.Marshal(req)
	p.wmu.Lock()
	_, err = p.stdin.Write(append(line, '\n'))
	p.wmu.Unlock()
	if err != nil {
		p.kill()
		return rep, false, fmt.Errorf("writing to the Paseo helper: %w", err)
	}
	t := time.NewTimer(h.timeout)
	defer t.Stop()
	select {
	case rep = <-ch:
		return rep, true, nil
	case <-p.done:
		p.mu.Lock()
		err = p.dead
		p.mu.Unlock()
		select { // an answer read just before the helper exited still counts
		case rep = <-ch:
			return rep, true, nil
		default:
		}
		return rep, true, err
	case <-t.C:
		p.kill()
		return rep, true, fmt.Errorf("the Paseo helper did not answer within %v and was stopped", h.timeout)
	case <-ctx.Done():
		return rep, true, ctx.Err()
	}
}

// ready returns the running helper, starting it when it is due; nil means the CLI serves the call.
func (h *PaseoHelper) ready() *helperProc {
	h.mu.Lock()
	for h.startDone != nil {
		wait := h.startDone
		h.mu.Unlock()
		<-wait
		h.mu.Lock()
	}
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	var stopLog string
	if p := h.proc; p != nil {
		if p.alive() && p.pkgKey == statKey(p.pkgPath) {
			h.mu.Unlock()
			return p
		}
		h.proc = nil
		if p.alive() { // the CLI package changed: start a helper for the new one now
			p.stop()
			h.nextTry = time.Time{}
		} else { // it stopped before its watcher below ran
			_, stopLog = h.stopped(p)
		}
	}
	if h.now().Before(h.nextTry) {
		h.mu.Unlock()
		if stopLog != "" {
			h.note("%s", stopLog)
		}
		return nil
	}
	restarting := h.status.State != "off"
	if restarting {
		h.status.Restarts++
	}
	h.status.State = "starting"
	done := make(chan struct{})
	h.startDone = done
	h.mu.Unlock()
	if stopLog != "" {
		h.note("%s", stopLog)
	}

	p, err := h.start()
	h.mu.Lock()
	h.startDone = nil
	close(done)
	if err != nil {
		h.status.State, h.status.LastError = "down", err.Error()
		h.nextTry = h.now().Add(h.retry)
		h.mu.Unlock()
		h.note("Paseo helper start failed: %v", err)
		return nil
	}
	if h.closed {
		h.mu.Unlock()
		p.stop()
		return nil
	}
	h.proc = p
	h.status.State, h.status.CLI, h.status.Daemon = "running", p.version, p.daemon
	h.status.Started = h.now().UTC().Format(time.RFC3339)
	h.mu.Unlock()
	if restarting {
		h.note("restarted the Paseo helper (CLI %s, daemon %s)", p.version, p.daemon)
	}
	go func() {
		<-p.done
		h.mu.Lock()
		if h.proc != p {
			h.mu.Unlock()
			return
		}
		h.proc = nil
		restart, msg := h.stopped(p)
		h.mu.Unlock()
		if msg != "" {
			h.note("%s", msg)
		}
		if restart {
			h.ready()
		}
	}()
	return p
}

// stopped records that helper p stopped by itself. If no helper stopped within h.retry before
// this, it reports restart=true so the caller restarts the helper in place; otherwise the next
// try waits h.retry while the CLI serves.
func (h *PaseoHelper) stopped(p *helperProc) (restart bool, logMsg string) {
	p.mu.Lock()
	dead := p.dead
	p.mu.Unlock()
	if dead != nil {
		h.status.LastError = dead.Error()
	}
	if h.closed {
		return false, ""
	}
	now := h.now()
	if h.lastStop.IsZero() || !now.Before(h.lastStop.Add(h.retry)) {
		h.lastStop = now
		h.nextTry = time.Time{}
		if dead != nil {
			logMsg = fmt.Sprintf("%v; restarting", dead)
		}
		return true, logMsg
	}
	h.status.State = "down"
	h.nextTry = now.Add(h.retry)
	if dead != nil {
		logMsg = dead.Error()
	}
	return false, logMsg
}

// statKey is a file's size and modification time, to notice a replaced CLI package.
func statKey(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprint(st.Size(), " ", st.ModTime().UnixNano())
}

// helperProc is one running helper.
type helperProc struct {
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	wmu             sync.Mutex // request writes
	version, daemon string
	pkgPath, pkgKey string
	done            chan struct{} // closed when the process has exited

	mu      sync.Mutex
	next    int64
	pending map[int64]chan helperReply
	dead    error // why it stopped, once it has
}

func (p *helperProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *helperProc) kill() {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

// stop closes the helper's input, which ends it, and kills it if it has not exited a second later.
func (p *helperProc) stop() {
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(time.Second):
		p.kill()
		select {
		case <-p.done:
		case <-time.After(time.Second):
		}
	}
}

func (h *PaseoHelper) start() (*helperProc, error) {
	dir, err := h.findCLI()
	if err != nil {
		return nil, fmt.Errorf("finding the paseo CLI: %w", err)
	}
	pkgPath := filepath.Join(dir, "package.json")
	key := statKey(pkgPath)
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return nil, err
	}
	script := filepath.Join(h.dir, "paseo-helper.mjs")
	if err := os.WriteFile(script, helperScript, 0o600); err != nil {
		return nil, err
	}
	cmd := h.command(script, dir)
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = nil
	// started from inside a Paseo agent? don't let the client think we are that agent
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PASEO_AGENT_ID=") && !strings.HasPrefix(kv, "PASEO_AGENT_CWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{max: 2000}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the Paseo helper: %w", err)
	}
	h.setOOMScore(cmd.Process.Pid)
	p := &helperProc{cmd: cmd, stdin: stdin, pkgPath: pkgPath, pkgKey: key, done: make(chan struct{}),
		pending: map[int64]chan helperReply{}}
	type hello struct {
		Ready       bool
		CLI, Daemon string
		Error       string
	}
	first := make(chan hello, 1)
	go func() {
		r := bufio.NewReaderSize(stdout, 64*1024)
		greeted := false
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				if !greeted {
					greeted = true
					var m hello
					if json.Unmarshal(line, &m) != nil {
						m = hello{Error: "unreadable first line: " + trunc(strings.TrimSpace(string(line)), 200)}
					}
					first <- m
				} else {
					var rep helperReply
					if json.Unmarshal(line, &rep) == nil {
						p.mu.Lock()
						ch := p.pending[rep.ID]
						p.mu.Unlock()
						if ch != nil {
							ch <- rep
						}
					}
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		dead := fmt.Errorf("the Paseo helper stopped (%v)", werr)
		if werr == nil {
			dead = errors.New("the Paseo helper stopped")
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			dead = fmt.Errorf("%w: %s", dead, trunc(msg, 400))
		}
		p.mu.Lock()
		p.dead = dead
		p.mu.Unlock()
		if !greeted {
			first <- hello{Error: dead.Error()}
		}
		close(p.done)
	}()
	var m hello
	select {
	case m = <-first:
	case <-time.After(h.timeout):
		p.kill()
		<-p.done
		return nil, fmt.Errorf("the Paseo helper did not connect within %v", h.timeout)
	}
	if !m.Ready {
		p.stop()
		if m.Error == "" {
			m.Error = "the Paseo helper did not start"
		}
		return nil, errors.New(m.Error)
	}
	p.version, p.daemon = m.CLI, m.Daemon
	return p, nil
}

// Status is the helper's state for /api/health.
func (h *PaseoHelper) Status() HelperStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Close stops the helper; later calls run the CLI.
func (h *PaseoHelper) Close() {
	h.mu.Lock()
	p := h.proc
	h.proc, h.closed = nil, true
	h.status.State = "closed"
	h.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
