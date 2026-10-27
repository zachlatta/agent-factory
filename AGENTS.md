# Agent instructions

Read `README.md` first.

## This repo is public

Everything that lands here is published, and a push cannot be taken back. Never commit:

- secrets of any kind: tokens, keys, passwords, cookies, recovery codes;
- hostnames, tailnet names, IP addresses, home-directory paths;
- names of private repos (repo lists and how each deploys live in `~/.config/agent-factory/lanes.json`);
- email addresses (other than GitHub noreply addresses), thread, message or session ids;
- quotes from Zach, card or incident history, or any personal fact.

Incident history and evidence go in the private work item that asked for the change, not here.
Write comments and docs as plain rules ("a dropped card stays dropped"), not as stories.

Host settings (board URL, default repo, private scan patterns, PATH) live in
`~/.config/agent-factory/` on the host, never in the repo.

## Commits and landing

- Commits must use a GitHub noreply email (`git config user.email`); the scan rejects others.
- `install.sh` sets `core.hooksPath=.githooks`: the pre-commit and pre-push hooks run `bin/scan`.
  Never use `--no-verify`.
- Land with `bin/land` (fetch, rebase, scan, vet, test, push `HEAD:main`). On the factory board,
  wait for the factory to tell you to land: it scans your commits itself and lands the card
  once the code review, told the repo is public, approves and that scan passes.
- A scan hit means the change is not published. Remove the value and rewrite your own unpushed
  commits (`git rebase -i`, `git commit --amend`); a later commit that deletes it is not enough,
  since the push would publish the earlier commit too.
- If something private is ever pushed: tell Zach at once, rotate any credential, and fix forward.
  Treat it as exposed even after it is removed.

## Code

- Go, test-first: write the failing test, then the change. `go vet ./...` and
  `go test -race ./...` pass before every commit.
- Do all work in this worktree, never in `/tmp` or another temp directory. Put transient scratch
  (JSON payloads, downloads, one-off helper scripts) in `.factory-scratch/`, listed in
  `.git/info/exclude` (`git rev-parse --git-path info/exclude`) before first use; never commit or
  force-add `.factory-scratch/`, and delete `.factory-scratch/` before landing or finishing so
  ignored files do not block worktree retirement. Never run commands from inside that folder; keep
  your working directory at the worktree root and delete scratch from there using its literal
  absolute path, without variables, substitutions, globs or ~.
- Never combine cd and git in a compound command (including cd X && git); use git -C X.
  For rm, use only literal absolute paths and no cd earlier in the same command.
- The secret scan (`factory/scan`, `.gitleaks.toml`, `.githooks/`, `bin/`) is reviewed as
  security code: a change that weakens it is a blocking finding.
