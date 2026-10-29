package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records preview host work. Get answers from loads (default: everything loads).
type fakeRunner struct {
	mu      sync.Mutex
	starts  []fakeStart
	served  map[int]bool
	unserve []int
	samples []string
	fails   map[string]bool // urls that do not load
	procs   []chan struct{}
	stopped int
}

type fakeStart struct {
	Dir, Command string
	Env          []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{served: map[int]bool{}, fails: map[string]bool{}}
}

func (r *fakeRunner) Start(dir, command string, env []string, logPath string) (PreviewProc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, fakeStart{dir, command, env})
	os.WriteFile(logPath, []byte("listening\n"), 0o600)
	done := make(chan struct{})
	r.procs = append(r.procs, done)
	var once sync.Once
	return PreviewProc{Done: done, Stop: func() {
		once.Do(func() {
			r.mu.Lock()
			r.stopped++
			r.mu.Unlock()
			close(done)
		})
	}}, nil
}

func (r *fakeRunner) Sample(dir, command string, env []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, command)
	return strings.Repeat("x", 5000), nil
}

func (r *fakeRunner) Serve(port int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.served[port] = true
	return nil
}

func (r *fakeRunner) Unserve(port int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.served, port)
	r.unserve = append(r.unserve, port)
	return nil
}

func (r *fakeRunner) Served() ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int
	for p := range r.served {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

func (r *fakeRunner) Get(url string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails[url] {
		return fmt.Errorf("%s does not load", url)
	}
	return nil
}

func (r *fakeRunner) Free(int) bool { return true }

func (r *fakeRunner) count() (starts, stopped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts), r.stopped
}

// previewFactory is a test factory with a fake runner whose repos declare spec in origin/main.
func previewFactory(t *testing.T, spec string, agents ...Agent) (*Factory, *fakePaseo, *fakeRunner) {
	t.Helper()
	f, p := newTestFactory(t, agents...)
	r := newFakeRunner()
	f.runner = r
	t.Cleanup(f.pv.wg.Wait) // before the temp dirs go
	f.GoEnv = func() map[string]string {
		return map[string]string{"GOPATH": "/host/go", "GOMODCACHE": "/host/go/pkg/mod", "GOCACHE": "/host/cache/go"}
	}
	f.cfg.BoardURL = "https://board.example:7443/"
	f.GitOut = func(_ context.Context, dir string, args ...string) (string, error) {
		if strings.Join(args, " ") == "show origin/main:.factory.json" && spec != "" {
			return spec, nil
		}
		return "", fmt.Errorf("no such file")
	}
	return f, p, r
}

const runSpec = `{"preview": {"run": "go run ./cmd/board-preview -listen $HOST:$PORT", "path": "/x", "env": {"MODE": "fake", "HOME": "/evil"}}}`

func readyCard(id string) Agent {
	return card(id, map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelWriting: ago(time.Minute)})
}

func waitPreview(t *testing.T, f *Factory, id, state string) Preview {
	t.Helper()
	var p Preview
	waitFor(t, id+" preview "+state, func() bool {
		var ok bool
		p, ok = f.previews.get(id)
		return ok && p.State == state
	})
	return p
}

func TestPreviewStartsChecksAndLinks(t *testing.T) {
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	f.tickPreviews(t0)
	p := waitPreview(t, f, "c1", previewReady)
	if p.Port != 7700 || p.URL != "https://board.example:7700/x" || !r.served[7700] {
		t.Errorf("preview = %+v, served %v", p, r.served)
	}
	st := r.starts[0]
	if st.Dir != "/w/c1" || !strings.Contains(st.Command, "board-preview") {
		t.Errorf("start = %+v", st)
	}
	c := mustCard(t, f, "c1")
	f.fillReady(c.Ask, "c1", "CHANGED: one")
	if c.Ask.Ready.Preview != p.URL || c.Ask.Ready.Changed[0] != "one" {
		t.Errorf("ready = %+v", c.Ask.Ready)
	}
}

