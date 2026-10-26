package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestRetirementCommandLiveReadsOnlySelectedHome(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	paseoHome := filepath.Join(home, ".paseo")
	files := map[string]any{
		"projects/workspaces.json": []any{map[string]any{"workspaceId": "record", "kind": "directory", "cwd": home}},
		"agents/test/done.json":    map[string]any{"id": "done", "cwd": home, "lastStatus": "closed"},
	}
	for path, value := range files {
		full := filepath.Join(paseoHome, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(value)
		if err := os.WriteFile(full, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	status, _ := json.Marshal(map[string]string{"home": paseoHome, "connectedDaemon": "reachable", "localDaemon": "running", "daemonVersion": "0.11.4"})
	if err := os.WriteFile(filepath.Join(bin, "status"), status, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$RETIREMENT_TEST_DIR/calls"
case "$*" in
  --version) printf '0.11.4\n' ;;
  "status --json --home $RETIREMENT_TEST_HOME") cat "$RETIREMENT_TEST_DIR/status" ;;
  "terminal ls --all --json --home $RETIREMENT_TEST_HOME") printf '[]\n' ;;
  "script ls --workspace record --json --home $RETIREMENT_TEST_HOME") printf '[]\n' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "paseo"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RETIREMENT_TEST_DIR", bin)
	t.Setenv("RETIREMENT_TEST_HOME", paseoHome)
	for _, live := range []bool{false, true} {
		args := []string{"-home", home}
		if live {
			args = append(args, "-live")
		}
		var out, diagnostics bytes.Buffer
		if code := retirementCmd(args, &out, &diagnostics); code != 0 {
			t.Fatalf("exit %d: %s", code, diagnostics.String())
		}
		var r struct {
			ExecutorEnabled bool
			Allowlist       []any
			Live            any
			Workspaces      []struct{ Observations []any }
		}
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.ExecutorEnabled || len(r.Allowlist) != 0 || (r.Live != nil) != live {
			t.Fatal("wrong authority or opt-in behavior")
		}
		if !live {
			if _, err := os.Stat(filepath.Join(bin, "calls")); !os.IsNotExist(err) {
				t.Fatal("default mode invoked paseo")
			}
		} else if len(r.Workspaces[0].Observations) != 2 {
			t.Fatal("live mode did not observe resources")
		}
	}
	b, err := os.ReadFile(filepath.Join(bin, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	want := "--version\nstatus --json --home " + paseoHome + "\nterminal ls --all --json --home " + paseoHome + "\nscript ls --workspace record --json --home " + paseoHome + "\n"
	if string(b) != want {
		t.Fatalf("unexpected commands: %q", b)
	}
	if strings.Contains(string(b), "archive") {
		t.Fatal("mutation issued")
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
