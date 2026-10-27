# Workspace retirement

`factory retirement-inventory` prints a read-only JSON inventory of local Paseo
workspaces. `-home directory` selects a different home containing `.paseo` records,
including a fixture home. No factory service or browser is required. Output includes
local paths and agent identifiers: keep it in private storage, outside public repos.

The command reads all persisted agent records without a board age cutoff or a CLI
page limit. It reads workspace records, including archived records, and Git metadata.
It never fetches, archives, stops resources, runs setup or teardown commands, removes
files, or deletes branches. It has no apply mode and emits no allowlist entries.

Exit 0 means the requested reads completed, **not** that retirement is safe.
Exit 1 means evidence is incomplete; inspect `problems`. Exit 2 means invalid
arguments. Per-workspace `checks` use these states:

| State | Meaning |
| --- | --- |
| observed | A limited read established the stated fact at inventory time. |
| blocked | A protection applies, conflicting evidence exists, or required evidence is absent. |
| unknown | This reader cannot establish the required fact. |

All four `operations` are currently `refused`; `executorEnabled` is false and
`allowlist` is null. An observed check never grants authority. Missing live-state,
resource or scheduler checks remain unknown, even if persisted records look idle.
Completion labels alone are insufficient evidence of a completed result.

## Optional live observations

`factory retirement-inventory -live` also reads the selected local daemon. The
default remains persisted-only. `-home directory` selects both the record home and
its `.paseo` daemon home; it never combines fixture records with the default daemon.
Live mode requires the inspected CLI and reachable local daemon version 0.11.4.
Unavailable, mismatched or unsupported versions leave checks unknown and exit 1.
There is no automatic upgrade, repair or remote-host fallback.

Live mode runs only these additional reads:

- `paseo --version`
- `paseo status --json --home <daemon-home>`
- `paseo terminal ls --all --json --home <daemon-home>`
- `paseo script ls --workspace <exact-id> --json --home <daemon-home>` for each
  persisted workspace, including archived records.

Terminal matches use exact workspace IDs **or** overlapping canonical paths,
including terminals attributed to another workspace. Running configured scripts
and services also block potentially overlapping folders. Sibling records stay
intact. The command never starts or stops a script, setup, terminal or agent.

Supplemental `observations` include source, scope, start/end times, completeness
and matching terminal or running-script count. Completeness applies only to the
stated source scope at that read, never to the workspace's overall safety. Script
projections cannot attest complete configuration or runtime coverage, so their
`complete` stays false even for a parsed response. Names, script commands and
raw CLI diagnostics are omitted. Failed, malformed, null, unsupported or paginated
responses remain unknown and add a problem; an empty array is not permission.

Persisted reads have a two-minute deadline. Live reads get a separate budget after
that phase: two minutes for startup and global reads, plus 21 seconds per persisted
workspace. Each CLI process still has a 20-second timeout and one second to close
its output pipes. `live.deadlineAt` reports the overall live deadline. A large
inventory therefore has time for every sequential workspace query instead of
sharing the persisted reader's fixed deadline. Script-read failures are reported
as one count; cancellation or timeout still leaves unread checks unknown.

An empty terminal list observes only the absence of matching daemon terminals at
that moment. Services without a positive blocker remain unknown. Agents, active
users, setups, arbitrary processes, mounts and scheduler ownership still require
complete readers from their owners. Hook checks retain their existing limited
configuration scope. Reads are sequential snapshots, not an atomic inventory or a
lease. The fingerprint still covers the original persisted evidence and checks;
supplemental live observations are not approval evidence.

For the inspected version, `exclusion` is blocked: its published API offers no
retirement lease spanning preflight, mutation and verification. Its internal
archiving marker is status bookkeeping; workspace archive can stop resources and
run teardown commands. Neither is a substitute for exclusion. Other versions
remain unknown until their API is separately verified. All operations stay refused.

## Ownership and protection

Match the workspace record to its canonical path, Git common directory, registered
worktree, current branch and HEAD. Names and generated slugs are not ownership.
A renamed branch that disagrees with a stored record blocks retirement until the
ownership records are reconciled by their owner. Never rename a branch for cleanup.