func TestPreviewEnvironmentIsAllowlisted(t *testing.T) {
	t.Setenv("FACTORY_ZACH_EMAIL", "someone@example.com")
	t.Setenv("GITHUB_TOKEN", "secret-value")
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	f.tickPreviews(t0)
	waitPreview(t, f, "c1", previewReady)
	env := map[string]string{}
	for _, kv := range r.starts[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	home := filepath.Join(f.cfg.StateDir, "previews", "c1", "home")
	want := map[string]string{"HOME": home, "PORT": "7700", "HOST": "127.0.0.1", "MODE": "fake",
		"GOPATH": "/host/go", "GOMODCACHE": "/host/go/pkg/mod", "GOCACHE": "/host/cache/go",
		"npm_config_cache": filepath.Join(f.cfg.Home, ".npm")}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for k := range env {
		if !slices.Contains([]string{"PATH", "LANG", "TZ"}, k) && want[k] == "" {
			t.Errorf("unexpected variable %s", k)
		}
	}
	if strings.HasPrefix(home, "/w/c1") {
		t.Error("the preview HOME must be outside the worktree")
	}
}

func TestPreviewReadsOnlyOriginMain(t *testing.T) {
	f, _, r := previewFactory(t, "", readyCard("c1"))
	// the card's own copy declares a preview; origin/main does not
	f.tickPreviews(t0)
	if p, _ := f.previews.get("c1"); p.State != previewNone {
		t.Errorf("preview = %+v", p)
	}
	if n, _ := r.count(); n != 0 {
		t.Error("started a preview origin/main does not declare")
	}
}

func TestPreviewStopsWhenTheCardLeaves(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"change requested": {labelStage: "building", labelNote: ""},
		"dropped":          {labelStage: "zach", labelNote: "closed by Zach"},
		"done":             {labelStage: "landed", labelNote: "marked done by Zach"},
		"landed":           {labelStage: "landed", labelNote: ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, r := previewFactory(t, runSpec, readyCard("c1"))
			f.tickPreviews(t0)
			waitPreview(t, f, "c1", previewReady)
			os.WriteFile(filepath.Join(f.previewDir("c1"), "home", "ro"), []byte("x"), 0o400)
			os.Chmod(filepath.Join(f.previewDir("c1"), "home"), 0o500)
			// landing keeps it
			f.SetAgents([]Agent{withLabels(readyCard("c1"), map[string]string{labelStage: "landing"})})
			f.tickPreviews(t0)
			if _, ok := f.previews.get("c1"); !ok {
				t.Fatal("landing stopped the preview")
			}
			f.SetAgents([]Agent{withLabels(readyCard("c1"), labels)})
			f.tickPreviews(t0)
			if _, ok := f.previews.get("c1"); ok {
				t.Error("preview still saved")
			}
			if _, stopped := r.count(); stopped != 1 || r.served[7700] || !slices.Contains(r.unserve, 7700) {
				t.Errorf("stopped %d, served %v", stopped, r.served)
			}
			if _, err := os.Stat(f.previewDir("c1")); !os.IsNotExist(err) {
				t.Errorf("preview folder left: %v", err)
			}
		})
	}
}

func TestPreviewStopsWhenArchived(t *testing.T) {
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	f.tickPreviews(t0)
	waitPreview(t, f, "c1", previewReady)
	gone := readyCard("c1")
	gone.ArchivedAt = t0
	f.SetAgents([]Agent{gone})
	f.tickPreviews(t0)
	if _, stopped := r.count(); stopped != 1 {
		t.Errorf("stopped = %d", stopped)
	}
}

func TestPreviewCapQueuesOldestFirst(t *testing.T) {
	var agents []Agent
	for i := range 3 {
		a := readyCard(fmt.Sprintf("c%d", i))
		a.Labels[labelPrefix+labelWriting] = ago(time.Duration(10-i) * time.Minute)
		agents = append(agents, a)
	}
	f, _, _ := previewFactory(t, runSpec, agents...)
	f.cfg.PreviewMax = 2
	f.tickPreviews(t0)
	waitPreview(t, f, "c0", previewReady)
	waitPreview(t, f, "c1", previewReady)
	if q, _ := f.previews.get("c2"); q.State != previewQueued || q.Port != 0 {
		t.Errorf("c2 = %+v", q)
	}
	f.SetAgents([]Agent{withLabels(agents[0], map[string]string{labelStage: "landed"}), agents[1], agents[2]})
	f.tickPreviews(t0)
	if q := waitPreview(t, f, "c2", previewReady); q.Port != 7700 {
		t.Errorf("c2 = %+v (the freed port is reused)", q)
	}
}

