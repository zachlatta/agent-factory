//go:build linux

package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func procFixture(t *testing.T, root string, pid, parent int, name string, start int, command, env string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Fields 3 through 22; the final value is starttime.
	fields := []string{"S", fmt.Sprint(parent)}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprint(start))
	for name, data := range map[string]string{
		"stat":    fmt.Sprintf("%d (%s) %s\n", pid, name, strings.Join(fields, " ")),
		"status":  "Name:\tfixture\nUid:\t1000\t1001\t1002\t1003\n",
		"cmdline": command, "environ": env,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcProcessReaderFields(t *testing.T) {
	root := t.TempDir()
	procFixture(t, root, 10, 1, "fixture with ) spaces)", 12345, "fixture-runner\x00arg with spaces\x00", "PASEO_AGENT_ID=agent-fixture\x00VALUE=a=b\x00")
	s, err := (ProcProcessReader{Root: root}).Snapshot(context.Background(), ProcessTarget{})
	if err != nil || !s.Complete || len(s.Processes) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	p := s.Processes[0]
	if p.ParentPID != 1 || p.StartTime != "12345" || p.StartOrder != 12345 || p.UserID != "1000" ||
		!p.EnvironmentReadable || p.Environment["VALUE"] != "a=b" ||
		!reflect.DeepEqual(p.Command, []string{"fixture-runner", "arg with spaces"}) {
		t.Fatalf("%+v", p)
	}
}

func TestProcProcessReaderReadFailures(t *testing.T) {
	for _, file := range []string{"environ", "stat", "status", "cmdline"} {
		t.Run(file, func(t *testing.T) {
			root := t.TempDir()
			procFixture(t, root, 10, 1, "fixture", 30, "fixture\x00", "")
			reader := ProcProcessReader{Root: root, readFile: func(path string) ([]byte, error) {
				if filepath.Base(path) == file {
					return nil, os.ErrPermission
				}
				return os.ReadFile(path)
			}}
			s, err := reader.Snapshot(context.Background(), ProcessTarget{})
			if err != nil {
				t.Fatal(err)
			}
			if file == "environ" {
				if !s.Complete || len(s.Processes) != 1 || s.Processes[0].EnvironmentReadable {
					t.Fatalf("%+v", s)
				}
			} else if s.Complete || len(s.Processes) != 0 {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestProcProcessReaderDisappearingProcess(t *testing.T) {
	root := t.TempDir()
	procFixture(t, root, 10, 1, "fixture", 30, "fixture\x00", "")
	reader := ProcProcessReader{Root: root, readFile: func(path string) ([]byte, error) {
		if filepath.Base(path) == "status" {
			return nil, os.ErrNotExist
		}
		return os.ReadFile(path)
	}}
	s, err := reader.Snapshot(context.Background(), ProcessTarget{})
	if err != nil || s.Complete || len(s.Processes) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestProcProcessReaderInspection(t *testing.T) {
	for _, child := range []string{"fixture-tool", "fixture-shell"} {
		t.Run(child, func(t *testing.T) {
			root := t.TempDir()
			procFixture(t, root, 1, 0, "parent", 10, "fixture-parent\x00", "")
			procFixture(t, root, 10, 1, "runner", 20, "fixture-runner\x00", "PASEO_AGENT_ID=agent-fixture\x00")
			procFixture(t, root, 20, 10, "child", 30, child+"\x00", "PASEO_AGENT_ID=agent-fixture\x00")
			got := InspectAgentProcesses(context.Background(), ProcProcessReader{Root: root}, ProcessTarget{
				AgentID: "agent-fixture", Locality: ProcessHostLocal, ToolServerCommands: [][]string{{"fixture-tool"}},
			})
			want := ProcessIdle
			if child == "fixture-shell" {
				want = ProcessLiveWork
			}
			if got.State != want {
				t.Fatalf("%+v want %v", got, want)
			}
		})
	}
}
