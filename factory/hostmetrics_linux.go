//go:build linux

package factory

import (
	"os"
	"runtime"
	"syscall"
)

func readHost(path string) hostReading {
	r := hostReading{Metrics: hostMetrics{CPUCount: runtime.NumCPU(), Errors: map[string]string{}}}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		if c, err := parseCPU(string(b)); err == nil {
			r.CPU = &c
		}
	}
	if r.CPU == nil {
		r.Metrics.Errors["cpu"] = "CPU counters unavailable"
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if l, err := parseLoad(string(b)); err == nil {
			r.Metrics.Load = &l
		}
	}
	if r.Metrics.Load == nil {
		r.Metrics.Errors["load"] = "Load averages unavailable"
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		r.Metrics.Memory, _ = parseMemory(string(b))
	}
	if r.Metrics.Memory == nil {
		r.Metrics.Errors["memory"] = "Memory counters unavailable"
	}
	var stat syscall.Statfs_t
	if path != "" && syscall.Statfs(path, &stat) == nil && stat.Bsize > 0 {
		r.Metrics.Disk, _ = diskSpace(stat.Blocks, stat.Bfree, stat.Bavail, uint64(stat.Bsize))
	}
	if r.Metrics.Disk == nil {
		r.Metrics.Errors["disk"] = "Home filesystem capacity unavailable"
	}
	return r
}
