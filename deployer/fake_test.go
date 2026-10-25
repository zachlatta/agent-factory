package deployer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// world fakes everything outside the deployer: the git remote, the Go toolchain, systemd and the
// running factory. A binary's file content says how it behaves once started:
//
//	"factory <rev>"        healthy, reports <rev>
//	"factory <rev> crash"  never answers
//	"factory <rev> dies"   healthy for its first probe only
//	"factory <rev> hang"   answers, but not live
//	"legacy"               a factory from before /api/health: answers /api/board only
//
// A unit file containing "BADFLAG" makes the service fail to start, whatever the binary.
type world struct {
	t   *testing.T
	cfg Config
	now time.Time

	rev, tree string            // origin/main
	units     map[string]string // deploy/<unit> on origin/main
	fetchErr  error
	failStage string // vet, test or build: that go command fails

	loadedUnits map[string]string // unit files as of the last daemon-reload
	running     string            // what the service runs; "" = stopped
	probes      int               // probes since the last start
	calls       []string
	notes       []string // notify --data bodies
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, now: time.Date(2026, 10, 25, 22, 0, 0, 0, time.UTC), rev: "rev1", tree: "tree1",
		units: map[string]string{"agent-factory.service": "unit v1", "agent-factory-deploy.timer": "timer v1"}}
	w.cfg = Config{Remote: "https://example.invalid/factory.git", Branch: "main", Src: filepath.Join(root, "src"),
		Subdir: "work/factory", Bin: filepath.Join(root, "bin"), UnitDir: filepath.Join(root, "units"),
		Units: []string{"agent-factory.service", "agent-factory-deploy.timer"}, Service: "agent-factory",
		StateFile: filepath.Join(root, "state", "deploy.json"), HealthURL: "http://127.0.0.1:1/api/health",
		Serve: []string{"tailscale", "serve", "--bg", "--https=7443", "http://127.0.0.1:7478"}}
	for _, d := range []string{w.cfg.Bin, w.cfg.UnitDir} {
		os.MkdirAll(d, 0o755)
	}
	return w
}

// installed sets up a factory already running from rev, as if deployed (or installed by hand).
func (w *world) installed(content string) {
	os.WriteFile(filepath.Join(w.cfg.Bin, "factory"), []byte(content), 0o755)
	for _, u := range w.cfg.Units {
		os.WriteFile(filepath.Join(w.cfg.UnitDir, u), []byte(w.units[u]), 0o644)
	}
	w.reload()
	w.running, w.probes = content, 0
}

func (w *world) reload() {
	w.loadedUnits = map[string]string{}
	for _, u := range w.cfg.Units {
		if b, err := os.ReadFile(filepath.Join(w.cfg.UnitDir, u)); err == nil {
			w.loadedUnits[u] = string(b)
		}
	}
}

func (w *world) deployer() *Deployer {
	d := New(w.cfg)
	d.Run = w.run
	d.Probe = w.probe
	d.Now = func() time.Time { return w.now }
	d.Sleep = func(dt time.Duration) { w.now = w.now.Add(dt) }
	d.Logf = func(format string, args ...any) { w.t.Logf(format, args...) }
	return d
}

func (w *world) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	w.calls = append(w.calls, call)
	switch name {
	case "git":
		return w.git(args)
	case "go":
		if args[0] == w.failStage {
			return "FAIL: " + args[0] + " broke", errors.New("exit status 1")
		}
		if args[0] == "build" {
			out := args[indexOf(args, "-o")+1]
			ld := args[indexOf(args, "-ldflags")+1]
			rev := strings.TrimPrefix(ld, "-X main.rev=")
			content := "factory " + rev
			if b, err := os.ReadFile(filepath.Join(dir, "BEHAVIOR")); err == nil {
				content += " " + strings.TrimSpace(string(b))
			}
			if strings.HasSuffix(out, "factory-deploy.new") {
				content = "deployer " + rev
			}
			return "", os.WriteFile(out, []byte(content), 0o755)
		}
		return "ok", nil
	case "systemctl":
		switch args[1] {
		case "daemon-reload":
			w.reload()
		case "restart":
			b, _ := os.ReadFile(filepath.Join(w.cfg.Bin, "factory"))
			w.running, w.probes = string(b), 0
			for _, u := range w.loadedUnits {
				if strings.Contains(u, "BADFLAG") {
					w.running = string(b) + " crash"
				}
			}
		}
		return "", nil
	case "journalctl":
		return "journal: factory exited", nil
	case "pdw":
		w.notes = append(w.notes, args[len(args)-1])
		return "{}", nil
	case "tailscale":
		return "", nil
	}
	return "", fmt.Errorf("fake: unexpected command %s", call)
}

func (w *world) git(args []string) (string, error) {
	if args[0] == "clone" {
		return "", os.MkdirAll(filepath.Join(args[len(args)-1], ".git"), 0o755)
	}
	if args[0] != "-C" {
		return "", fmt.Errorf("fake git: %v", args)
	}
	switch args[2] {
	case "sparse-checkout":
		return "", nil
	case "fetch":
		return "", w.fetchErr
	case "rev-parse":
		return w.rev + "\n", nil
	case "ls-tree":
		return w.tree + "\n", nil
	case "checkout":
		dir := filepath.Join(w.cfg.Src, w.cfg.Subdir, "deploy")
		os.RemoveAll(filepath.Join(w.cfg.Src, w.cfg.Subdir))
		os.MkdirAll(dir, 0o755)
		for u, c := range w.units {
			os.WriteFile(filepath.Join(dir, u), []byte(c), 0o644)
		}
		if strings.Contains(w.tree, "crash") || strings.Contains(w.tree, "dies") || strings.Contains(w.tree, "hang") {
			os.WriteFile(filepath.Join(w.cfg.Src, w.cfg.Subdir, "BEHAVIOR"), []byte(w.tree[strings.LastIndex(w.tree, "-")+1:]), 0o644)
		}
		return "", nil
	}
	return "", fmt.Errorf("fake git: %v", args)
}

func (w *world) probe(ctx context.Context) (Health, error) {
	w.probes++
	f := strings.Fields(w.running)
	switch {
	case w.running == "":
		return Health{}, errors.New("connection refused")
	case w.running == "legacy":
		return Health{Live: true, OK: true}, nil
	case len(f) > 2 && f[2] == "crash":
		return Health{}, errors.New("connection refused")
	case len(f) > 2 && f[2] == "dies" && w.probes > 1:
		return Health{}, errors.New("connection refused")
	case len(f) > 2 && f[2] == "hang":
		return Health{Rev: f[1]}, nil
	}
	return Health{Rev: f[1], Live: true, OK: true}, nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func (w *world) file(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (w *world) state() State {
	st, err := loadState(w.cfg.StateFile)
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

func (w *world) called(prefix string) int {
	n := 0
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
