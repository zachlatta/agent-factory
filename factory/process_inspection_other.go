//go:build !linux

package factory

import (
	"context"
	"fmt"
)

// ProcProcessReader is unavailable on platforms without Linux proc evidence.
type ProcProcessReader struct{ Root string }

func (ProcProcessReader) Snapshot(context.Context, ProcessTarget) (ProcessSnapshot, error) {
	return ProcessSnapshot{}, fmt.Errorf("process inspection requires Linux")
}
