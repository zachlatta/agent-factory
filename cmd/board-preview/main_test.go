package main

import (
	"path/filepath"
	"testing"
)

func TestPreviewRejectsWildcardBeforeCreatingState(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "localhost:0"} {
		if err := run(addr, filepath.Join(t.TempDir(), "state")); err == nil {
			t.Fatalf("accepted %q", addr)
		}
	}
}
