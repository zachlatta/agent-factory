# Workspace exclusion before retirement

This is a proposed upstream contract, not an available API or permission to
retire a workspace. No daemon, client, host setting or executor changes here.
The [retirement inventory](workspace-retirement.md) remains observation only;
all retirement operations remain refused and execution remains disabled.

A workspace is the folder in which an agent edits files and runs commands.
Finishing one agent does not prove that nobody else uses its folder. A safety
check followed by deletion leaves time for another user to enter the folder.
Exclusion must prevent that new use from before authoritative preflight until
after verification. It must also account for work already underway.

The proposed strict archive changes one workspace record. Removing its folder
or branch is separate, more demanding work. Completing the daemon changes below
alone still leaves folder and branch removal refused: outside actors need
separately proven host enforcement.

## Source baseline and current behavior

The authoritative public source is [getpaseo/paseo][upstream]. The source map below
uses commit `8419c212`, not a moving branch. The comparison baseline is published
version 0.11.4, with the v0.13.2 archive implementation also inspected. Neither a
newer CLI nor a successful connection supplies missing server capabilities.

The inspected [protocol][messages] exposes `archive_workspace_request` with
`workspaceId` and `requestId`. It has no retirement reservation, fencing token or
strict no-stop/no-hook archive policy. The [client][client] sends that request
through `archiveWorkspace`. Existing subscriptions and `archivingAt` status do
not supply exclusion across preflight, mutation and verification.

The ordinary [archive service][archive] resolves its target, stops setups when
that dependency is supplied, and then marks records as archiving. It archives
agents, kills terminals and archives workspace records. For an owned worktree
it can run teardown commands and remove the backing directory. Several errors
are logged while processing continues. Its result separates record archive from
directory removal, but success is not evidence of this proposed safety contract.

The [worktree deletion helper][worktree] can run teardown, use forced Git worktree
removal, fall back to recursive removal, and prune worktrees. It cannot implement
the proposed strict operation. Passing an empty teardown list would only suppress
one behavior; it would not establish exclusion, completeness or safe removal.

[Automatic archive after merge][merge] calls ordinary archive machinery after
selected merge, dirty-tree, ahead-of-origin and ownership checks. The existing
name `archiveIfSafe` does not mean absence of concurrent users, resources or hooks.
Do not confuse it with the distinct proposed `archiveIfUnused` operation.

## Identity and conflict rules

A retirement reservation binds all of these values together:

- Exact workspace ID and durable record generation, including ownership facts.
- Canonical workspace path, backing root and filesystem object identity.
- Registered Git worktree and its administrative directory.
- Canonical Git common directory and its filesystem identity.
- Full branch ref, current HEAD and the expected worktree-to-branch association.
- Daemon instance generation, reservation generation, owner and expiry.
- Required coverage profile and configuration/evidence generations.

Names, titles, generated slugs and lexical path prefixes are not identity.
Use component-aware canonical comparisons for equal, ancestor and descendant
paths. Resolve aliases and links before admission; detect replacement or retargeting
before acting. Include archived workspace and agent records in identity resolution.
An unresolved path, detached or ambiguous branch, missing registry association,
record/Git disagreement or unreadable common directory causes refusal.

Two operations conflict if any of these apply:

1. They name the same workspace ID, regardless of the supplied path.
2. Their canonical paths overlap, even with different workspace IDs or projects.
3. They affect the reserved Git common directory or its metadata.
4. They use or change the reserved branch through any worktree or alternate path.

The initial implementation may conservatively serialize use sharing a common
directory. It must not narrow that scope without a separately reviewed proof of
which Git operations can run concurrently. Git's own short-lived lock files do
not reserve a repository for the full retirement sequence.

A conflicting sibling is a blocker, not an additional retirement target.
Never archive a sibling, force a branch change or broaden the requested operation
to resolve a conflict. Preserve primary checkouts, protected branches, anchors,
parked workspaces and unfinished or unlanded work under the existing rules.

