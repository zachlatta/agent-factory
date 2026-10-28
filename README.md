# agent-factory

A Kanban board and review pipeline for coding agents run by Paseo. You throw
any request at the start of the queue; an agent plans it, other agents review the plan and the
code, and the work is built and reviewed before you see it. What you asked for comes to you
finished, once, before it lands; what the factory started lands on its own unless it is on the
always-ask list.

One Go binary. It reads Paseo's agent files every 15 seconds and whenever one changes, applies the
rules below, and calls the `paseo` CLI for every move (labels on agents are the state:
`factory.stage`, `factory.tier` and so on, see `factory/labels.go`). It serves the board on
`127.0.0.1:7478`; the deployer puts it on the tailnet with `tailscale serve`.

### Hand-offs and Paseo calls

- **Agent watcher** (`factory/watch.go`, inotify on Linux). When an agent file's status, archive
  time, attention flag or `factory.*` labels change, that agent is acted on within about a second:
  the factory rereads the files (no paseo calls) and applies the rules to that agent first, even in
  the middle of a pass; each agent is handled at most once per pass. Writes that change only
  activity times are ignored. The 15-second poll stays as the fallback, and is all there is when
  the watcher cannot start (not Linux, no `~/.paseo/agents`), which is logged once.
- The loop also wakes when a rule's grace window ends (a reviewer's first minute, a card's 45 s
  after a move) instead of waiting for the next poll.
- Paseo calls stay in order: a card's move is check, labels, send, and moves of different cards
  share the board and labels, so they never run at the same time. Instead, a finished reviewer's or
  Scout's archive waits until the pass's moves are sent.
- `paseo permit ls` runs at most every 15 seconds; reads in between keep the prompts it last found,
  unless an agent's file flags a permission wait it did not see. The archive CLI's version check
  runs once per executable (again if the file changes or an archive fails).
- `/api/health` reports `paseoCalls`: the CLI calls of the last 10 minutes, with count and wall
  time per command.

**Unread** on a card: its agent finished a turn you have not opened. Opening the card or loading its
transcript page marks it seen (`factory.seen`) until the agent is sent another message and finishes
again; a running card is never unread.

The board remembers the Lanes grouping you last picked, in your browser's local storage, and
restores it on the next visit; an unrecognized or unavailable saved value falls back to grouping
by repo.

Every card and idea shows its repo at the start of its grey info line (the lane name, else the
name after `owner/`; the full `owner/name` on hover), so the repo is visible in any grouping and on
the phone.

## How a card moves

Plan → Plan review → Building → Code review → Landing → Done, with **Needs Zach** for anything
that waits on a person.

The planner (the "Fellow") first picks a track:

- **Direct track**: errands that change no code (a question, research, a report). Done in one pass;
  the factory checks the work reached `origin/main`, then moves it to Done.
  Zach's message to an errand card (before its first plan, or after it finished direct) keeps it
  on the direct track: it is not sent to code review, so it never asks to approve landing work it
  already landed. Public repos have no direct track, so their cards still get a code review.
- **Build track**: everything else. The plan ends with `TIER:`; the plan reviewer sets its own and
  the card takes the higher. A code reviewer can raise the tier, never lower it.
- The planner names the card once, from the `TITLE:` line of its first plan or direct result; titles
  given by an idea or the email dispatcher are kept, and other titles are cut at a word. A card's
  worktree starts on a git branch slugged from its initial title (`--new-branch`, with `-2`, `-3`, …
  when a local branch already has that name) and renames the branch when the planner renames the
  card; worktrees started outside the factory (such as email or voice dispatchers) name their own
  branches.

| Tier | What | Gates |
| --- | --- | --- |
| routine | scheduled runs of an approved job | none per run; surfaces only if blocked |
| low | docs, copy, an isolated fix, tests, a small feature on existing patterns | plan review, code review |
| risky | prod data, deletes, migrations, money, auth/secrets, hosts/deploys/cron, messages to others, unattended jobs | plan review and code review both pass |
| architecture | new service/datastore/framework/dependency, shared interfaces, a refactor or cleanup outside the area of the ask, hard to undo or public | plan review, code review; always shown to a person before it lands |

- **No plan approvals.** Agents review plans; a person is asked before building only when building
  has side effects. Every plan and plan review ends with `BUILD SIDE EFFECTS: none`, or what
  building (not landing) would change outside the worktree: prod data written or deleted, messages
  sent, money spent, hosts or services changed. Only a bare none counts (`none`, `None.`, `**none**`;
  also no, n/a or nothing): its reason goes on the next line. Anything else on the line, even a
  reason that sounds safe (`none (code and tests only)`), is read as a side effect
  (`ParseSideEffects`), so no reading of free text can let a plan with real effects skip the person.
  Only when the plan reviewer writes none and the plan lists nothing does the card go to building;
  otherwise (or with no line) it waits in Needs Zach as "Allow this build?", with Allow & build, a
  reply (back to plan review) or Drop. Any tier: the tier does not decide this stop.
- **Finished work** (`factory/alwaysask.go`). After code review passes (and, in a public repo, the
  scan), a card comes to Zach once, before it lands, when any of these holds; otherwise it lands on
  its own:
  - he asked for it: every card except ideas, Scouts and scheduled runs, including email, voice and
    board cards, cards with no source label, and those whose build he allowed;
  - it had no plan review (sent to review by hand);
  - it is architecture tier;
  - the code reviewer's `ALWAYS ASK:` line is not `none` (architecture he did not ask for, a change
    to the factory's own gates or rules, a new service), or the line is missing;
  - the always-ask file check hits.

  It stops at Ready for you (below), which says why it came to him (`factory.ask-why`).
- **Always-ask file check.** The factory lists the files `git diff --no-renames --name-only
  origin/main...HEAD` changes and matches them against built-in patterns (agent rules and gates:
  `AGENTS.md`, `CLAUDE.md`, `.githooks/`, CI workflows, gitleaks config, `.factory.json`; services:
  `deploy/`, systemd units, Dockerfiles and compose files, `Procfile`; dependencies: `go.mod`,
  `package.json`, `pyproject.toml`, requirements files, `Gemfile`, `Cargo.toml`) and the repo's own
  `alwaysAsk` patterns in `.factory.json` (`*` within a name, `**` across directories). The repo's
  file is read from `origin/main`, never the card's copy, so a card cannot loosen the check that
  applies to it, and a card that edits it is always asked. A diff or file it cannot read asks.
  This repo lists its gates there.
