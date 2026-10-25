package deployer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runOnce(t *testing.T, w *world) {
	t.Helper()
	if err := w.deployer().Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
}

func TestFirstDeployOverLegacyInstall(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w)
	if got := w.file(filepath.Join(w.cfg.Bin, "factory")); got != "factory rev1" {
		t.Errorf("installed %q", got)
	}
	if w.running != "factory rev1" {
		t.Errorf("running %q", w.running)
	}
	if got := w.file(filepath.Join(w.cfg.Bin, "good", "factory")); got != "factory rev1" {
		t.Errorf("good = %q, want the new build after success", got)
	}
	if got := w.file(filepath.Join(w.cfg.Bin, "factory-deploy")); got != "deployer rev1" {
		t.Errorf("deployer = %q, want replaced", got)
	}
	st := w.state()
	if st.Deployed == nil || st.Deployed.Rev != "rev1" || st.Deployed.Key == "" || st.Failed != nil {
		t.Errorf("state = %+v", st)
	}
	if w.called("tailscale serve") != 1 {
		t.Errorf("tailscale serve not re-run: %v", w.calls)
	}
	for _, step := range []string{"go vet ./...", "go test ./...", "systemctl --user daemon-reload", "systemctl --user restart agent-factory"} {
		if w.called(step) != 1 {
			t.Errorf("%q ran %d times: %v", step, w.called(step), w.calls)
		}
	}
	// the deployer replaces itself last, after the new factory passed its checks
	last := w.calls[len(w.calls)-1]
	if !strings.HasPrefix(last, "tailscale") {
		t.Errorf("last call %q", last)
	}
}

func TestNoChangeNoBuild(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w)
	w.calls = nil
	w.rev = "rev2" // a docs-only commit: main moved, the build inputs did not
	runOnce(t, w)
	if w.called("go ") != 0 || w.called("systemctl") != 0 {
		t.Errorf("rebuilt or restarted on a docs-only change: %v", w.calls)
	}
	if w.running != "factory rev1" {
		t.Errorf("running %q", w.running)
	}
}

func TestKeyCoversBuildInputsOnly(t *testing.T) {
	w := newWorld(t)
	var lsTree string
	d := w.deployer()
	d.Run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 2 && args[2] == "ls-tree" {
			lsTree = strings.Join(args, " ")
		}
		return w.run(ctx, dir, name, args...)
	}
	d.Once(context.Background())
	fields := strings.Fields(lsTree)
	for _, p := range []string{"work/factory/cmd", "work/factory/factory", "work/factory/deployer", "work/factory/deploy", "work/factory/go.mod", "work/factory/go.sum"} {
		if indexOf(fields, p) < 0 {
			t.Errorf("ls-tree %q misses %s", lsTree, p)
		}
	}
	if strings.Contains(lsTree, "README") || strings.HasSuffix(lsTree, "work/factory") {
		t.Errorf("ls-tree %q covers docs", lsTree)
	}
}

// The factory's own repo has the module at its root: no sparse checkout, paths without a prefix.
func TestRootModule(t *testing.T) {
	w := newWorld(t)
	w.cfg.Subdir = ""
	var lsTree string
	d := w.deployer()
	d.Run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 2 && args[2] == "ls-tree" {
			lsTree = strings.Join(args, " ")
		}
		return w.run(ctx, dir, name, args...)
	}
	d.Once(context.Background())
	fields := strings.Fields(lsTree)
	for _, p := range []string{"cmd", "factory", "deployer", "deploy", "go.mod"} {
		if indexOf(fields, p) < 0 {
			t.Errorf("ls-tree %q misses %s", lsTree, p)
		}
	}
	for _, c := range w.calls {
		if strings.Contains(c, "sparse-checkout") {
			t.Errorf("sparse checkout for a root module: %s", c)
		}
	}
}

func TestFetchFailureMarksNothing(t *testing.T) {
	w := newWorld(t)
	w.installed("factory rev0")
	w.fetchErr = os.ErrDeadlineExceeded
	runOnce(t, w)
	st := w.state()
	if st.Failed != nil || st.Deployed != nil || w.called("go ") != 0 {
		t.Errorf("fetch failure: state %+v calls %v", st, w.calls)
	}
	if w.running != "factory rev0" {
		t.Errorf("running %q", w.running)
	}
}

