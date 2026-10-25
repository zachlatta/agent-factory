// Package deployer keeps the agent factory running what is on its repo's main branch.
//
// Each run (a systemd timer, every minute) fetches main into its own sparse clone. When the
// factory's build inputs changed, it vets, tests and builds them, swaps the new binary and unit
// files in, restarts the factory and waits for /api/health to report the new rev. A build that
// fails keeps the old instance; a new instance that does not come up is rolled back to the last
// good set (binary and unit files). Every run also checks that the factory is live, restarts it
// after 3 misses, rolls back if it is not running the good set, and pushes one alert an hour to
// Zach's phone only if the good set itself will not come up.
package deployer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Remote    string   // git URL of the factory's repo
	Branch    string   // deployed branch (main)
	Src       string   // the deployer's own sparse clone
	Subdir    string   // factory module inside the repo; "" = the repo root
	Bin       string   // factory, factory-deploy and good/ live here
	UnitDir   string   // ~/.config/systemd/user
	Units     []string // files in <Subdir>/deploy installed into UnitDir
	Service   string   // the unit restarted on deploy
	StateFile string   // deploy.json; the board reads it for its banner
	HealthURL string   // the factory's /api/health
	Serve     []string // command that publishes the board on the tailnet; nil skips it
	Retry     bool     // try a failed key again now, whatever its backoff
}

// buildInputs are the paths under Subdir that change the binary or the units. Docs do not.
// ls-tree lists only the ones that exist (there is no go.sum yet).
var buildInputs = []string{"cmd", "factory", "deployer", "deploy", "go.mod", "go.sum"}

// backoff is the wait before retry n of a key that failed; after the last, it waits for a new key.
var backoff = []time.Duration{5 * time.Minute, 15 * time.Minute, 60 * time.Minute}

const (
	upWithin     = 120 * time.Second // a restarted factory must answer this soon
	stayUp       = 30 * time.Second  // and still answer after this (catches a crash in the first Tick)
	pollEvery    = 3 * time.Second
	missesBefore = 3 // watchdog: failed checks in a row before a restart
	alertEvery   = time.Hour
)

type Health struct {
	Rev        string `json:"rev"`
	Live       bool   `json:"live"`
	OK         bool   `json:"ok"`
	RefreshErr string `json:"refreshErr"`
}

type Release struct {
	Key string    `json:"key"`
	Rev string    `json:"rev"`
	At  time.Time `json:"at"`
}

type Failure struct {
	Key      string    `json:"key"`
	Rev      string    `json:"rev"`
	Stage    string    `json:"stage"` // vet, test, build, install or health
	Log      string    `json:"log"`
	Attempts int       `json:"attempts"`
	At       time.Time `json:"at"`
	NextTry  time.Time `json:"nextTry,omitzero"` // zero: no more automatic tries for this key
}

type State struct {
	Deployed   *Release  `json:"deployed,omitempty"`
	Failed     *Failure  `json:"failed,omitempty"`
	Misses     int       `json:"misses"`
	Alerted    time.Time `json:"alerted,omitzero"`
	CheckedAt  time.Time `json:"checkedAt,omitzero"`
	RunningRev string    `json:"runningRev,omitempty"`
}

type Deployer struct {
	cfg   Config
	Run   func(ctx context.Context, dir, name string, args ...string) (string, error)
	Probe func(ctx context.Context) (Health, error)
	Now   func() time.Time
	Sleep func(time.Duration)
	Logf  func(format string, args ...any)
}

func New(cfg Config) *Deployer {
	return &Deployer{cfg: cfg, Run: runCmd, Probe: HTTPProbe(cfg.HealthURL), Now: time.Now, Sleep: time.Sleep,
		Logf: func(format string, args ...any) { fmt.Printf(format+"\n", args...) }}
}

func runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HTTPProbe reads the factory's /api/health. A factory from before /api/health (404) counts as
// live, with no rev, if its board answers.
func HTTPProbe(url string) func(ctx context.Context) (Health, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(ctx context.Context, u string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return 0, nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, b, err
	}
	return func(ctx context.Context) (Health, error) {
		code, b, err := get(ctx, url)
		if err != nil {
			return Health{}, err
		}
		if code == http.StatusNotFound {
			if c, _, err := get(ctx, strings.TrimSuffix(url, "/health")+"/board"); err == nil && c == 200 {
				return Health{Live: true, OK: true}, nil
			}
		}
		var h Health
		if err := json.Unmarshal(b, &h); err != nil {
			return Health{}, fmt.Errorf("health %d: %s", code, trunc(string(b), 200))
		}
		return h, nil
	}
}

// Once is one timer run: deploy main if its build inputs changed, else check the factory is live.
func (d *Deployer) Once(ctx context.Context) error {
	unlock, ok, err := d.lock()
	if err != nil || !ok {
		return err // !ok: another run holds the lock
	}
	defer unlock()
	st, err := loadState(d.cfg.StateFile)
	if err != nil {
		return err
	}
	defer func() {
		st.CheckedAt = d.Now()
		if err := saveState(d.cfg.StateFile, st); err != nil {
			d.Logf("save state: %v", err)
		}
	}()

	key, rev, err := d.fetch(ctx)
	if err != nil {
		d.Logf("fetch %s: %v", d.cfg.Branch, err) // nothing marked; the next run fetches again
	} else if d.due(st, key) {
		d.deploy(ctx, &st, key, rev)
		return nil
	}
	d.watch(ctx, &st)
	return nil
}

// TestAlert sends one push labelled as a test, to check the alert path from the unit's environment.
func (d *Deployer) TestAlert(ctx context.Context) error {
	return d.alert(ctx, "Agent factory canary test", "Test alert from the factory deployer. Nothing is wrong.", "agent-factory-test")
}

func (d *Deployer) lock() (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(d.cfg.StateFile), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(d.cfg.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true, nil
}

func (d *Deployer) git(ctx context.Context, args ...string) (string, error) {
	out, err := d.Run(ctx, "", "git", append([]string{"-C", d.cfg.Src}, args...)...)
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", args[0], err, trunc(out, 300))
	}
	return out, nil
}

// fetch updates the clone and returns the build-input key and the commit of origin/<branch>.
func (d *Deployer) fetch(ctx context.Context) (key, rev string, err error) {
	if _, err := os.Stat(filepath.Join(d.cfg.Src, ".git")); err != nil {
		if out, err := d.Run(ctx, "", "git", "clone", "--quiet", "--no-checkout", "--filter=blob:none", d.cfg.Remote, d.cfg.Src); err != nil {
			return "", "", fmt.Errorf("clone: %v: %s", err, trunc(out, 300))
		}
		if d.cfg.Subdir != "" { // the factory is one folder of a bigger repo: check out only that
			if _, err := d.git(ctx, "sparse-checkout", "set", "--cone", d.cfg.Subdir); err != nil {
				return "", "", err
			}
		}
	}
	ref := "refs/remotes/origin/" + d.cfg.Branch
	if _, err := d.git(ctx, "fetch", "--quiet", "origin", "+refs/heads/"+d.cfg.Branch+":"+ref); err != nil {
		return "", "", err
	}
	out, err := d.git(ctx, "rev-parse", ref)
	if err != nil {
		return "", "", err
	}
	rev = strings.TrimSpace(out)
	args := []string{"ls-tree", "-r", ref, "--"}
	for _, p := range buildInputs {
		args = append(args, path.Join(d.cfg.Subdir, p))
	}
	tree, err := d.git(ctx, args...)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(tree))
	return hex.EncodeToString(sum[:8]), rev, nil
}

func (d *Deployer) due(st State, key string) bool {
	if st.Deployed != nil && st.Deployed.Key == key {
		return false
	}
	if f := st.Failed; f != nil && f.Key == key && !d.cfg.Retry {
		return !f.NextTry.IsZero() && !d.Now().Before(f.NextTry)
	}
	return true
}