## Admission and reservation lifetime

All names and methods in this section are proposals. The supported protocol,
SDK and CLI must negotiate them explicitly before Factory can call them.

### Acquire before authoritative preflight

Preliminary discovery can locate a candidate, but cannot authorize mutation.
Acquisition resolves the scope and atomically closes admission against conflicting
operations. It revalidates scope after identity resolution under the same
coordinator. There must be no interval between conflict checking and registration
of the reservation in which another operation can enter.

Every use path acquires an admission ticket before asynchronous work begins and
keeps it until that work, callbacks and resource publication have finished.
Pending starts, queued mutations, rollback, background archive and directory
removal count as admitted work. Acquisition refuses if conflicting work is in
flight. It does not cancel or wait indefinitely for that work, stop resources,
disconnect users, or run cleanup to make the workspace eligible.

An archive already running prevents a grant until it finishes and its actual
outcome is reconciled. Existing loaded provider runtimes, user attachments and
resources block even when their agent's saved status says idle or archived.
Detached descendants and failed or uncertain closes remain blockers.

After closing admission, obtain complete, fresh inventories under the reservation.
If any source is incomplete or any blocker exists, refuse further operations.
Release only after proving that no authorized operation is still in flight.

### Proposed public operations

| Operation | Required behavior |
| --- | --- |
| Acquire | Return an owner-bound token, fencing generation, exact scope, expiry and coverage; refuse conflicts or unsupported coverage. |
| Inspect | Read reservation, coverage, blockers and operation outcomes without loading providers or starting resources. |
| Renew | Extend a still-valid reservation only for the same owner and scope; never revive an expired token. |
| Verify | Recheck identity, evidence and individual outcomes while conflicting admission stays closed. |
| Release | Finish only after operation completion and reconciliation; invalidate the token before reopening admission. |

Every mutation validates current owner, generation, expiry, scope, authorization
and evidence immediately before its irreversible step. A token is not a general
bypass for workspace access. An authorized operation receives a narrow ticket for
its exact operation and expected state transition. Internal Git work and proof
fetches need the same treatment; unrelated ref or filesystem writes stay blocked.

Use monotonic deadlines within a daemon lifetime and a durable generation across
restart. Bind tokens to an authenticated principal and keep them out of logs and
ordinary workspace records. Another user, connection or daemon cannot assume the
owner's authority by knowing the workspace ID or request ID.

### Crash, expiry and uncertain outcomes

Persist the exclusion/recovery state before exposing a grant or starting a
mutation. Initialize recovery blocks before accepting requests, starting jobs or
running reconciliation. A restart invalidates old tokens and does not imply that
old operations, processes or descendants stopped.

Expiry or disconnect prevents new mutations. It must not interrupt a committed
step halfway or reopen admission while that step might still be running. Keep a
recovery block until an explicit supported reconciliation proves the outcome and
accounts for outstanding resource tickets. A newer token cannot overlap an older
operation merely because the older token expired.

Failed verification retains the block and reports the last proven state. No retry
may infer permission from an archived record, absent directory, absent branch or
previous success response. Release and expiry recovery require bounded, testable
rules; no automatic stop, kill, hook execution or forced cleanup is allowed.

## Daemon use-path coverage

Enforce at shared runtime, registry and mutation boundaries, not only in client
handlers. Old clients, agent tools and background callbacks must encounter the
same coordinator. Pure stored-state reads may remain available only if they
cannot load a provider, launch a process, mutate Git or trigger a callback.

Paths below are relative to `packages/server/src/` at the pinned source baseline.
The same rule applies to exact workspace, overlapping path and Git conflicts.
Each row needs its own entry-point test and shared-boundary test.