func TestPreviewFailsWithLog(t *testing.T) {
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	r.fails["https://board.example:7700/x"] = true
	f.tickPreviews(t0)
	p := waitPreview(t, f, "c1", previewFailed)
	if !strings.Contains(p.Log, "did not load") || !strings.Contains(p.Log, "listening") {
		t.Errorf("log = %q", p.Log)
	}
}

func TestPreviewRestartsAfterFactoryRestart(t *testing.T) {
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	f.tickPreviews(t0)
	waitPreview(t, f, "c1", previewReady)
	// a new factory on the same state: its previews stopped with the old one
	g, _, r2 := previewFactory(t, runSpec, readyCard("c1"))
	g.cfg.StateDir = f.cfg.StateDir
	g.previews = f.previews
	t.Cleanup(g.pv.wg.Wait)
	g.tickPreviews(t0)
	waitFor(t, "restart", func() bool { n, _ := r2.count(); return n == 1 })
	if p := waitPreview(t, g, "c1", previewReady); p.Port != 7700 {
		t.Errorf("port = %d", p.Port)
	}
	_ = r
}

func TestPreviewRecheckRestartsOnceThenFails(t *testing.T) {
	f, _, r := previewFactory(t, runSpec, readyCard("c1"))
	f.tickPreviews(t0)
	waitPreview(t, f, "c1", previewReady)
	r.mu.Lock()
	r.fails["http://127.0.0.1:7700/x"] = true
	r.mu.Unlock()
	later := t0.Add(previewCheckEvery + time.Second)
	f.tickPreviews(later)
	waitFor(t, "restart", func() bool { n, _ := r.count(); return n == 2 })
	waitPreview(t, f, "c1", previewFailed) // the restart does not load either (its local check fails)
	r.mu.Lock()
	r.fails = map[string]bool{"http://127.0.0.1:7700/x": true}
	r.mu.Unlock()
	f.previews.edit("c1", func(q *Preview) { q.State, q.Checked = previewReady, t0 })
	f.tickPreviews(later)
	waitPreview(t, f, "c1", previewFailed)
	if n, _ := r.count(); n != 2 {
		t.Errorf("starts = %d: only one automatic restart", n)
	}
}

func TestPreviewSampleForNoUIRepos(t *testing.T) {
	f, _, r := previewFactory(t, `{"preview": {"sample": "go run ./cmd/example"}}`, readyCard("c1"))
	f.tickPreviews(t0)
	p := waitPreview(t, f, "c1", previewSampled)
	if len(p.Sample) > previewSampleMax || p.Port != 0 || len(r.starts) != 0 || r.samples[0] != "go run ./cmd/example" {
		t.Errorf("preview = %+v", p)
	}
}

func TestPassiveCopiesRunNoPreviews(t *testing.T) {
	for name, set := range map[string]func(*Config){
		"dry-run":    func(c *Config) { c.DryRun = true },
		"tick=false": func(c *Config) { c.ReadOnly = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, r := previewFactory(t, runSpec, readyCard("c1"), readyCard("c2"))
			f.previews.put(Preview{Card: "c1", Port: 7700, URL: "https://board.example:7700/", State: previewReady})
			f.previews.put(Preview{Card: "c9", Port: 7701, State: previewReady}) // its card is gone
			r.served[7702] = true                                                // not owned by any preview
			set(&f.cfg)
			f.tickPreviews(t0)
			f.Tick()
			time.Sleep(10 * time.Millisecond)
			if n, stopped := r.count(); n != 0 || stopped != 0 || len(r.unserve) != 0 || len(r.samples) != 0 {
				t.Errorf("starts %d, stops %d, unserve %v", n, stopped, r.unserve)
			}
			if len(f.previews.all()) != 2 {
				t.Errorf("saved previews changed: %v", f.previews.all())
			}
		})
	}
}

func TestPreviewOrphansAreRemoved(t *testing.T) {
	f, _, r := previewFactory(t, runSpec)
	r.served[7443], r.served[7705] = true, true
	os.MkdirAll(filepath.Join(f.cfg.StateDir, "previews", "gone1", "home"), 0o700)
	f.tickPreviews(t0)
	if r.served[7705] || !r.served[7443] {
		t.Errorf("served = %v (only the preview range is cleaned)", r.served)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "previews", "gone1")); !os.IsNotExist(err) {
		t.Error("orphan folder left")
	}
}

