//go:build linux

package factory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHostLinux(t *testing.T) {
	dir := t.TempDir()
	r := readHost(dir)
	if r.CPU == nil || r.Metrics.CPUCount < 1 || r.Metrics.Load == nil || r.Metrics.Memory == nil || r.Metrics.Disk == nil || len(r.Metrics.Errors) != 0 {
		t.Fatalf("Linux metrics unavailable: %+v", r.Metrics)
	}
	if r.Metrics.Memory.Used+r.Metrics.Memory.Available != r.Metrics.Memory.Total {
		t.Fatal("memory totals inconsistent")
	}
	r = readHost(filepath.Join(dir, "missing"))
	if r.Metrics.Disk != nil || r.Metrics.Errors["disk"] == "" || r.Metrics.Memory == nil {
		t.Fatal("disk failure must preserve memory")
	}
	b, err := json.Marshal(r.Metrics)
	if err != nil || strings.Contains(string(b), dir) {
		t.Fatal("disk error must not disclose path")
	}
}