| Entry point / source | Required fence and preflight accounting |
| --- | --- |
| `server/session.ts`: workspace archive | Reject conflicting ordinary archive before setup stop, archiving markers, agents, terminals, record writes or hooks. |
| `server/agent/tools/paseo-tools.ts`: agent MCP workspace archive | Reject at the tool and shared archive boundary; a tool call cannot inherit retirement authority from its parent agent. |
| `server/worktree/commands.ts`: legacy worktree archive | Fence both path and exact-workspace variants; resolving a path must not widen scope. |
| `server/auto-archive-on-merge/archive-if-safe.ts` | Count merge-triggered archive already underway; refuse new archive under exclusion even when Git checks pass. |
| `server/agent/create-agent-lifecycle-dispatch.ts` | Fence auto-archive after completion, failure or cancellation, and failed-agent-create worktree cleanup. |
| `server/schedule/service.ts` | Fence scheduled-run finalization and interrupted-run cleanup after restart, including failure paths. |
| `server/bootstrap.ts` | Install coordinator/recovery before services start; fence external archive callbacks, scheduler cleanup and Hub lifecycle wiring. |
| `server/hub/daemon-executions.ts` | Fence Hub execution archive and failed-create cleanup through their callbacks. |
| `server/session.ts`: project removal | Fence `archiveWorkspaceContents` and record writes even though this path does not call `archiveByScope`. |
| `server/workspace-reconciliation-service.ts` | Fence direct registry archive for missing folders; expected owner removal must not be mistaken for unrelated disappearance. |
| `server/session/workspace-provisioning/workspace-provisioning-service.ts` | Fence failed-import record removal and restored/upserted identity; rollback is still a mutation. |
| `server/paseo-worktree-service.ts`, `utils/worktree.ts` | Fence failed-creation rollback, direct deletion helper, forced Git removal, recursive fallback and pruning. |
| `server/workspace-archive-service.ts` | Fence `archiveByScope`, `archiveWorkspaceContents` and persisted-record archive independently before side effects. |
| `server/workspace-registry.ts` | Require scoped mutation tickets for archive, remove, update and upsert; include indirect mutation listeners. |
| `server/agent/agent-loading.ts`, `server/agent/agent-manager.ts` | Fence create, persistence resume and implicit loading before provider creation; do not treat history reads as inherently inert. |
| `server/agent/create-agent/`, lifecycle and session agent handlers | Fence import, fork, send, refresh, reload, rewind and unarchive; account for pending initialization. |
| `server/schedule/service.ts`, Hub and plugin agent creation | Fence new agents, queued turns, heartbeats and restored work below transport handlers. |
| `terminal/`, `server/terminal-*`, session terminal handlers | Fence create, restore, attach and input; existing PTYs and attachments block acquisition. |
| `server/session/workspace-scripts/`, `server/workspace-setup-runtime.ts` | Fence services, scripts, setup and retries before launch; never stop them for strict archive. |
| Service proxy, health monitor, managed process and plugin process boundaries | Fence lazy start/restart and descendant creation; existing or unaccounted resources block. |
| `server/session/files/`, `server/file-explorer/service.ts`, uploads | Fence create, write, rename, duplicate and delete against canonical overlap; nested workspace roots also matter. |
| `server/session/git-mutation/`, checkout handlers and worktree creation | Fence branch checkout/rename, commits, fetches and shared metadata changes, including background Git writers. |
| `server/session/project-config/`, configuration reload and hook/skill installation | Fence effective configuration writes and hook changes; invalidate evidence for affected workspaces. |
| `server/session/workspace-recovery/`, provisioning and project directory creation | Fence restore, registration, alias changes and directory rollback. |
| Plugin lifecycle hooks and workspace mutation listeners | Prevent executable callbacks during strict archive; an event emitted after a record write is still a possible side effect. |

### Archive and removal caller audit

The pinned source has ordinary archive callers in session, agent tools, legacy
worktree commands, automatic merge archive, agent lifecycle dispatch and bootstrap.
Bootstrap supplies archive callbacks to schedules and Hub execution. These call
the common service but still need tests for their distinct admission timing.

