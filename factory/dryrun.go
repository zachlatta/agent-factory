package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Dry run (Config.DryRun, `factory -dry-run`): the board reads the real Paseo agents, ideas and
// transcripts, and its buttons answer as if they worked, but nothing changes. Only the reads listed
// below reach paseo and git; every other call is logged to the Activity panel and skipped. The
// loop runs no Tick, email is off, and the state directory is a copy (DryRunState).

// paseoReads are the paseo subcommands that only read. Anything else, including a subcommand the
// factory starts using later, counts as a write.
var paseoReads = map[string]bool{"inspect": true, "ls": true, "permit ls": true, "provider ls": true}

func paseoRead(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if len(args) > 1 && paseoReads[args[0]+" "+args[1]] {
		return true
	}
	return args[0] != "permit" && args[0] != "provider" && paseoReads[args[0]]
}

// gitReads are the git subcommands that only read.
var gitReads = map[string]bool{"rev-parse": true, "show": true, "diff": true, "status": true, "cherry": true,
	"reflog": true, "merge-base": true, "log": true, "remote get-url": true}

func gitRead(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "remote" {
		return len(args) > 1 && gitReads["remote "+args[1]]
	}
	return gitReads[args[0]]
}

// dryRunArgs shows args for the log: in full, except message and prompt bodies (long or multi-line
// arguments), which show as their length.
func dryRunArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 80 || strings.Contains(a, "\n") {
			a = fmt.Sprintf("<%d chars>", len(a))
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

type dryRunPaseo struct {
	inner Paseo
	logf  func(format string, args ...any)
	runs  atomic.Int32
}

func (d *dryRunPaseo) Run(ctx context.Context, args ...string) (string, error) {
	if paseoRead(args) {
		return d.inner.Run(ctx, args...)
	}
	note := ""
	if len(args) > 0 && args[0] == "archive" {
		note = " (the card hides on this board for 10 minutes; Paseo is unchanged)"
	}
	d.logf("dry run: would paseo %s%s", dryRunArgs(args), note)
	if len(args) > 0 && args[0] == "run" {
		// a placeholder id, so the flow that started it can finish
		return fmt.Sprintf("dry-run-%d\n", d.runs.Add(1)), nil
	}
	return "", nil
}

// dryRunGit wraps git and gitOut so that only reads run.
func (f *Factory) dryRunGit(git func(context.Context, string, ...string) error,
	gitOut func(context.Context, string, ...string) (string, error)) (func(context.Context, string, ...string) error,
	func(context.Context, string, ...string) (string, error)) {
	skip := func(dir string, args []string) {
		f.logf("dry run: would git -C %s %s", dir, dryRunArgs(args))
	}
	return func(ctx context.Context, dir string, args ...string) error {
			if gitRead(args) {
				return git(ctx, dir, args...)
			}
			skip(dir, args)
			return nil
		}, func(ctx context.Context, dir string, args ...string) (string, error) {
			if gitRead(args) {
				return gitOut(ctx, dir, args...)
			}
			skip(dir, args)
			return "", nil
		}
}

// passive reports whether the loop only refreshes: no Tick, no provider diagnostic, no usage probes.
func (f *Factory) passive() bool { return f.cfg.ReadOnly || f.cfg.DryRun }

// uploadDir is where a message's attachments are saved: under ~/.paseo, or in a dry run under the
// (copied) state directory.
func (f *Factory) uploadDir(id string) string {
	if f.cfg.DryRun {
		return filepath.Join(f.cfg.StateDir, "factory-uploads", id[:min(8, len(id))])
	}
	return filepath.Join(f.cfg.Home, ".paseo/factory-uploads", id[:min(8, len(id))])
}

// DryRunState copies the state directory's files into a new temporary directory and returns it, so a
// dry run shows the real ideas and writes only to the copy. A missing state directory gives an
// empty copy.
func DryRunState(dir string) (string, error) {
	tmp, err := os.MkdirTemp("", "agent-factory-dry-run-")
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return tmp, nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), b, 0o600); err != nil {
			return "", err
		}
	}
	return tmp, nil
}