- **Ready for you** is the one stop where a person sees finished work before it lands. The builder
  writes three `CHANGED:` lines in plain words and any `BEFORE:` / `AFTER:` screenshots; it starts no
  server and uploads nothing. The factory runs the repo's preview (below) and sends one email:
  subject "Ready to try: <title>", the three lines, the screenshots, a Preview button and the board
  link. The board item shows the same with Land, Change and Drop. Email replies: `land` or `land it`
  lands it; the drop words drop it; letters (`1A`, or `A` for one question) answer the builder's
  questions, cut to the decision email's lengths, and go to it; anything else is a change request,
  which sends the card back to building and code review and then to a new Ready for you
  (`factory.ready` counts them; each has its own note, so its own email). The email waits for the preview to load or fail, at most 10 minutes
  after the builder was asked.
- **Previews** (`factory/tryout.go`, `previews.json`). A repo declares one in `.factory.json`, read
  from `origin/main` only, so a card cannot change what runs on the host before it lands:
  `"preview": {"run": "<command>", "path": "/", "env": {"KEY": "literal"}}`, or
  `"preview": {"sample": "<command>"}` for a repo with no UI (its output, up to 4 KB, is shown
  instead; `run` and `sample` may both be set). With none, Ready for you has the screenshots only.
  - The factory gives each card a port from `FACTORY_PREVIEW_PORTS` (default 7700-7799), at most
    `FACTORY_PREVIEW_MAX` (default 8) at once, oldest wait first; the rest show "too many running"
    and start when one stops.
  - `run` runs through `sh -c` in the card's worktree, in its own process group, with only `PATH`,
    `LANG`, `TZ`, the host's Go cache paths (`go env`), `npm_config_cache`, `HOME` set to
    `StateDir/previews/<card>/home`, `PORT`, `HOST=127.0.0.1` and the declared literals. It must
    listen on `$HOST:$PORT`. Its output goes to `StateDir/previews/<card>/log`; nothing is written
    in the worktree.
  - The factory publishes it with `tailscale serve --bg --https=<port>` on the board URL's host,
    checks `path` loads on 127.0.0.1 and on that link within 2 minutes, and links it; a failure
    shows the last 20 log lines. Every 5 minutes it checks again and restarts a dead one once.
  - It stops when the card leaves Ready for you (landing keeps it): change requested, dropped,
    done, landed or archived. Stopping kills the process group, removes the tailnet entry and the
    preview's folder. Tailnet entries in the range and folders no saved preview owns are removed.
  - The factory's processes stop with it, so after a restart the next Tick starts saved previews
    again on the same port and link. Only Tick starts or stops previews: a `-dry-run` or
    `-tick=false` copy never does, even with a copied `previews.json`.
  - It is not a sandbox: the process runs as the factory's user and could read files by absolute path.
- Changes are whole (`wholeChangeRule`): one way of doing each thing; every caller, test, doc and
  README the old behaviour touched is updated, and no old path is left (or each instance left alone is
  listed with why). Plan and code reviews check it, and a partial change is a blocking finding. Scope
  is the area of the ask: refactoring inside it is part of the job and does not raise the tier; a
  refactor outside it is architecture Zach did not ask for, which the code reviewer's `ALWAYS ASK:`
  line names.
- Plans and build summaries name their deploy steps (if any). Builders make no live changes
  (install scripts, enabling or restarting services, resizing VMs, external settings) before code
  review: the code reviewer checks the steps, and the landing step runs and verifies them after pushing.
- Code review is one reviewer or a panel (`factory/panel.go`). One reviewer, with the whole rubric,
  reviews a change that is not architecture tier, touches no UI and no secret-scan file
  (`factory/scan/`, `.gitleaks.toml`, `.githooks/`, `bin/`) and changes at most 150 lines outside
  `_test.go` files (`git diff --numstat origin/main...HEAD`; when git cannot say, only an
  architecture change gets the panel). Anything else gets a panel, all on the same commit: a
  correctness-and-tests reviewer, an architecture-and-consistency reviewer and, for a UI change, a
  phone UX reviewer (the only one given the UI check). Reviewers carry `factory.lens` and
  `factory.panel` (the round); the card carries `factory.panel` while its panel is open.
  - A panel reviewer's verdict alone never moves the card. Once all have one: if all approve, the
    factory decides with their notes merged under one heading each and the highest tier; otherwise one
    panel arbiter (Opus, `factory.lens=arbiter`) reads their reports, checks each blocking finding
    against the code, and decides the round, so the builder gets one merged list. It decides a
    round, unlike the gate arbiter below, which settles a gate at its round cap or on an escalation. Plan reviews and outside landing reviews stay single.
  - Either way the card moves once per round, and each round counts once toward the cap.
  - A rework round re-runs the correctness reviewer (the code changed), every reviewer that did not
    approve, and the UX reviewer when the fix touched a UI, then the panel arbiter if they still differ.
  - A panel reviewer that ends without a verdict is re-run once on its own; with still none, the
    round has no verdict. A panel arbiter without a verdict is re-run once. Re-run the review replaces
    the whole panel. Reviews started before panels have no lens and decide as single reviews.
- Circuit breakers: 3 review rounds per gate. At the plan-review cap, a last round whose only
  blocking findings the reviewer marks with `SMALL FIX:` goes to Building with those fixes applied
  and verified by code review. Otherwise, at the cap of either gate and on any reviewer's
  `escalate`, an **arbiter** agent (the gate arbiter) rules instead of a person (`factory/arbiter.go`): a fresh Opus
  reviewer (`factory.arbiter`) that reads the builder's conversation and the last review and ends
  with `VERDICT: approve|changes|escalate|close`, `changes` only with `MUST FIX:` lines. On a plan,
  approve builds it and changes builds it with the fixes (code review checks each, as with
  `SMALL FIX:`), both through the build side-effects check above; escalate is a side effect Zach
  must allow. On code, approve goes on to the finished-work rule, changes sends the fixes for one
  more round whose review checks them, and escalate brings the card to Zach with its reason. The
  arbiter runs once per gate per card (`factory.arbitrated`); a gate that still does not settle,
  or a ruling it cannot give, waits on the board (never emailed) as "Reviews didn't settle" with Let
  an agent decide (another arbiter), a reply, or Drop; QUESTION blocks from an earlier brief are not
  offered there, since the arbiter's action carries no answers. Reviewers may answer `nits` or `close`; a reviewer
  that ends or disappears without a verdict is re-run once on its own before the card moves to
  Needs Zach (`factory.no-verdict`); a card that stops with nothing queued goes to Needs Zach after
  2 minutes.
- Provider errors: a working card in error with a recognized quota reset gets one resume
  attempt after that reset plus 30 seconds. The deadline and attempt survive restarts. A second
  error, an unknown reset format, or uncertain delivery goes to Needs Zach with Resume, Reply
  and Drop. Resume and Reply each make one explicit attempt at the interrupted step; neither
  rearms automatic retries. Unknown Codex limit formats require human intervention. An errored
  reviewer sends its card to Needs Zach with Re-run the review.
