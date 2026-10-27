package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The subprocess fixture owns a directory of 450 records. Reads address one ID;
// archive lookup models the CLI's old first-page and corrected paginated behavior.
type archiveFixture struct {
	Version, Mode string
	Agents        []archiveRecord
}
type archiveRecord struct {
	ID, Status, ArchivedAt string
	Archived               bool
}

func TestArchiveCLIProcess(t *testing.T) {
	sep := slices.Index(os.Args, "--")
	if sep < 0 {
		return
	}
	path, args := os.Args[sep+1], os.Args[sep+2:]
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var s archiveFixture
	if err := json.Unmarshal(b, &s); err != nil {
		panic(err)
	}
	log, err := os.OpenFile(path+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	fmt.Fprintln(log, strings.Join(args, " "))
	switch args[0] {
	case "--version":
		fmt.Println(s.Version)
	case "inspect":
		for _, a := range s.Agents {
			if a.ID == args[1] {
				if a.Archived {
					switch s.Mode {
					case "false-success":
						a.Archived = false
						a.ArchivedAt = ""
					case "wrong-id":
						a.ID = "another-agent"
					case "no-time":
						a.ArchivedAt = ""
					case "bad-time":
						a.ArchivedAt = "invalid"
					case "inspect-error":
						os.Exit(1)
					}
				}
				json.NewEncoder(os.Stdout).Encode(a)
				os.Exit(0)
			}
		}
		os.Exit(1)
	case "archive":
		if s.Mode == "hang" || s.Mode == "loop" {
			for {
				if s.Mode == "loop" {
					fmt.Fprintln(log, "page 0")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		for start := 0; start < len(s.Agents); start += 200 {
			fmt.Fprintf(log, "page %d\n", start)
			for i := start; i < min(start+200, len(s.Agents)); i++ {
				if s.Agents[i].ID == args[1] {
					if s.Agents[i].Status == "running" {
						fmt.Fprintln(os.Stderr, "currently running")
						os.Exit(1)
					}
					s.Agents[i].Archived = true
					s.Agents[i].ArchivedAt = "2026-01-01T00:00:00Z"
					b, _ := json.Marshal(s)
					if err := os.WriteFile(path, b, 0600); err != nil {
						panic(err)
					}
					os.Exit(0)
				}
			}
			if s.Version == "0.11.4" {
				break
			}
		}
		os.Exit(1)
	default:
		os.Exit(1)
	}
	os.Exit(0)
}

func archiveCLI(t *testing.T, version, mode string) (CLI, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "records.json")
	s := archiveFixture{Version: version, Mode: mode}
	for i := range 450 {
		s.Agents = append(s.Agents, archiveRecord{ID: fmt.Sprintf("agent-%03d", i), Status: "idle"})
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	bin := filepath.Join(dir, "paseo")
	script := "#!/bin/sh\nexport GORACE=atexit_sleep_ms=0\nexec " + quote(exe) + " -test.run=^TestArchiveCLIProcess$ -- " + quote(path) + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return CLI{Bin: bin}, path + ".calls"
}
func archiveCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestArchiveCLIRequiresFixedVersion(t *testing.T) {
	for _, version := range []string{"0.11.4", "0.12.0", "0.13.0-beta.1", "garbage"} {
		t.Run(version, func(t *testing.T) {
			cli, log := archiveCLI(t, version, "")
			if _, err := cli.Run(context.Background(), "archive", "agent-425"); err == nil {
				t.Fatal("unsupported archive succeeded")
			}
			if strings.Contains(archiveCalls(t, log), "archive ") {
				t.Fatal("unsupported CLI mutated")
			}
		})
	}
}
func TestArchiveBeyondTwoPagesIsVerifiedIndividually(t *testing.T) {
	cli, log := archiveCLI(t, "0.13.2", "")
	f, _ := newTestFactory(t, card("agent-225", nil), card("agent-425", nil))
	f.p = cli
	for _, id := range []string{"agent-225", "agent-425"} {
		if err := f.archiveAgent(id); err != nil {
			t.Fatal(err)
		}
		if !f.agent(id).archived() {
			t.Fatal("missing archive mark")
		}
	}
	calls := archiveCalls(t, log)
	for _, id := range []string{"agent-225", "agent-425"} {
		if strings.Count(calls, "inspect "+id+" --json") != 2 {
			t.Fatalf("missing individual verification: %s", calls)
		}
	}
	if !strings.Contains(calls, "page 200") || !strings.Contains(calls, "page 400") {
		t.Fatalf("lookup did not paginate: %s", calls)
	}
}
func TestArchiveRejectsUnverifiedSuccess(t *testing.T) {
	for _, mode := range []string{"false-success", "wrong-id", "no-time", "bad-time", "inspect-error"} {
		t.Run(mode, func(t *testing.T) {
			cli, _ := archiveCLI(t, "0.13.0", mode)
			f, _ := newTestFactory(t, card("agent-425", nil))
			f.p = cli
			if err := f.archiveAgent("agent-425"); err == nil {
				t.Fatal("unverified success accepted")
			}
			if f.agent("agent-425").archived() {
				t.Fatal("unverified archive marked")
			}
		})
	}
}
func TestArchiveCLISelectionAndCancellation(t *testing.T) {
	old, oldLog := archiveCLI(t, "0.11.4", "")
	fixed, fixedLog := archiveCLI(t, "0.13.2", "")
	old.ArchiveBin = fixed.Bin
	if _, err := old.Run(context.Background(), "inspect", "agent-425", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Run(context.Background(), "archive", "agent-425"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(archiveCalls(t, oldLog), "archive") || !strings.Contains(archiveCalls(t, fixedLog), "archive agent-425") {
		t.Fatal("wrong executable selected")
	}
	for _, mode := range []string{"hang", "loop"} {
		cli, log := archiveCLI(t, "0.13.2", mode)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		start := time.Now()
		_, err := cli.Run(ctx, "archive", "agent-425")
		cancel()
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("%s: cancellation lost: %v", mode, err)
		}
		if !strings.Contains(archiveCalls(t, log), "archive agent-425") {
			t.Fatal("deadline test never reached archive")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("CLI was not killed promptly")
		}
	}
}
func TestAutomaticArchiveDoesNotStopFreshRunningAgent(t *testing.T) {
	f, p := newTestFactory(t, card("c1", nil))
	p.status["c1"] = "running"
	if err := f.archiveStoppedAgent("c1"); err == nil {
		t.Fatal("running agent accepted")
	}
	if p.called("archive", "c1") || p.called("stop", "c1") || f.agent("c1").archived() {
		t.Fatal("running agent touched")
	}
}

// Keep the same CLI execution path while shortening its production deadline in tests.
type archiveDeadline struct{ Paseo }

func (p archiveDeadline) Run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	return p.Paseo.Run(ctx, args...)
}
func TestArchiveTimeoutNeverMarksSuccess(t *testing.T) {
	for _, mode := range []string{"hang", "loop"} {
		cli, log := archiveCLI(t, "0.13.2", mode)
		f, _ := newTestFactory(t, card("agent-425", nil))
		f.p = archiveDeadline{cli}
		if err := f.archiveStoppedAgent("agent-425"); err == nil {
			t.Fatal("timed out archive accepted")
		}
		if f.agent("agent-425").archived() {
			t.Fatal("timed out archive marked")
		}
		if !strings.Contains(archiveCalls(t, log), "archive agent-425") {
			t.Fatal("deadline test never reached archive")
		}
	}
}
func TestArchiveDryRunNeverInvokesArchiveExecutable(t *testing.T) {
	cli, log := archiveCLI(t, "0.11.4", "")
	cli.ArchiveBin = filepath.Join(t.TempDir(), "does-not-exist")
	f := New(Config{Home: t.TempDir(), StateDir: t.TempDir(), DryRun: true}, cli)
	f.SetAgents([]Agent{card("agent-425", nil)})
	if err := f.archiveAgent("agent-425"); err != nil {
		t.Fatal(err)
	}
	if got := archiveCalls(t, log); got != "inspect agent-425 --json\n" {
		t.Fatalf("unexpected dry-run calls: %s", got)
	}
	if !f.agent("agent-425").archived() {
		t.Fatal("dry run did not simulate archive")
	}
}

// The version check runs once per executable: two archives make one --version call, and a
// replaced executable is checked again (and refused if it no longer qualifies).
func TestArchiveVersionCheckedOncePerExecutable(t *testing.T) {
	cli, log := archiveCLI(t, "0.13.2", "")
	for _, id := range []string{"agent-001", "agent-002"} {
		if _, err := cli.Run(context.Background(), "archive", id); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(archiveCalls(t, log), "--version"); n != 1 {
		t.Fatalf("--version ran %d times for two archives, want 1", n)
	}

	// replace the executable with one reporting an unsupported version
	fixture := strings.TrimSuffix(log, ".calls")
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var s archiveFixture
	json.Unmarshal(b, &s)
	s.Version = "0.11.4"
	b, _ = json.Marshal(s)
	if err := os.WriteFile(fixture, b, 0600); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(cli.Bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli.Bin, append(script, "# replaced\n"...), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Run(context.Background(), "archive", "agent-003"); err == nil {
		t.Fatal("replaced executable with an unsupported version archived")
	}
	calls := archiveCalls(t, log)
	if n := strings.Count(calls, "--version"); n != 2 {
		t.Fatalf("--version ran %d times after the executable changed, want 2", n)
	}
	if strings.Contains(calls, "archive agent-003") {
		t.Fatal("unsupported CLI mutated")
	}
}

// A failed archive forgets the check: the next archive runs --version again.
func TestFailedArchiveRechecksVersion(t *testing.T) {
	cli, log := archiveCLI(t, "0.13.2", "")
	if _, err := cli.Run(context.Background(), "archive", "agent-001"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Run(context.Background(), "archive", "no-such-agent"); err == nil {
		t.Fatal("archive of an unknown agent succeeded")
	}
	if _, err := cli.Run(context.Background(), "archive", "agent-002"); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(archiveCalls(t, log), "--version"); n != 2 {
		t.Fatalf("--version ran %d times, want 2 (once more after the failure)", n)
	}
}
