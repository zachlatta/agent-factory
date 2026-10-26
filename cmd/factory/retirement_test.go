package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRetirementCommandFailsClosedOnMissingInventory(t *testing.T) {
	var out, diagnostics bytes.Buffer
	if code := retirementCmd([]string{"-home", t.TempDir()}, &out, &diagnostics); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var report struct {
		ExecutorEnabled bool
		Problems        []string
		Allowlist       []any
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ExecutorEnabled || len(report.Problems) == 0 || len(report.Allowlist) != 0 {
		t.Fatalf("unsafe report: %+v", report)
	}
}

func TestRetirementCommandRejectsApplyAndPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"-apply"}, {"workspace"}} {
		var out, diagnostics bytes.Buffer
		if retirementCmd(args, &out, &diagnostics) != 2 || out.Len() != 0 {
			t.Fatal("accepted unsupported operation")
		}
	}
}

func TestRetirementCommandEmptyWorkspaceList(t *testing.T) {
	home := t.TempDir()
	for path, value := range map[string]string{"projects/workspaces.json": "[]", "agents/test/done.json": `{"id":"done","cwd":"/example","lastStatus":"closed"}`} {
		full := filepath.Join(home, ".paseo", path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, diagnostics bytes.Buffer
	if code := retirementCmd([]string{"-home", home}, &out, &diagnostics); code != 0 {
		t.Fatalf("exit %d: %s", code, diagnostics.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("invalid report")
	}
}