func TestBuildFailureKeepsOldAndBacksOff(t *testing.T) {
	for _, stage := range []string{"vet", "test", "build"} {
		t.Run(stage, func(t *testing.T) {
			w := newWorld(t)
			w.installed("factory rev0")
			w.failStage = stage
			runOnce(t, w)
			if w.running != "factory rev0" || w.called("systemctl") != 0 {
				t.Errorf("restarted after a failed %s: running %q, calls %v", stage, w.running, w.calls)
			}
			st := w.state()
			if st.Failed == nil || st.Failed.Stage != stage || st.Failed.Attempts != 1 || !strings.Contains(st.Failed.Log, "broke") {
				t.Fatalf("state %+v", st)
			}
			if want := w.now.Add(5 * time.Minute); !st.Failed.NextTry.Equal(want) {
				t.Errorf("next try %v, want %v", st.Failed.NextTry, want)
			}
			if _, err := os.Stat(filepath.Join(w.cfg.Bin, "factory-deploy.new")); err == nil {
				t.Error("factory-deploy.new left behind")
			}
		})
	}
}

func TestBackoffSchedule(t *testing.T) {
	w := newWorld(t)
	w.installed("factory rev0")
	w.failStage = "test"
	runOnce(t, w)
	builds := func() int { return w.called("go test") }
	w.now = w.now.Add(4 * time.Minute)
	runOnce(t, w)
	if builds() != 1 {
		t.Fatalf("retried inside the 5 min backoff (%d)", builds())
	}
	for i, wait := range []time.Duration{5 * time.Minute, 15 * time.Minute, 60 * time.Minute} {
		w.now = w.state().Failed.NextTry
		runOnce(t, w)
		st := w.state()
		if builds() != i+2 || st.Failed.Attempts != i+2 {
			t.Fatalf("retry %d: builds %d attempts %d", i+1, builds(), st.Failed.Attempts)
		}
		_ = wait
	}
	if st := w.state(); !st.Failed.NextTry.IsZero() {
		t.Errorf("after 3 retries next try = %v, want none", st.Failed.NextTry)
	}
	w.now = w.now.Add(24 * time.Hour)
	runOnce(t, w)
	if builds() != 4 {
		t.Errorf("retried after the last backoff (%d builds)", builds())
	}
	// -retry forces one more try
	d := w.deployer()
	d.cfg.Retry = true
	d.Once(context.Background())
	if builds() != 5 {
		t.Errorf("-retry did not try (%d builds)", builds())
	}
	// a new commit to the build inputs starts over
	w.tree, w.failStage = "tree2", ""
	runOnce(t, w)
	if st := w.state(); st.Failed != nil || st.Deployed.Rev != "rev1" || w.running != "factory rev1" {
		t.Errorf("new key: state %+v running %q", st, w.running)
	}
}

func TestNeverHealthyRollsBack(t *testing.T) {
	for _, behavior := range []string{"crash", "dies", "hang"} {
		t.Run(behavior, func(t *testing.T) {
			w := newWorld(t)
			w.installed("factory rev0")
			w.tree = "tree-" + behavior
			runOnce(t, w)
			if w.running != "factory rev0" {
				t.Errorf("running %q, want rolled back to rev0", w.running)
			}
			if got := w.file(filepath.Join(w.cfg.Bin, "factory")); got != "factory rev0" {
				t.Errorf("bin/factory = %q", got)
			}
			if _, err := os.Stat(filepath.Join(w.cfg.Bin, "factory-deploy")); err == nil {
				t.Error("deployer replaced after a failed deploy")
			}
			st := w.state()
			if st.Failed == nil || st.Failed.Stage != "health" || !strings.Contains(st.Failed.Log, "journal") || st.Deployed != nil {
				t.Errorf("state %+v", st)
			}
			if len(w.notes) != 0 {
				t.Errorf("notified though the rollback came up: %v", w.notes)
			}
		})
	}
}

func TestWrongRevRollsBack(t *testing.T) {
	w := newWorld(t)
	w.installed("factory rev0")
	d := w.deployer()
	d.Probe = func(ctx context.Context) (Health, error) {
		h, err := w.probe(ctx)
		if h.Rev == "rev1" {
			h.Rev = "something-else"
		}
		return h, err
	}
	d.Once(context.Background())
	if w.running != "factory rev0" || w.state().Failed == nil {
		t.Errorf("running %q state %+v", w.running, w.state())
	}
}