func (d *Deployer) path(name string) string { return filepath.Join(d.cfg.Bin, name) }

func (d *Deployer) deploy(ctx context.Context, st *State, key, rev string) {
	d.Logf("deploying %s (inputs %s)", short(rev), key)
	defer os.Remove(d.path("factory.new"))
	defer os.Remove(d.path("factory-deploy.new"))
	if _, err := d.git(ctx, "checkout", "--quiet", "--force", "--detach", "refs/remotes/origin/"+d.cfg.Branch); err != nil {
		d.fail(st, key, rev, "checkout", err.Error())
		return
	}
	dir := filepath.Join(d.cfg.Src, d.cfg.Subdir)
	ld := "-X main.rev=" + rev
	steps := []struct {
		stage string
		args  []string
	}{
		{"vet", []string{"vet", "./..."}},
		{"test", []string{"test", "./..."}},
		{"build", []string{"build", "-ldflags", ld, "-o", d.path("factory.new"), "./cmd/factory"}},
		{"build", []string{"build", "-ldflags", ld, "-o", d.path("factory-deploy.new"), "./cmd/factory-deploy"}},
	}
	for _, s := range steps {
		if out, err := d.Run(ctx, dir, "go", s.args...); err != nil {
			d.fail(st, key, rev, s.stage, fmt.Sprintf("go %s: %v\n%s", strings.Join(s.args, " "), err, failureLines(out)))
			return
		}
	}

	// The running set is the fallback if it is live, or if there is no fallback yet. A fresh host
	// has neither: the first deploy goes in with nothing to roll back to.
	if !exists(d.path("factory")) {
		d.Logf("no factory installed yet: nothing to roll back to")
	} else if h, err := d.Probe(ctx); (err == nil && h.Live) || !exists(filepath.Join(d.goodDir(), "factory")) {
		if err := d.snapshot(); err != nil {
			d.fail(st, key, rev, "install", "snapshot the good set: "+err.Error())
			return
		}
	}
	if err := d.install(dir); err != nil {
		d.Logf("install %s: %v", short(rev), err)
		d.rollback(ctx, st)
		d.fail(st, key, rev, "install", err.Error())
		return
	}
	if err := d.restart(ctx); err == nil && d.cameUp(ctx, rev) {
		if err := d.snapshot(); err != nil {
			d.Logf("snapshot after deploy: %v", err)
		}
		d.publish(ctx)
		if err := os.Rename(d.path("factory-deploy.new"), d.path("factory-deploy")); err != nil {
			d.Logf("replace the deployer: %v", err)
		}
		st.Deployed = &Release{Key: key, Rev: rev, At: d.Now()}
		st.Failed, st.Misses, st.RunningRev = nil, 0, rev
		d.Logf("deployed %s", short(rev))
		return
	}
	journal, _ := d.Run(ctx, "", "journalctl", "--user", "-u", d.cfg.Service, "-n", "30", "--no-pager")
	d.Logf("%s did not come up; rolling back", short(rev))
	d.rollback(ctx, st)
	d.fail(st, key, rev, "health", "new instance did not report ok with its rev within "+upWithin.String()+
		" (or died within "+stayUp.String()+")\n"+tail(journal, 30))
}

// cameUp: the restarted factory reports ok with rev within upWithin, and again stayUp later.
func (d *Deployer) cameUp(ctx context.Context, rev string) bool {
	ok := func(h Health, err error) bool { return err == nil && h.OK && h.Rev == rev }
	if !d.waitFor(ctx, func() bool { return ok(d.Probe(ctx)) }) {
		return false
	}
	d.Sleep(stayUp)
	return ok(d.Probe(ctx))
}

func (d *Deployer) waitLive(ctx context.Context) bool {
	return d.waitFor(ctx, func() bool { h, err := d.Probe(ctx); return err == nil && h.Live })
}

