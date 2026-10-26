package factory

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

// Guarded repos are public: anything that lands there is published. For their cards the factory
// runs the secret scan itself before any landing (from this binary, with origin/main's gitleaks
// config, so a card cannot weaken the scan that checks it). Reviewers are told the repo is public,
// and private data in the diff is blocking. A card lands on its own once its code review approves
// and that scan passes, under the usual tier rules; a scan hit or error stops it for Zach. Their
// ideas wait for Zach's Start, and the direct track is off. Other repos are unaffected.

// guarded reports whether repo is in Config.Guarded.
func (f *Factory) guarded(repo string) bool {
	return repo != "" && slices.Contains(f.cfg.Guarded, repo)
}

const publicCardNote = `
This repo is public: everything that lands here is published. So:
- No direct track: every change is a build-track card. It lands on its own only after both reviews
  and the factory's secret scan pass.
- Never commit secrets, hostnames, IP addresses, email addresses, home-directory paths, thread or
  session ids, quotes from Zach or personal facts, in code, comments, tests, docs or commit messages.
  Keep incident history and evidence in the private work item that asked for the change, not here.
- Never use --no-verify. Land only with bin/land, which scans first; the factory scans again.
`

const publicReviewRule = `
This repo is public. Blocking, whatever the tier: any secret, hostname, IP address, email address,
home-directory path, thread or session id, quote from Zach or personal fact in the diff, the commit
messages or the commit authors; and any change to the secret scan (factory/scan/, .gitleaks.toml,
.githooks/, bin/) that weakens it.`

// guardedLandMsg replaces the landing message for a guarded card.
const guardedLandMsg = "Land it now with bin/land (it scans, tests, rebases and pushes HEAD:main). Land exactly the " +
	"reviewed commits: do not commit anything else first. Never use --no-verify. Then run any deploy steps from your reviewed summary and verify the landing and deploy (if a deploy step fails after the push, stop and report it; do not improvise a fix on the live system), delete .factory-scratch/ if present so ignored files do not block worktree retirement, then stop with " +
	"a one-line summary. " + overviewReminder

// scanCard runs the secret scan on what the card at cwd would publish (origin/main..HEAD). It
// returns a short note: "" when clean, else what was found (rule and place, never the text). An
// error means the scan could not run, which counts as a failure.
func (f *Factory) scanCard(cwd, rng string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if rng == "" {
		if err := f.Git(ctx, cwd, "fetch", "-q", "origin", "main"); err != nil {
			f.logf("fetch in %s failed, scanning against local origin/main: %v", cwd, err)
		}
		rng = "origin/main..HEAD"
	}
	hits, err := f.Scan(ctx, cwd, rng)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}
	var parts []string
	for i, h := range hits {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(hits)-5))
			break
		}
		parts = append(parts, h.String())
	}
	return strings.Join(parts, "; "), nil
}

// scanNote is the Needs Zach note for a guarded card after its scan.
func (f *Factory) scanNote(cwd, rng string) string {
	found, err := f.scanCard(cwd, rng)
	switch {
	case err != nil:
		f.logf("secret scan in %s could not run: %v", cwd, err)
		return "secret scan could not run: " + trunc(err.Error(), 200)
	case found != "":
		f.logf("secret scan in %s found: %s", cwd, found)
		return "secret scan failed: " + found
	}
	return scanPassedNote
}

// scanPassed reports whether a scanNote is the clean one.
func scanPassed(note string) bool { return note == scanPassedNote }

// scanPassedNote has no comma: label values cannot hold one (labelArg).
const scanPassedNote = "public repo: scan passed"

// runScan is the default Factory.Scan: this binary's scanner, the private rules file, and the
// gitleaks config as it is on origin/main (never the card's own copy).
func (f *Factory) runScan(ctx context.Context, dir, rng string) ([]scan.Hit, error) {
	priv, err := scan.LoadPrivate(f.cfg.PrivateRules)
	if err != nil {
		return nil, fmt.Errorf("private rules: %v", err)
	}
	o := scan.Options{Range: rng, Private: priv, Gitleaks: f.cfg.Gitleaks}
	if toml, err := f.GitOut(ctx, dir, "show", "origin/main:.gitleaks.toml"); err == nil {
		tmp, err := os.CreateTemp("", "factory-gitleaks-*.toml")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		tmp.WriteString(toml)
		tmp.Close()
		o.GitleaksConfig = tmp.Name()
	}
	return scan.Run(ctx, dir, o)
}
