package factory

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fleet is the factory's own host plus the SSH aliases in Config.MetricHosts (host config,
// FACTORY_METRIC_HOSTS). Each alias gets one long-lived, read-only SSH stream, shared by every
// viewer, that runs only while someone polls /api/hosts and stops a minute after the last poll.

// fleetScript is the only command sent to a host: it prints a sample every 2 seconds and reads
// nothing else. It is fed to `sh -s` on stdin, so no part of it is parsed by the remote login shell.
const fleetScript = `while :; do
echo @@factory-sample
if [ -r /proc/stat ]; then
echo @os; echo linux
echo @ncpu; getconf _NPROCESSORS_ONLN
echo @stat; head -n 1 /proc/stat
echo @load; cat /proc/loadavg
echo @mem; cat /proc/meminfo
echo @df; df -kP "$HOME" | tail -n 1
echo @end
sleep 2
else
echo @os; echo darwin
echo @ncpu; sysctl -n hw.ncpu
echo @memsize; sysctl -n hw.memsize
echo @vmstat; vm_stat
echo @df; df -kP "$HOME" | tail -n 1
echo @iostat; iostat -c 2 -w 1 -n 0 | tail -n 1
echo @end
sleep 1
fi
done
`

type fleetHost struct {
	Name    string       `json:"name"`
	Local   bool         `json:"local"`
	Metrics *hostMetrics `json:"metrics"`
	Error   string       `json:"error,omitempty"`
}

type hostDialer func(ctx context.Context, alias string) (io.ReadCloser, error)

type hostFleet struct {
	mu        sync.Mutex
	aliases   []string // the ones streamed
	names     []string // every configured host, in config order
	hosts     map[string]*fleetHost
	lastWant  time.Time
	cancel    context.CancelFunc
	dial      hostDialer
	now       func() time.Time
	idle      time.Duration // streams stop this long after the last poll
	backoff   time.Duration // first retry delay after a stream ends
	reapEvery time.Duration
}

var hostAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validHostAlias keeps an alias from reading as an ssh option, a user@ or a shell word.
func validHostAlias(a string) bool { return len(a) <= 253 && hostAlias.MatchString(a) }

// newHostFleet drops duplicates and the factory's own host (self), which is read locally.
func newHostFleet(aliases []string, self string) *hostFleet {
	h := &hostFleet{hosts: map[string]*fleetHost{}, dial: sshDial, now: time.Now,
		idle: time.Minute, backoff: 2 * time.Second, reapEvery: 10 * time.Second}
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" || a == self || h.hosts[a] != nil {
			continue
		}
		fh := &fleetHost{Name: a, Error: "Waiting for first sample"}
		if !validHostAlias(a) {
			fh.Error = "Not a valid SSH alias; fix FACTORY_METRIC_HOSTS"
		} else {
			h.aliases = append(h.aliases, a)
		}
		h.hosts[a] = fh
		h.names = append(h.names, a)
	}
	return h
}

func sshDial(ctx context.Context, alias string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "--", alias, "sh", "-s")
	cmd.Stdin = strings.NewReader(fleetScript)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdReader{out, cmd}, nil
}

type cmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c cmdReader) Close() error { c.ReadCloser.Close(); return c.cmd.Wait() }

// want records a poll and starts the streams if none run. Concurrent viewers share them.
func (h *hostFleet) want(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastWant = now
	if h.cancel != nil || len(h.aliases) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	for _, a := range h.aliases {
		go h.run(ctx, a)
	}
	go func() {
		t := time.NewTicker(h.reapEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.reap(h.now())
			}
		}
	}()
}

// reap stops the streams once nobody has polled for h.idle; it reports whether it did.
func (h *hostFleet) reap(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel == nil || now.Sub(h.lastWant) < h.idle {
		return false
	}
	h.cancel()
	h.cancel = nil
	return true
}

func (h *hostFleet) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

func backoffAfter(base time.Duration, failures int) time.Duration {
	d := base
	for i := 0; i < failures && d < time.Minute; i++ {
		d *= 2
	}
	return min(d, time.Minute)
}

// run keeps one stream to alias open until ctx ends, retrying with backoff.
func (h *hostFleet) run(ctx context.Context, alias string) {
	failures := 0
	for ctx.Err() == nil {
		got := h.stream(ctx, alias)
		if ctx.Err() != nil {
			return
		}
		if got {
			failures = 0
		}
		wait := backoffAfter(h.backoff, failures)
		failures++
		h.setError(alias, fmt.Sprintf("Unreachable; retrying every %ds", int(math.Ceil(wait.Seconds()))))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stream reads samples from one SSH session. CPU deltas use only this session's counters, so a
// restarted stream never compares against a dead one. It reports whether any sample arrived.
func (h *hostFleet) stream(ctx context.Context, alias string) bool {
	r, err := h.dial(ctx, alias)
	if err != nil {
		log.Printf("host metrics: %s: %v", alias, err)
		return false
	}
	defer r.Close()
	var prev *cpuCounters
	got := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var body strings.Builder
	inSample := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "@@factory-sample":
			body.Reset()
			inSample = true
		case line == "@end" && inSample:
			inSample = false
			s, err := parseFleetSample(body.String())
			if err != nil {
				continue
			}
			if s.CPU != nil && prev != nil {
				s.Metrics.CPUPercent = cpuPercent(*prev, *s.CPU)
			}
			prev = s.CPU
			s.Metrics.SampledAt = h.now()
			h.set(alias, s.Metrics)
			got = true
		case inSample:
			if body.Len() < 256*1024 {
				body.WriteString(line)
				body.WriteByte('\n')
			}
		}
	}
	return got
}

