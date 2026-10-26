# Workspace retirement

`factory retirement-inventory` prints a read-only JSON inventory of local Paseo
workspaces. `-home directory` selects a different home containing `.paseo` records,
including a fixture home. No factory service or browser is required. Output includes
local paths and agent identifiers: keep it in private storage, outside public repos.

The command reads all persisted agent records without a board age cutoff or a CLI
page limit. It reads workspace records, including archived records, and Git metadata.
It never fetches, archives, stops resources, runs setup or teardown commands, removes
files, or deletes branches. It has no apply mode and emits no allowlist entries.

Exit 0 means the persisted inventory was read, **not** that retirement is safe.
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

## Ownership and protection

Match the workspace record to its canonical path, Git common directory, registered
worktree, current branch and HEAD. Names and generated slugs are not ownership.
A renamed branch that disagrees with a stored record blocks retirement until the
ownership records are reconciled by their owner. Never rename a branch for cleanup.

Protect primary checkouts, protected branches, anchors, parked cards, unfinished or
unlanded work, active users, and unknown ownership. Include agents using an overlapping
directory even when they have a different workspace ID. Overlapping workspace records
block exclusive directory ownership. Archived records remain relevant to ownership.
Tracked changes, untracked files and any ignored files block removal.

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
agent archive pagination correction. It must use the corrected supported archive
path rather than introducing another one. Enabling scheduled-run retirement follows
both the executor and separate scheduler-owner work. Until both stages are built,
the lifecycle remains incomplete and unattended deletion stays disabled.
