package factory

import (
	"context"
	"errors"
	"testing"
)

type fakeProcessReader struct {
	snapshots []ProcessSnapshot
	err       error
	calls     int
	cancel    context.CancelFunc
}

func (r *fakeProcessReader) Snapshot(ctx context.Context, _ ProcessTarget) (ProcessSnapshot, error) {
	r.calls++
	if r.cancel != nil {
		r.cancel()
	}
	if r.err != nil {
		return ProcessSnapshot{}, r.err
	}
	i := r.calls - 1
	if i >= len(r.snapshots) {
		i = len(r.snapshots) - 1
	}
	return r.snapshots[i], nil
}
func processFixture() ProcessSnapshot {
	return ProcessSnapshot{Complete: true, Processes: []ProcessInfo{
		{PID: 1, UserID: "fixture-user", StartTime: "root-start", EnvironmentReadable: true},
		{PID: 10, UserID: "fixture-user", ParentPID: 1, StartTime: "runner-start", EnvironmentReadable: true, Environment: map[string]string{"PASEO_AGENT_ID": "agent-fixture"}, Command: []string{"fixture-runner"}},
	}}
}
func childFixture(pid, parent int, command ...string) ProcessInfo {
	return ProcessInfo{PID: pid, UserID: "fixture-user", ParentPID: parent, StartTime: "child-start", EnvironmentReadable: true, Environment: map[string]string{"PASEO_AGENT_ID": "agent-fixture"}, Command: command}
}
func TestInspectAgentProcesses(t *testing.T) {
	tests := []struct {
		name     string
		children []ProcessInfo
		commands [][]string
		want     ProcessState
	}{
		{"idle", nil, nil, ProcessIdle},
		{"tool tree", []ProcessInfo{childFixture(20, 10, "fixture-tool", "serve"), childFixture(30, 20, "fixture-helper")}, [][]string{{"fixture-tool", "serve"}}, ProcessIdle},
		{"shell", []ProcessInfo{childFixture(20, 10, "fixture-shell", "run")}, nil, ProcessLiveWork},
		{"command", []ProcessInfo{childFixture(20, 10, "fixture-job")}, nil, ProcessLiveWork},
		{"grandchild tool", []ProcessInfo{childFixture(20, 10, "fixture-job"), childFixture(30, 20, "fixture-tool", "serve")}, [][]string{{"fixture-tool", "serve"}}, ProcessLiveWork},
		{"extra argument", []ProcessInfo{childFixture(20, 10, "fixture-tool", "serve", "extra")}, [][]string{{"fixture-tool", "serve"}}, ProcessLiveWork},
		{"empty list", []ProcessInfo{childFixture(20, 10, "fixture-tool", "serve")}, nil, ProcessLiveWork},
		{"no inherited id", []ProcessInfo{{PID: 20, UserID: "fixture-user", ParentPID: 10, StartTime: "child-start", EnvironmentReadable: true, Command: []string{"fixture-job"}}}, nil, ProcessLiveWork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := processFixture()
			s.Processes = append(s.Processes, tt.children...)
			r := &fakeProcessReader{snapshots: []ProcessSnapshot{s, s}}
			got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal, ToolServerCommands: tt.commands})
			if got.State != tt.want || r.calls != 2 {
				t.Fatalf("got %+v, calls %d; want %v", got, r.calls, tt.want)
			}
		})
	}
}
func TestProcessInspectionUncertain(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProcessSnapshot)
	}{
		{"no runner", func(s *ProcessSnapshot) { s.Processes = s.Processes[:1] }},
		{"two runners", func(s *ProcessSnapshot) { s.Processes = append(s.Processes, childFixture(40, 1, "fixture-runner")) }},
		{"unreadable environment", func(s *ProcessSnapshot) { s.Processes[0].EnvironmentReadable = false }},
		{"partial", func(s *ProcessSnapshot) { s.Complete = false }},
		{"missing parent", func(s *ProcessSnapshot) { s.Processes[1].ParentPID = 99 }},
		{"missing runner owner", func(s *ProcessSnapshot) { s.Processes[1].UserID = "" }},
		{"missing identity", func(s *ProcessSnapshot) { s.Processes[1].StartTime = "" }},
		{"duplicate pid", func(s *ProcessSnapshot) { s.Processes = append(s.Processes, s.Processes[1]) }},
		{"cycle", func(s *ProcessSnapshot) { s.Processes[0].ParentPID = 10 }},
		{"missing command", func(s *ProcessSnapshot) { s.Processes[1].Command = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := processFixture()
			tt.mutate(&s)
			r := &fakeProcessReader{snapshots: []ProcessSnapshot{s, s}}
			if got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal}); got.State != ProcessUncertain {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
func TestProcessInspectionRaces(t *testing.T) {
	for _, kind := range []string{"runner exit", "child exit", "pid reuse", "new child", "command change", "ownership change", "user change", "reparent"} {
		t.Run(kind, func(t *testing.T) {
			a := processFixture()
			a.Processes = append(a.Processes, childFixture(20, 10, "fixture-tool"))
			b := processFixture()
			b.Processes = append(b.Processes, childFixture(20, 10, "fixture-tool"))
			switch kind {
			case "runner exit":
				b.Processes = b.Processes[:1]
			case "child exit":
				b.Processes = b.Processes[:2]
			case "pid reuse":
				b.Processes[2].StartTime = "replacement-start"
			case "new child":
				b.Processes = append(b.Processes, childFixture(30, 10, "fixture-job"))
			case "command change":
				b.Processes[2].Command = []string{"fixture-job"}
			case "ownership change":
				b.Processes[1].Environment = nil
			case "user change":
				b.Processes[1].UserID = "replacement-user"
			case "reparent":
				b.Processes[2].ParentPID = 1
			}
			r := &fakeProcessReader{snapshots: []ProcessSnapshot{a, b}}
			got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal, ToolServerCommands: [][]string{{"fixture-tool"}}})
			if got.State != ProcessUncertain {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
func TestProcessInspectionGuards(t *testing.T) {
	for _, locality := range []ProcessHostLocality{ProcessHostUnknown, ProcessHostRemote} {
		r := &fakeProcessReader{}
		if got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: locality}); got.State != ProcessUncertain || r.calls != 0 {
			t.Fatalf("got %+v, calls %d", got, r.calls)
		}
	}
	s := processFixture()
	for _, cancelDuring := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		r := &fakeProcessReader{snapshots: []ProcessSnapshot{s, s}}
		if cancelDuring {
			r.cancel = cancel
		} else {
			cancel()
		}
		got := InspectAgentProcesses(ctx, r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal})
		cancel()
		if got.State != ProcessUncertain {
			t.Fatalf("cancellation: %+v", got)
		}
	}
	r := &fakeProcessReader{err: errors.New("fixture failure")}
	if got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal}); got.State != ProcessUncertain {
		t.Fatalf("error: %+v", got)
	}
	if (ProcessInspection{}).State != ProcessUncertain {
		t.Fatal("zero value must defer")
	}
}