- Blocked agents: a child agent waiting on a permission prompt (`paseo permit ls`) or running with
  no activity for 20 minutes flags its card ("reviewer waiting on permission", "reviewer stalled N
  min"). Such a reviewer no longer counts as a pending verdict, so its card really moves to Needs
  Zach (its labels change) with "Re-run the review", which retires the old reviewer first. If the
  old reviewer finishes before a re-run, its verdict still moves the card on. The factory never
  answers permission prompts; agents are told how to write `rm` so Claude Code does not ask.
- A plan can name one card it lands after, with a line `Waits on: <short id>`. After code review passes,
  the factory holds the land prompt until that card is Done, then sends it. If that card was dropped
  or closed, or is not on the board (an agent that is not a card counts as dropped once archived), the
  card goes to Needs Zach with one question: land it anyway, reply, or drop it.
- A landing card is Done only if its HEAD is on `origin/main`, it made a commit of its own, and its
  last message ends with `REMAINING: none`. A card that lists work left (`REMAINING:` then one `-`
  line per item), or says nothing, waits in Needs Zach with the list in its note ("Landed, with work
  left" or "Landed: did it finish?", shown on the board and not emailed): reply to send the agent on, or
  mark it Done. REMAINING lists only this card's own request: another card's work, a problem the
  change did not cause (an expired login, an unrelated failing test) and work outside the area of the
  ask go in the summary or become ideas, so they never hold a finished card. Each item starts with who
  must do it, `Zach:` or `agent:`. When the message's `NEEDS FROM ZACH:` says nothing (and names no
  ask), it has no question and no item is Zach's, the work left goes back to the same card instead: the
  agent finishes it in its worktree, new commits get a code review (and, on a card Zach asked for, his
  finished-work stop again), and it lands again. Only the first landing emails Zach. At most 2 such
  rounds (`factory.work-left-rounds`, carried across handoffs and not reset by "Send it on"); after
  that the card waits in Needs Zach with the list. A landing whose commits differ from the reviewed
  ones waits as "Landed something other than what was reviewed".
- Code and landing reviews of a change to frontend files (`factory/uireview.go`) include a UI check,
  given to the panel's phone UX reviewer (or the single reviewer when git cannot list the files):
  the reviewer runs it on test data, served on this host's tailnet IP (`tailscale ip -4`, never
  `0.0.0.0`), and clicks through it with computer use. Its non-blocking "UI feedback:" block goes to
  the builder, who repeats it in the landing summary. In this repo, build the card's binary in the
  worktree's `.factory-scratch/` and run `factory -dry-run -listen "$(tailscale ip -4):0"` (the log names the
  port); stopping it (SIGINT or SIGTERM) deletes the dry run's temp state copy. That meets the
  check's data rule: it reads the real cards, ideas and transcripts, and its buttons only log what
  they would do.
- Drop, Close and dragging to Done are final: the card is stopped and archived, and late verdicts
  for it are not forwarded. Its running reviewers are stopped and archived with it.
- Every code approval, landing approval, landing retry and dependency release checks that HEAD
  is still the commit captured for code review. A changed HEAD queues a fresh code review; a
  failed read, or a public card with no captured reviewed commit, stops without landing. That
  technical stop is board-only and offers Re-run the review, never an approval that skips it.
- An approval for a parked card held on a dependency authorizes one release when that dependency
  lands. The landing message wakes it only after the same review and scan checks. The approval
  follows a replacement card and is cleared on release or cancellation; a stale snapshot cannot
  wake a cancelled card.
- Replaced cards (`factory/replace.go`): when a card hands off to another agent (`factory.replaces=<old-id>`
  on the replacement, `factory.replaced-by=<new-id>` on the retired predecessor), `resolveCardID`
  follows `factory.replaced-by` to the current card across board cards, ideas, `Waits on:` dependencies,
  reviewer `review-of` links, email reply threads (`factory.zach-thread` and `gmail-thread`) and message
  deduplication (`factory.answered` and `factory.told`), timings, token usage, peers, steers, evidence,
  board `#<id>` links, and `/t/<id>` transcripts (which stitch the predecessor turns and a system
  switch divider ahead of the replacement session). Carried lifecycle, gate, and thread labels
  (`carriedFactoryLabels` and `carriedRawLabels`) are both copied at handoff and read through the
  replacement chain so approval, review, and public-repo scan gates survive even a partial handoff.
  Write primitives (`setLabels`, `send`, `stop`, `archiveChecked`) always act on the exact single agent ID.
- A duplicate passes its request on. A reviewer (or the Fellow) that finds a duplicate names the
  card it duplicates on a `DUPLICATE OF: <card id>` line. When Zach closes the card at that close
  verdict, the factory keeps its request and the reviewer's notes (`steers.json`) and sends them to
  the kept card once it is planning or building; an idle one then gets the review for its stage
  again. While the kept card is in review or Needs Zach the steer waits; if it is landing, landed or
  dropped, the request becomes an idea titled "From closed duplicate <id>: ..." (`factory/steer.go`).

### Quiet factory agents

Factory cards, builders, reviewers, repo pickers and Scouts carry a
`paseo.parent-agent-id` label before their first turn. Paseo suppresses finished,
permission and error attention notifications for these agents. Personal agents
started in Paseo keep their notifications; factory requests reach the board and
Needs Zach email instead.

The factory keeps one idle `factory.role=quiet-anchor` agent in a dedicated
workspace under its state directory. A live anchor in another workspace preserves
Paseo workspace status and mark-unread behavior. The anchor's own bootstrap is
quiet. It costs one short provider turn when created and retains an idle runtime;
it is hidden on the board, counted separately by kind in health, and never reaped.
Archiving the anchor detaches cross-workspace cards, so the factory recreates it
and repairs their labels. Launches and follow-ups fail if quiet labeling fails.
Real sub-agent parents are preserved, and quiet labels survive parking/resumption.

Existing factory agents, including externally launched email/voice cards, are
reconciled on each Tick. An external launcher must label cards before starting or
resuming them for silence from the first event: reconciliation cannot prevent a
notification before the factory observes a launch or a removed label. Read-only
and dry-run modes create no anchor or quiet-label writes.

### Idle agents

An idle agent stays loaded in Paseo until it is archived, so every Tick reaps the factory's own
idle agents (`factory/reap.go`), at most 20 archives per Tick:

- A Done card is archived once it has been idle 5 minutes; a reviewer once its verdict is applied.
- A Needs Zach card is **parked** when it has been idle `FACTORY_IDLE_TTL` (default 3 h), or when
  more than `FACTORY_IDLE_CAP` (default 10) factory agents are idle: then the longest idle go
  first, none idle under 30 minutes. Parked means labelled `factory.parked`, then archived: the
  card stays in Needs Zach for 14 days (then it leaves the board, and an email reply no longer
  finds it), and Zach's reply on the board or by email wakes it,
  because `paseo send` unarchives an agent and reloads its session. The factory never wakes a
  parked card on its own. Close, Archive or Done on a parked card drops the park and it is Done.
  Paseo's archive keeps the worktree; nothing may remove a parked card's worktree while it is
  parked.