func TestServedPorts(t *testing.T) {
	got, err := servedPorts([]byte(`{"TCP":{"7443":{"HTTPS":true},"7700":{"HTTPS":true}},"Web":{}}`))
	if err != nil || !slices.Equal(got, []int{7443, 7700}) {
		t.Errorf("servedPorts = %v, %v", got, err)
	}
}

// A real process on a real port, with only tailscale faked: the host runner starts, loads and
// stops it, and the card's worktree is left with no ignored files.
func TestHostRunnerRealPreview(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@users.noreply.github.com")
	run("config", "user.name", "t")
	os.WriteFile(filepath.Join(repo, "index.html"), []byte("hi"), 0o644)
	run("add", ".")
	run("commit", "-qm", "x")

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	a := readyCard("c1")
	a.Cwd = repo
	f, _, _ := previewFactory(t, `{"preview": {"run": "exec python3 -m http.server --bind $HOST $PORT"}}`, a)
	fake := newFakeRunner()
	f.runner = tailscaleFaked{hostRunner{}, fake}
	f.cfg.PreviewPorts = strconv.Itoa(port) + "-" + strconv.Itoa(port)
	f.cfg.BoardURL = "http://127.0.0.1/"
	f.Sleep = time.Sleep
	f.tickPreviews(t0)
	p := waitPreviewLong(t, f, "c1")
	if p.State != previewReady {
		t.Fatalf("preview = %+v", p)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	home := filepath.Join(f.previewDir("c1"), "home")
	os.MkdirAll(filepath.Join(home, "go", "pkg"), 0o700)
	os.WriteFile(filepath.Join(home, "go", "pkg", "ro"), []byte("x"), 0o400)
	os.Chmod(filepath.Join(home, "go", "pkg"), 0o500)

	f.SetAgents(nil)
	f.tickPreviews(t0)
	if _, err := os.Stat(f.previewDir("c1")); !os.IsNotExist(err) {
		t.Errorf("preview folder left: %v", err)
	}
	if out, _ := exec.Command("git", "-C", repo, "status", "--ignored", "--porcelain").Output(); len(out) != 0 {
		t.Errorf("worktree has files left: %s", out)
	}
	waitFor(t, "server stopped", func() bool {
		_, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		return err != nil
	})
}

// tailscaleFaked is the host runner with its tailscale calls faked and the tailnet link check
// answered by the local one.
type tailscaleFaked struct {
	hostRunner
	fake *fakeRunner
}

func (r tailscaleFaked) Serve(port int) error   { return r.fake.Serve(port) }
func (r tailscaleFaked) Unserve(port int) error { return r.fake.Unserve(port) }
func (r tailscaleFaked) Served() ([]int, error) { return r.fake.Served() }
func (r tailscaleFaked) Get(u string) error {
	if strings.HasPrefix(u, "https://") {
		return nil
	}
	return r.hostRunner.Get(u)
}

func waitPreviewLong(t *testing.T, f *Factory, id string) Preview {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := f.previews.get(id); ok && p.State != previewStarting {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("preview did not settle")
	return Preview{}
}

func TestExistingReadyPreviewNonePicksUpTrustedDeclaration(t *testing.T) {
	f, _, runner := previewFactory(t, `{}`, readyCard("existing"))
	f.tickPreviews(t0)
	waitPreview(t, f, "existing", previewNone)
	before, err := os.ReadFile(f.previews.path)
	if err != nil {
		t.Fatal(err)
	}
	// An absent declaration must not rewrite the same no-preview record on each Tick.
	if err := os.Chtimes(f.previews.path, t0, t0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.tickPreviews(t0.Add(time.Duration(i+1) * time.Minute))
	}
	st, err := os.Stat(f.previews.path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(t0) {
		t.Fatal("unchanged no-preview state was rewritten")
	}
	// The same Ready card must pick up a later declaration, only from trusted main.
	f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Join(args, " ") != "show origin/main:.factory.json" {
			return "", fmt.Errorf("untrusted preview declaration read")
		}
		return runSpec, nil
	}
	for _, mode := range []string{"dry-run", "read-only"} {
		f.cfg.DryRun = mode == "dry-run"
		f.cfg.ReadOnly = mode == "read-only"
		f.tickPreviews(t0.Add(4 * time.Minute))
		after, err := os.ReadFile(f.previews.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%s changed saved preview state", mode)
		}
		if n, _ := runner.count(); n != 0 {
			t.Fatalf("%s started a preview", mode)
		}
	}
	f.cfg.DryRun = false
	f.cfg.ReadOnly = false
	f.tickPreviews(t0.Add(5 * time.Minute))
	p := waitPreview(t, f, "existing", previewReady)
	if p.Port != 7700 || p.URL != "https://board.example:7700/x" {
		t.Fatalf("unexpected preview: %+v", p)
	}
	f.tickPreviews(t0.Add(5 * time.Minute))
	if n, _ := runner.count(); n != 1 {
		t.Fatalf("started %d processes for one Ready card", n)
	}
	// The retry path must use the normal cap and queue, not bypass it.
	f.cfg.PreviewMax = 1
	capped := readyCard("capped")
	if err := f.previews.put(Preview{Card: capped.ID, Cwd: capped.Cwd, State: previewNone}); err != nil {
		t.Fatal(err)
	}
	f.SetAgents([]Agent{readyCard("existing"), capped})
	f.tickPreviews(t0.Add(5 * time.Minute))
	waitPreview(t, f, "capped", previewQueued)
	if n, _ := runner.count(); n != 1 {
		t.Fatalf("saved-none retry bypassed preview cap: %d", n)
	}
	f.SetAgents(nil)
	f.tickPreviews(t0.Add(6 * time.Minute))
	if _, stopped := runner.count(); stopped != 1 {
		t.Fatalf("owned preview cleanup: %d", stopped)
	}
}

// A fresh lander replaces the Ready agent in the same worktree: the preview started for the Ready
// agent keeps its port, link, process and folder, is found under the new agent, starts again after
// a factory restart, and is cleaned up once when the card is done or dropped.
func TestPreviewFollowsTheCardThroughAHandoff(t *testing.T) {
	handoff := func(listPredecessor bool) []Agent {
		old := withLabels(readyCard("c1"), map[string]string{labelReplacedBy: "c2"})
		old.ArchivedAt = t0
		lander := withLabels(readyCard("c2"), map[string]string{labelStage: "landing", labelReplaces: "c1"})
		lander.Cwd = "/w/c1"
		if listPredecessor {
			return []Agent{old, lander}
		}
		return []Agent{lander}
	}
	for name, leave := range map[string]map[string]string{
		"done":    {labelStage: "landed", labelNote: "marked done by Zach"},
		"dropped": {labelStage: "zach", labelNote: "closed by Zach"},
	} {
		for _, listed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/predecessor listed %v", name, listed), func(t *testing.T) {
				f, _, r := previewFactory(t, runSpec, readyCard("c1"))
				f.tickPreviews(t0)
				before := waitPreview(t, f, "c1", previewReady)
				f.SetAgents(handoff(listed))
				f.tickPreviews(t0)
				if p, ok := f.previews.get("c1"); !ok || p != before {
					t.Fatalf("after the hand-off preview = %+v %v, want %+v", p, ok, before)
				}
				if starts, stopped := r.count(); starts != 1 || stopped != 0 || !r.served[7700] {
					t.Fatalf("starts %d, stopped %d, served %v", starts, stopped, r.served)
				}
				if _, err := os.Stat(filepath.Join(f.previewDir("c1"), "home")); err != nil {
					t.Fatalf("preview folder: %v", err)
				}
				if !f.previewSettled("c2") {
					t.Error("the lander's preview is not settled")
				}
				ask := readyAsk("")
				f.fillReady(ask, "c2", "CHANGED: one")
				if ask.Ready.Preview != before.URL {
					t.Errorf("lander's link = %q, want %q", ask.Ready.Preview, before.URL)
				}

				// a factory restart starts it again on the same port, folder and worktree
				g, _, r2 := previewFactory(t, runSpec, handoff(listed)...)
				g.cfg.StateDir = f.cfg.StateDir
				g.previews = f.previews
				t.Cleanup(g.pv.wg.Wait)
				g.tickPreviews(t0)
				waitFor(t, "restart", func() bool { n, _ := r2.count(); return n == 1 })
				g.pv.wg.Wait() // the restart publishes and checks it in the background
				if p := waitPreview(t, g, "c1", previewReady); p.Port != 7700 || p.URL != before.URL {
					t.Errorf("after restart preview = %+v", p)
				}
				st := r2.starts[0]
				if st.Dir != "/w/c1" || !slices.Contains(st.Env, "HOME="+filepath.Join(f.cfg.StateDir, "previews", "c1", "home")) {
					t.Errorf("restart = %+v", st)
				}
				if _, ok := g.previews.get("c2"); ok {
					t.Error("a second preview was saved for the lander")
				}

				agents := handoff(listed)
				agents[len(agents)-1] = withLabels(agents[len(agents)-1], leave)
				g.SetAgents(agents)
				g.tickPreviews(t0)
				g.tickPreviews(t0)
				if _, ok := g.previews.get("c1"); ok {
					t.Error("preview still saved")
				}
				if _, stopped := r2.count(); stopped != 1 || r2.served[7700] || !slices.Equal(r2.unserve, []int{7700}) {
					t.Errorf("stopped %d, served %v, unserved %v", stopped, r2.served, r2.unserve)
				}
				if _, err := os.Stat(f.previewDir("c1")); !os.IsNotExist(err) {
					t.Errorf("preview folder left: %v", err)
				}
			})
		}
	}
}

