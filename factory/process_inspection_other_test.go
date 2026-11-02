//go:build !linux

package factory

import (
	"context"
	"testing"
)

func TestProcProcessReaderUnsupported(t *testing.T) {
	got := InspectAgentProcesses(context.Background(), ProcProcessReader{Root: t.TempDir()}, ProcessTarget{AgentID: "agent-fixture", Locality: ProcessHostLocal})
	if got.State != ProcessUncertain {
		t.Fatalf("%+v", got)
	}
}