- Cards in other columns are not parked or counted against the cap: the factory moves them on,
  or to Needs Zach, within minutes. Running agents and agents the factory did not start (email
  agents, schedules, your own sessions) are never touched, only counted.

Archive writes require a stable Paseo CLI version 0.13.0 or newer: earlier versions search only
one page of agents, even for a full ID. `FACTORY_ARCHIVE_CLI` selects a separate executable for
archive writes; other commands use the existing CLI on PATH. Factory checks the selected
executable's version, reads each agent by full ID before archiving, and verifies its archive
timestamp afterward. Automatic archives never stop a running agent. Each CLI call has a
90-second deadline, including paginated lookups; failures leave the agent unmarked for retry.
Dry-run skips writes and simulates the local result.

For an archive-only CLI rollout, install exact version 0.13.2 into a separate versioned prefix
outside the repository, with `npm install --prefix "$archive_prefix" --ignore-scripts --no-audit
--no-fund --save-exact @getpaseo/cli@0.13.2` (one shell command). Do not overwrite the daemon's
package tree or restart it. Check the separate executable's version and read-only paginated
listing against the current daemon before setting `FACTORY_ARCHIVE_CLI` in the host environment
file to its `node_modules/.bin/paseo` executable. The existing deployer then loads that setting
when it deploys the landed change. Verify board health and that the daemon's process did not
restart. Restoring the previous setting rolls back selection; an unsupported CLI refuses
archive writes.

`/api/health` reports live, running, idle and parked agents, per kind and per card (a card's own
agent plus its reviewers and sub-agents), and the last reap; the `/host` page shows them.

Its `workflow` summary reports active factory cards, cards in Needs Zach, held landings, held cards whose
dependency chain reaches Needs Zach, the longest dependency chain, and cards held by
cycles or missing or dropped dependencies. Parked cards remain counted; finished, dropped and
replaced cards do not. These work counts are separate from process health: a dependency hold
does not trigger the deployer's restart or rollback checks.
Only cards with a factory stage label are counted; personal agents shown on the board are excluded.

### Repo and agent per card

For the read-only completed-workspace inventory and the gates required before any
future deletion, see [Workspace retirement](docs/workspace-retirement.md).

- **Repo: Auto.** A read-only picker agent reads the request and names the repo it belongs in
  (`REPO: owner/name`); the card then starts there. If it can't tell, it asks in Needs Zach with
  buttons for its candidates (`factory/route.go`).
- **Agent.** The builder can be any provider Paseo has (`factory/agents.go`: Opus, Codex, Gemini,
  Muse); reviewers are always Opus. Providers that are not available are greyed out. The agent
  pickers (New card dialog and card panel) show each provider's remaining subscription usage (cached
  for 5 minutes; for Claude, every account `cswap` reports is listed by account number and email,
  with the active one marked and no organization names or ids shown). Available choices are ordered
  and auto-suggested by how far each plan's remaining weekly quota is ahead of its 7-day reset pace
  (plans with a 5-hour window at `<= 20%` or an exhausted window rank lower, and a missing reset
  time counts as a full week left; Muse's `server_sent_no_usage` state before the first use in a new
  window counts as 100% left). Board cards with no agent picked and auto-started ideas use this
  auto-suggested agent; cards started from an email thread always default to Opus 5.5 unless
  another agent is explicitly named.

The model pickers prefer GPT-6.1 Sol within OpenAI and Opus 5.5 within Claude; Sonnet 5.5 is
also available. Frontier models (`Frontier: true` in `factory/agents.go`: Opus 5.5, GPT-6.1 Sol
and Gemini 4 Argon) are labeled `· Frontier` in the New Task and Start Idea dropdown options and
allowance rows; Sonnet 5.5, GPT-5.3 Codex Spark and Muse Spark remain unlabeled. Opus, Sonnet
and Sol start at medium thinking. New Task and Start Idea let you choose a supported thinking
level; switching models restores that model's default. Models without an adjustable thinking
level hide the control. Explicit choices survive Repo Auto. The supported levels are listed per
model in `factory/agents.go`, matching Paseo's provider model metadata.

### Sub-agents

A card's agent and its reviewers may hand parts of their work to sub-agents, on any provider Paseo
has available, so Opus can ask Codex, Gemini or Muse and the other way round (`factory/delegate.go`).

- The Fellow and reviewer prompts list, for each available agent choice, the exact
  `paseo run -d -q --provider …` command, its plan, remaining usage and where it sends the work. Run
  from the agent's shell, `paseo run` labels the new agent with `paseo.parent-agent-id` and puts it in
  the caller's workspace, so a sub-agent works in the card's worktree.