Project removal calls archive contents directly. Reconciliation archives through
the registry directly. Import rollback removes records directly. Fencing only
`archiveByScope` would miss all three. Registry mutation listeners and runtime
teardown called by session/bootstrap must be classified before strict archive
can use any existing persistence helper.

Directory removal reaches `deletePaseoWorktree` through ordinary archive or
`rollbackCreatedPaseoWorktree` after failed worktree creation. The helper reaches
forced Git removal, recursive removal and prune. File-explorer deletion can remove
a nested workspace even though it rejects its own explorer root. Project-directory
rollback uses `rmdir`; clone failure cleanup removes a staging directory. All need
path admission if their targets overlap a reserved workspace.

Other filesystem cleanup sites include temporary Git indexes, config staging,
uploads, skill synchronization, plugin data, managed-process records, provider
artifacts, model downloads and observer canaries. These are not permission to
remove workspace backing roots. Classify their actual target paths and shared
configuration effects: a disjoint private cache is outside the workspace scope;
an overlapping target or hook/configuration dependency must be fenced. Test
helpers are not production entry points. Uncontrolled plugin/provider code belongs
in the outside-actor threat model unless its access is actually mediated.

Before implementing or advertising support, repeat the source-wide audit on the
chosen release for callers and aliases of archive, registry remove/archive,
worktree rollback/deletion, filesystem removal and Git worktree/ref mutation.
Trace callback injection as well as direct calls. New or unclassified callers
block the capability. The map is a baseline, not a claim about later releases.

## Complete evidence under exclusion

Each source must report scope, freshness, completeness, generation and a bounded
failure result. An empty list, successful RPC, saved status or completion label
does not establish completeness. Missing pages, unsupported readers, malformed
responses, unavailable providers and permission errors cause refusal.

| Source | Required evidence |
| --- | --- |
| Agents | All stored and loaded agents, internal/provider children, pending starts/resumes and descendants, matched by identity or conflict scope. |
| Users | Daemon sessions, editor integrations, terminal consumers and outstanding resource handles; absence of daemon sessions says nothing about outside users. |
| Terminals | All live/restoring PTYs, attachments and associated processes, including terminals attributed to another workspace. |
| Services and setups | Complete configured and runtime state, pending/lazy starts, retries, health restarts and background commands. |
| Processes | Managed processes and descendants plus supported host evidence for other processes; idle provider processes still count. |
| Mounts | Relevant mount points, bind/namespace aliases and changes to availability or filesystem identity. |
| Schedulers | Exact references, pending executions and cleanup ownership; idle/archived runs are not sufficient. |
| Hooks/configuration | Effective teardown/setup commands, Git hooks/config includes, plugin lifecycle handlers and executable mutation callbacks. |

Read effective configuration from every relevant scope, including shared repo,
worktree, user/global config and configured hook paths. Cover config indirection,
symlinks, executable replacement and plugin reload. Hashes detect changes but do
not prevent them; admission or host enforcement must close that race. Unreadable
or unenumerable hook sources cause refusal rather than an assumption of no hooks.

Refuse if an applicable hook is configured, even if it is expected to do nothing.
Never run a hook to test eligibility, run setup to clear a block, stop resources,
or detach users to manufacture a safe candidate. An unrelated non-executable
transport notification may be allowed only after proving it cannot invoke hooks,
load providers, mutate evidence or start resources.

Revalidate configuration and resource generations before every mutation and after
it. The approved operation's own expected record transition must be explicit;
all other changes invalidate its evidence. Do not reuse the existing inventory
fingerprint as a signature, lease or proof of complete live coverage.

## Proposed supported strict archive

`archiveIfUnused` is a new operation with explicit capability negotiation. It
must not be an optional field that an older server can ignore. Unknown capability,
unsupported coverage or schema mismatch returns refusal; there is no fallback to
ordinary archive, legacy worktree archive, recursive removal or direct record edits.