func TestProcessInspectionEnvironmentScope(t *testing.T) {
	for _, tt := range []struct {
		name   string
		user   string
		parent int
		want   ProcessState
	}{
		{"unrelated system process", "system-user", 1, ProcessIdle},
		{"unreadable same user", "fixture-user", 1, ProcessUncertain},
		{"unknown owner", "", 1, ProcessUncertain},
		{"unreadable descendant", "system-user", 10, ProcessUncertain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := processFixture()
			s.Processes = append(s.Processes, ProcessInfo{PID: 50, ParentPID: tt.parent, UserID: tt.user, StartTime: "system-start"})
			r := &fakeProcessReader{snapshots: []ProcessSnapshot{s, s}}
			got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal})
			if got.State != tt.want {
				t.Fatalf("got %+v; want %v", got, tt.want)
			}
		})
	}
}

func TestProcessInspectionUnreadableSystemAncestor(t *testing.T) {
	s := processFixture()
	s.Processes[0].ParentPID = 2
	s.Processes = append(s.Processes, ProcessInfo{PID: 2, UserID: "system-user", StartTime: "system-start"})
	r := &fakeProcessReader{snapshots: []ProcessSnapshot{s, s}}
	got := InspectAgentProcesses(context.Background(), r, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal})
	if got.State != ProcessIdle {
		t.Fatalf("got %+v", got)
	}
}
