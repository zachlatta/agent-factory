package factory

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startLandRun starts a real process with bin/land's command name and returns its marker.
// exec keeps one PID, so stopping the fixture leaves no sleeping child behind.
func startLandRun(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "exec -a bin/land sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd, runMarker(t, cmd.Process.Pid)
}

// runMarker builds pid's marker with the same pipeline bin/land uses.
func runMarker(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", fmt.Sprintf(
		"LC_ALL=C TZ=UTC ps -o lstart= -p %d | tr -s ' :' '__' | sed 's/^_//;s/_$//'", pid)).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("ps for %d: %v", pid, err)
	}
	return fmt.Sprintf("%d.%s", pid, strings.TrimSpace(string(out)))
}

func TestLandRunLiveOnlyForThatExactProcess(t *testing.T) {
	cmd, marker := startLandRun(t)
	if live, why := landRunLive(marker, time.Now()); !live {
		t.Fatalf("running bin/land %q not live: %s", marker, why)
	}
	pid := cmd.Process.Pid
	// the same PID with another start time is a reused PID, not this run
	if live, _ := landRunLive(fmt.Sprintf("%d.Thu_Jan_1_00_00_00_1970", pid), time.Now()); live {
		t.Error("a reused PID reads as the landing run")
	}
	// a live process that is not bin/land (this test) does not count, even with its own start time
	if live, _ := landRunLive(runMarker(t, os.Getpid()), time.Now()); live {
		t.Error("a process other than bin/land reads as the landing run")
	}
	// a run older than landRunMax no longer holds the card
	if live, _ := landRunLive(marker, time.Now().Add(landRunMax+time.Minute)); live {
		t.Error("a run past landRunMax still reads as live")
	}
	for _, bad := range []string{"", "x", "0.Wed_Oct_28_22_05_52_2026", "-1.a", pid2s(pid), "12;34.a"} {
		if live, _ := landRunLive(bad, time.Now()); live {
			t.Errorf("malformed marker %q reads as live", bad)
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	if live, _ := landRunLive(marker, time.Now()); live {
		t.Error("an exited run reads as live")
	}
}

func pid2s(pid int) string { return fmt.Sprint(pid) }

// A replacement agent runs its own landing: an earlier agent's run does not carry over.
func TestLandRunIsNotCarriedToAReplacement(t *testing.T) {
	old := card("c0", map[string]string{labelLandRun: "123.Wed_Oct_28_22_05_52_2026"})
	if v, ok := copyCarriedLabels(old)[labelPrefix+labelLandRun]; ok {
		t.Errorf("land run carried to the replacement: %q", v)
	}
	cur := card("c1", map[string]string{labelReplaces: "c0"})
	for _, a := range applyLineageLabels([]Agent{old, cur}) {
		if a.ID == "c1" && a.L(labelLandRun) != "" {
			t.Errorf("land run read through the chain: %q", a.L(labelLandRun))
		}
	}
}