The request must bind:

- Exact workspace ID and expected identity/record generation.
- Reservation token, fencing generation and negotiated coverage profile.
- Current evidence generation and operation-specific approval provenance.
- Owner-bound idempotency key and expected archive state transition.

The operation validates all inputs, reservation and complete evidence atomically
with its record write. It archives only that record. Other agent records must
already satisfy separately approved agent-archive outcomes; it cannot silently
archive them. Loaded agents block, including loaded runtimes for archived records.

It refuses users, terminals, services, setups, relevant processes, mounts,
applicable hooks, ambiguous ownership and changed or incomplete evidence. Where
outside activity can invalidate these conditions, absent host coverage also means
refusal. Record archive is not evidence that a directory is unused or safe to delete.

It performs no stop, kill, runtime close, setup, teardown, executable lifecycle
callback, sibling archive, folder removal, worktree prune or branch deletion.
Use a persistence transaction whose callback behavior is known, not a wrapper
around ordinary archive with some dependencies omitted. Return an explicit outcome
and storage-only verification data without invoking existing teardown listeners.

Idempotency is scoped to principal, workspace, exact request and generation. A
different request using the same key fails. Store the outcome so a lost response
can be inspected without replaying effects. A retry after expiry or restart can
inspect its old result, but any new mutation needs fresh authority and evidence.

Refusal leaves resource and record state unchanged. A post-commit response failure
is not refusal: report or recover the committed outcome through the operation
record. Never report success for an unverified archive or hide partial failure.

## Separate filesystem and branch operations

