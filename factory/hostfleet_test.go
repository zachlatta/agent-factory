package factory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func linuxSample(total, idle int) string {
	return "@@factory-sample\n@os\nlinux\n@ncpu\n4\n@stat\ncpu  " +
		strconv.Itoa(total-idle) + " 0 0 " + strconv.Itoa(idle) + " 0 0 0 0 0 0\n" +
		"@load\n0.50 0.40 0.30 1/200 999\n@mem\nMemTotal: 1000 kB\nMemAvailable: 250 kB\n" +
		"@df\n/dev/vda1 100 60 30 67% /\n@end\n"
}

const darwinSample = `@@factory-sample
@os
darwin
@ncpu
8
@memsize
17179869184
@vmstat
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                   200000.
Pages active:                                 257552.
Pages inactive:                               250000.
Pages speculative:                              4288.
Pages wired down:                             140839.
@iostat
 10  6 84  2.17 2.01 1.97
@df
/dev/disk3s5   482797652 399111220  44454004    90%    /System/Volumes/Data
@end
`

func TestParseFleetSampleLinux(t *testing.T) {
	s, err := parseFleetSample(strings.TrimPrefix(linuxSample(1000, 400), "@@factory-sample\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := s.Metrics
	if m.CPUCount != 4 || s.CPU == nil || s.CPU.total != 1000 || s.CPU.idle != 400 || m.CPUPercent != nil {
		t.Fatalf("cpu: %+v %+v", m, s.CPU)
	}
	if m.Load == nil || m.Load[0] != 0.5 || m.Memory == nil || m.Memory.Used != 768000 {
		t.Fatalf("load/mem: %+v", m)
	}
	if m.Disk == nil || m.Disk.Total != 102400 || m.Disk.Used != 61440 || m.Disk.Available != 30720 || len(m.Errors) != 0 {
		t.Fatalf("disk: %+v %v", m.Disk, m.Errors)
	}
}

func TestParseFleetSampleDarwin(t *testing.T) {
	s, err := parseFleetSample(strings.TrimPrefix(darwinSample, "@@factory-sample\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := s.Metrics
	if m.CPUCount != 8 || m.CPUPercent == nil || *m.CPUPercent != 16 || s.CPU != nil {
		t.Fatalf("cpu: %+v", m)
	}
	if m.Load == nil || *m.Load != [3]float64{2.17, 2.01, 1.97} {
		t.Fatalf("load: %v", m.Load)
	}
	avail := uint64(200000+250000+4288) * 16384
	if m.Memory == nil || m.Memory.Total != 17179869184 || m.Memory.Available != avail || m.Memory.Used != 17179869184-avail {
		t.Fatalf("memory: %+v", m.Memory)
	}
	if m.Disk == nil || m.Disk.Total != 482797652*1024 || len(m.Errors) != 0 {
		t.Fatalf("disk: %+v %v", m.Disk, m.Errors)
	}
}

func TestParseFleetSampleBadSections(t *testing.T) {
	if _, err := parseFleetSample("garbage\n"); err == nil {
		t.Fatal("accepted a sample with no OS")
	}
	s, err := parseFleetSample("@os\nlinux\n@ncpu\nx\n@stat\ncpu 1 x\n@load\nNaN\n@mem\nMemTotal: 1 kB\n@df\n/dev/a 1 2\n@end\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"cpu", "load", "memory", "disk"} {
		if s.Metrics.Errors[k] == "" {
			t.Errorf("no %s error: %+v", k, s.Metrics.Errors)
		}
	}
	s, _ = parseFleetSample("@os\ndarwin\n@memsize\n100\n@vmstat\nno page size\n@iostat\n 1 2\n@df\n/dev/a 10 20 0 1% /\n@end\n")
	for _, k := range []string{"cpu", "load", "memory", "disk"} {
		if s.Metrics.Errors[k] == "" {
			t.Errorf("darwin: no %s error: %+v", k, s.Metrics.Errors)
		}
	}
}

func TestValidHostAlias(t *testing.T) {
	for _, a := range []string{"alpha", "beta-2", "gamma.lan", "d_e"} {
		if !validHostAlias(a) {
			t.Errorf("rejected %q", a)
		}
	}
	for _, a := range []string{"", "-oProxyCommand=x", "a b", "a;b", "a$(b)", "a/b", "u@h", ".x"} {
		if validHostAlias(a) {
			t.Errorf("accepted %q", a)
		}
	}
}

// fakeStreams hands each dial a pipe the test writes samples into.
type fakeStreams struct {
	mu    sync.Mutex
	dials map[string]int
	pipes map[string]*io.PipeWriter
	ready chan string
	fail  map[string]bool
}

func newFakeStreams() *fakeStreams {
	return &fakeStreams{dials: map[string]int{}, pipes: map[string]*io.PipeWriter{}, ready: make(chan string, 64), fail: map[string]bool{}}
}

func (s *fakeStreams) dial(ctx context.Context, alias string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.dials[alias]++
	fail := s.fail[alias]
	r, w := io.Pipe()
	if !fail {
		s.pipes[alias] = w
	}
	s.mu.Unlock()
	if fail {
		s.ready <- alias
		return nil, errors.New("dial failed")
	}
	go func() { <-ctx.Done(); w.CloseWithError(ctx.Err()) }()
	s.ready <- alias
	return r, nil
}

func (s *fakeStreams) write(t *testing.T, alias, text string) {
	t.Helper()
	s.mu.Lock()
	w := s.pipes[alias]
	s.mu.Unlock()
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatalf("write %s: %v", alias, err)
	}
}

func (s *fakeStreams) count(alias string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials[alias]
}

func waitFleet(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newTestFleet(aliases []string, s *fakeStreams) *hostFleet {
	h := newHostFleet(aliases, "self")
	h.dial = s.dial
	h.backoff = time.Millisecond
	h.reapEvery = time.Hour // tests call reap directly
	return h
}

func remote(h *hostFleet, name string) fleetHost {
	for _, r := range h.snapshotRemote() {
		if r.Name == name {
			return r
		}
	}
	return fleetHost{}
}

func TestFleetOneStreamPerHostForConcurrentViewers(t *testing.T) {
	s := newFakeStreams()
	h := newTestFleet([]string{"alpha", "beta", "alpha", "self", "bad alias"}, s)
	defer h.stop()
	now := time.Unix(1000, 0)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { h.want(now) })
	}
	wg.Wait()
	<-s.ready
	<-s.ready
	time.Sleep(20 * time.Millisecond)
	if s.count("alpha") != 1 || s.count("beta") != 1 || s.count("self") != 0 {
		t.Fatalf("dials: %v", s.dials)
	}
	hosts := h.snapshotRemote()
	if len(hosts) != 3 || hosts[0].Name != "alpha" || hosts[1].Name != "beta" || hosts[2].Error == "" {
		t.Fatalf("hosts: %+v", hosts)
	}
}

func TestFleetSamplesAndCPUDeltaPerStream(t *testing.T) {
	s := newFakeStreams()
	h := newTestFleet([]string{"alpha"}, s)
	defer h.stop()
	h.want(time.Unix(1000, 0))
	<-s.ready
	s.write(t, "alpha", linuxSample(1000, 500))
	waitFleet(t, "first sample", func() bool { return remote(h, "alpha").Metrics != nil })
	if remote(h, "alpha").Metrics.CPUPercent != nil {
		t.Fatal("first sample must warm up")
	}
	s.write(t, "alpha", linuxSample(1100, 525))
	waitFleet(t, "cpu", func() bool { m := remote(h, "alpha").Metrics; return m != nil && m.CPUPercent != nil })
	if p := *remote(h, "alpha").Metrics.CPUPercent; p != 75 {
		t.Fatalf("cpu %v", p)
	}
	// The stream dies; the restarted one reports counters from a rebooted host.
	s.mu.Lock()
	s.pipes["alpha"].Close()
	s.mu.Unlock()
	<-s.ready
	waitFleet(t, "error", func() bool { return remote(h, "alpha").Error != "" })
	if remote(h, "alpha").Metrics == nil {
		t.Fatal("last sample must survive a dropped stream")
	}
	s.write(t, "alpha", linuxSample(100, 50))
	waitFleet(t, "restart sample", func() bool { return remote(h, "alpha").Error == "" })
	if m := remote(h, "alpha").Metrics; m.CPUPercent != nil {
		t.Fatalf("restarted stream used the dead stream's counters: %v", *m.CPUPercent)
	}
	s.write(t, "alpha", linuxSample(200, 75))
	waitFleet(t, "cpu after restart", func() bool { m := remote(h, "alpha").Metrics; return m.CPUPercent != nil })
	if p := *remote(h, "alpha").Metrics.CPUPercent; p != 75 {
		t.Fatalf("cpu after restart %v", p)
	}
}

func TestFleetDialFailureBacksOffAndRetries(t *testing.T) {
	s := newFakeStreams()
	s.fail["alpha"] = true
	h := newTestFleet([]string{"alpha"}, s)
	defer h.stop()
	h.want(time.Unix(1000, 0))
	<-s.ready
	<-s.ready
	waitFleet(t, "error", func() bool { return remote(h, "alpha").Error != "" })
	if strings.Contains(remote(h, "alpha").Error, "dial failed") {
		t.Fatal("raw SSH errors stay off the board")
	}
	if d := backoffAfter(time.Second, 10); d != 60*time.Second {
		t.Fatalf("backoff cap %v", d)
	}
	if d := backoffAfter(2*time.Second, 1); d != 4*time.Second {
		t.Fatalf("backoff %v", d)
	}
}

func TestFleetStopsWhenNobodyWatches(t *testing.T) {
	s := newFakeStreams()
	h := newTestFleet([]string{"alpha"}, s)
	defer h.stop()
	now := time.Unix(1000, 0)
	h.want(now)
	<-s.ready
	if h.reap(now.Add(30 * time.Second)) {
		t.Fatal("reaped while watched")
	}
	h.want(now.Add(40 * time.Second))
	if !h.reap(now.Add(101 * time.Second)) {
		t.Fatal("not reaped after a minute idle")
	}
	// The cancelled stream's reader ends; a new viewer starts a fresh one.
	waitFleet(t, "stream closed", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, err := s.pipes["alpha"].Write([]byte("x"))
		return err != nil
	})
	h.want(now.Add(200 * time.Second))
	<-s.ready
	if s.count("alpha") != 2 {
		t.Fatalf("dials %d", s.count("alpha"))
	}
}

func TestHostsRoute(t *testing.T) {
	f, _ := newTestFactory(t)
	s := newFakeStreams()
	f.fleet = newTestFleet([]string{"alpha"}, s)
	defer f.fleet.stop()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/hosts", nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code %d", rec.Code)
	}
	var out struct{ Hosts []fleetHost }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Hosts) != 2 || out.Hosts[0].Name != "host1" || !out.Hosts[0].Local || out.Hosts[0].Metrics == nil || out.Hosts[1].Name != "alpha" {
		t.Fatalf("hosts: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), f.cfg.Home) {
		t.Fatal("hosts disclose configured path")
	}
	<-s.ready
}
