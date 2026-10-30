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

The proposed [upstream exclusion contract](workspace-exclusion.md) defines the
required daemon coverage and strict record-only archive operation. It is design
only: destructive retirement also requires separately proven host enforcement.

This inventory is the first stage. Existing agent archive, reviewer retirement,
picker handoff and waiting-card parking remain in their current paths because this
stage adds observation only. They do not authorize workspace or branch deletion.

The executor is separate reviewed work, following this inventory and the completed
agent archive pagination correction, plus supported concurrent-use exclusion from
separately reviewed upstream work. It must use the corrected supported archive
path rather than introducing another one. Enabling scheduled-run retirement follows
both the executor and separate scheduler-owner work. Until both stages are built,
the lifecycle remains incomplete and unattended deletion stays disabled.

## Standalone schedule evidence consumer

Generic v1 consumer support is inactive: no CLI, board, inventory or executor calls
it. The production owner adapter always reports unavailable. Configuration cannot
activate acceptance. Scheduled runs retain all existing protections.

The standalone validator requires exact receipt, declaration, outcome, identity,
manifest and evidence fields. Missing, duplicate or unknown keys and unsupported
versions refuse. Only `recordKind=success`, independently successful outcomes and
`retirementAuthorized=false` qualify. Declaration and manifest digests must match;
digests are change detectors, never authentication. Non-Git identity uses null for
all Git fields. Unknown result kinds or acceptance requirements refuse.

Commit evidence requires the intended canonical local remote or unauthenticated
HTTPS URL and full landing ref. Each validation fetches into an isolated proof
repository and proves the run HEAD remains an ancestor of the fetched tip. Normal
tip advances need no receipt rewrite; failed fetches and rewritten history refuse.
Durable outputs must match the exact declared set of stable, digest-checked regular
files outside the disposable workspace. Symlinks and changed files refuse.

The internal release projection covers v1's exact scheduler, schedule, run, agent,
workspace, path and Git identities, ownership generation, receipt digest, approved
declaration provenance, release time and expiry. It defines no external scheduler
wire format. A later reviewed adapter must authenticate these bindings, establish
`scheduler-owned -> released-for-Factory -> Factory-acknowledged`, check current
revocation and complete scheduler references, and hold exclusion through acceptance.
An agent assertion or schema-valid receipt cannot provide those guarantees.

The standalone policy reader requires `FACTORY_SCHEDULE_RECEIPT_MAX_AGE` and
`FACTORY_SCHEDULE_ACK_STORE` from approved host configuration outside the repo.
Neither has a default; the running service does not yet consume these settings.
Receipt age starts at `verifiedAt`; future timestamps, invalid ordering and expiry
at the boundary refuse. Release expiry also bounds acceptance. Executor integration
must additionally honor permit expiry and recheck evidence immediately before each
operation. Revalidation never refreshes the immutable receipt's age.

The acknowledgment store must already exist, be canonical, privately owned and
inaccessible to other users, and lie outside the workspace and Git common directory.
The integration owner must additionally protect it from run agents and provide
local-filesystem locking, durability, retention and backups. Claims serialize across
processes, publish without replacement, fsync before acknowledgment and retain exact
identity, generation and receipt digest. Replay, conflicting ownership, overlapping
paths, unknown state and I/O failures refuse. Expiry, restart, lost replies and
partial publication never return authority automatically; preserve and reconcile.
Do not delete records to retry or infer authority from partial archive outcomes.

A handoff never grants a RetirementPermit or overrides protected work. Parked cards,
anchors, primary/shared checkouts and unfinished work remain protected. Per-operation
approval, fetched landing proof, complete resource and scheduler inventories and
supported concurrent-use exclusion remain mandatory. Unknown means refusal.

## Integration readiness and activation proposal

This section is the enablement gate for schedule handoffs. It is a proposal,
not permission to accept a release or retire a workspace. The standalone consumer
has no production call sites; `scheduleUnavailableOwner` refuses acceptance, and
there is no retirement executor. Standalone tests exercise validation and local
claims with test owners. They do not prove an authenticated scheduler transfer,
deployed exclusion or successful retirement. Factory integration and genuine
end-to-end verification remain unfinished until the prerequisites below land.

### Readiness evidence

Before starting integration, record the implementation owner, reviewed source
revision, supported version, deployment verification and evidence location for each
row in a private readiness record. A design, passing unit test or unverified source
revision does not establish a deployed capability. Missing evidence blocks the
corresponding gate; do not replace missing implementations with permissive adapters.