The [existing permit contract](workspace-retirement.md#allowlist-contract-for-the-later-executor)
continues to authorize one operation at a time. A reservation adds concurrency
protection; it does not replace approval, ownership, completion or landing proof.

Keep `agent-archive`, `workspace-archive`, `filesystem-remove` and `branch-remove`
separate. This proposal implements none of them in Factory. Filesystem and branch
operations need separately reviewed supported implementations and continuous host
coverage; `archiveIfUnused` cannot imply them.

Before removal, require a successful fetch from the intended remote and prove
the exact recorded HEAD is landed on the fetched landing ref. Recheck tracked,
untracked and ignored content, all worktree/branch references and ownership under
exclusion. Never use stale-ref fallback, forced removal, unrelated prune or an
automatic recursive-removal fallback. Scratch cleanup retains the separate
confinement and authorization rules in the retirement contract.

Verification stays inside the reservation and records independent outcomes:

1. Each approved agent is individually archived without an unaccounted runtime.
2. The exact workspace record has the expected archived state and generation.
3. An owned worktree is removed and unregistered, or a shared folder is preserved.
4. A separately approved branch removal uses only `git branch -d`; verify absence
   and preserve/report Git's refusal.

Expected absence after a proven operation is distinct from unexplained absence.
Partial results persist for inspection and retries; they never broaden authority.

## Outside actors and host-enforcement gate

Paseo cannot make direct shell, editor, Git, another daemon or privileged process
access safe merely by refusing its own requests. A process can enter a folder,
open files, change a branch or create a mount after a snapshot. Untracked children,
remote filesystems and other mount namespaces widen this limitation.

Advisory locks, process scans, mount scans and repeated cleanliness checks alone
cannot prevent those races. A proposed host mechanism must mediate entry and writes
continuously, account for existing access, cover aliases and shared Git metadata,
and remain effective through verification and recovery. Unsupported platforms,
privileges, filesystems, actors or namespaces mean incomplete coverage and refusal.
Do not claim protection against privileged bypass without an enforceable boundary.

Host gate H below is mandatory for destructive retirement. The nine daemon cards
alone leave directory and branch removal disabled. They may provide a strict
record-only operation, but that operation also refuses any case whose safety
conditions require unavailable outside-actor coverage. No caller may interpret a
record-only outcome as filesystem authority.

The host investigation must test an enforceable mechanism on isolated data and
state its threat model, required privileges, process/mount coverage, crash behavior,
rollback and performance limits. If feasibility cannot be demonstrated, retain
record-only scope where provable and refuse destruction indefinitely. No risk
acceptance or operator checkbox substitutes for exclusion.

## Independently reviewed implementation cards

These are conditional briefs, not started cards or implementation authorization.
Every estimate includes tests and must stay below 1,000 changed lines. Split a
card before building if the actual scope approaches that bound. Keep partial
primitives internal and the supported capability unavailable until coverage passes.

Paths in this table are relative to the future Paseo implementation repository.
Each brief must be refreshed against its chosen baseline before implementation.

| Card | Files/areas and acceptance boundary | Estimated lines |
| --- | --- | --- |
| 1. Identity | New exclusion identity module, path utilities, workspace registry readers; exact ID, overlap, common directory, branch and replacement tests. | 600–850 |
| 2. Coordinator | New admission/reservation and durable operation state; bootstrap recovery; atomic acquisition, fencing, expiry, owner and restart tests. | 650–900 |
| 3. Agent execution | Agent loading/manager, create/lifecycle, scheduler and Hub start boundaries; implicit history loads, pending resumes and queued starts. | 600–900 |
| 4. Users/resources | Terminal runtime, scripts, setup, proxy/health and managed-process boundaries; attachments, restores, lazy starts and descendants. | 650–900 |
| 5. Files/Git/config | Session file, Git mutation, checkout, project config, recovery and provisioning boundaries; aliases, branch/config changes and rollback. | 600–900 |
| 6. Evidence | Complete resource/configuration readers and coverage profile assembly; missing scopes, pages, host coverage and unknown sources refuse. | 550–850 |
| 7. Strict archive | New strict persistence operation and archive fences across the caller map, including tools, bootstrap, automatic cleanup and reconciliation; zero-stop/zero-hook tests. | 650–900 |
| 8. Supported API | Protocol schemas/capability, client methods and CLI; typed refusals, no legacy fallback, idempotent outcome inspection and version tests. | 600–900 |
| 9. End-to-end proof | Isolated fake-provider daemon tests for every matrix entry, cross-client races, rollback, restart and verification. | 650–900 |
| H. Host feasibility | Separate host-enforcement design and isolated proof; supported actors/platforms, privileges, failure recovery and implementation split. | 400–650 |

Cards 1–2 precede guarded use paths; cards 3–7 establish the complete daemon
coverage behind an unavailable capability. Card 8 exposes it only when that
coverage and its tests are complete; card 9 is required before a supported release.
H may run alongside design. A successful H produces separately reviewed host
implementation cards below the same line limit, in an explicitly approved writable
repository. H itself changes no live host or service.

The strict operation and ordinary archive must remain distinct in intent, while
both obey the same exclusion coordinator. Existing Factory agent archive, parking,
reviewer retirement and picker handoff are unchanged; they confer no workspace
or branch deletion authority. This is not a Factory archive refactor.

## Repository, rollout and final enablement gates

The public design belongs here. Future daemon work requires a separately selected,
writable repository; upstream source inspection does not establish push authority.
No implementation brief may silently assume that a fork exists or that upstream
will accept or release a change.

Recommended route: separately approve a public writable fork and offer reviewed
changes upstream. Alternatives are upstream PRs while waiting for their release,
a private maintained fork, or deferring implementation. Fork creation, credentials,
upstream submissions and maintenance ownership require their own concrete proposal.

Recommended rollout route: separately review an upgrade to a released supported
version. A maintained 0.11.4 backport or leaving the daemon unchanged are separate
choices. Select exact source/release versions, test compatibility and protect
running work before deployment. A new API in source or a newer client is not a
deployed capability. Specify rollback and verify capabilities after rollout.

The decisions above are deferred gates, not questions that must be answered to
write this document. Host H must also resolve whether enforceable outside-actor
coverage is feasible. If it is not, keep folder and branch removal disabled.

The ordering for eventual destructive retirement is:

1. Approve this contract and independently reviewed implementation destination.
2. Complete daemon coverage, strict archive and supported API verification.
3. Complete H and its reviewed host implementation, or refuse destruction.
4. Separately approve and verify the daemon and host rollout with rollback.
5. Complete scheduler ownership/handoff and the separately reviewed executor.
6. Review final Factory enablement against actual deployed capabilities and every
   existing permit/protection rule. No earlier step enables retirement implicitly.

Executor, schedule ownership and direct-daemon integration work consume this
contract but cannot bypass its gates. A supported archive release alone is
insufficient for final enablement. This document has no deploy steps.

## Acceptance and review checks

The implementation acceptance suite uses temporary fixture homes, repositories,
fake providers and test-only daemons. Never exercise mutation paths on a live
daemon. No UI change is required by this design card.

- For every matrix row, race entry against acquisition and hold it pending before
  acquisition. Exactly one admission wins; no granted reservation overlaps use.
- Explicitly test agent MCP tool archive, both legacy worktree command scopes and
  bootstrap callbacks, not only the common archive function.
- Test merge-triggered archive, agent terminal-state cleanup, failed creation,
  import rollback, scheduled-run finalization and restart cleanup before and
  after acquisition. No callback is exempt because it runs without a client.
- Test direct registry reconciliation and project removal. An unexplained missing
  folder cannot turn into successful retirement or erase required evidence.
- Test same ID/different path, different ID/same path, parent/child paths, symlinks,
  replaced directories, common-directory aliases and branch use in other worktrees.
- Test pending provider initialization, archived-but-loaded runtimes, terminal
  restore, attached users, health restarts, descendants and lazy script starts.
- Test partial inventories, missing pages, unavailable resources, malformed data,
  unreadable hooks and unsupported host coverage. Unknown always refuses.
- Insert or replace hooks/configuration during each phase. Assert no setup,
  teardown, Git hook, executable plugin callback or mutation listener runs.
- Assert strict refusal leaves records, folders, branches and resources unchanged:
  zero stops, kills, runtime closes, forced commands and sibling changes.
- Test old clients against a guarded server and new clients against an old server.
  Missing capability or coverage never selects ordinary archive as a fallback.
- Test wrong-owner tokens, wrong scope, stale generations, expiry, disconnect,
  restart during mutation, duplicate requests, lost responses and partial outcomes.
- Verify separate archive, folder and branch outcomes while admission stays closed.
  Failed verification retains the recovery block rather than reporting completion.
- H must demonstrate shell/editor/Git entry and mount races being prevented under
  its stated threat model, or mark those cases unsupported and refused.

For this documentation change, verify source links and caller classification,
review public-data safety, and run the repository secret scan, `go vet ./...` and
`go test -race ./...` before committing. Future cards use upstream unit, type and
isolated integration checks appropriate to their actual implementation. Approval
of this design is not evidence that those future tests or capabilities exist.

[upstream]: https://github.com/getpaseo/paseo
[messages]: https://github.com/getpaseo/paseo/blob/8419c212274d60792934e53145d6abdcbadc0d91/packages/protocol/src/messages.ts
[client]: https://github.com/getpaseo/paseo/blob/8419c212274d60792934e53145d6abdcbadc0d91/packages/client/src/daemon-client.ts
[archive]: https://github.com/getpaseo/paseo/blob/8419c212274d60792934e53145d6abdcbadc0d91/packages/server/src/server/workspace-archive-service.ts
[worktree]: https://github.com/getpaseo/paseo/blob/8419c212274d60792934e53145d6abdcbadc0d91/packages/server/src/utils/worktree.ts
[merge]: https://github.com/getpaseo/paseo/blob/8419c212274d60792934e53145d6abdcbadc0d91/packages/server/src/server/auto-archive-on-merge/archive-if-safe.ts