// A new unit file that stops the service from starting: the rollback must restore the unit files
// too (and reload them), or the old binary cannot come back either.
func TestBadUnitFileRollsBackUnitsToo(t *testing.T) {
	w := newWorld(t)
	w.installed("factory rev0")
	w.units = map[string]string{"agent-factory.service": "unit v2 BADFLAG", "agent-factory-deploy.timer": "timer v1"}
	w.tree = "tree2"
	runOnce(t, w)
	if got := w.file(filepath.Join(w.cfg.UnitDir, "agent-factory.service")); got != "unit v1" {
		t.Errorf("unit file = %q, want restored", got)
	}
	if w.loadedUnits["agent-factory.service"] != "unit v1" {
		t.Errorf("systemd still has %q: daemon-reload missing before restart", w.loadedUnits["agent-factory.service"])
	}
	if w.running != "factory rev0" {
		t.Errorf("running %q", w.running)
	}
	if len(w.notes) != 0 {
		t.Errorf("notified: %v", w.notes)
	}
}

// A unit that did not exist before the deploy is removed again by the rollback.
func TestRollbackRemovesNewUnit(t *testing.T) {
	w := newWorld(t)
	w.cfg.Units = []string{"agent-factory.service"}
	w.installed("factory rev0")
	w.cfg.Units = []string{"agent-factory.service", "agent-factory-deploy.timer"}
	w.tree = "tree-crash"
	runOnce(t, w)
	if _, err := os.Stat(filepath.Join(w.cfg.UnitDir, "agent-factory-deploy.timer")); err == nil {
		t.Error("new unit left in place after rollback")
	}
}

func TestLockHeldExits(t *testing.T) {
	w := newWorld(t)
	os.MkdirAll(filepath.Dir(w.cfg.StateFile), 0o755)
	f, err := os.OpenFile(w.cfg.StateFile+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	runOnce(t, w)
	if len(w.calls) != 0 {
		t.Errorf("ran with the lock held: %v", w.calls)
	}
}

func TestWatchdogRestartsAfterThreeMisses(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w) // deploys rev1, which becomes good
	w.calls = nil
	w.running = "factory rev1 hang"
	for i := 1; i <= 2; i++ {
		runOnce(t, w)
		if w.called("systemctl --user restart") != 0 || w.state().Misses != i {
			t.Fatalf("miss %d: restarted early (%v), misses %d", i, w.calls, w.state().Misses)
		}
	}
	runOnce(t, w)
	if w.called("systemctl --user restart") != 1 || w.running != "factory rev1" || w.state().Misses != 0 {
		t.Errorf("third miss: calls %v running %q misses %d", w.calls, w.running, w.state().Misses)
	}
	if len(w.notes) != 0 {
		t.Errorf("notified: %v", w.notes)
	}
}

func TestWatchdogRollsBackWhenNotGood(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w) // good = factory rev1
	// someone hand-installed a broken binary
	os.WriteFile(filepath.Join(w.cfg.Bin, "factory"), []byte("factory hand crash"), 0o755)
	w.running = "factory hand crash"
	for i := 0; i < 3; i++ {
		runOnce(t, w)
	}
	if w.running != "factory rev1" || w.file(filepath.Join(w.cfg.Bin, "factory")) != "factory rev1" {
		t.Errorf("running %q, want rolled back to good", w.running)
	}
	if len(w.notes) != 0 {
		t.Errorf("notified: %v", w.notes)
	}
}

func TestWatchdogNotifiesOncePerHourWhenGoodIsDown(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w)
	// the good binary itself cannot start (say the host is out of disk)
	breakIt := func() { w.running = "factory rev1 crash" }
	breakIt()
	d := w.deployer()
	d.Run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		out, err := w.run(ctx, dir, name, args...)
		if name == "systemctl" && args[1] == "restart" {
			breakIt()
		}
		return out, err
	}
	w.calls = nil
	for i := 0; i < 3; i++ {
		d.Once(context.Background())
	}
	if len(w.notes) != 1 || !strings.Contains(w.notes[0], "agent-factory-down") {
		t.Fatalf("notes = %v, want one down alert", w.notes)
	}
	if n := w.called("systemctl --user restart"); n != 1 {
		t.Errorf("%d restarts, want 1: the good set is running, so no rollback", n)
	}
	d.Once(context.Background())
	if len(w.notes) != 1 {
		t.Errorf("notified again within the hour: %d", len(w.notes))
	}
	w.now = w.now.Add(time.Hour)
	d.Once(context.Background())
	if len(w.notes) != 2 {
		t.Errorf("no alert after an hour: %d", len(w.notes))
	}
}