| Prerequisite | Evidence required before integration or activation |
| --- | --- |
| Producer | Landed receipt producer and approved declaration provenance; supported v1 fixtures generated by that implementation for each proposed result kind. |
| Scheduler authority | Landed authoritative release implementation and authenticated adapter; exact bindings, revocation, complete references and exclusion through acceptance as specified above. Name who owns release recovery. |
| Executor | Separately reviewed operation call sites implementing the allowlist contract above, with fresh evidence checks and independent outcome inspection. Acceptance must not mint permits. |
| Exclusion | Supported daemon API and enforced host coverage, with verified deployment and rollback under the [exclusion rollout gates](workspace-exclusion.md#repository-rollout-and-final-enablement-gates). Design briefs alone do not qualify. |
| Claim storage | Verified isolation from run agents, local locking and durability, retention, backup and recovery. Prove that a lost reply or partial write cannot transfer ownership twice. |
| Integration | Separately reviewed wiring from authenticated release through durable acknowledgment to the executor, with the isolated proof below and all existing protections preserved. |

The producer and scheduler owners supply their authoritative source records; the
Factory integration owner verifies compatibility and records the exact versions
actually tested. Do not infer upstream completion from a receipt's schema or an
agent's report. Implementation destination, release and host feasibility decisions
remain governed by the exclusion document, without a second rollout path here.

### Isolated integration proof

Run against disposable workspaces, a dedicated scheduler scope, a private test
claim store and a test remote. Use the real reviewed producer, scheduler adapter,
executor and exclusion implementations. Fake providers may supply disposable run
results; a fake owner or exclusion adapter cannot demonstrate authority transfer.
No live schedule, user workspace or production resource may be a test target.

Capture exact revisions, capability versions, configuration scope, expected and
observed outcomes, and cleanup verification in private evidence. Acceptance refusals
must leave resources intact and preserve any already-published claim. Operation
failures must retain verified partial outcomes for reconciliation. Test at least:

| Check | Required result |
| --- | --- |
| Authenticated handoff | Observe scheduler-owned, released-for-Factory and Factory-acknowledged in order, with identical identity, generation, receipt digest and declaration provenance. Scheduler cannot reuse the workspace after transfer. |
| Forgery and rebinding | Untrusted credentials, agent assertions and altered identities, paths, generation, receipt or provenance refuse without acknowledgment. |
| Revocation and references | Revoke before acceptance and race revocation with acceptance. Scheduler serializes the transition; revoked or still-referenced work cannot be accepted. Incomplete reference coverage refuses. |
| Replay and concurrent claims | Replay a release and race separate Factory processes over the same run and overlapping paths. At most one claim publishes; losers refuse and cannot mutate. |
| Restart and lost reply | Crash before publication, after publication and before reply, then restart both owners. Recover durable state without duplicate transfer, automatic release or deletion of claim records. |
| Storage faults | Inject failed locks, writes, publication and fsync, plus malformed or partial state. Refuse uncertain outcomes and reconcile retained state before retry. |
| Time boundaries | Exercise receipt age, future times, release expiry and permit expiry at the exact boundary, including expiry during a long operation. Revalidation never renews the receipt. |
| Changed evidence | Change output files, fetched history, workspace identity or ownership between checks. Refuse stale evidence before mutation. |
| Protected work | Parked cards, anchors, primary/shared checkouts, unfinished or unlanded work, overlapping records and unknown ownership remain protected even with a valid handoff. |
| Concurrent use | Attempt agent, user and resource starts and file/Git changes during preflight, mutation and verification. Required exclusion coverage holds; missing capability or coverage refuses. |
| Independent outcomes | Verify agent archive, record-only workspace archive, filesystem removal or shared-folder preservation, and branch removal separately. Inject partial failure and prove retries need fresh authorization for each remaining operation. |

Also verify no teardown hooks run, no resource is stopped to create eligibility,
no fallback removal occurs and branch deletion uses only the permitted safe path.
The exclusion document owns the full daemon and host coverage matrix; this proof
must reference its passing evidence rather than reduce that matrix to these cases.
Passing standalone consumer tests cannot satisfy this gate. Record unavailable
checks as unrun, with their missing prerequisite, never as successful simulations.

### Narrow activation for separate approval

After readiness and isolated proof pass, submit a concrete private activation
record. Review the integrated code separately before asking to activate it. This
public proposal contains no real schedule names, identities, receipts or settings.
The private record must specify:

- One exact schedule and scheduler authority, its approved declaration and result
  kind, and the exact reviewed/deployed versions and passing proof evidence.
- An explicit allowlist of operation names and target identities. No wildcard,
  directory-wide scope, inferred operation or automatic addition of later runs.
  Issue each permit separately under the existing allowlist contract.
- One preselected disposable completed run for the initial trial. Other runs of
  that schedule remain protected; future runs require a fresh approval.
- A maximum one-hour activation window with an absolute start and end, no automatic
  renewal, and a named operator able to stop admission and reconcile outcomes.
  Receipt, release and permit expiry may shorten this window, never lengthen it.
- Private claim-store location, retention and recovery procedures, monitoring
  destination, and who checks acknowledgment and each operation's verified outcome.
- The precise admission control and rollback procedure supplied by the reviewed
  integration. Verify it can stop new acceptance and mutation without erasing claims
  or allowing the scheduler to reclaim a workspace whose ownership is uncertain.

Recommend approval only for that bounded trial after every gate passes. Approval
of this document, a declaration or a release is not activation approval. Record the
separate approval against the exact scope and reviewed revision; any change to the
schedule, operations, versions or window requires review and renewed approval.

During the trial, observe each transition and operation before proceeding. Stop
new admission and mutation immediately on identity mismatch, stale evidence,
revocation, incomplete inventories, lost exclusion, expiry, claim-store uncertainty,
unexpected resource effects or an unverifiable outcome. Preserve evidence and
partial results; do not force deletion, clear claims or return ownership on a timer.
An in-flight operation must follow the executor's reviewed interruption/recovery
protocol; a stop cannot pretend to undo an irreversible completed operation.

At the window's end, disable further acceptance and mutation, inspect scheduler
and Factory ownership together, and reconcile all claims and independent operation
outcomes. Verify test resources were cleaned up only within their permits and that
protected resources remain intact. Unknown outcomes stay blocked for investigation.
Publish no private evidence in this repository. A successful trial authorizes no
broad rollout; expanding scope needs its own proposal and separate approval.