Protect primary checkouts, protected branches, anchors, parked cards, unfinished or
unlanded work, active users, and unknown ownership. Include agents using an overlapping
directory even when they have a different workspace ID. `local_checkout` records
are recognized primary checkouts, and agents in `error` remain unfinished. These
protections block retirement locally without invalidating the whole inventory.
Overlapping workspace records block exclusive directory ownership. Archived records
remain relevant to ownership.
Tracked changes, untracked files and any ignored files block removal.
The future executor removes a leftover root `.factory-scratch/` only after exclusive-use
authorization and ownership checks, and before the final Git cleanliness check. Scratch
cleanup must remain confined to that directory and must not follow symlinks outside it
or delete tracked content. The read-only inventory never performs this cleanup.

Completed-card candidates require independent result evidence and fresh landing proof.
Reviewers require an applied verdict and a completed target; their archive cannot
authorize deletion of their target's workspace. Routed pickers require a recorded
successful handoff. A picker directory record can eventually be archived by its own
ID only after its owned agents and resources are safe; its shared folder stays intact.
Other users of that folder retain their records and resources.

Scheduled runs have an external owner. Their idle or archived state is not completion
proof. They remain protected until separately reviewed work establishes isolation,
completion evidence, scheduling references, and exactly one retirement owner.

## Allowlist contract for the later executor

`RetirementPermit` defines an explicit operation permission bound to `workspaceId`,
`canonicalPath`, `commonDir`, `branch`, `head`, the evidence `fingerprint`, and
`expiresAt`. Each entry authorizes exactly one operation: `agent-archive`,
`workspace-archive`, `filesystem-remove`, or `branch-remove`. A separately reviewed
executor must validate operation names, expiry, schema version, approval provenance,
and exact identity. It must reject missing, stale, changed or ambiguous evidence.
The fingerprint is a change detector over selected persisted evidence and checks,
not a signature, approval or proof against concurrent use.

Before every mutation, reread ownership and completion evidence and obtain current,
complete inventories of agents, active users, services, terminals, setups, processes,
mounts, scheduler references and teardown configuration. Unknown means refusal.
Establish exclusion against concurrent workspace use; if the daemon cannot provide
that exclusion, leave deletion disabled. A preflight snapshot alone is insufficient.
Never stop users or resources to manufacture eligibility. Never execute teardown
hooks, including hooks that appeared after inventory generation.

Require a successful fetch from the intended remote and prove the recorded HEAD is
an ancestor of the fetched landing ref. Local remote-tracking ancestry is only an
observation and cannot replace fetched proof. Do not use the existing landing
helper's stale-ref fallback for retirement.

Record and verify these outcomes independently, retaining partial results for retries:

1. **Agent archive:** individually confirm each approved agent's archived state.
2. **Workspace archive:** confirm that exact record is archived. A successful RPC
   alone does not establish archive or disk removal.
3. **Filesystem:** confirm an owned worktree was removed and unregistered, or confirm
   a directory record's shared folder still exists. Do not run a fallback removal.
4. **Branch:** recheck ownership, fetched landing proof and use by every worktree;
   attempt only `git branch -d`, then confirm the ref is absent. Report Git's refusal.

Never move a shared checkout, force a ref, force-delete a branch, prune unrelated
worktrees, or broaden an operation to a directory-wide archive. A retry cannot infer
permission from a missing directory, archived record, or previous partial success.

## Ordered implementation

This inventory is the first stage. Existing agent archive, reviewer retirement,
picker handoff and waiting-card parking remain in their current paths because this
stage adds observation only. They do not authorize workspace or branch deletion.

The executor is separate reviewed work, following this inventory and the completed
agent archive pagination correction, plus supported concurrent-use exclusion from
separately reviewed upstream work. It must use the corrected supported archive
path rather than introducing another one. Enabling scheduled-run retirement follows
both the executor and separate scheduler-owner work. Until both stages are built,
the lifecycle remains incomplete and unattended deletion stays disabled.