- Agents wait with `paseo wait <id> --timeout 100` (inside Claude Code's 120-second shell limit),
  read the answer with `paseo logs <id> --filter text --tail 1`, and archive each sub-agent once
  answered. Before ending any turn they archive every sub-agent they started
  (`paseo ls -g -q --label paseo.parent-agent-id=$PASEO_AGENT_ID`), since Paseo archives children
  only with their parent and a card is archived only once Done.
- At most 6 running at once per agent; no secrets in their prompts. A card's sub-agents edit only
  the files it assigns them and never commit; a reviewer's sub-agents are read-only. An agent's own
  sub-agent tool (Claude's Agent tool, say) is allowed too.
- Email and voice-memo cards get the same section from the factory, not from the dispatcher's
  prompt: one note while the card is in plan, running and under 2 minutes old (checked again with
  Paseo just before sending, since a message interrupts the command a running agent is on and would
  start an extra turn for a stopped one), or else appended to the next message the factory sends it.
  `factory.delegated` marks it sent.
- On the board, sub-agents fold into their card at any depth (a reviewer's own sub-agents too).
  The card shows how many there are and how many run, with a chip (title and model) for each
  running one; the "sub-agents" box shows all of them. The card panel has a row per sub-agent with
  its model, status and last activity, and Open (its transcript), Message, Stop and Archive; reviews
  get rows with Open. A sub-agent on a permission prompt or stalled flags the card as other children
  do; a card agent or reviewer waiting on a working sub-agent is not counted as stalled; and
  "sub-agent still running" flags one left running after the agent that started it stopped.

### Recorded token usage

`GET /api/usage` serves recorded tokens from retained local Paseo sessions, including
archived agents. The first request starts a background scan and returns an empty snapshot
with `refreshing: true`; later requests serve the cached snapshot while a refresh runs,
at most once every five minutes once coverage is complete. While coverage is limited, requests
can start the next bounded slice after 15 seconds. No collection runs without a request, and
concurrent requests share one collection. It makes no agent turns or changes to sessions.

`plans` lists remaining subscription allowances once per provider, using the existing
allowance cache. `rows` groups tokens by plan, provider, model, role, card, agent and recorded
stage. `totals` sums those disjoint rows. Input includes cached input; cache read/write and
reasoning columns are subsets, so total is input plus output. Claude messages are counted
once, Codex cumulative snapshots become increments, and Muse model completions are counted
without their duplicate attribution records. Unsupported providers show unavailable counts.
Claude's built-in sub-agent files are included under their owning agent, card and stage,
even when the main file is missing. Coverage counts include those files and their errors
or scan limits; they use the same per-file and overall budgets as main sessions.

Stage attribution uses the same timing events as the board and metrics. `span` identifies
each visit to a stage, including repeated building visits; `round` identifies a recorded
review round when available. Initial cumulative readings, intervals crossing stage boundaries
and missing/expired timing remain `unassigned`. Timing history is retained for 30 days.
Sessions shared by conflicting agent/card roles also remain unassigned and are flagged.

Coverage reports missing/unsupported sessions, errors, duplicate session references, timing
entries without agent metadata and scan limits. Each refresh parses at most 2,000 new or changed
metadata files and reads at most 32 MiB per session, 128 MiB of session data and 20 seconds in total.
Extracted counters, file identities and read offsets stay cached in memory, without transcript text.
Unchanged sessions need no further byte reads; appends and older unread sessions progress across
refreshes. Larger sessions are read in slices. Limited readings remain flagged and report bytes
read and bytes still pending; their known counts are partial totals. A restart starts collection
again. A process remembers previously observed sources and reports their disappearance
on subsequent complete scans. Restarting forgets that inventory. Totals can shrink after
cleanup; they describe retained local records, not every device's activity or a subscription
bill. Historical subscription account attribution is unknown. No prompts, session contents
or filesystem paths are returned. The board’s Usage link opens `/usage`, which consumes this feed without another collector.
The page separates subscription allowances across devices from retained local tokens, with
filters for provider, plan, model, role, card, agent and recorded stage. Rows show stage spans,
review rounds, session status and links to transcripts currently available on the board.
Filters affect recorded counts only; allowances and coverage remain visible for the snapshot.
Refresh and fetch failures keep the last snapshot visibly dated. Missing or unsupported records
are unavailable, not evidence of zero usage. Cache and reasoning fields remain subsets.


### Ideas

Any agent can suggest work with `POST /api/idea` (header `X-Factory: 1`; `repo`, `title`,
`summary`, `why`, `doneWhen`, `request`, optional `tier`). The factory starts the oldest waiting
idea, at most 2 in flight and 5 a day; a person can Start or Dismiss any idea. Errored cards do not count
toward the in-flight cap; when they resume, the count can temporarily exceed that cap. Titles that repeat
a waiting, dismissed or recently started idea are rejected. "Suggest ideas" on the board starts a
read-only Scout that submits up to 3, guided by the dialog's prompt; with Repo Auto, a picker first
chooses the repo from that prompt, as for new cards. The text in “What should it do?” is passed to the Scout
as guidance; leave it empty for general suggestions. A running Scout shows in the Ideas column, then in Done.
Every Fellow and reviewer is told how to suggest out-of-scope work it finds as an idea (at most 2 per
card) instead of doing it in its card; agents started before this rule do not have it.

### Email and voice-memo cards

An email dispatcher (not in this repo) can start agents as cards with labels
`factory.source=email|voice` and `gmail-thread=<id>`. Those cards follow the same tracks and
report back in their email thread when they land, in plain words (see Needs Zach email). They
never email that work started, stalled or failed: the board shows that.

### Needs Zach overview

Needs Zach cards show the original request, later instructions, the decision needed, optional
before/after images, and choices with Other. The full agent report and operational explanation
are folded into details. Missing original requests are labeled unavailable, with a transcript link.

Agents keep the `DECISION:`, `OVERVIEW:` and `NEEDS FROM ZACH:` block (`factory/overview.go`).
The decision and needs fields describe the concrete choice and its consequence; overview prose
stays in details. Existing `QUESTION:` blocks provide up to four questions, with the recommendation
first. Ordinary approval requires every question to be answered. Let an agent decide carries no
answers and refuses filled answers or instructions: send them through a reply choice. Drop and
Other do not require every question to be answered.
Every action requires an explicit choice, including cards with only one action. Named reply choices
keep their hints and recommendation. Send question answers through a reply choice; completion, retry,
stop and drop refuse to discard filled answers or instructions. Select the action and press Submit once; Other sends typed
instructions without approving. Drop still asks for confirmation.

For visible changes, agents save useful screenshots in the card worktree's `.factory-evidence/`
folder, excluded from Git before saving. Never commit or force-add evidence. After the overview
and a blank line, optional whole lines name the files:

```text
BEFORE: before.png
AFTER: after.png
```

Each key occurs at most once. Filenames contain letters, digits, underscores or hyphens and a
`.png`, `.jpg` or `.jpeg` extension. Images must decode as PNG or JPEG, at most 5 MB and
20 megapixels. Copy captures from another machine into the worktree before referencing them.
For text decisions, show the exact before and after text in the report instead.

Factory serves only the current ask's named images at `/api/evidence/{card}/{before|after}`
under the board's existing access boundary. It rejects symlinks, paths, special files, unsupported
formats and oversized images; it disables caching and content sniffing. The card accepts images
only from these generated routes, including inside report Markdown. No public image service or
external image URL is used. Images become unavailable if their worktree or file is removed.

### Needs Zach email

Zach is emailed only for decisions, by the factory itself through the zrl-bot mailbox
(`pdw call zrl_bot_email_control__*`, `factory/mail.go`), once per wait reason. Each ask on the board
is either emailed or board-only (`Ask.boardOnly`):

- **Emailed:** allow a build with side effects, try finished work before it lands, the secret
  scan's stop, a public-repo push without review, and a repo choice.
- **Board only:** finished cards and cards moved by hand; a running agent (permission prompt, stall,
  writing its summary); errors and usage limits; a stuck step or reviewer; a review with no
  verdict; a review that did not settle even with the arbiter (the arbiter's call, not his);
  a reviewer's close or duplicate verdict; dependency waits; and every check after landing
  (not verified, work left, did it finish, landed diff differs). These are problems for the factory
  and its agents; the board shows them until a later digest lists them.

The email is the decision alone:

- The subject is the question: the agent's `DECISION:` line, or the ask's own question when the
  factory raised the stop or the agent decided nothing.
- The body is under 60 words, not counting the board link: the recommendation first ("I recommend
  yes.", or "I recommend A." with up to 4 lettered options for one question), what it is about,
  the agent's overview as far as the words allow, and the one-word replies. Inline `Before` /
  `After` screenshots from `.factory-evidence/` follow, then an Open board button. The full report
  stays on the board.
- Agent text in it passes through `plainText`: backticks, commit hashes, agent ids, file names and
  the owner of `owner/repo-name` slugs are removed. Factory copy avoids pipeline words (card, track,
  landing, Fellow), and agents are told to write the same way for anything Zach reads by email.

- Inbox cards answer in their own thread. Other cards start one thread to `FACTORY_ZACH_EMAIL` on
  their first wait (`factory.zach-thread`); later waits reply in it, always addressed to
  `FACTORY_ZACH_EMAIL`. If sending with inline screenshots fails, it retries once without them.
- `factory.told` holds the wait already emailed; it is cleared when the card leaves Needs Zach.
  A wait where the builder was just asked for a decision or preview (`factory.writing`) is emailed
  once it has written it (at most 10 minutes later).
- Not emailed either: cards with no thread when `FACTORY_ZACH_EMAIL` is unset. A failed send is
  retried after 5 minutes; a timed-out one is not, since it may have gone out.
- `/api/health` reports the last mailbox check as `mail`; it never fails the health check.

Replies: the email dispatcher checks the sender, then posts his reply to
`POST /api/answer` (`thread`, `message_id`, `text`; 404 when no card has the thread, 409 when none
of its cards waits, or when two or more do: then the reply could be for either, so the dispatcher
passes it on as a follow-up). Quoted text and signatures are cut first. Only a whole reply from these lists
acts:

| Reply | Does |
| --- | --- |
| `approve`, `approved`, `yes`, `y`, `ok`, `okay`, `lgtm`, `go`, `go ahead`, `ship it` | the ask's Yes button, when it is approve or let an agent decide; on an approve with questions, it also answers each with its recommended (first) option |
| `done`, `mark done`, `mark it done` | Mark done, when the ask has it |
| `drop`, `drop it`, `close`, `close it`, `cancel` | Drop, when the ask has it |
| `land`, `land it` | Land, on Ready for you (there, only these and the drop words act; anything else is a change request) |
| `1B 2A` (only numbers and letters of the email's questions), or one letter when there is one question | answers them, with the Yes button when it approves |

Anything else goes to the agent as his instructions, written as a follow-up email block. Each
message is applied once (`factory.answered`). A reply that worked gets no email back (the board
shows the new state); one that did nothing gets one line in the thread saying so, with the board
link. An emailed approve of a public-repo landing still runs the secret scan first.

#### Prompt blocks parsed from email cards

The email dispatcher writes Zach's own words into an email card's prompts in these blocks, and the
factory reads them from the card's Claude session file to show the card's request and follow-ups
(`/api/peers`, reviewer prompts; `factory/peers.go`). The same text is in the dispatcher's README.
Change both or neither.

- `Zach's email:` is the header line in the first prompt.
- `Zach's follow-up email (<RFC 5322 date>):` is the header line in each steered follow-up.
- `(end of Zach's email)` closes either block.
- Between the header and the end line: Zach's own words, with the quoted history cut.

Each line must be whole (from the start of a line, nothing after it), the date must parse, and a
block without its end line is ignored, so text that only quotes this format is not a follow-up.
Only user messages count, not tool output. An email card with no `Zach's email:` block (older
cards, or an empty body) uses its `- Subject:` line. A follow-up overrides the first request where
they conflict. Only Claude session files are read again as they grow.

The factory writes one block of its own, not part of the dispatcher's contract: a closed duplicate's
request, under `Zach's request from closed duplicate card <id> (<RFC 5322 date>):` and closed by the
same end line. It holds only Zach's words (the reviewer's notes follow the block), and it adds to the
kept card's request rather than overriding it.

### Card timing

Every factory card (one with a `factory.stage` label that is not a reviewer, Scout or repo picker)
records its times in `timings.json` in the state directory, so a restart keeps them
(`factory/timing.go`). Entries are dropped 30 days after their last event.

- **Asked**: the email's `- Date:` line or the voice memo's `- Recorded:` line in an email card's
  first prompt; else the card's creation.
- **Columns**: each column the card entered, read from the board the factory already builds, so it
  costs no paseo call. Only reads the board accepted count. A move is dated by `factory.since`
  when it set one, else when the factory saw it (within one poll).
- **Reviews**: each round's start (the reviewer's creation), end (its verdict applied) and verdict.
- **Landed**: the last verified commit time from the card's own linked-worktree commits made
  after Asked. The commit must be on the card's branch and main; a rebased version must match
  the original patch. Errands that commit nothing have no landing time. Failed Git reads never
  substitute the clock or another card's commit.
- **Replied** (email and voice cards): 5 minutes after Done, one read of the card's thread takes
  the last message the mailbox sent after he asked and by Done plus 5 minutes. The factory's own
  emails (Needs Zach notes, failed-answer notes) are recorded when sent and never count.
  Reply checks also run for errands that commit nothing. A completed inbox card with no confirmed
  reply has `noReply: true` in saved timing, board timing and the timing export; the Timeline says
  “no reply”. This includes exhausted mailbox retries or a missing thread. A later reply outside
  the original window is not attributed to that card; it remains excluded from completed metrics.
- **Partial**: a card first seen past its start (in flight when timing began, or created while the
  factory was down) has an incomplete history.

The card panel shows it as one Timeline line. `GET /api/metrics?days=7` (1 to 30) gives the
count, arithmetic mean (`average`), `median` and `p90`, in seconds, for:

- **Card turnaround** (`turnaround`, `bySource`, `byTier`, `bySourceTier`, `byRepo`,
  `bySourceRepo`, `byAgent`): `endToEnd` (asked to replied, else to landed), `toLanded` (asked to
  landed), `toDone` (asked to Done), and `zachWait` (time in Needs Zach up to Done).
- **Per-step durations** (`steps`, `stepsByRepo`): `cards`, `visits`, `repeatedVisits`,
  `visitSecs` (per visit) and `cardSecs` (summed across visits per card) for `plan`,
  `plan-review`, `building`, `review`, `landing` and `zach`.
- **Per-turn agent durations** (`agentTurns`, `agentTurnsByRole`, `agentTurnsByModel`,
  `agentTurnsByStep`, `agentTurnsByRepo`): `cards`, `sessions`, `turns`, `turnSecs` (per turn)
  and `sessionTurnSecs` (summed turns per session), measured directly from local transcript files
  via `ReadTranscript` from each user prompt to the final assistant event of that turn.

It counts cards that reached Done in the window and have a landing after Asked, or are inbox cards
with a confirmed reply. Replied email and voice errands count in turnaround, step and agent-turn
metrics even when they commit nothing. Only cards with a landing after Asked contribute to
`toLanded`; each turnaround group reports that sample count as `landedN`, separately from its
overall card count `n`. With no landing samples, `landedN` is zero and `toLanded` has zero-valued
quantiles. Unanswered inbox cards are counted in `excluded.noReply`, including cards still awaiting
their reply check. Dropped or closed cards and partial ones are only counted in `excluded`.
Query parameters (`repo`, `source`, `tier`, `role`,
`provider`, `model`, `step`) filter the results (`?source=email`, `?repo=owner/name`,
`?role=reviewer&step=review`).

Historical timing correction clears a landing only when it precedes Asked, or a surviving,
complete worktree reflog proves there is no own commit after Asked. All other old values stay:
a retired worktree, expired reflog or failed read cannot erase history. The upgrade inspects at
most three worktrees per Tick and persists its progress. Read-only and dry-run instances do
not run the upgrade.

#### Session links and transcript turn timing

The same timing records keep the repo and a history of observed agent-to-session links for the
Fellow, reviewers and nested Paseo sub-agents. A link contains the Paseo agent and immediate
parent, device, provider, native transcript session id, model, role, review kind and card step.
Session ids use the same decoding as the transcript reader, including plugin providers.

`GET /api/timings` is a read-only snapshot of the timing store. It returns `{"cards":[...]}`,
sorted by card id, with `id`, `repo`, `source`, `tier`, `partial`, `requestedAt`, `requestedFrom`,
`landedAt`, `repliedAt`, `noReply`, `doneAt`, `events`, `rounds` and `sessions` when available. It excludes
titles, folders, transcript bodies, mailbox thread and message ids, and reply-check bookkeeping.
Like the other board APIs, it uses the server's existing access boundary; it must not be published
on the public internet.

A link's `observedAt` dates when Factory saw that association, not when a turn began or ended.
`createdAt` is the Paseo agent's creation, not necessarily the transcript session's creation.
Links are added on changes to the session, step or attribution metadata; unchanged polls and
export reads write nothing. Reviewers and their descendants keep their review step. Archived
children keep their previous step. Earlier links survive session changes, restarts and agents
leaving the board until the card timing record expires under the existing 30-day rule.

When `/api/metrics` computes per-turn agent durations, it deduplicates session links by
`(provider, sessionId)` per card, reads local transcripts through `ReadTranscript` (caching only
extracted turn boundary timestamps by file mtime and size, never transcript text), and attributes
non-reviewer turns against the card's `events` timeline. Older records may lack a repo or session
links, and in-flight or interrupted turns may lack a final assistant event; `/api/metrics` reports
`coverage` (`cardsWithRepo`, `cardsWithoutRepo`, `cardsWithSessions`, `cardsWithoutSessions`,
`linkedSessions`, `matchedSessions`, `unmatchedSessions`, `turns`, `missingBoundaries`) so missing
sessions and incomplete turn boundaries stay visible. Session span is not uninterrupted work, and
overlapping agents' durations are not card elapsed time.

Factory decisions also read session files only: reviewer verdicts, plan tiers and tracks,
landing replies and card names never load an agent through Paseo logs. If required history
is unavailable, the factory retries for two minutes after the turn stops, then moves the
card to Needs Zach. It leaves the review unapplied and does not infer a verdict or tier.
Outside landing-review requests instead finish with an escalation and a history-unavailable
finding after the same grace period; their requesters are not moved onto the board.
Naming alone can wait for a later readable reply.

## Public repos: review and secret scan

Repos listed in `FACTORY_GUARDED_REPOS` (default: this one) are public, so everything that lands
in them is published. For their cards:

- After code review passes, the factory runs the secret scan on `origin/main..HEAD` in the card's
  worktree. It uses its own installed binary and the gitleaks config from `origin/main`, never the
  card's copy, so a card cannot weaken the scan that checks it. A hit, or a scan that cannot run,
  stops the card in Needs Zach; nothing is published.
- After a clean scan the finished-work rule applies, as in any repo: what Zach asked for comes to
  him first, and what the factory started (ideas, Scouts, scheduled runs) lands on its own with
  `bin/land`, which scans again before its push, as does the pre-push hook, unless it is on the
  always-ask list. For those, no person approves the landing: what keeps private data out is the
  code reviewer, told the repo is public, and the scans. Landing a card at Ready for you
  scans again before the land message.
- The direct track is off, ideas wait for a person's Start, and reviewers get a public-repo rule
  (any hostname, email, id, quote or secret is blocking).

Other repos are unaffected.

### The scanner

`factory scan [--staged | --tree | <range>]` (package `factory/scan`) checks the lines a change
adds and the commits' identities:

1. **gitleaks** (credentials), with inline `gitleaks:allow` comments and `.gitleaksignore` files
   ignored, since a change could add either to hide itself.
2. **Generic patterns**: tailnet hostnames (`*.ts.net`), home-directory paths, email addresses
   (GitHub noreply and `example.*` allowed), 16-character hex ids, dated quotes.
3. **Private patterns** from `~/.config/agent-factory/denylist`, one regexp per line: the real
   host names and the like. They live outside the repo because a public pattern list would
   publish what it hides. Hits are reported as `private-<line>`.
4. **Commit emails**: every author and committer must be a `@users.noreply.github.com` address.

Every intermediate commit is scanned, since a push publishes them all. Reports name the rule and
the place, never the matched text.

Where it runs, in order:

| Where | When | Private patterns |
| --- | --- | --- |
| `.githooks/pre-commit` (`bin/scan --staged`) | every commit | required on a factory host |
| `.githooks/pre-push` | every push, each commit it would send | required on a factory host |
| `bin/land` | before its push | required on a factory host |
| the factory, in-process | before it lets a guarded card land, and again on approval | always required |
| GitHub Actions (`.github/workflows/scan.yml`) | after every push (already public) | not available |
| GitHub secret scanning and push protection | on push | not applicable |

CI is only a backstop: it runs after the push, and without the private patterns it catches
credentials and generic leaks only. The checks that keep a leak from being published are the
local ones and the factory's.

## Install

On the host that runs Paseo:

```sh
go install github.com/zricethezav/gitleaks/v8@v8.30.1   # then put gitleaks on PATH
mkdir -p ~/.config/agent-factory && chmod 700 ~/.config/agent-factory
$EDITOR ~/.config/agent-factory/env        # see below
$EDITOR ~/.config/agent-factory/denylist   # private scan patterns, one regexp per line
git config user.email <id>+<login>@users.noreply.github.com
./install.sh
```

`install.sh` checks the host config, sets `core.hooksPath`, vets and tests, then installs the
deployer (`cmd/factory-deploy`, package `deployer`) and its timer and runs one deploy. After that
the deployer keeps the factory running this repo's `main` from GitHub: every minute it fetches
`main`, rebuilds when the code changed, restarts the factory, rolls it back if it does not come up
healthy, and restarts a hung one. So work lands live within a minute, and `install.sh` is only for
the first install. Logs: `journalctl --user -u agent-factory -u agent-factory-deploy`.

### Landing reviews before publication

`bin/land` and the pre-push hook require a landing review for agent pushes
(`PASEO_AGENT_ID` set). It checks the actual commit after the rebase, including conflict
resolutions. A completed code approval of that commit is reused; a changed commit gets a
fresh independent review. A factory card must still be authorized to land, with no queued
or pending code review. Dropped, replaced, held and parked cards cannot publish.

An agent outside the card flow can also run `land-review -why "..."` (a link to the factory
binary) once its work is committed. It exits 0 on approve, 2 on changes, 3 when Zach must
decide. The same completed approval can be reused for the same commit and checkout.
An unavailable or rejecting review stops publication. The secret scan still runs on every
push. Agent pushes can publish only the checkout's reviewed HEAD.

### Host config

`~/.config/agent-factory/env` (the unit's `EnvironmentFile`; flags of the same names work too):

| Variable | Flag | Meaning |
| --- | --- | --- |
| `FACTORY_BOARD_URL` | `-board-url` | board link put in emails (none compiled in) |
| `FACTORY_DEFAULT_REPO` | `-default-repo` | `owner/repo` for cards with none given; errands are recorded there (required for cards with no repo) |
| `FACTORY_GUARDED_REPOS` | `-guarded` | comma-separated public repos (default this one) |
| `FACTORY_PRIVATE_RULES` | `-private-rules` | private scan patterns (default `~/.config/agent-factory/denylist`) |
| `FACTORY_LANES` | `-lanes` | board lane names per repo, `{"owner/repo": {"name": "...", "lands": "..."}}` (default `~/.config/agent-factory/lanes.json`) |
| `FACTORY_IDLE_TTL` | `-idle-ttl` | park a Needs Zach card idle this long (default `3h`) |
| `FACTORY_IDLE_CAP` | `-idle-cap` | park the longest idle Needs Zach cards while more factory agents than this are idle (default 10) |
| `FACTORY_INBOX_SOURCE` | | the email dispatcher's `source` label value on its agents (none: no dispatcher) |
| `FACTORY_INBOX_ANCHOR_ROLE` | | the dispatcher's anchor agent `role` label, hidden on the board |
| `FACTORY_ZACH_EMAIL` | `-zach-email` | where cards with no email thread email Zach (none: they are not emailed) |
| `FACTORY_PREVIEW_PORTS` | `-preview-ports` | `lo-hi` ports Ready for you previews run and are published on (default `7700-7799`) |
| `FACTORY_PREVIEW_MAX` | `-preview-max` | previews running at once (default 8) |
| `FACTORY_METRIC_HOSTS` | `-metric-hosts` | comma-separated SSH aliases whose CPU, load, RAM and disk the board's host icon shows besides this host (none: this host only) |
| | `-listen` | address to serve on (default `127.0.0.1:<port>`); `host:0` picks a free port |
| | `-dry-run` | real data, no changes: only paseo and git reads run, every write is logged to Activity, no Tick, no email, state copied to a temp dir that is deleted when it stops |
| `FACTORY_ARCHIVE_CLI` | | separate Paseo executable for archive writes (default: `paseo` on PATH; stable version >= 0.13.0 required) |
| `PATH` | | must reach `paseo`, `node`, `git`, `gitleaks`, `tailscale` (previews) and `pdw` (logged in) |

Cards can start in any checkout under `~/dev/<owner>/<repo>`.

### Computer use for agents

Fellows get computer-use instructions on both tracks, including web research, and must click through
their own UI changes before code review. Code and landing reviewers get the same setup instructions
when a change touches a UI. Repo pickers, Scouts and plan reviewers do not get browser instructions.

Agents use the logged-in `pdw` CLI and read the current `openai-computer-use` skill for connection
names, tools and recovery steps. The browser runs remotely; a test server must use the execution
host's tailnet address. Open sessions only when needed, close owned tabs and stop test servers when
finished. Report unavailable access or gateway capacity honestly; reviewers may skip with a reason.
Computer-use access keeps the role's existing action approvals and read-only restrictions.

## Layout

- `cmd/factory`: the binary (board server, `factory scan`, `land-review`).
- `cmd/factory-deploy`, `deployer/`: the self-deployer; `deploy/`: the systemd units.
- `factory/policy.go`: tiers, prompts, verdicts, the automatic moves (`Tick`). `factory/panel.go`:
  code-review panels and the panel arbiter.
- `factory/guard.go`: guarded (public) repos. `factory/scan/`: the secret scanner.
- `factory/alwaysask.go`: who sees finished work before it lands, and the always-ask file check.
  `factory/arbiter.go`: the arbiter for reviews that do not settle. `.factory.json`: this repo's
  always-ask patterns.
- `factory/route.go`: Repo Auto. `factory/agents.go`: the agent choices. `factory/reap.go`: the idle reap.
- `factory/board.go`, `collect.go`, `ask.go`, `ideas.go`, `inbox.go`, `actions.go`,
  `transcript.go`, `server.go`; the board and transcript pages are in `factory/web/`.
- `factory/transcript.go`, `transcript_providers.go`: the transcript page, the board's card summary,
  Needs Zach text and email, Close, Retry and plan-review reads use each agent's own session file
  (Claude `~/.claude/projects`, Codex `~/.codex/sessions`, Antigravity
  `~/.gemini/antigravity-cli/brain`, Muse `~/.local/share/muse/sessions`). With no readable file
  they say "history unavailable" and why; they never call `paseo logs`, which loads the agent in
  Paseo (resuming an archived one). Decision reads use the same session-file readers.
  `/api/transcript/{id}` sends pages of about 300 KB at whole lines: `tail=1`
  is the newest page, `before=N` the page before line N, `after=N` new lines to poll (`more` means
  read again now; a line still being written waits until it ends). Images carry a `ref` instead of
  their data and are fetched from `/api/transcript/{id}/image/{ref}` (PNG, JPEG, GIF or WebP, for
  agents on the board), so a tool's images load only when it is opened. The page loads older pages
  as you scroll up, and all of them before a search. A card whose agent was replaced shows the earlier agents' history
  (events with `prev`, image refs ending in `.<prev>`) on the newest-first page that reaches its
  agent's first line; polls never send it again.
- `factory/hostmetrics*.go`, `hostfleet.go`: host load for the header icon and `/host`. Each
  `FACTORY_METRIC_HOSTS` alias gets one read-only SSH stream (`ssh -o BatchMode=yes`, a fixed
  script on stdin, Linux or macOS), shared by all viewers, run only while someone polls
  `/api/hosts` and stopped a minute after the last poll.
- `factory/delegate.go`: the sub-agent prompt section and the sub-agent checks.
- Tests sit beside the code, with a fake `paseo` (`factory/fake_test.go`).

## License

MIT.