func TestWatchdogIgnoresRefreshErrors(t *testing.T) {
	w := newWorld(t)
	w.installed("legacy")
	runOnce(t, w)
	w.calls = nil
	d := w.deployer()
	d.Probe = func(ctx context.Context) (Health, error) {
		return Health{Rev: "rev1", Live: true, OK: true, RefreshErr: "paseo down"}, nil
	}
	for i := 0; i < 5; i++ {
		d.Once(context.Background())
	}
	if w.called("systemctl") != 0 || len(w.notes) != 0 || w.state().Misses != 0 {
		t.Errorf("acted on a Paseo error: calls %v notes %v", w.calls, w.notes)
	}
}

func TestTestAlert(t *testing.T) {
	w := newWorld(t)
	if err := w.deployer().TestAlert(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.notes) != 1 || !strings.Contains(w.notes[0], "test") {
		t.Errorf("notes %v", w.notes)
	}
}

func TestHTTPProbe(t *testing.T) {
	mux := http.NewServeMux()
	code, body := 200, `{"rev":"r1","live":true,"ok":true}`
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		if code == 404 {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	})
	mux.HandleFunc("GET /api/board", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	probe := HTTPProbe(srv.URL + "/api/health")
	if h, err := probe(context.Background()); err != nil || h.Rev != "r1" || !h.OK {
		t.Errorf("200: %+v %v", h, err)
	}
	code, body = 503, `{"rev":"r1","live":true,"ok":false}`
	if h, err := probe(context.Background()); err != nil || !h.Live || h.OK {
		t.Errorf("503: %+v %v", h, err)
	}
	code = 404 // a factory from before /api/health: alive if the board answers
	if h, err := probe(context.Background()); err != nil || !h.Live || h.Rev != "" {
		t.Errorf("legacy: %+v %v", h, err)
	}
	srv.Close()
	if _, err := probe(context.Background()); err == nil {
		t.Error("closed server: no error")
	}
}

// A fresh host: no factory binary and no units yet. The first deploy installs both; there is no
// good set to fall back on until it succeeds.
func TestFreshInstall(t *testing.T) {
	w := newWorld(t)
	runOnce(t, w)
	if w.running != "factory rev1" || w.state().Deployed == nil {
		t.Fatalf("running %q state %+v", w.running, w.state())
	}
	if got := w.file(filepath.Join(w.cfg.UnitDir, "agent-factory.service")); got != "unit v1" {
		t.Errorf("unit = %q", got)
	}
	if got := w.file(filepath.Join(w.cfg.Bin, "good", "factory")); got != "factory rev1" {
		t.Errorf("good = %q", got)
	}
}

func TestFailureLines(t *testing.T) {
	out := strings.Repeat("16:00:00 noisy log line from a test\n", 100) +
		"--- FAIL: TestBroken (0.00s)\n    broken_test.go:5: deliberately broken\n" +
		strings.Repeat("16:00:00 more noise\n", 100) + "FAIL\nFAIL\tgithub.com/x/factory\t0.3s\nok  \tgithub.com/x/deployer\t0.1s\n"
	got := failureLines(out)
	for _, want := range []string{"--- FAIL: TestBroken", "broken_test.go:5: deliberately broken", "FAIL\tgithub.com/x/factory"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "noise") || strings.Contains(got, "noisy") {
		t.Errorf("kept noise:\n%s", got)
	}
	if got := failureLines("factory/x.go:12:3: undefined: foo\n"); !strings.Contains(got, "undefined: foo") {
		t.Errorf("compile error: %q", got)
	}
	if got := failureLines("a\nb\nweird failure\n"); !strings.Contains(got, "weird failure") {
		t.Errorf("fallback tail: %q", got)
	}
}
