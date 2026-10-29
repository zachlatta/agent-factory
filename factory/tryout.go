package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Factory-run previews for the Ready for you stop (ready.go). Each repo declares how to start one in
// .factory.json, read from origin/main so a card cannot change what runs on the host:
//
//	"preview": {"run": "<command>", "path": "/", "env": {"K": "literal"}}
//	"preview": {"sample": "<command>"}
//
// The factory runs the command from the card's worktree on 127.0.0.1:$PORT, with an allowlisted
// environment and a HOME of its own under StateDir/previews/<card>, publishes it with tailscale serve
// on the same port, checks it loads and links it. It stops it once the card leaves Ready for you
// (landing keeps it), removing the serve entry and the folder. previews.json keeps each card's port,
// so after a restart the next Tick starts it again on the same link. Only Tick starts or stops
// previews, and never in a dry-run or read-only copy (passive).

// Preview states.
const (
	previewStarting = "starting"
	previewReady    = "ready"
	previewFailed   = "failed"
	previewQueued   = "queued"  // over the cap: starts when one stops
	previewNone     = "none"    // the repo declares no preview
	previewSampled  = "sampled" // a no-UI repo: sample output only
)

// previewCheckEvery is how often Tick checks a running preview still loads.
const previewCheckEvery = 5 * time.Minute

// previewSampleMax is the most sample output kept.
const previewSampleMax = 4096