// fakeTailscale answers the host runner's tailscale calls: each port's first conflicts calls
// report a serve config conflict, err (if set) fails every call, and overlapping calls are counted.
type fakeTailscale struct {
	mu        sync.Mutex
	conflicts int
	err       error
	calls     map[string]int
	inFlight  int
	overlaps  int
	slept     []time.Duration
}

func (ts *fakeTailscale) run(args ...string) ([]byte, error) {
	ts.mu.Lock()
	ts.inFlight++
	if ts.inFlight > 1 {
		ts.overlaps++
	}
	key := strings.Join(args, " ")
	ts.calls[key]++
	n := ts.calls[key]
	ts.mu.Unlock()
	time.Sleep(time.Millisecond)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.inFlight--
	switch {
	case ts.err != nil:
		return nil, ts.err
	case n <= ts.conflicts:
		return nil, fmt.Errorf("tailscale %s: exit status 1: Another client is changing the serve config; please try again. (preconditions failed: etag mismatch)", key)
	}
	return nil, nil
}

func (ts *fakeTailscale) sleep(d time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.slept = append(ts.slept, d)
}

func newFakeTailscale() (*fakeTailscale, hostRunner) {
	ts := &fakeTailscale{calls: map[string]int{}}
	return ts, hostRunner{tailscale: ts.run, sleep: ts.sleep}
}

