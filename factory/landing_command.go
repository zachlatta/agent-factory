package factory

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// labelLandRun is the bin/land run an agent started, as "<pid>.<start>": bin/land sets it on its own
// agent before any fetch, scan, test or review, and stops without landing if it cannot. <start> is
// `ps -o lstart=` in the C locale and UTC with each run of spaces and colons made one "_". It is not
// carried to a replacement (replace.go lists what is): a new agent runs its own landing.
const labelLandRun = "land-run"

// landRunMax bounds how long one bin/land run holds its card: a run that hangs longer is judged as
// if it had exited, so a forgotten or wedged process cannot keep a card in Landing.
const landRunMax = 2 * time.Hour

var landRunRe = regexp.MustCompile(`^([1-9][0-9]*)\.([A-Za-z0-9_]+)$`)

// landRunLive reports whether marker names a bin/land process that is running now: the PID is alive,
// started at the recorded time (so a reused PID does not count), runs bin/land, and started less
// than landRunMax before now. Anything else, including a failed or unavailable ps, is not live.
func landRunLive(marker string, now time.Time) (bool, string) {
	m := landRunRe.FindStringSubmatch(marker)
	if m == nil {
		return false, "no run recorded"
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return false, "bad pid"
	}
	start, err := psField(pid, "lstart")
	if err != nil {
		return false, "process gone"
	}
	if got := runStart(start); got != m[2] {
		return false, "pid " + m[1] + " is another process (started " + got + ")"
	}
	if args, err := psField(pid, "args"); err != nil || !strings.Contains(args, "bin/land") {
		return false, "pid " + m[1] + " is not bin/land"
	}
	at, err := time.Parse("Mon Jan 2 15 04 05 2006", strings.ReplaceAll(m[2], "_", " "))
	if err != nil {
		return false, "unreadable start " + m[2]
	}
	if now.Sub(at) > landRunMax {
		return false, "running since " + at.Format(time.RFC3339) + ", past the limit"
	}
	return true, ""
}

// runStart normalizes ps lstart output as bin/land records it.
func runStart(lstart string) string {
	return strings.Join(strings.FieldsFunc(lstart, func(r rune) bool { return r == ' ' || r == ':' || r == '\t' }), "_")
}

// psField is ps's field for pid, in the C locale and UTC; an exited pid is an error.
func psField(pid int, field string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", field+"=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", os.ErrNotExist
	}
	return s, nil
}

// landRunning reports whether landing card a's own bin/land run is still going, so its idle turn
// (a progress line, or a command left running) is not the landing's result yet.
func (f *Factory) landRunning(a Agent) bool {
	live, why := landRunLive(a.L(labelLandRun), time.Now()) // ps start times are the host's clock
	if !live && a.L(labelLandRun) != "" {
		f.logf("%s land run %s not running: %s", short(a.ID), a.L(labelLandRun), why)
	}
	return live
}