func (h *hostFleet) set(alias string, m hostMetrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[alias].Metrics, h.hosts[alias].Error = &m, ""
}

func (h *hostFleet) setError(alias, e string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hosts[alias].Error = e
}

// snapshotRemote returns the configured hosts in config order, without the local one.
func (h *hostFleet) snapshotRemote() []fleetHost {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]fleetHost, 0, len(h.names))
	for _, n := range h.names {
		out = append(out, *h.hosts[n])
	}
	return out
}

func (f *Factory) handleHosts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	local := f.hostMetrics.sample(now, f.cfg.Home, readHost)
	hosts := []fleetHost{{Name: f.cfg.Host, Local: true, Metrics: &local}}
	if f.fleet != nil {
		f.fleet.want(now)
		hosts = append(hosts, f.fleet.snapshotRemote()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"now": now, "hosts": hosts})
}

// parseFleetSample reads one sample body (the lines between @@factory-sample and @end).
func parseFleetSample(text string) (hostReading, error) {
	sec := map[string]string{}
	name := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "@") && !strings.Contains(line, " ") {
			name = line[1:]
			continue
		}
		if name != "" {
			sec[name] += line + "\n"
		}
	}
	r := hostReading{Metrics: hostMetrics{Errors: map[string]string{}}}
	kind := strings.TrimSpace(sec["os"])
	if kind != "linux" && kind != "darwin" {
		return r, errHostCounters
	}
	if n, err := strconv.Atoi(strings.TrimSpace(sec["ncpu"])); err == nil && n > 0 {
		r.Metrics.CPUCount = n
	}
	if kind == "linux" {
		if c, err := parseCPU(sec["stat"]); err == nil {
			r.CPU = &c
		} else {
			r.Metrics.Errors["cpu"] = "CPU counters unavailable"
		}
		if l, err := parseLoad(sec["load"]); err == nil {
			r.Metrics.Load = &l
		} else {
			r.Metrics.Errors["load"] = "Load averages unavailable"
		}
		if m, err := parseMemory(sec["mem"]); err == nil {
			r.Metrics.Memory = m
		} else {
			r.Metrics.Errors["memory"] = "Memory counters unavailable"
		}
	} else {
		parseDarwin(sec, &r.Metrics)
	}
	if d, err := parseDF(sec["df"]); err == nil {
		r.Metrics.Disk = d
	} else {
		r.Metrics.Errors["disk"] = "Home filesystem capacity unavailable"
	}
	return r, nil
}

// parseDarwin fills CPU and load from one iostat line ("us sy id 1m 5m 15m") and memory from
// hw.memsize and vm_stat. Available memory is free + inactive + speculative pages, an estimate.
func parseDarwin(sec map[string]string, m *hostMetrics) {
	f := strings.Fields(sec["iostat"])
	if len(f) == 6 {
		if idle, err := strconv.ParseFloat(f[2], 64); err == nil && idle >= 0 && idle <= 100 {
			p := 100 - idle
			m.CPUPercent = &p
		}
		if l, err := parseLoad(strings.Join(f[3:], " ")); err == nil {
			m.Load = &l
		}
	}
	if m.CPUPercent == nil {
		m.Errors["cpu"] = "CPU counters unavailable"
	}
	if m.Load == nil {
		m.Errors["load"] = "Load averages unavailable"
	}
	if mem, err := parseVMStat(sec["memsize"], sec["vmstat"]); err == nil {
		m.Memory = mem
	} else {
		m.Errors["memory"] = "Memory counters unavailable"
	}
}

var vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

func parseVMStat(memsize, text string) (*hostSpace, error) {
	total, err := strconv.ParseUint(strings.TrimSpace(memsize), 10, 64)
	ps := vmPageSize.FindStringSubmatch(text)
	if err != nil || total == 0 || ps == nil {
		return nil, errHostCounters
	}
	page, err := strconv.ParseUint(ps[1], 10, 64)
	if err != nil || page == 0 || page > 1<<30 {
		return nil, errHostCounters
	}
	var pages uint64
	seen := 0
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || k != "Pages free" && k != "Pages inactive" && k != "Pages speculative" {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil || n > math.MaxUint64/page/4 {
			return nil, errHostCounters
		}
		pages += n
		seen++
	}
	avail := pages * page
	if seen != 3 || avail > total {
		return nil, errHostCounters
	}
	return &hostSpace{Total: total, Used: total - avail, Available: avail}, nil
}

// parseDF reads the last line of `df -kP`: filesystem, 1024-blocks, used, available, ...
func parseDF(text string) (*hostSpace, error) {
	f := strings.Fields(text)
	if len(f) < 4 {
		return nil, errHostCounters
	}
	var n [3]uint64
	for i := range n {
		v, err := strconv.ParseUint(f[i+1], 10, 64)
		if err != nil || v > math.MaxUint64/1024 {
			return nil, errHostCounters
		}
		n[i] = v * 1024
	}
	if n[0] == 0 || n[1] > n[0] || n[2] > n[0] {
		return nil, errHostCounters
	}
	return &hostSpace{Total: n[0], Used: n[1], Available: n[2]}, nil
}