func (d *Deployer) waitFor(ctx context.Context, cond func() bool) bool {
	deadline := d.Now().Add(upWithin)
	for {
		if cond() {
			return true
		}
		if !d.Now().Before(deadline) || ctx.Err() != nil {
			return false
		}
		d.Sleep(pollEvery)
	}
}

func (d *Deployer) restart(ctx context.Context) error {
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "restart", d.cfg.Service}} {
		if out, err := d.Run(ctx, "", "systemctl", args...); err != nil {
			d.Logf("systemctl %s: %v: %s", strings.Join(args, " "), err, trunc(out, 300))
			return err
		}
	}
	return nil
}

func (d *Deployer) publish(ctx context.Context) {
	if len(d.cfg.Serve) == 0 {
		return
	}
	if out, err := d.Run(ctx, "", d.cfg.Serve[0], d.cfg.Serve[1:]...); err != nil {
		d.Logf("%s: %v: %s", strings.Join(d.cfg.Serve, " "), err, trunc(out, 300))
	}
}

func (d *Deployer) fail(st *State, key, rev, stage, log string) {
	n := 1
	if st.Failed != nil && st.Failed.Key == key {
		n = st.Failed.Attempts + 1
	}
	f := &Failure{Key: key, Rev: rev, Stage: stage, Log: trunc(log, 4000), Attempts: n, At: d.Now()}
	if n-1 < len(backoff) {
		f.NextTry = d.Now().Add(backoff[n-1])
	}
	st.Failed = f
	d.Logf("deploy of %s failed at %s (attempt %d); next try %v", short(rev), stage, n, f.NextTry)
}

// watch is the watchdog: restart after missesBefore failed checks, then roll back if the running
// set is not the good one, and alert Zach only if the good set will not come up.
func (d *Deployer) watch(ctx context.Context, st *State) {
	h, err := d.Probe(ctx)
	if err == nil && h.Live {
		st.Misses, st.RunningRev = 0, h.Rev
		return
	}
	st.Misses++
	d.Logf("factory not live (%d in a row): %v", st.Misses, errOr(err, "loop hung"))
	if st.Misses < missesBefore {
		return
	}
	if d.restart(ctx) == nil && d.waitLive(ctx) {
		d.Logf("factory live again after a restart")
		st.Misses = 0
		return
	}
	if !d.runningGood() && d.rollback(ctx, st) {
		st.Misses = 0
		return
	}
	if d.Now().Sub(st.Alerted) >= alertEvery {
		if err := d.alert(ctx, "Agent factory is down", "The factory will not come up, even on its last good build. "+
			"Restarts and rollback failed. Logs: journalctl --user -u "+d.cfg.Service, "agent-factory-down"); err != nil {
			d.Logf("alert: %v", err)
		} else {
			st.Alerted = d.Now()
		}
	}
}

func (d *Deployer) alert(ctx context.Context, title, body, collapse string) error {
	data, _ := json.Marshal(map[string]string{"title": title, "body": body, "collapse_id": collapse,
		"thread_id": "agent-factory", "interruption_level": "time-sensitive"})
	out, err := d.Run(ctx, "", "pdw", "call", "notify", "--data", string(data))
	if err != nil {
		return fmt.Errorf("pdw notify: %v: %s", err, trunc(out, 300))
	}
	return nil
}

// The good set: good/factory, good/<unit> for every unit that existed, good/manifest.json listing them.

func (d *Deployer) goodDir() string { return d.path("good") }

type manifest struct {
	Units []string `json:"units"` // units present when the snapshot was taken
}