// PreviewSpec is a repo's preview declaration.
type PreviewSpec struct {
	Run    string            `json:"run,omitempty"`
	Path   string            `json:"path,omitempty"`
	Sample string            `json:"sample,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
}

// Preview is one card's preview, as saved in previews.json.
type Preview struct {
	Card     string    `json:"card"`
	Cwd      string    `json:"cwd"`
	Port     int       `json:"port,omitempty"`
	URL      string    `json:"url,omitempty"`
	Path     string    `json:"path,omitempty"` // the page checked, from the declaration
	State    string    `json:"state"`
	Log      string    `json:"log,omitempty"`
	Sample   string    `json:"sample,omitempty"`
	Checked  time.Time `json:"checked,omitzero"`
	Restarts int       `json:"restarts,omitempty"`
}

// PreviewStore is previews.json: card id -> Preview.
type PreviewStore struct {
	path string
	mu   sync.Mutex
}

func (s *PreviewStore) load() map[string]Preview {
	m := map[string]Preview{}
	readJSON(s.path, &m)
	return m
}

func (s *PreviewStore) all() map[string]Preview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *PreviewStore) get(id string) (Preview, bool) {
	p, ok := s.all()[id]
	return p, ok
}

func (s *PreviewStore) update(fn func(map[string]Preview)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	fn(m)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
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

func (s *PreviewStore) put(p Preview) error {
	return s.update(func(m map[string]Preview) { m[p.Card] = p })
}

// edit changes card id's saved preview, if it is still saved.
func (s *PreviewStore) edit(id string, fn func(*Preview)) error {
	return s.update(func(m map[string]Preview) {
		if p, ok := m[id]; ok {
			fn(&p)
			m[id] = p
		}
	})
}

func (s *PreviewStore) del(id string) error {
	return s.update(func(m map[string]Preview) { delete(m, id) })
}

// PreviewRunner does the host work of previews. Tests replace it.
type PreviewRunner interface {
	// Start runs command in dir with env, its output appended to logPath, in a process group of its own.
	Start(dir, command string, env []string, logPath string) (PreviewProc, error)
	// Sample runs command in dir with env for at most 2 minutes and returns its output.
	Sample(dir, command string, env []string) (string, error)
	Serve(port int) error   // publish 127.0.0.1:port on the tailnet at the same port
	Unserve(port int) error // remove that
	Served() ([]int, error) // ports the tailnet serve config publishes
	Get(url string) error   // nil when url answers 2xx or 3xx
	Free(port int) bool     // nothing listens on 127.0.0.1:port
}

// PreviewProc is a running preview process.
type PreviewProc struct {
	Stop func()          // stop the process group
	Done <-chan struct{} // closed when the process exits
}

// previews is the factory's in-memory side of previews: running processes and cards with a check
// in progress. previews.json is the saved side.
type previews struct {
	wg    sync.WaitGroup // background starts, checks and samples
	mu    sync.Mutex
	procs map[string]PreviewProc
	busy  map[string]bool
	goEnv map[string]string
}

func (s *previews) proc(id string) (PreviewProc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[id]
	return p, ok
}

// claim marks card id busy; false if it already is.
func (s *previews) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *previews) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, id)
}

func alive(p PreviewProc) bool {
	select {
	case <-p.Done:
		return false
	default:
		return true
	}
}

// previewSpec reads the preview declaration from origin/main's .factory.json in the card's repo.
func (f *Factory) previewSpec(cwd string) *PreviewSpec {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, cwd, "show", "origin/main:.factory.json")
	if err != nil {
		return nil
	}
	var cfg struct {
		Preview *PreviewSpec `json:"preview"`
	}
	if json.Unmarshal([]byte(out), &cfg) != nil || cfg.Preview == nil || (cfg.Preview.Run == "" && cfg.Preview.Sample == "") {
		return nil
	}
	return cfg.Preview
}

// parsePortRange reads "lo-hi".
func parsePortRange(s string) (int, int) {
	lo, hi, _ := strings.Cut(s, "-")
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || l <= 0 || h < l || h > 65535 {
		return 0, 0
	}
	return l, h
}

func (f *Factory) inPreviewRange(port int) bool {
	lo, hi := parsePortRange(f.cfg.PreviewPorts)
	return lo > 0 && port >= lo && port <= hi
}

// freePort is the lowest port in the range no saved preview holds and nothing listens on, or 0.
func (f *Factory) freePort(saved map[string]Preview) int {
	lo, hi := parsePortRange(f.cfg.PreviewPorts)
	taken := map[int]bool{}
	for _, p := range saved {
		taken[p.Port] = true
	}
	for port := lo; lo > 0 && port <= hi; port++ {
		if !taken[port] && f.runner.Free(port) {
			return port
		}
	}
	return 0
}

// previewURL is the tailnet link to port: the board's host at that port.
func (f *Factory) previewURL(port int, path string) string {
	u, err := url.Parse(f.cfg.BoardURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port)) + localPath(path)
}

var previewIDSafe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// previewDir holds card id's preview HOME and log, outside its worktree.
func (f *Factory) previewDir(id string) string { return filepath.Join(f.cfg.StateDir, "previews", id) }

// previewEnvKeys are the host variables a preview gets; nothing else of the host's environment.
var previewEnvKeys = []string{"PATH", "LANG", "TZ"}

// previewEnv is a preview's whole environment: allowlisted host values, the host's Go caches (so
// modules are not downloaded into the preview's HOME), the npm cache, its own HOME and PORT, and the
// repo's literal values, which cannot replace any of those.
func previewEnv(host func(string) string, goEnv map[string]string, npmCache string, spec *PreviewSpec, home string, port int) []string {
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	for _, k := range previewEnvKeys {
		if v := host(k); v != "" {
			env[k] = v
		} else {
			delete(env, k)
		}
	}
	for _, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if v := goEnv[k]; v != "" {
			env[k] = v
		}
	}
	if npmCache != "" {
		env["npm_config_cache"] = npmCache
	}
	env["HOME"], env["PORT"], env["HOST"] = home, strconv.Itoa(port), "127.0.0.1"
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// hostGoEnv is the host's Go cache paths, read once.
func (f *Factory) hostGoEnv() map[string]string {
	f.pv.mu.Lock()
	defer f.pv.mu.Unlock()
	if f.pv.goEnv == nil {
		f.pv.goEnv = map[string]string{}
		if f.GoEnv != nil {
			f.pv.goEnv = f.GoEnv()
		}
	}
	return f.pv.goEnv
}

func runGoEnv() map[string]string {
	out, err := exec.Command("go", "env", "GOPATH", "GOMODCACHE", "GOCACHE").Output()
	m := map[string]string{}
	if err != nil {
		return m
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if i < len(lines) {
			m[k] = strings.TrimSpace(lines[i])
		}
	}
	return m
}

func (f *Factory) previewEnvFor(id string, spec *PreviewSpec, port int) []string {
	return previewEnv(os.Getenv, f.hostGoEnv(), filepath.Join(f.cfg.Home, ".npm"), spec,
		filepath.Join(f.previewDir(id), "home"), port)
}

// tickPreviews starts, checks and stops previews so that exactly the cards at Ready for you (or
// landing from it) have one, at most cfg.PreviewMax running, oldest wait first.
func (f *Factory) tickPreviews(now time.Time) {
	if f.passive() || f.runner == nil {
		return
	}
	saved := f.previews.all()
	var wanted []Agent
	want := map[string]bool{}
	for _, a := range f.agents() {
		if a.gone() || a.L(labelRole) != "" || !previewIDSafe.MatchString(a.ID) {
			continue
		}
		_, has := saved[a.ID]
		if readyStop(a) || (has && a.L(labelStage) == "landing") {
			wanted, want[a.ID] = append(wanted, a), true
		}
	}
	for id, p := range saved {
		if !want[id] {
			f.stopPreview(p)
			delete(saved, id)
		}
	}
	f.previewOrphans(saved)
	sort.SliceStable(wanted, func(i, j int) bool { return wanted[i].L(labelWriting) < wanted[j].L(labelWriting) })
	running := 0
	for _, p := range saved {
		if p.Port > 0 && (p.State == previewStarting || p.State == previewReady) {
			running++
		}
	}
	for _, a := range wanted {
		p, ok := saved[a.ID]
		_, live := f.pv.proc(a.ID)
		switch {
		case !ok || p.State == previewQueued || (p.State == previewNone && f.previewSpec(a.Cwd) != nil):
			// A later trusted declaration can enable an existing Ready card.
			// Missing declarations only read state; they do not rewrite previewNone.
			if f.startPreview(a, saved, running) {
				running++
			}
			saved = f.previews.all()
		case p.Port > 0 && !live && (p.State == previewStarting || p.State == previewReady):
			// the factory restarted (its previews stop with it): start it again on its saved port
			f.relaunch(p, false)
		case p.State == previewReady && now.Sub(p.Checked) >= previewCheckEvery:
			f.recheck(p)
		}
	}
}

// startPreview starts card a's preview, or records that it has none or must wait. It reports whether
// a process was started.
func (f *Factory) startPreview(a Agent, saved map[string]Preview, running int) bool {
	spec := f.previewSpec(a.Cwd)
	p := Preview{Card: a.ID, Cwd: a.Cwd, State: previewNone}
	if spec == nil {
		f.savePreview(p)
		return false
	}
	if spec.Sample != "" && spec.Run == "" {
		p.State = previewStarting
		if f.savePreview(p) {
			f.runSample(p, spec, previewSampled)
		}
		return false
	}
	if running >= f.cfg.PreviewMax {
		p.State = previewQueued
		f.savePreview(p)
		return false
	}
	if p.Port = f.freePort(saved); p.Port == 0 {
		p.State = previewQueued
		f.savePreview(p)
		return false
	}
	if p.URL = f.previewURL(p.Port, spec.Path); p.URL == "" {
		p.State, p.Port, p.Log = previewFailed, 0, "the factory has no board URL to link previews on"
		f.savePreview(p)
		return false
	}
	p.State, p.Path = previewStarting, localPath(spec.Path)
	if !f.savePreview(p) { // saved before serve: a crash leaves an entry Tick cleans up, not a stray port
		return false
	}
	if spec.Sample != "" {
		f.runSample(p, spec, "")
	}
	return f.launch(p, spec, true)
}

func (f *Factory) savePreview(p Preview) bool {
	if err := f.previews.put(p); err != nil {
		f.logf("%s preview: saving: %v", short(p.Card), err)
		return false
	}
	return true
}

// relaunch starts p again on its port, rereading its declaration. counted restarts count toward
// the one automatic restart a failing preview gets.
func (f *Factory) relaunch(p Preview, counted bool) {
	if !f.pv.claim(p.Card) {
		return
	}
	spec := f.previewSpec(p.Cwd)
	f.pv.release(p.Card)
	if spec == nil || spec.Run == "" {
		f.previews.edit(p.Card, func(q *Preview) { q.State, q.Log = previewFailed, "the repo no longer declares a preview" })
		return
	}
	if counted {
		f.previews.edit(p.Card, func(q *Preview) { q.Restarts++; q.State = previewStarting })
	}
	f.launch(p, spec, false)
}

// launch starts p's process, then (in the background) publishes and checks it.
func (f *Factory) launch(p Preview, spec *PreviewSpec, serve bool) bool {
	if !f.pv.claim(p.Card) {
		return false
	}
	dir := f.previewDir(p.Card)
	fail := func(why string) bool {
		f.previews.edit(p.Card, func(q *Preview) { q.State, q.Log = previewFailed, why })
		f.pv.release(p.Card)
		return false
	}
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		return fail("creating its folder: " + err.Error())
	}
	proc, err := f.runner.Start(p.Cwd, spec.Run, f.previewEnvFor(p.Card, spec, p.Port), filepath.Join(dir, "log"))
	if err != nil {
		return fail("starting: " + err.Error())
	}
	f.pv.mu.Lock()
	f.pv.procs[p.Card] = proc
	f.pv.mu.Unlock()
	f.pv.wg.Add(1)
	go func() {
		defer f.pv.wg.Done()
		defer f.pv.release(p.Card)
		state, why := previewReady, ""
		if err := f.runner.Serve(p.Port); err != nil {
			state, why = previewFailed, "publishing on the tailnet: "+err.Error()
		} else if err := f.waitLoads(proc, p, spec.Path); err != nil {
			state, why = previewFailed, err.Error()
		}
		if state == previewFailed {
			why = strings.TrimSpace(why + "\n" + logTail(filepath.Join(dir, "log"), 20))
		}
		f.previews.edit(p.Card, func(q *Preview) { q.State, q.Log, q.Checked = state, why, f.Now() })
		f.poke()
	}()
	return true
}

// waitLoads waits up to 2 minutes for p to answer on 127.0.0.1 and then on its tailnet link.
func (f *Factory) waitLoads(proc PreviewProc, p Preview, path string) error {
	local := localURL(p.Port, path)
	var err error
	for range 60 {
		if !alive(proc) {
			return errors.New("the preview exited")
		}
		if err = f.runner.Get(local); err == nil {
			if err = f.runner.Get(p.URL); err == nil {
				return nil
			}
		}
		f.Sleep(2 * time.Second)
	}
	return fmt.Errorf("it did not load within 2 minutes: %v", err)
}

func localPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func localURL(port int, path string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + localPath(path)
}

// recheck checks a ready preview still loads; a dead or failing one is restarted once, then failed.
func (f *Factory) recheck(p Preview) {
	if !f.pv.claim(p.Card) {
		return
	}
	proc, live := f.pv.proc(p.Card)
	f.pv.wg.Add(1)
	go func() {
		defer f.pv.wg.Done()
		ok := live && alive(proc) && f.runner.Get(localURL(p.Port, p.Path)) == nil
		f.pv.release(p.Card)
		switch {
		case ok:
			f.previews.edit(p.Card, func(q *Preview) { q.Checked = f.Now() })
		case p.Restarts < 1:
			if live {
				proc.Stop()
			}
			f.relaunch(p, true)
		default:
			if live {
				proc.Stop()
			}
			f.previews.edit(p.Card, func(q *Preview) {
				q.State, q.Log = previewFailed, strings.TrimSpace("it stopped loading\n"+logTail(filepath.Join(f.previewDir(p.Card), "log"), 20))
			})
		}
	}()
}

// runSample runs a repo's sample command once in the background and saves its output. A state
// other than "" is set when it finishes.
func (f *Factory) runSample(p Preview, spec *PreviewSpec, state string) {
	dir := f.previewDir(p.Card)
	f.pv.wg.Add(1)
	go func() {
		defer f.pv.wg.Done()
		if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
			f.logf("%s preview sample: %v", short(p.Card), err)
		}
		out, err := f.runner.Sample(p.Cwd, spec.Sample, f.previewEnvFor(p.Card, spec, p.Port))
		if err != nil {
			out = strings.TrimSpace(out + "\n(" + err.Error() + ")")
		}
		out = trunc(strings.TrimSpace(out), previewSampleMax)
		f.previews.edit(p.Card, func(q *Preview) {
			q.Sample = out
			if state != "" {
				q.State = state
			}
		})
		f.poke()
	}()
}

// stopPreview stops p's process, its tailnet entry and its folder, and forgets it.
func (f *Factory) stopPreview(p Preview) {
	f.pv.mu.Lock()
	proc, ok := f.pv.procs[p.Card]
	delete(f.pv.procs, p.Card)
	f.pv.mu.Unlock()
	if ok {
		proc.Stop()
	}
	if p.Port > 0 {
		if err := f.runner.Unserve(p.Port); err != nil {
			f.logf("%s preview: removing tailnet port %d: %v", short(p.Card), p.Port, err)
		}
	}
	if previewIDSafe.MatchString(p.Card) {
		if err := removeAll(f.previewDir(p.Card)); err != nil {
			f.logf("%s preview: removing its folder: %v", short(p.Card), err)
		}
	}
	if err := f.previews.del(p.Card); err != nil {
		f.logf("%s preview: %v", short(p.Card), err)
	}
	f.logf("%s preview stopped", short(p.Card))
}

// previewOrphans removes tailnet entries in the preview range and preview folders that no saved
// preview owns (left by a crash between saving and serving, or a lost previews.json).
func (f *Factory) previewOrphans(saved map[string]Preview) {
	owned := map[int]bool{}
	for _, p := range saved {
		owned[p.Port] = true
	}
	if ports, err := f.runner.Served(); err == nil {
		for _, port := range ports {
			if f.inPreviewRange(port) && !owned[port] {
				if err := f.runner.Unserve(port); err != nil {
					f.logf("preview: removing stray tailnet port %d: %v", port, err)
				}
			}
		}
	}
	entries, _ := os.ReadDir(filepath.Join(f.cfg.StateDir, "previews"))
	for _, e := range entries {
		if _, ok := saved[e.Name()]; !ok && previewIDSafe.MatchString(e.Name()) {
			removeAll(filepath.Join(f.cfg.StateDir, "previews", e.Name()))
		}
	}
}

// removeAll removes dir even when it holds read-only files (Go's module cache makes them).
func removeAll(dir string) error {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// logTail is the last n lines of the file at path.
func logTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// previewSettled reports whether card id's preview has a result for the email (ready, failed,
// queued, none or sampled). A factory that runs no previews is always settled.
func (f *Factory) previewSettled(id string) bool {
	if f.passive() || f.runner == nil {
		return true
	}
	p, ok := f.previews.get(id)
	return ok && p.State != previewStarting
}

// hostRunner is the PreviewRunner for this host: sh, tailscale serve and HTTP.
type hostRunner struct{}

func (hostRunner) Start(dir, command string, env []string, logPath string) (PreviewProc, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return PreviewProc{}, err
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return PreviewProc{}, err
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		logf.Close()
		close(done)
	}()
	pid := cmd.Process.Pid
	stop := func() {
		syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
	}
	return PreviewProc{Stop: stop, Done: done}, nil
}

func (hostRunner) Sample(dir, command string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tailscale(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("tailscale %s: %v: %s", strings.Join(args, " "), err, trunc(strings.TrimSpace(string(out)), 300))
	}
	return out, nil
}

func (hostRunner) Serve(port int) error {
	p := strconv.Itoa(port)
	_, err := tailscale("serve", "--bg", "--https="+p, "http://127.0.0.1:"+p)
	return err
}

func (hostRunner) Unserve(port int) error {
	_, err := tailscale("serve", "--https="+strconv.Itoa(port), "off")
	return err
}

func (hostRunner) Served() ([]int, error) {
	out, err := tailscale("serve", "status", "--json")
	if err != nil {
		return nil, err
	}
	return servedPorts(out)
}

// servedPorts reads the ports of `tailscale serve status --json`.
func servedPorts(b []byte) ([]int, error) {
	var st struct {
		TCP map[string]json.RawMessage `json:"TCP"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	var ports []int
	for k := range st.TCP {
		if n, err := strconv.Atoi(k); err == nil {
			ports = append(ports, n)
		}
	}
	slices.Sort(ports)
	return ports, nil
}

func (hostRunner) Get(u string) error {
	c := http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	return nil
}

func (hostRunner) Free(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}
