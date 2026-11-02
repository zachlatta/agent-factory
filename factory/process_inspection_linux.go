//go:build linux

package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcProcessReader reads a complete Linux process table. Root must explicitly
// name the proc filesystem (or a fixture tree); no host settings are inferred.
type ProcProcessReader struct {
	Root     string
	readFile func(string) ([]byte, error) // Instance-local seam for read races and permission tests.
}

func (r ProcProcessReader) Snapshot(ctx context.Context, _ ProcessTarget) (ProcessSnapshot, error) {
	s := ProcessSnapshot{Complete: true}
	if r.Root == "" {
		return ProcessSnapshot{}, fmt.Errorf("missing proc root")
	}
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	read := r.readFile
	if read == nil {
		read = os.ReadFile
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ProcessSnapshot{}, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(r.Root, entry.Name())
		p, err := readProcProcess(read, dir, pid)
		if err != nil {
			s.Complete = false
			continue
		}
		s.Processes = append(s.Processes, p)
	}
	return s, nil
}

func readProcProcess(read func(string) ([]byte, error), dir string, pid int) (ProcessInfo, error) {
	p := ProcessInfo{PID: pid}
	stat, err := read(filepath.Join(dir, "stat"))
	if err != nil {
		return p, err
	}
	// comm is parenthesized and may itself contain spaces or closing parentheses.
	close := strings.LastIndexByte(string(stat), ')')
	open := strings.IndexByte(string(stat), '(')
	if open < 0 || close <= open {
		return p, fmt.Errorf("invalid process stat")
	}
	statPID, err := strconv.Atoi(strings.TrimSpace(string(stat[:open])))
	if err != nil || statPID != pid {
		return p, fmt.Errorf("invalid process identity")
	}
	fields := strings.Fields(string(stat[close+1:]))
	if len(fields) < 20 {
		return p, fmt.Errorf("short process stat")
	}
	p.ParentPID, err = strconv.Atoi(fields[1]) // Field 4.
	if err != nil || p.ParentPID < 0 {
		return p, fmt.Errorf("invalid process parent")
	}
	p.StartOrder, err = strconv.ParseUint(fields[19], 10, 64) // Field 22.
	if err != nil {
		return p, err
	}
	p.StartTime = strconv.FormatUint(p.StartOrder, 10)
	status, err := read(filepath.Join(dir, "status"))
	if err != nil {
		return p, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "Uid:" {
			if len(f) != 5 {
				return p, fmt.Errorf("invalid process uid")
			}
			uid, err := strconv.ParseUint(f[1], 10, 32)
			if err != nil {
				return p, err
			}
			p.UserID = strconv.FormatUint(uid, 10)
			break
		}
	}
	if p.UserID == "" {
		return p, fmt.Errorf("missing process uid")
	}
	command, err := read(filepath.Join(dir, "cmdline"))
	if err != nil {
		return p, err
	}
	p.Command = procNULFields(command)
	env, err := read(filepath.Join(dir, "environ"))
	if err != nil {
		if !os.IsPermission(err) {
			return p, err
		}
	} else {
		p.EnvironmentReadable = true
		p.Environment = map[string]string{}
		for _, item := range procNULFields(env) {
			key, value, ok := strings.Cut(item, "=")
			if !ok || key == "" {
				return p, fmt.Errorf("invalid process environment")
			}
			if _, exists := p.Environment[key]; exists {
				return p, fmt.Errorf("duplicate process environment key")
			}
			p.Environment[key] = value
		}
	}
	// Reject exits and PID reuse during the individual record read as well.
	after, err := read(filepath.Join(dir, "stat"))
	if err != nil {
		return p, err
	}
	if string(after) != string(stat) {
		// Volatile stat counters change normally; only compare parent and start identity.
		end := strings.LastIndexByte(string(after), ')')
		if end < 0 {
			return p, fmt.Errorf("invalid process stat")
		}
		f := strings.Fields(string(after[end+1:]))
		if len(f) < 20 || f[1] != fields[1] || f[19] != fields[19] {
			return p, fmt.Errorf("process identity changed")
		}
	}
	return p, nil
}

func procNULFields(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}
