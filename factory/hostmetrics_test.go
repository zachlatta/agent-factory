package factory

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHostCounterParsing(t *testing.T) {
	c, err := parseCPU("cpu  100 20 30 400 50 10 5 2 90 0\ncpu0 1 2 3 4\n")
	if err != nil || c.total != 617 || c.idle != 450 {
		t.Fatalf("cpu: %+v %v", c, err)
	}
	for _, s := range []string{"", "cpu 1 2", "cpu 1 x 3 4", "cpu -1 2 3 4"} {
		if _, err := parseCPU(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	m, err := parseMemory("MemTotal: 1000 kB\nMemFree: 10 kB\nMemAvailable: 250 kB\nCached: 500 kB\n")
	if err != nil || m.Total != 1024000 || m.Available != 256000 || m.Used != 768000 {
		t.Fatalf("memory: %+v %v", m, err)
	}
	for _, s := range []string{"MemTotal: 1 kB", "MemTotal: 1 kB\nMemAvailable: 2 kB", "MemTotal: x kB\nMemAvailable: 0 kB"} {
		if _, err := parseMemory(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	l, err := parseLoad("1.25 2.50 3.75 1/100 123")
	if err != nil || l != [3]float64{1.25, 2.5, 3.75} {
		t.Fatalf("load: %v %v", l, err)
	}
	for _, s := range []string{"1 2", "NaN 1 2", "-1 0 0"} {
		if _, err := parseLoad(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestHostCPUDelta(t *testing.T) {
	for _, tc := range []struct {
		prev, next cpuCounters
		want       float64
		valid      bool
	}{
		{cpuCounters{100, 50}, cpuCounters{200, 75}, 75, true},
		{cpuCounters{100, 50}, cpuCounters{100, 50}, 0, false},
		{cpuCounters{100, 50}, cpuCounters{50, 25}, 0, false},
		{cpuCounters{100, 50}, cpuCounters{200, 49}, 0, false},
		{cpuCounters{100, 50}, cpuCounters{110, 70}, 0, false},
	} {
		got := cpuPercent(tc.prev, tc.next)
		if (got != nil) != tc.valid || got != nil && *got != tc.want {
			t.Errorf("delta %+v: %v", tc, got)
		}
	}
}

func TestHostDiskSpace(t *testing.T) {
	d, err := diskSpace(100, 30, 20, 1024)
	if err != nil || d.Total != 102400 || d.Used != 71680 || d.Available != 20480 {
		t.Fatalf("disk: %+v %v", d, err)
	}
	if _, err := diskSpace(10, 11, 0, 1); err == nil {
		t.Fatal("accepted invalid disk counters")
	}
}

func TestHostSamplerCacheAndFailure(t *testing.T) {
	var s hostSampler
	now := time.Unix(100, 0)
	calls := 0
	read := func(string) hostReading {
		calls++
		return hostReading{CPU: &cpuCounters{uint64(calls * 100), uint64(calls * 25)}, Metrics: hostMetrics{Errors: map[string]string{"disk": "unavailable"}}}
	}
	first := s.sample(now, "", read)
	if first.CPUPercent != nil {
		t.Fatal("first sample must warm up")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { s.sample(now.Add(time.Second), "", read) })
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("cache reads: %d", calls)
	}
	next := s.sample(now.Add(2*time.Second), "", read)
	if calls != 2 || next.CPUPercent == nil || *next.CPUPercent != 75 || next.Errors["disk"] == "" {
		t.Fatalf("next: %+v", next)
	}
	s.sample(now.Add(4*time.Second), "", func(string) hostReading {
		return hostReading{Metrics: hostMetrics{Errors: map[string]string{"cpu": "unavailable"}}}
	})
	next = s.sample(now.Add(6*time.Second), "", read)
	if next.CPUPercent != nil {
		t.Fatal("must warm up after failed CPU read")
	}
}

func TestHostRoutes(t *testing.T) {
	f, _ := newTestFactory(t)
	h := f.Handler()
	for _, path := range []string{"/host", "/host-metrics.js", "/api/host"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if path == "/api/host" {
			var m hostMetrics
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.SampledAt.IsZero() {
				t.Fatalf("metrics: %v", err)
			}
			if strings.Contains(rec.Body.String(), f.cfg.Home) {
				t.Fatal("metrics disclose configured path")
			}
		}
	}
}

func TestHostSamplerIdleGapWarmsUp(t *testing.T) {
	var s hostSampler
	now := time.Unix(100, 0)
	read := func(string) hostReading { return hostReading{CPU: &cpuCounters{100, 50}} }
	s.sample(now, "", read)
	next := s.sample(now.Add(time.Hour), "", func(string) hostReading { return hostReading{CPU: &cpuCounters{200, 75}} })
	if next.CPUPercent != nil {
		t.Fatal("CPU must warm up after an idle gap instead of averaging over the gap")
	}
}