func (d *Deployer) snapshot() error {
	tmp := d.goodDir() + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := copyFile(d.path("factory"), filepath.Join(tmp, "factory"), 0o755); err != nil {
		return err
	}
	var m manifest
	for _, u := range d.cfg.Units {
		src := filepath.Join(d.cfg.UnitDir, u)
		if !exists(src) {
			continue
		}
		if err := copyFile(src, filepath.Join(tmp, u), 0o644); err != nil {
			return err
		}
		m.Units = append(m.Units, u)
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), b, 0o644); err != nil {
		return err
	}
	os.RemoveAll(d.goodDir())
	return os.Rename(tmp, d.goodDir())
}

// install swaps in factory.new and the unit files from the checkout.
func (d *Deployer) install(dir string) error {
	for _, u := range d.cfg.Units {
		src := filepath.Join(dir, "deploy", u)
		if !exists(src) {
			continue
		}
		if err := atomicCopy(src, filepath.Join(d.cfg.UnitDir, u), 0o644); err != nil {
			return err
		}
	}
	return os.Rename(d.path("factory.new"), d.path("factory"))
}

// rollback restores the good set (binary and unit files), reloads systemd, restarts and reports
// whether the factory is live again.
func (d *Deployer) rollback(ctx context.Context, st *State) bool {
	good := d.goodDir()
	b, err := os.ReadFile(filepath.Join(good, "manifest.json"))
	var m manifest
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if err != nil {
		d.Logf("rollback: no good set: %v", err)
		return false
	}
	had := map[string]bool{}
	for _, u := range m.Units {
		had[u] = true
		if err := atomicCopy(filepath.Join(good, u), filepath.Join(d.cfg.UnitDir, u), 0o644); err != nil {
			d.Logf("rollback %s: %v", u, err)
		}
	}
	for _, u := range d.cfg.Units {
		if !had[u] {
			os.Remove(filepath.Join(d.cfg.UnitDir, u))
		}
	}
	if err := atomicCopy(filepath.Join(good, "factory"), d.path("factory"), 0o755); err != nil {
		d.Logf("rollback binary: %v", err)
		return false
	}
	if d.restart(ctx) != nil || !d.waitLive(ctx) {
		d.Logf("rollback: the good set did not come up")
		return false
	}
	if h, err := d.Probe(ctx); err == nil {
		st.RunningRev = h.Rev
	}
	d.Logf("rolled back to the good set")
	return true
}

func (d *Deployer) runningGood() bool {
	a, err1 := fileSum(d.path("factory"))
	b, err2 := fileSum(filepath.Join(d.goodDir(), "factory"))
	return err1 == nil && err2 == nil && a == b
}

func loadState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %v", path, err)
	}
	return st, nil
}

func saveState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// atomicCopy writes dst through a temp file and a rename, so a running binary is never half-written.
func atomicCopy(src, dst string, mode os.FileMode) error {
	tmp := dst + ".deploy-tmp"
	if err := copyFile(src, tmp, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func fileSum(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// tail keeps the last n lines of s.
func tail(s string, n int) string {
	lines := bytes.Split([]byte(strings.TrimRight(s, "\n")), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

// failureLines keeps what explains a failed go vet, test or build (failed tests and their messages,
// panics, compile errors, package results) and drops the log lines tests print. With none, the tail.
func failureLines(out string) string {
	var keep []string
	inPanic := false
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "panic:"):
			inPanic = true
			keep = append(keep, l)
		case inPanic && (strings.HasPrefix(l, "\t") || strings.HasPrefix(t, "goroutine ") || strings.Contains(t, ".go:")):
			keep = append(keep, l)
		case strings.HasPrefix(t, "--- FAIL"), t == "FAIL", strings.HasPrefix(t, "FAIL\t"), strings.HasPrefix(t, "ok  \t"),
			strings.HasPrefix(t, "#"), goPos.MatchString(t):
			inPanic = false
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return tail(out, 40)
	}
	return tail(strings.Join(keep, "\n"), 60)
}

// goPos matches "file.go:12:" or "file.go:12:3:", how vet, compile errors and t.Error lines start.
var goPos = regexp.MustCompile(`^\S+\.go:\d+(:\d+)?: `)

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}