func TestServeRetriesAServeConfigConflict(t *testing.T) {
	ts, r := newFakeTailscale()
	ts.conflicts = 2
	if err := r.Serve(7700); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := r.Unserve(7700); err != nil {
		t.Fatalf("Unserve: %v", err)
	}
	if n := ts.calls["serve --bg --https=7700 http://127.0.0.1:7700"]; n != 3 {
		t.Errorf("serve calls = %d, want 3", n)
	}
	if n := ts.calls["serve --https=7700 off"]; n != 3 {
		t.Errorf("unserve calls = %d, want 3", n)
	}
	if len(ts.slept) != 4 {
		t.Errorf("slept %v", ts.slept)
	}
}

func TestServeConflictThatPersistsFails(t *testing.T) {
	ts, r := newFakeTailscale()
	ts.conflicts = 100
	err := r.Serve(7700)
	if err == nil || !strings.Contains(err.Error(), "etag mismatch") {
		t.Fatalf("Serve = %v, want the conflict", err)
	}
	if n := ts.calls["serve --bg --https=7700 http://127.0.0.1:7700"]; n != serveTries {
		t.Errorf("calls = %d, want %d", n, serveTries)
	}
}

func TestServeOtherErrorsAreNotRetried(t *testing.T) {
	ts, r := newFakeTailscale()
	ts.err = errors.New("tailscale serve: exit status 1: serve config denied")
	if err := r.Serve(7700); err == nil {
		t.Fatal("Serve succeeded")
	}
	if n := ts.calls["serve --bg --https=7700 http://127.0.0.1:7700"]; n != 1 || len(ts.slept) != 0 {
		t.Errorf("calls = %d, slept %v", n, ts.slept)
	}
}

func TestServeChangesAreSerialized(t *testing.T) {
	ts, r := newFakeTailscale()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { r.Serve(7700 + i) })
		wg.Go(func() { r.Unserve(7750 + i) })
	}
	wg.Wait()
	if ts.overlaps != 0 {
		t.Errorf("%d tailscale serve changes overlapped", ts.overlaps)
	}
}
