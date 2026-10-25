package factory

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Values describe the execution environment of the factory, not an underlying hypervisor.
type hostMetrics struct {
	SampledAt  time.Time         `json:"sampledAt"`
	CPUCount   int               `json:"cpuCount"`
	CPUPercent *float64          `json:"cpuPercent"`
	Load       *[3]float64       `json:"load"`
	Memory     *hostSpace        `json:"memory"`
	Disk       *hostSpace        `json:"disk"`
	Errors     map[string]string `json:"errors"`
}

type hostSpace struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

type cpuCounters struct{ total, idle uint64 }
type hostReading struct {
	Metrics hostMetrics
	CPU     *cpuCounters
}
type hostSampler struct {
	mu       sync.Mutex
	last     hostMetrics
	previous *cpuCounters
}

func (s *hostSampler) sample(now time.Time, path string, read func(string) hostReading) hostMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.last.SampledAt.IsZero() && now.Sub(s.last.SampledAt) >= 0 && now.Sub(s.last.SampledAt) < 2*time.Second {
		return s.last
	}
	r := read(path)
	if r.CPU != nil && s.previous != nil && now.Sub(s.last.SampledAt) > 0 && now.Sub(s.last.SampledAt) <= 6*time.Second {
		r.Metrics.CPUPercent = cpuPercent(*s.previous, *r.CPU)
	}
	s.previous = r.CPU
	r.Metrics.SampledAt = now
	s.last = r.Metrics
	return s.last
}

func (f *Factory) handleHost(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, f.hostMetrics.sample(time.Now(), f.cfg.Home, readHost))
}

func cpuPercent(prev, next cpuCounters) *float64 {
	if next.total <= prev.total || next.idle < prev.idle {
		return nil
	}
	total, idle := next.total-prev.total, next.idle-prev.idle
	if idle > total {
		return nil
	}
	p := 100 * float64(total-idle) / float64(total)
	return &p
}

var errHostCounters = errors.New("invalid host counters")

func parseCPU(text string) (cpuCounters, error) {
	lines := strings.SplitN(text, "\n", 2)
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, errHostCounters
	}
	var c cpuCounters
	// Guest times (fields 9 and 10) are already included in user and nice.
	for i := 1; i < len(fields) && i <= 8; i++ {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil || n > math.MaxUint64-c.total {
			return cpuCounters{}, errHostCounters
		}
		c.total += n
		if i == 4 || i == 5 {
			c.idle += n
		}
	}
	return c, nil
}

func parseMemory(text string) (*hostSpace, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "MemTotal:" && f[0] != "MemAvailable:" {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return nil, errHostCounters
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || n > math.MaxUint64/1024 {
			return nil, errHostCounters
		}
		values[f[0]] = n * 1024
	}
	total, available := values["MemTotal:"], values["MemAvailable:"]
	if _, ok := values["MemAvailable:"]; !ok || total == 0 || available > total {
		return nil, errHostCounters
	}
	return &hostSpace{Total: total, Used: total - available, Available: available}, nil
}

func parseLoad(text string) ([3]float64, error) {
	var load [3]float64
	f := strings.Fields(text)
	if len(f) < 3 {
		return load, errHostCounters
	}
	for i := range load {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return [3]float64{}, errHostCounters
		}
		load[i] = v
	}
	return load, nil
}

func diskSpace(blocks, free, available, blockSize uint64) (*hostSpace, error) {
	if blocks == 0 || blockSize == 0 || free > blocks || available > free || blocks > math.MaxUint64/blockSize {
		return nil, errHostCounters
	}
	return &hostSpace{Total: blocks * blockSize, Used: (blocks - free) * blockSize, Available: available * blockSize}, nil
}
