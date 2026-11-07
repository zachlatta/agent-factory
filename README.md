# agent-factory

A Kanban board and review pipeline for coding agents run by Paseo. A prompt comes in, an agent builds
the proposed solution, one gate grades it, and it lands. You are asked only when the gate says you sign off.

One Go binary. It reads Paseo's agent files every 15 seconds and whenever one changes, applies the
rules below, and makes every move through Paseo: the Paseo helper below for agent checks, label
and name changes, messages and archives, and the `paseo` CLI for the rest (labels on agents are the state:
`factory.stage`, `factory.submitted-by` and so on, see `factory/labels.go`). It serves the board on
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
- Within a pass, card moves run in order: check, labels, send. A finished reviewer's or
  Scout's archive waits until the pass's moves are sent. Board actions can interleave with a pass;
  landing approvals and dependency releases recheck the current card state before proceeding.
- Agent prompts require waiting on the original command's task handle or a verified exact process
  ID. Process-pattern polling can match its own watcher or another agent's work. A watcher timeout
  is checked against the original command result before retrying.
- `paseo permit ls` runs at most every 15 seconds; reads in between keep the prompts it last found,
  unless an agent's file flags a permission wait it did not see. The archive CLI's version check
  runs once per executable (again if the file changes or an archive fails).
- **Paseo helper** (`factory/paseohelper.go`, `factory/paseohelper.mjs`). One long-lived `node`
  process, connected to the daemon, serves `inspect`, `agent update` (name and labels), `send
  --no-wait` and `archive`; the factory writes it to the state directory and talks to it in JSON
  lines over stdin and stdout. It loads the client code of the `paseo` CLI on PATH and serves only
  while the daemon reports that CLI's version; a changed CLI package restarts it. Its answers and
  errors are the CLI's, word for word.
  - When it cannot serve a call (cannot start, another version, stopped, crashed, or no answer
    within 20 seconds, which stops it), the call runs the CLI. An unexpected helper exit is
    logged and restarted in place at once; if starting fails or it exits again within 30 seconds,
    the next try waits 30 seconds while the CLI serves. A daemon error (not found, currently
    running, an invalid label) is returned without running the CLI.
  - `deploy/agent-factory.service` sets `OOMPolicy=continue` and runs the board at the user-level
    `OOMScoreAdjust` floor (`100`, since a user unit cannot go below its manager's value), while
    each helper start on Linux writes `1000` to `/proc/<pid>/oom_score_adj` (logging and
    continuing if the write fails), so under memory pressure the kernel kills the helper first and
    the board stays up and restarts it in place.
  - A message runs the CLI only when it surely never reached the daemon (no helper, the request
    could not be written, or the helper was not connected). One that may have gone out returns
    an error and is not sent again.
  - Archive uses the helper only when its version passes the archive version rule below;
    otherwise the CLI (and `FACTORY_ARCHIVE_CLI`) archives. The check after each archive is the
    same on both paths.
  - The helper exits when its connection drops or the factory stops.
- `/api/health` reports `paseoCalls`: the paseo calls of the last 10 minutes, with count and wall
  time per command. `cliCalls` started the CLI (one process each), `helperCalls` were served by
  the helper, each command has `cli` and `helper` counts, and `helper` shows its state
  (`running`, `down`, ...), CLI and daemon versions, start time, restarts and last error.

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

Pick repo → Build → Gate → Landing → Done, with **Needs Zach** for anything that waits on a person, including
his sign-off when the gate asks for it (`factory/gates.go`).

1. **Submit.** The prompt is tagged `factory.submitted-by`: `zach`, or `system` for ideas, Scouts, scheduled
   runs and anything else automatic. `POST /api/new` from the board sends `"submittedBy":"zach"` and email and voice
   origins count as Zach; anything untagged is `system`, so an agent's own request never counts as his. An untagged older board card counts as `system`, since an agent may have filed it. Reviewers see
   the tag and the card's source. A scheduled run is recognised without its word: for a request over
   loopback the factory finds the calling process in `/proc`, walks up to the nearest `PASEO_AGENT_ID`, and marks
   the card `origin=schedule` only when that agent is a running scheduled run (`paseo.schedule-id`; `factory/caller.go`).
   A `"schedule"` field on `POST /api/new` is ignored; it still names the job that filed an idea.
2. **Pick the repo.** With Repo on Auto, Opus 5.5 at medium thinking names the repo (`factory/route.go`).
3. **Build the proposed solution.** Most of the work happens here, on any Frontier model (see Repo and agent per
   card). There is no plan stop. The output is one or both of:
   - **Code changes:** commits on the card's branch and a clean worktree, plus proof that fits the change: a webp video
     for UI work (under `.factory-evidence/`, out of Git), test output or a before-and-after log otherwise.
   - **Live system changes:** a runbook in the final message under `RUNBOOK:`, already investigated read-only: exact
     targets, values and commands, what each step should show, rollback and a final check.

   The builder ends with `RESULT: code`, `RESULT: runbook` or `RESULT: both`. It is read-only against production:
   it can do anything in the worktree, including dev servers bound to loopback or this host's tailnet IP, and makes no
   external change. That is a rule the agent follows; nothing enforces it yet.
4. **Gate.** One fresh Opus 5.5 at medium, the gatekeeper, decides whether the work moves forward or goes back to the
   builder. It does not read the start-here skill: its prompt has every rule it needs. It audits the worktree against
   the original request and grades each check green (nothing left to do), yellow (minor issues) or red (wrong or
   missing), one line each before its `VERDICT:` line:
   - `MEETS REQUEST:` does it do everything that was asked, with the unsaid context it needed: what else the old
     behaviour did, and who depends on it.
   - `WORKS:` part of meets request. The gate tests it itself and does not take the builder's proof on trust: it runs
     the tests and build, then uses the change (a dev server and computer use for UI, the changed commands or requests
     otherwise, the key queries for data work) and pastes each command and its last lines. A runbook is checked step
     by step read-only against the live system, never run. What cannot be tested without changing production is yellow.
   - `DESIGN:` code quality, built on the latest `main` (it fetches and rebases), and the codebase or system stays
     cohesive. Needed refactors get done instead of local patches: small ones the gate does itself on the branch,
     with the tests passing; big ones (a redesign or more than about 150 changed lines) are graded yellow or red.
   - `PUBLIC SAFETY:` in a public repo, every commit that would be published: the repo's secret scan, no private data
     (secrets, internal hostnames, paths, private repo names, ids, emails other than GitHub noreply, quotes or incident
     history), noreply commit emails, and nothing that weakens the scanner, which is always red. A small leak the gate
     removes itself by rewriting the unpublished commits. `n/a` in a private repo.
   - `SIGN OFF:` does Zach need to sign off before it goes live? Green when he submitted it and it is what he asked
     for (including deletes, spending and messages he asked for); red when he submitted it but it includes larger
     architectural changes than he asked for, and when the system submitted it, except a scheduled run of a job he
     already approved (green); yellow when unsure. Sign-off never sends work back.

   The card panel shows gate checks by pass: a colored grade and text label for each check, plus an
   expandable gate report. Completed passes keep their grades and reports across restarts. Older passes
   without saved checks say so; the latest label is shown when no pass history exists.

   The factory reads the grades (`ParseGrades`) and keeps the last ones on the card (`factory.grades`, shown in the
   card panel). Any of meets request, works, design and (in a public repo) public safety that is not green, including
   a missing line, sends the work back to the builder with the gate's findings, and the gate runs again when the builder
   stops (`factory.auto-review`), until they are green. So does `VERDICT: changes`. When they are green, sign-off
   decides: green goes to landing; yellow, red, a missing line or `VERDICT: escalate` comes to Zach at Ready for you
   (below). Approve & land is offered, and accepted, only on a card waiting on Zach or landing: a card still building
   or at the gate has nothing to approve. If the gate committed a fix, its approval covers a commit that is no longer
   HEAD, so the gate runs again on the final commit before anything lands. Its approval of the exact HEAD is the code
   review that `bin/land` and the pre-push hook reuse.
7. **Land.** Opus 5.5 at medium lands it on `main` in a fresh agent, whatever built it. For a runbook it first checks that
   it still matches the live system: it corrects minor changes itself, and for huge ones ends with
   `RETURN TO BUILD: <what changed>`, which sends the card to step 3 and then the gate. A runbook-only card
   has no commits to verify, so the lander's report ends it. In a public repo the factory's own scan runs before landing.

The gate has at most 3 rounds (`factory.gate-rounds`; Zach's follow-up resets them). At the cap the card stays at the gate
marked stuck, on the board only. A failed landing returns to the builder and goes through the gate again.

A new card's branch starts from `origin/main`, fetched just before the card starts (`--base origin/main`), never from
the checkout's own `main`, which may be behind or carry commits that never reached `origin`.

Cards left in the three gates of the earlier flow (Meets request, Design, Sign-off) move to the gate once their builder
has stopped (`migrateOldGate`): their old reviewers are retired and the gate runs from the start, since an old verdict
covers none of the new checks. One marked stuck there stays stuck, in the gate column. Timing and usage history keep
the old gate names.

A builder that stops on a question with no `RESULT:` line waits in Needs Zach with that question, and his answer goes back to it.

**Errands.** A request that changes nothing (a question, research, a report, a PDW mutation he approves himself)
takes the direct track: the builder does it in one pass, lands its work item and ends `TRACK: direct`; the gate does not run. An email or voice card with no `RESULT:` line counts as an errand only when it has no unpublished commits: commits always go through the gates. A
last reply that asks Zach something goes to Needs Zach with that question: Answer, Mark done or Drop. It can carry a
`DUE:` line after NEEDS FROM ZACH: an ISO time with a zone, shown on the board, listed first in For you, and turning to Overdue
when it passes. A bad value is logged and ignored. Public repos have no direct track: a first pass that says it took one has what it pushed scanned and stops for Zach, and replies go through the gate. Work already published when it reaches sign-off is scanned too; a hit waits for him.

Cards left in the old flow's columns (plan, plan review, code review) move on their next stopped pass
(`migrateFlow`): built work goes to the gate, and the rest is told to build.

A card's title comes from the builder's
first `TITLE:` line; titles given by an idea or the email dispatcher are kept. A card's worktree starts on a branch
slugged from its initial title and renames with it.

- **Ready for you** is the one stop where a person sees finished work before it lands. The builder
  writes three `CHANGED:` lines in plain words and any `BEFORE:` / `AFTER:` screenshots; it starts no
  server and uploads nothing. The factory runs the repo's preview (below). The board item shows it with Land, Change and
  Drop. Email replies in a thread he started: `yes`, `land` or `land it` lands it; drop words drop it; letters answer the
  builder's questions; anything else is a change request, which sends the card back to building and the gate. An answer
  to his feedback on the version he was shown that asks nothing goes on to landing, unless the gate's sign-off still gives a reason.
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
    Serve changes run one at a time; one refused because another client is changing the serve
    config is tried up to 5 times in all, with about 4 seconds of backoff; any other error fails at once.
  - It stops when the card leaves Ready for you (landing keeps it): change requested, dropped,
    done, landed or archived. Stopping kills the process group, removes the tailnet entry and the
    preview's folder. Tailnet entries in the range and folders no saved preview owns are removed.
  - A preview belongs to the card, not the agent: it stays saved under the agent that started it,
    and a fresh agent that takes over the card (such as the lander) keeps its port, link, process
    and folder.
  - The factory's processes stop with it, so after a restart the next Tick starts saved previews
    again on the same port and link. Only Tick starts or stops previews: a `-dry-run` or
    `-tick=false` copy never does, even with a copied `previews.json`.
  - It is not a sandbox: the process runs as the factory's user and could read files by absolute path.
- Changes are whole (`wholeChangeRule`): one way of doing each thing; every caller, test, doc and
  README the old behaviour touched is updated, and no old path is left (or each instance left alone is
  listed with why). The gates check it, and a partial change is a blocking finding. Scope
  is the area of the ask: refactoring inside it is part of the job and is no change in scope; a
  refactor outside it is architecture Zach did not ask for, which the gate's sign-off names.
- Builders name their deploy steps (if any) and make no live changes (install scripts, enabling or restarting
  services, resizing VMs, external settings): the gate checks the steps, and the landing step runs and verifies them.
- Automatic recovery (`factory/recover.go`). Errors, usage limits, stalls, failed reviewers,
  dropped dependencies and failed landings never go to Needs Zach or email. The factory retries
  them, or hands them back to the card, up to a hard cap per kind. When a cap runs out, the card
  stays in its column marked stuck (`factory.stuck=<why>`, read with `stuckReason`): a
  "stuck: <why>" flag on the board and a board-only ask with Re-run (Resume for agent
  errors), Reply and Drop. A stuck card gets no automatic move until it runs again or Zach acts; that clears the mark.
  The state lives in one label, `factory.recovery` (base64 JSON, which replaces
  `factory.limit-recovery` and is read from it once). It survives restarts, and a handoff to a
  replacement agent carries it and `factory.reviewer-retries` (`carriedFactoryLabels`), so a
  handoff cannot reset a cap. The stuck mark is not carried.
  - Card agent errors: an errored card whose reviewed work is already verified on `origin/main` and
    whose last report says nothing remains (`REMAINING: none` and nothing needed from Zach) moves to
    Done and keeps the error in its note. Otherwise a recognized quota reset gets one resume on the
    same model at the reset plus 30 seconds. Unknown errors, and a repeat after that resume, retry
    on the same model 2, 10 and 30 minutes after each failure. Then the card is marked
    `stuck=agent error`. A send whose delivery is uncertain is never repeated within its 2-minute
    grace; the next try waits its step. A completed turn ends the episode. Resume and Reply on a
    stuck card each make one attempt and do not rearm the retries.
  - Model hand-off (`factory/handoff.go`): when those same-model retries are used up, or a known
    reset is more than 2 hours away, the card moves once to another model in the same worktree
    (`paseo run --workspace`) before it is marked stuck: Opus 5.5 unless Claude is the failing
    provider, else the best-ranked Frontier model (`rankAgentChoices`), skipping the failing
    provider, unavailable ones and exhausted plans. The new agent gets the card's first message
    verbatim (so its gate and the public-repo scan rule carry over), the proposed solution with its
    reviewer notes and the old agent's last reply, plus the carried labels (`factory.recovery`
    included, so the caps are not reset) and `factory.replaces`. The steps are stored in
    `factory.handoff` on the old agent (starting, started, confirmed, failed), so a restart finishes
    them: a missing replacement is started once more after 2 minutes, and one never confirmed within
    5 minutes ends the hand-off. The old agent gets `factory.replaced-by` and is archived only after
    the new one is seen running; until then reap leaves it alone, and a failed hand-off's old agent
    stays until the card lands, is archived or is dropped. With no other model, no readable session
    or a hand-off already used, the card is marked stuck as before.
  - Reviewers: an errored reviewer, one waiting on a permission prompt (`paseo permit ls`) for 2
    minutes or running with no activity for 20 (without a working sub-agent of its own), and one that
    ends or disappears without a verdict, are superseded and archived, and a fresh one starts, at most
    twice per round (`factory.reviewer-retries`, cleared when a verdict counts the round). After that
    the card is marked `stuck=review could not finish` in its gate. If a superseded reviewer finishes
    first, its verdict still moves the card on. The prompt itself is refused by the factory (below), so
    a reviewer usually carries on without being replaced.
  - A card that stops with nothing queued (for 2 minutes in plan, building or landing) is nudged once
    with what its step is missing. If it stops again, its step re-runs (a review, or the land prompt
    with its scan). The third time it is marked `stuck=stopped with nothing queued`. Moving to another
    column starts the count afresh.
  - Failed landings: see the landing checks below. Each card gets at most 2 returns to building,
    then `stuck=landing failed`.
- Fresh agent per stage (`factory/stagehandoff.go`): every move of a card to new work starts a new
  agent instead of resuming its session, whose context by then is re-read on every call. That is a
  gate's fixes, the land prompt (after the gate, Zach's approval, a re-run or a released hold),
  and a failed landing, a dropped dependency or work left after a landing going back to building.
  The new agent runs the same model, except a lander, which runs Opus 5.5 at medium (`factory.agent`, else the agent's provider and model) in the
  same worktree, through the same
  steps as a model hand-off (`factory.handoff` kind `stage`), with the card's labels: reviewed commit,
  code approval, held approval, review generation (`factory.code-rounds-from`), recovery counts and
  email thread. A fresh lander is authorized like any card agent: the landing review reads its own
  labels, and the replaced agent can no longer land. Its first message is a short handoff: the card's
  original instructions verbatim, Zach's follow-ups, the proposed solution, the branch's commits and
  diffstat, the previous agent's last message (once) and the stage message,
  plus every earlier agent's Paseo id and session id, with how to read their full transcripts
  when the handoff is not enough: only in PDW (`pdw readme agent-sessions`, `marts_ai_conversations`,
  `pdw search`), never `paseo logs`, which loads (and can resume) an archived agent, nor
  `/api/transcript/{id}`, which shows the card's current agent and stitches whole earlier sessions in; a lander is told to take the deploy steps from the reviewed build summary.
  The prompt is kept in the state dir (`handoffs/`) until the new agent is confirmed, so a restart
  retries the same one; a swap that does not start (no known model, no readable first session, two
  failed starts) sends the stage message to the old session instead, as before, and so does a parked
  card, which the send wakes. Messages within a stage stay in the session: Zach's replies, steers,
  nudges, and summary, brief or preview requests. Stage swaps do not use up the card's one model
  hand-off (`factory.model-handoff` marks that). `FACTORY_STAGE_HANDOFF=off` turns the swaps off.
- Permission prompts: the factory answers every prompt of its own agents (cards, reviewers, Scouts,
  pickers and their sub-agents) with no, once per prompt, telling the agent how to do the step
  without one (`rm` only with literal absolute paths, `git -C` instead of `cd`, work in the
  worktree) or to say in its summary what it needs (`factory/blocked.go`). It never allows a prompt,
  never answers one of an agent it did not start, and never in a dry or read-only run. An agent
  still on a prompt 20 minutes after its last activity gets Zach one email in its card's thread,
  if it has one, once per move of the card (`factory.permit-told`, `factory/permit.go`); he
  approves or denies it in
  Paseo, stops the agent or messages it.
- A plan can name one card it lands after, with a line `Waits on: <short id>`, or one its build needs
  first, with `Waits on: <short id> before build`. A plain `Waits on:` holds the land prompt after code
  review passes until that card is Done, then sends it. `Waits on: <short id> before build` holds the
  card in Building when a
  build turn stops with that line, with no
  running agent (`factory.waits-before=build`, `factory.held=<id>`, deferred build prompt in
  `held-builds/`), and starts the build once that
  card (or its replacement) lands. If the blocker was dropped or closed, or is not on the board (an agent that is not a card
  counts as dropped once archived), the card is told and goes to building with its `Waits on` cleared:
  before build, it builds without that card if it still can or stops and says why if
  the scope must change; after build, it decides whether to rebuild without the other card's work or
  keep the change as is, and either way the gate runs again before the land prompt.
  Held work stays in Building or Landing and in Working. When its dependency chain reaches Needs Zach,
  its tile, panel and Working line name and link to that decision. The decision shows how many
  held cards wait on it, including indirect dependencies, matching the workflow health count.
  These explanations clear automatically and create no extra asks or counts in Needs Zach, and
  no emails are sent for them.
- A landing card is Done only if its HEAD is on `origin/main`, it made a commit of its own, and its
  last message ends with `REMAINING: none` or lists only optional tries as described below. A card that lists work left (`REMAINING:` then one `-`
  line per item), or says nothing, waits in Needs Zach with the list in its note, shown on the board and
  not emailed. Optional `Zach:` tries close as Done with "Try it if you like:" in the note,
  only when every item is a try, NEEDS says nothing or only a try, DECISION is none and no questions remain.
  Tries with real action words (approval, login, payment, deployment and similar actions) still wait.
  A required `Zach:` item says "Work landed. Do this:"; agent items left offer Send it back to finish it or Mark done; no line asks
  "Landed: did it finish?". REMAINING lists only this card's own request: another card's work, a problem the
  change did not cause (an expired login, an unrelated failing test) and work outside the area of the
  ask go in the summary or become ideas, so they never hold a finished card. Each item starts with who
  must do it, `Zach:` or `agent:`. When the message's `NEEDS FROM ZACH:` says nothing (and names no
  ask), it has no question and no item is Zach's, the work left goes back to the same card instead: the
  agent finishes it in its worktree, new commits go through the gate, and it lands again. Only the first landing is reported in its
  thread. At most 2 such
  rounds (`factory.work-left-rounds`, carried across handoffs and not reset by "Send it on"); after
  that the card waits in Needs Zach with the list.
- A landing whose HEAD is not on `origin/main` (a push failure, a rebase conflict, a `bin/land` scan
  hit) goes back to building, with the failure in the message. The agent rebases on `origin/main`,
  resolves conflicts, reruns the tests and commits; on a scan hit it rewrites its own unpushed
  commits instead. Then the gate runs again and the land prompt follows. A direct-track card
  not on main is told once to land its work item again, then marked stuck.
- A landing whose commits differ from the reviewed ones goes back to building the same way: the
  agent checks what landed and fixes forward, the gate covers what landed, and the land prompt
  says not to push again if HEAD is already on main. This review still runs when an earlier landing
  was verified and there are no unpushed commits. On a public repo the factory first scans the
  range that landed (`factory.base..HEAD`). A hit, or a scan that cannot run, waits in Needs Zach as
  "Landed something other than what was reviewed" and is emailed in its thread, if it has one:
  something unreviewed is already public.
- Code and landing reviews of a change to frontend files (`factory/uireview.go`) include a UI check,
  given to the gate.
  In this repo use the screenshot helper below for desktop and phone layout, and computer use
  for interactive flows. Other repos retain computer-use checks at both widths. Reviewers return
  non-blocking feedback under "UI feedback:" for the builder's landing summary.
  For interactive action checks, build the binary in the worktree's `.factory-scratch/` and
  run `factory -dry-run -listen "$(tailscale ip -4):0"`. It reads real data but only logs writes.
  Code and landing reviewers may run this documented read-only check without a build-approval request.
  Stopping it with SIGINT or SIGTERM deletes its temporary state copy. Bind direct remote checks
  to the tailnet IP, never `0.0.0.0`.
- Drop, Close and dragging to Done are final: the card is stopped and archived, and late verdicts
  for it are not forwarded. Its running reviewers are stopped and archived with it.
- Every code approval, landing approval, landing retry and dependency release checks that HEAD
  is still the commit the gate captured and the worktree is clean. Changed or uncommitted
  work queues the gate again; a
  failed read, or a card with no captured reviewed commit, stops without landing. That
  technical stop is board-only and offers Re-run the review, never an approval that skips it.
- A follow-up retires the earlier gates. A verdict for work from before the
  follow-up cannot declare the revised request ready; the builder finishes it and a new review runs.
- Marking a card Done cancels its queued reviews and retires its open gates.
  A new follow-up or explicit action can reopen the finished work.
- An approval for a parked card held on a dependency authorizes one release when that dependency
  lands. The landing message wakes it only after the same review and scan checks. The approval
  follows a replacement card and is cleared on release or cancellation; a stale snapshot cannot
  wake a cancelled card.
- Replaced cards (`factory/replace.go`): when a card hands off to another agent (the model hand-off
  above writes `factory.replaces=<old-id>`
  on the replacement, `factory.replaced-by=<new-id>` on the retired predecessor), `resolveCardID`
  follows `factory.replaced-by` to the current card across board cards, ideas, `Waits on:` dependencies,
  reviewer `review-of` links, email reply threads (`factory.zach-thread` and `gmail-thread`) and message
  deduplication (`factory.answered` and `factory.told`), timings, token usage, peers, steers, evidence,
  board `#<id>` links, and `/t/<id>` transcripts (which stitch the predecessor turns and a system
  switch divider ahead of the replacement session). Carried lifecycle, gate, and thread labels
  (`carriedFactoryLabels`, and `carriedRawLabels`: the card's email thread `gmail-thread`, and the
  email dispatcher's `source` and `voice-memo`) are both copied at handoff and read through the
  replacement chain so approval, review, and public-repo scan gates survive even a partial handoff.
  Write primitives (`setLabels`, `send`, `stop`, `archiveChecked`) always act on the exact single agent ID.
  Drop and Close archive the card's predecessors too.
- A duplicate passes its request on. A reviewer (or the builder) that finds a duplicate names the
  card it duplicates on a `DUPLICATE OF: <card id>` line. When Zach closes the card at that close
  verdict, the factory keeps its request and the reviewer's notes (`steers.json`) and sends them to
  the kept card once it is building; an idle one then goes through the gate again. While the kept card is in review or Needs Zach the steer waits; if it is landing, landed or
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

An agent archived from outside during Build or a gate stays in its
column marked Stuck: “Agent archived before this step finished.”, unless it has no
gate rounds and its stored last reply already ends `TRACK: direct`, in which case
it finishes through the direct track (with note `done in one pass; worktree removed before its
landing was checked` when the archive already removed its worktree, where a question in that reply
cannot be continued and is not asked). A card marked Stuck does not
count as Done, release dependencies or free an idea slot. It is retained for 14 days after
archival, like parked work. Automatic ticks and late reviews do not wake it.
Resume, Re-run and Reply wake it explicitly and keep the normal review gates; when its worktree
directory was removed, actions that wake the agent or use its worktree return a clear error
instead of calling `paseo send`, while Drop and Mark done still finish it.
New factory cards and stage successors are watched from creation. Existing live cards in those
four stages are enrolled on Tick. Historical archives without the watch label stay Done; an
external card archived before its first enrollment is outside this detection.

A finished-work Land approval records the revision and summary shown at Ready for you, including
when landing waits on another card. If a failed landing returns to building, that authorization
survives restarts and fresh agents. The follow-up gate's sign-off
compares against that baseline and reports `APPROVED BEHAVIOR: unchanged|changed|unsure` with evidence.
Only an explicit unchanged finding can reuse approval. Changed, uncertain or missing findings
retain the normal Ready rules. New user instructions invalidate the saved authorization; factory Resume and Re-run messages do not.

Reuse removes only the sign-off reason already answered by Land. New repair-only approval reasons still require approval. The exact-commit gates, public-repo scans and
dependency gates always apply; a test-only repair is not automatically behavior preserving.
Reviewers report new approval reasons separately as `REPAIR SIGN OFF: none|reason`, excluding
reasons already covered by the approved revision.

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
- A direct result that asks Zach something when its agent is already archived is parked at once, with
  no idle time or cap: the agent is archived already, so the park is what keeps it in Needs Zach.
- Idle factory cards in other columns count against the cap but are never parked. Only eligible
  Needs Zach cards are parked to reduce the count, so the cap can remain exceeded. Automatic recovery
  retries a stopped card up to its caps, then leaves it in its column marked stuck (see Automatic
  recovery). Running agents and agents outside the factory do not count against the cap; they are
  included in health reporting. The reap never touches them except a retired reviewer, as below.
- A retired reviewer (its verdict applied as superseded, or given up on with none) no longer
  counts as reviewing: the board does not mark its card Reviewing or blocked by it, and recovery
  does not replace it again. It is stopped and archived when it is retired. A stop cannot settle a
  run record left running after its process ended (a host or daemon restart mid-turn), so when a
  stop does not settle it, it is archived with `--force`, which keeps its session. If that fails,
  the reap tries again 2, 10 and 30 minutes, 1 hour and 3 hours after each failed try, at most 2
  per reap. The count is stored on the reviewer (`factory.retire-tries`, `factory.retire-failed-at`),
  so a restart keeps it. After 6 tries the reap stops and the card shows "retired reviewer could not
  be archived". Retiring writes `factory.retired` with the time. A reviewer messaged after that is never
  stopped, forced or retried, only archived once it stops. One retired by an older factory (no
  marker) counts as untouched only when its session file has exactly one user message (its
  prompt) and Paseo's last-message time is not later; that check's time is then stored as its
  `factory.retired`. An unreadable session or unknown last-message time protects it.
  Only retired reviewers are forced; nothing is deleted, and the card is not touched.

Archive writes require a stable Paseo CLI version 0.13.0 or newer: earlier versions search only
one page of agents, even for a full ID. `FACTORY_ARCHIVE_CLI` selects a separate executable for
archive writes; other commands use the existing CLI on PATH (or the Paseo helper, which archives
only when its own version passes the same rule). Factory checks the selected
executable's version, reads each agent by full ID before archiving, and verifies its archive
timestamp afterward. Automatic archives never stop a running agent, except a retired reviewer (above). Each paseo call has a
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
  window counts as 100% left). Cards with no agent picked use this auto-suggested agent. Ideas carry no agent of their own: one that starts on its own runs on the best-ranked
  Frontier model (Opus 5.5, GPT-6.1 Sol or Gemini 4 Argon) that is available, not exhausted and
  above the idea usage floor, at its default thinking level, and the idea panel's picker defaults to
  that same model.

Board, email, voice and idea cards all default to the best-ranked available Frontier model, whichever built them; the balancer counts Claude and Codex at 80% of their real remaining capacity (`planCapacityWeight`), leaving Opus headroom for the other steps and Codex for PDW's operations. The model pickers prefer GPT-6.1 Sol within OpenAI and Opus 5.5 within Claude; Sonnet 5.5 is
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

- The builder and reviewer prompts list, for each available agent choice, the exact
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
- Email and voice-memo cards get the same section in their first prompt, since the factory writes
  it (see "Email and voice-memo cards"). A fresh stage agent reads it in the original instructions
  its handoff quotes, so the handoff does not add it again; a card the dispatcher started itself
  before then, whose first prompt lacks it, gets it once in each fresh stage's handoff instead.
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
refreshes. Larger sessions are read in slices. An unfinished last record waits while any known
session owner may still write it, or agent metadata is incomplete. A damaged tail whose owners
are all closed or archived is reported as an error, retaining the valid counters. If that session
later completes the record, collection counts it once and clears the tail error. Limited readings
remain flagged and report bytes
read and bytes still pending; their known counts are partial totals. A restart starts collection
again. A process remembers previously observed sources and reports their disappearance
on subsequent complete scans. Restarting forgets that inventory. Totals can shrink after
cleanup; they describe retained local records, not every device's activity or a subscription
bill. Historical subscription account attribution is unknown. No prompts, session contents
or filesystem paths are returned. The board’s Usage link opens `/usage`, which consumes this feed without another collector.
The page starts with a compact overview of every subscription and account: last
known weekly and five-hour usage, account emails, percent used and left, resets
and observation age. Old readings stay visible and labeled; missing windows never become zero.
Capacity advice follows the readings. It then asks **Where did usage go?**
and **Can I trust these numbers?** Capacity advice uses only available providers
with recent observed headroom. Codex reads its live subscription meter, with local
session readings as an explicitly failed-refresh fallback. Provider commands run
in a bounded background refresh. Failed refreshes retain the newest successful
provider and account readings. Automatic handoff and idea auto-start ignore only elapsed windows and treat
failed readings as unknown. A current low weekly window still holds ideas when
a short window resets; an elapsed weekly window does not hold them.
Elapsed resets never manufacture 100% capacity; estimates, old readings,
and unknown allowance are labeled and excluded from recommendations.

Consumption defaults to seven complete UTC days, with Today, thirty complete days
and all retained history available. Ranked consumers include a card's reviews and
delegated work; model and role summaries, review share, repeated Building share,
and daily counts use the same filtered selection. Complete periods compare with
the preceding equal period; percentage comparisons wait until a bounded scan
finishes, and partial totals are labeled while collection is in progress. Unknown-date intervals stay in retained history but
are excluded from dated comparisons. `day`, `timeReason`, `stageReason`, `rework`
and `evidence` describe these limits on each disjoint row. Filters and paginated
source rows live under Explore records. Allowances and source coverage stay
visible independently of filters.

Data confidence explains coverage gaps and links to affected rows. Row details
show exact counters and subset counts. `/usage/agent/{id}` opens read-only retained
main-session evidence through `/api/usage/evidence/{id}`, including archived
agents, without loading or resuming them. Only the accounting inventory's agents
are eligible; missing records remain unavailable. This separate evidence route
returns bounded transcript pages; the usage snapshot itself contains no text.
Refresh/fetch failures keep the last snapshot visibly dated. Missing or unsupported
records are unavailable, not proof of zero usage. Cleanup can change historical
comparisons. See [Usage product contracts](docs/usage.md) for acceptance criteria.



### Ideas

Any agent can suggest work with `POST /api/idea` (header `X-Factory: 1`; `repo`, `title`,
`summary`, `why`, `doneWhen`, `request`, optional `priority` and `schedule`). A person can
Start or Dismiss any idea; otherwise the factory starts them on its own:

- **Priority.** `priority` is `high`, `normal` (the default) or `low`; anything else is rejected.
  Waiting ideas start by priority, then oldest first, and the board lists them in that order with
  a High or Low chip and each idea's place in its repo's line.
- **Caps per repo.** Each repo has its own caps in `lanes.json`:
  `"ideas": {"inFlight": 2, "daily": 5}` (those are the defaults, for any field or repo not listed;
  0 means never on its own). Each tick starts at most one idea, the first in line whose repo is
  under both caps. Errored cards do not count toward the in-flight cap; when they resume, the count
  can temporarily exceed it. Public repos are no exception: their cards land under the
  finished-work rule and the secret scan.
- **Usage floor.** An auto-start runs on a Frontier model with more than 20% of its 7-day window
  left (see Repo and agent per card). Only while every available Frontier model has 20% or less
  left does no idea start on its own; the board's idea panel says why, and Start still works.
  Unknown usage does not hold.
- **Start errors.** If an idea fails to start, the error and time are saved on the idea and shown on
  its card, in the idea panel and under Stuck. That tick attempts no further starts; the next tick
  moves on to healthy waiting ideas, and the failed idea is retried automatically after an hour (or
  immediately with Start now).
- **Schedules.** A Paseo schedule that files an idea sends `schedule` (its name, at most 80
  characters): the idea is stored with it, and `by` reads `schedule <name>`. Scheduled self-improve
  jobs that would change code file one batched idea per repo this way instead of committing, so
  their fixes go through the same review as any card.

Titles that repeat
a waiting, dismissed or recently started idea are rejected. "Suggest ideas" on the board starts a
read-only Scout that submits up to 3, guided by the dialog's prompt; with Repo Auto, a picker first
chooses the repo from that prompt, as for new cards. The text in “What should it do?” is passed to the Scout
as guidance; leave it empty for general suggestions. A running Scout shows in the Ideas column, then in Done.
Every builder and reviewer is told how to suggest out-of-scope work it finds as an idea (at most 2 per
card) instead of doing it in its card; agents started before this rule do not have it.

### Email and voice-memo cards

An email dispatcher (not in this repo) starts every card for an email or a voice memo through
`POST /api/new`, as the board does, so all three get the same builder prompt, tracks and gates:

- `request`: Zach's own words, verbatim (the email body with its quoted history cut, the subject for
  a subject-only email, or the transcript). It goes under the prompt's `Zach's request:` line, which
  `/api/peers` and reviewer prompts read for every card.
- `origin`: `email` or `voice` (the card's `factory.source`); `thread`: the email thread the card
  reports in (`gmail-thread=<id>`); `title`: the dispatcher's subject line (kept, not renamed).
- `inbox`: the dispatcher's own section, added last: how to read the thread, which message to answer
  and how, and when Zach asked (a `- Date:` or `- Recorded:` line, for card timing).
- `labels`: only the dispatcher's own, without the `factory.` prefix: `source=<FACTORY_INBOX_SOURCE>`
  and `voice-memo=<recording>`. Any other label, an `inbox` or `labels` on a board card, an inbox card
  with no thread, or one with repo `auto` is refused.

Those cards report back in their email thread when they land, in plain words (see Needs Zach
email). They never email that work started, stalled or failed: the board shows that. A board card
answered by email reports the same way, in that thread; one answered only on the board stays quiet.

### For you board

The board opens on one column, at every width (`factory/sections.go`, the `sections` of
`/api/board`), in this order:

- **For you**: the Needs Zach cards whose ask is not board-only (`Ask.BoardOnly`, the same rule that
  decides what is emailed in its thread): finished work to try, a decision (including a landed diff
  on a public repo whose secret scan did not pass), or an agent's question;
  and work that landed with a required action for Zach (`action`, shown here though not emailed). Each shows its label
  (what kind of input it wants: "Ready: try it, then land", "Allow the build", "Work landed. Do this:"),
  its question (the agent's `DECISION:` line, else the ask's title) and the one thing to do (its
  `NEEDS FROM ZACH:` line); on Ready for you, what changed and
  the factory-run Preview button (or why there is none yet); its before/after images; and its buttons.
  A direct result's question with a `DUE:` time also shows a Due or Overdue flag, here, on its kanban
  card and in its panel. Those cards come first, soonest first.
- **No answer yet**: requests from Zach that got no answer, from one source: the email dispatcher's
  watchdog (see "Unanswered requests"). Each says why, in the watchdog's plain words when it gives
  them ("no answer yet: from before the watchdog started"), and can be dismissed. A card's `noReply` timing is a metric
  only: a card that landed is shipped even when its reply is missing, one that ended with neither a
  landing nor a reply is not, and neither is listed here, since the watchdog flags its request.
- **Working on it**: one plain line per card ("building", "checking the design", "waiting on <title>"),
  stuck ones first: board-only waits (the factory's own trouble, finished or hand-moved cards),
  stalls and errors.
- **Shipped today**: cards that reached Done today (Zach's time zone) with a landing or a reply, plus
  Paseo-started (`app`) cards that finished on their own (`finished in Paseo`);
  dropped cards are left out. Each pass records finished cards in `digest.json`, so they stay
  listed all day after Done cards leave the board (12 hours).
  When a Paseo-started (`app`) agent is idle after a final assistant report (not asking a question or
  decision), or errored after sending a reply email or landing its work on `origin/main`, the board
  moves the card to Done (`finished in Paseo`) instead of Stuck or For you without writing
  `factory.stage=landed` to Paseo (so personal sessions are not reaped after 5 minutes and return to
  Building if resumed); a real error with no delivered result stays in For you as
  "Waiting on you: error".
- **Routine runs (N)**: scheduled runs started today, shown only when the filter is on.

The list never reads a session file or log. Opening a card reads them once: the card starts with
Zach's own request, what it needs from him (or what is happening), the preview and the buttons;
ids, branch, worktree, model, timeline, sub-agents, the operator buttons and the latest messages are
folded under Details. On the phone the header is the title, the For you count and a menu (filter,
New card, Usage); there are no stage tabs, no sideways scroll and no activity ticker (the For you
view has none at any width). On a desktop, "All columns" shows the kanban, with the same buttons on
each Needs Zach card.

### Answers

Every ask has one fixed set of buttons (`factory/choose.go`), the same on the board and by email:

| Ask (`Ask.Kind`) | Buttons |
| --- | --- |
| Finished work (`ready`): Ready for you | **Land** / **Change** / **Drop** |
| A secret-scan stop (`decision`): fix first, then scan again | **Tell it to fix** / **Scan again & ship** / **Drop it** |
| Decision (`decision`): check what was published without review | **Yes** / **No** / **Change** / **Drop** |
| Which repo (`decision`) | one button per repo, Other repo, **Change**, **Drop** |
| Work landed with a required action for Zach (`action`) | **Done it, mark done** / **Something's wrong** / **Skip it** |
| An agent's open question (`question`) | its options, **Answer**, **Drop**: not a yes/no question, so no Yes or No |
| Board-only (`problem`): stuck, errors, a review that did not settle, a close suggestion | its own action (Re-run, Resume, Let an agent decide, Close it, Mark done) with Change and Drop, in the card's details |
| Agent work left after landing (`problem`) | **Send it back to finish it** / **Mark done** / **Drop it**, in the card's details |

One tap answers: No sends the agent a plain "No.", the same as replying `no`. Drop takes a second
tap in place. Change, Answer and Other repo open a text box with Send, and need words. On a decision
or an open question the options start on the recommended one, which Yes or Answer sends; on finished
work none is picked, since Land sends no answers: picked options go through Change, and the card says
so above them ("Optional: tap an option to send it with Change. Land ships what is built as it is."). With one
question, tapping an option answers at once. The board posts `POST /api/choose` (`id`, `choice`, `message`, `repo`), and an email reply is
parsed into the same button (Needs Zach email below), so both do the same thing. Either one moves
the card out of Needs Zach; the other surface then finds nothing waiting (the dispatcher gets 409)
and does nothing.

Every ask also has a label saying what kind of input it wants, shown above the question on the
board. `factory/askcheck_test.go` builds every ask and fails when one has no label, Yes / No outside a
decision, a Land button outside finished work, a main button that approves what its ask does not ask
for, a reply box as the main answer to landed work, or a button nothing runs; it also counts the places
that build an ask, so a new one must be added there first. A landing the factory wants checked (not
what was reviewed, published without review) keeps its label and buttons even when the agent asks
Zach to try it. Answer buttons share a row evenly, are never narrower than their longest word, and
never split one.


After landing, optional use, feedback and optional observation awaiting a future live event belong
in the summary, with no decision or needs ask and `REMAINING: none`. Required acceptance checks,
unfinished deployment and required settings remain unfinished work. Pre-landing previews keep
their test asks and approval choices. A PDW mutation already queued for his review has its own
Approve button in email and on the PDW review page. Mention it in summary prose, not as a `Zach:`
item or in NEEDS FROM ZACH; write `DECISION: None.`, `NEEDS FROM ZACH: Nothing.`, and `REMAINING: none`
when that is all that is left. Error recovery applies the same optional-try completion rule.

Agents keep the `DECISION:`, `OVERVIEW:` and `NEEDS FROM ZACH:` block (`factory/overview.go`).
A decision or needs line that says nothing ("None.") is dropped on a card that asks for something,
so the ask's title leads instead. The decision line is the question shown; the overview is shown under it in the card; the full report
is folded. Existing `QUESTION:` blocks provide up to four questions, with the recommendation first.

For visible changes, agents save useful screenshots or a webp video in the card worktree's `.factory-evidence/`
folder, excluded from Git before saving. Never commit or force-add evidence. After the overview
and a blank line, optional whole lines name the files:

```text
BEFORE: before.png
AFTER: after.png
```

Each key occurs at most once. Filenames contain letters, digits, underscores or hyphens and a
`.png`, `.jpg`, `.jpeg` or `.webp` extension. Images must decode as PNG or JPEG, or be a WebP container whose
canvas can be read, at most 5 MB and 20 megapixels. An animated WebP is how a builder shows a UI working. Copy captures from another machine into the worktree before referencing them.
For text decisions, show the exact before and after text in the report instead.

Factory serves only the current ask's named images at `/api/evidence/{card}/{before|after}`
under the board's existing access boundary. It rejects symlinks, paths, special files, unsupported
formats and oversized images or videos; it disables caching and content sniffing. The card accepts images
only from these generated routes, including inside report Markdown. No public image service or
external image URL is used. Images become unavailable if their worktree or file is removed.

### Daily digest

No daily digest is sent, whatever `FACTORY_DIGEST_TO` holds; the setting sends nothing. The
board is where the day's work shows. `/api/health` reports no digest state.

- Finished cards are still recorded each pass in `digest.json` in the state directory
  (`factory/digest.go`): the board's Shipped today reads that ledger. The ledger keeps a line
  for a week, so a finished card stays listed after it leaves the board. The file also keeps
  what each digest sent earlier numbered, so a reply to one still answers by number, below.

#### Answering the digest by number

No new digest goes out; a reply to one sent earlier is still read this way.

- An ask was numbered only if it was emailed, its go answer is Land or Yes and does not close the card,
  it has no questions (Yes would take the recommended options), and its builder is not still writing.
  Agents' open questions, repo choices, Done it actions and approvals with questions stay unnumbered.
- `digest.json` keeps what each sent digest numbered for 30 days: the card, its title, its
  `factory.since`, the question line, its choices, and the thread the send returned, so a number means
  what that digest showed and never shifts. A send that surely failed removes the list. One that may
  have gone out has no thread, so a reply to it is not recognised (404); the same holds for a reply
  after those 30 days.
- `POST /api/answer` checks a reply's thread against these lists before any card. The whole reply must
  read as word then numbers (`yes 1 3, drop 2`, `approve 3 and 5`) or numbers then word
  (`1 yes, 2 drop`). The words are the approve, land and drop lists of the table under Needs Zach
  email, plus `no`. A bare `1 2`, `approve all`, free text, an empty reply, or one number with two
  answers changes nothing, and one reply says so. A word is required because a landing cannot be taken
  back. No text from these replies reaches an agent.
- Each number is refused if it is not in that digest's list, if the card is gone, replaced or no longer
  waiting on Zach, if it no longer passes the numbering rule above, if its `factory.since`, question
  line or choices differ from what was stored, or if the card does not offer that word (`land` on a
  Yes ask). Otherwise it runs the same choice as the card's own email answer, through the same gates; a
  gate's error is reported as refused with its reason. It clears any saved clarification for that card.
- The message id is stored before anything acts, so a repeated POST answers `duplicate` and sends
  nothing. A stop partway leaves the remaining numbers unanswered and still waiting on the board.
- Replies are handled one at a time, so board answers wait while a reply's landings scan.
- One reply in the thread lists what was done and what was refused. A failed send is logged and not
  repeated. A card answered this way does not take the digest's thread, so later replies there stay
  the digest's. `POST /api/answer` returns 200 with action `digest` (with `done` and `refused`
  counts) or `digest-unparsed`.
- The subject prefix `Factory today: ` is a contract with the email dispatcher: a reply with it and no
  matching card goes to `/api/answer` first, and on 404 continues as usual. A quiet-delivery filter
  on that subject must leave "Re:" messages alone, or it hides what was done and refused.

### Needs Zach email

The factory starts no email of its own. A card that waits on Zach is emailed only in a thread
Zach started (`gmail-thread`), by the factory itself through the zrl-bot mailbox
(`pdw call zrl_bot_email_control__*`, `factory/mail.go`), once per wait reason; every other card
waits on the board only, and no told-family label is set for a send that never happens. A
question with a `DUE:` time also gets one reminder per due time in its thread
(`factory/due.go`). Each ask on the board is either emailed (in its thread) or board-only
(`Ask.boardOnly`):

- **Emailed in its thread:** try finished work before it lands, the secret
  scan's stop, a public-repo push without review, a landed diff on a public repo whose secret scan
  did not pass (something unreviewed may already be public), a repo choice, and a direct result's
  question, though its work has already landed.
- **Board only:** finished cards and cards moved by hand; a running agent (permission prompt, stall,
  writing its summary); a card marked stuck by automatic recovery; a paused decision; a review that
  a reviewer's close or
  duplicate verdict; and the build track's checks after landing (work left, did it finish, an older
  landed diff).
  Errors, usage limits, failed reviewers, stopped cards, dropped dependencies and failed landings
  never reach Needs Zach at all (automatic recovery). These are problems for the factory and its
  agents; the board shows them under Working on it.

The email is the decision alone:

- The subject is the question: the agent's `DECISION:` line, or the ask's own question when the
  factory raised the stop or the agent decided nothing. A decision that names no change uses the
  card's title ("Land: <title>?") and also opens the body. Only the subject line cuts a long decision,
  at 90 characters; the body then opens with it whole, and the board shows it whole.
- The body is under 60 words, not counting the board link or the questions: the due time first when
  the question has one, then the recommendation ("I recommend yes.", or "I recommend A." for one
  question), what it is about, as many whole sentences of the agent's overview as fit, and the
  one-word replies. Inline `Before` / `After` screenshots from `.factory-evidence/` follow, then an
  Open board button. The full report stays on the board.
- Questions and their lettered options have their own 60 words and print in full. If they do not fit
  together, or any has no letter or digit left once stripped, none print: the email says "1 question is too long for email.
  Open the board to answer." and offers no letters, so a reply cannot answer only some of them. It
  offers no `yes` either, and a `yes` or `land` reply does nothing but say to answer on the board (or
  land it there, at Ready for you), so
  one word never approves an option he has not read. An answer to his feedback prints its questions
  the same way.
- Agent text in it passes through `plainText`: backticks, commit hashes, agent ids, file names and
  the owner of `owner/repo-name` slugs are removed. Factory copy avoids pipeline words (card, track,
  landing, Fellow), and agents are told to write the same way for anything Zach reads by email.

- Inbox cards answer in their own thread; that thread is the only place a card is emailed. A card
  with no email thread is never emailed: neither a factory-started thread
  (`factory.zach-thread`, kept from earlier mail) nor `FACTORY_ZACH_EMAIL` sends anything for it.
  `FACTORY_ZACH_EMAIL` still addresses replies in a factory-started thread once an email answer
  has made that thread the card's own, below. If sending with inline screenshots fails, it retries
  once without them.
- The first email answer that acts on a card with no email thread stores the answered thread as its
  `gmail-thread` (`factory/answer.go`), before the answer runs, so its land message tells it to report
  there and later stages carry it. A reply that only gets a clarifying question or starts a separate
  card stores nothing, so that card's later waits stay on the board.
- `factory.told` holds the wait already emailed; it is cleared when the card leaves Needs Zach.
  `factory.feedback-told` holds the feedback exchange whose goes-ahead answer was emailed, and is not.
  A wait where the builder was just asked for a decision or preview (`factory.writing`) is emailed
  once it has written it (at most 10 minutes later).
- `factory.permit-told` holds the move (`<stage>@<since>`) whose stuck permission prompt was already
  emailed (`factory/permit.go`); a later move that gets stuck again is emailed again.
- `factory.due` holds when a direct result's question is due, and `factory.due-told` the due time Zach
  was already reminded of (`factory/due.go`), so asking again with the same time does not resend and
  a new time reminds him once more. The reminder goes in the card's thread like any later wait.
- A failed send is retried after 5 minutes; a timed-out one is not, since it may have gone out.
- Cards that were waiting in Needs Zach for a reason automatic recovery now handles take the
  recovery path on their next pass.
- `/api/health` reports the last mailbox check as `mail`; it never fails the health check.
- Every factory email ends with the board link (the card's own when it is about one), added on the
  one path all of them take (`sendMail`); a body that already carries the link keeps it in place.
  Email cards are told to end their landing email with it too.

Replies: the email dispatcher checks the sender, then posts his reply to
`POST /api/answer` (`thread`, `message_id`, `text`; 404 when no card or digest has the thread, 409 when none
of its cards waits, or when two or more do: then the reply could be for either, so the dispatcher
passes it on as a follow-up). A reply in a digest's thread is read by "Answering the digest by number" instead. Quoted text and signatures are cut first. Only a whole reply from these lists
acts:

| Reply | Does |
| --- | --- |
| `approve`, `approved`, `yes`, `y`, `ok`, `okay`, `lgtm`, `go`, `go ahead`, `ship it` | Yes (Land, Done it, or Scan again & ship on a secret-scan stop); on an approve with questions, it takes the recommended (first) answers. Does nothing when the email left the questions out |
| `no` | No |
| `done`, `mark done`, `mark it done`, `works`, `it works` | Mark done, when the ask has it |
| `drop`, `drop it`, `close`, `close it`, `cancel` | Drop, when the ask has it |
| `land`, `land it` | Land, on Ready for you (approval words and drop words also act; anything else is Change). Does nothing when the email left the questions out |
| `1B 2A` (only numbers and letters of the ask's questions, whether or not the email printed them), or one letter when there is one question | answers them, with Yes when it approves, else through the text choice |

Other free text receives a tool-free relevance check with a 12-second deadline and no retry.
Related answers and changes use the ask's text choice, including Change on Ready for you;
whole approval, land and drop words bypass the check there.
Unrelated requests start independently with Repo Auto, without the waiting thread.
Unclear or mixed replies and failed checks leave the waiting work untouched. The factory saves
the reply privately and asks for whole-reply `answer` (send the saved text) or `separate`
(start it independently). This question is a request for clarification, not a success confirmation.
Exact offered choices replace the saved clarification. New free text replaces the saved reply
and receives a new check. A changed question or a board answer discards the clarification.
If starting a separate request fails, the reply gets the same failure notice as other failed answers.
Dry-run never invokes the checker: exact choices log their intended actions, and free text
takes the clarification path without saving it.
Handled replies return HTTP 200 with action `send`, `separate`, `clarify` or `duplicate`
(exact choices keep their existing action names); unmatched threads retain the fallback statuses.

Each reply runs through the board's buttons
(`choose.go`), so it does exactly what that button does, and a word is offered and acts only where
its button exists; a reply with no button passes on as a follow-up. Each message is applied once
(`factory.answered`). A reply that worked gets no email back (the board shows the new state); one
that did nothing gets one line in the thread saying so, with the board link. An emailed approve of
a public-repo landing still runs the secret scan first.

#### Prompt blocks parsed from email cards

The email dispatcher writes Zach's own words into a card's follow-ups in these blocks, and the
factory reads them from the card's Claude session file to show the card's request and follow-ups
(`/api/peers`, reviewer prompts; `factory/peers.go`). The same text is in the dispatcher's README.
Change both or neither. The first prompt is the board's, with the request under `Zach's request:`.

- `Zach's email:` was the header line in the first prompt of cards the dispatcher started itself.
  The factory still reads it, and an old card's `- Subject:` line, for those cards' history; remove
  both readers after 2026-11-27, when such session files are 30 days old.
- `Zach's follow-up email (<RFC 5322 date>):` is the header line in each steered follow-up.
- `(end of Zach's email)` closes either block.
- Between the header and the end line: Zach's own words, with the quoted history cut.

Each line must be whole (from the start of a line, nothing after it), the date must parse, and a
block without its end line is ignored, so text that only quotes this format is not a follow-up.
Only user messages count, not tool output. An email card with no `Zach's email:` block (older
cards, or an empty body) uses its `- Subject:` line. A Paseo-started (`app`) card with no factory
prompt block uses the agent's first user prompt as its request (unwrapping Claude slash-command
`<command-args>` and skipping Codex system preamble messages). A follow-up overrides the first request where
they conflict. Only Claude session files are read again as they grow.

The factory writes one block of its own, not part of the dispatcher's contract: a closed duplicate's
request, under `Zach's request from closed duplicate card <id> (<RFC 5322 date>):` and closed by the
same end line. It holds only Zach's words (the reviewer's notes follow the block), and it adds to the
kept card's request rather than overriding it.

### Unanswered requests

The email dispatcher keeps a ledger of every request Zach sends it (each email and follow-up, each
voice-memo task) and checks that each got an answer in its thread. Those that did not, and that
nothing is working on, it writes to a JSON file whose path is host config
(`FACTORY_UNANSWERED_PATH`): `{"updated": "<RFC 3339>", "requests": [{"key", "source", "thread",
"asked", "subject", "why"}]}`. The dispatcher is the file's only writer and rewrites it whole.

The factory reads it into the board's **No answer yet** list (`factory/unanswered.go`) and
serves it at `GET /api/unanswered`. Dismiss (`POST /api/dismiss-request`
with `{"id": "<key>"}`) records the key in the factory's own `dismissed-requests.json`, never in
the dispatcher's file, so a dismissal survives a restart and the dispatcher's rewrites, for as long
as the dispatcher lists the request (a key it no longer lists is dropped a month after its
dismissal). A missing or unreadable file lists nothing. A card that ended with no reply is not listed
here or on the board (the watchdog flags its request); if it landed, it is still shipped.

### Card timing

Every factory card and top-level request started directly in Paseo (source `app`)
records its times in `timings.json` in the state directory, so a restart keeps them
(`factory/timing.go`). Reviewers, Scouts, repo pickers and anchors are excluded. Entries are dropped
30 days after their last event. All stored and exported timestamps use UTC with `Z`.

- **Asked**: the email's `- Date:` line or the voice memo's `- Recorded:` line in an email card's
  first prompt; else the card's creation. App requests use agent creation and retain the first
  user prompt internally. Missing transcripts are retried; request text is not exported.
- **Columns**: each column the card entered, read from the board the factory already builds, so it
  costs no paseo call. Only reads the board accepted count. A move is dated by `factory.since`
  when it set one, else when the factory saw it (within one poll).
- **Reviews**: each round's start (the reviewer's creation), end (its verdict applied) and verdict.
- **Landed**: the time the final landing was verified, using the card's own linked-worktree commits
  made after Asked. Earlier landed events remain in the history. The commit must be on the card's branch and main; a rebased or squash-merged version
  must match the original patch. Errands that commit nothing have no landing time. Failed Git reads never
  acquire a landing time or substitute another card's commit.
- **Replied** (email and voice cards): 5 minutes after Done, one read of the card's thread takes
  the first eligible answer for voice cards, or the last for email cards, sent after Asked and
  by Done plus 5 minutes. The first message of a voice thread is its filing email and never
  counts. Messages whose known recipients are only the sending mailbox also never count.
  Missing or malformed recipient metadata does not disable the voice filing exclusion. The factory's own
  emails (Needs Zach notes, failed-answer notes) are recorded when sent and never count.
  A separate per-record voice reply revision clears retained voice reply results once, including
  exhausted retries, and rechecks them through the existing bounded checker. The revision persists
  across restarts without rerunning the landing migration. Failed reads leave the stale reply cleared.
  Reply checks also run for errands that commit nothing. A completed inbox card with no confirmed
  reply has `noReply: true` in saved timing, board timing and the timing export; the Timeline says
  “no reply”. This includes exhausted mailbox retries or a missing thread. A later reply outside
  the original window is not attributed to that card; it remains excluded from completed metrics.
- **Asks** (`asks`): how many times the card entered Needs Zach, counted from the columns above, so
  every retained record has it. Short stays and hand moves count.
- **Closed** (`closedAt`): when Zach closed the card by Drop, Mark done, Archive or a drag to Done,
  recorded at the action. The card's Done event gets the same time, so a parked card closed later ends
  at the close rather than at its park. Closing a card already in Done changes nothing, and moving a
  card out of Done clears it. `noReply` with `closedAt` means Zach closed the card; `noReply` alone
  means it went unanswered.
- **Partial**: a card first seen past its start (in flight when timing began, or created while the
  factory was down) has an incomplete history.

A per-record version makes the retained-state migration restart-safe. It normalizes timestamps,
merges duplicate session rows with their attribution history, and uses the final recorded landed
event for a verified historical landing. Missing evidence preserves uncertain history; migration
never substitutes its current clock. Previously verified records use their final landed event
without requiring a surviving agent or worktree. Unverified records inspect at most three
worktrees per tick and remain eligible for retry when evidence is unavailable. A landing is
cleared only when it precedes Asked or a complete worktree reflog proves there is no own commit
after Asked. Read-only and dry-run instances do not run reconciliation. Retained agents can
supply missing app requests; removed agents cannot recover lost prompts or creation times. An app first observed after its start is
partial, including finished requests; no earlier stages are invented.

A separate per-record close revision gives retained records a `closedAt` once, without rerunning the
landing migration, by reading the agent files back 31 days. A card whose note says Zach closed it gets
its Done time. If that Done time equals its archive time, it stayed at least 30 minutes in Needs Zach
before it (the least a card idles before parking), and its agent file was written later, the card was
parked and closed later: `closedAt` and its Done event both move to that write, the latest the close
can have happened. Landing times do not change. Records with no note or no agent file keep `closedAt`
unset, and so do past Archive and drag actions, which left no note. Read-only and dry-run instances
do not run it.

The card panel shows it as one Timeline line. `GET /api/metrics?days=7` (1 to 30) gives the
count, arithmetic mean (`average`), `median` and `p90`, in seconds, for:

- **Card turnaround** (`turnaround`, `bySource`, `byRepo`,
  `bySourceRepo`, `byAgent`): `endToEnd` (asked to replied, else to landed), `toLanded` (asked to
  landed), `toDone` (asked to Done), and `zachWait` (time in Needs Zach up to Done).
- **Per-step durations** (`steps`, `stepsByRepo`): `cards`, `visits`, `repeatedVisits`,
  `visitSecs` (per visit) and `cardSecs` (summed across visits per card) for each step: `building`,
  `gate`, `landing` and `zach`, plus `request`, `design`, `signoff`, `plan`, `plan-review` and `review` from older cards.
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
Query parameters (`repo`, `source`, `role`,
`provider`, `model`, `step`) filter the results (`?source=email`, `?repo=owner/name`,
`?role=reviewer&step=review`).

#### Session links and transcript turn timing

Each card has one session object per decoded native `sessionId`. Its `history` array retains
distinct associations in observation order, including the current one. Top-level metadata reflects
the latest association and the earliest known creation time. Attribution readers expand history
so changes of step, model or agent preserve their meaning.


The same timing records keep the repo and a history of observed agent-to-session links for the
Fellow, reviewers and nested Paseo sub-agents. A link contains the Paseo agent and immediate
parent, device, provider, native transcript session id, model, role, review kind and card step.
Session ids use the same decoding as the transcript reader, including plugin providers.

`GET /api/timings` is a read-only snapshot of the timing store. It returns `{"cards":[...]}`,
sorted by card id, with `id`, `repo`, `source`, `partial`, `requestedAt`, `requestedFrom`,
`landedAt`, `repliedAt`, `noReply`, `asks`, `closedAt`, `doneAt`, `events`, `rounds` and `sessions` when available. It excludes
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

Factory decisions also read session files only: reviewer verdicts, tracks,
landing replies and card names never load an agent through Paseo logs. If required history
is unavailable, the factory retries for two minutes after the turn stops, then moves the
card to Needs Zach. It leaves the review unapplied and does not infer a verdict.
Outside landing-review requests instead finish with an escalation and a history-unavailable
finding after the same grace period; their requesters are not moved onto the board.
Naming alone can wait for a later readable reply.

## Public repos: review and secret scan

Repos listed in `FACTORY_GUARDED_REPOS` (default: this one) are public, so everything that lands
in them is published. For their cards:

- After the gate passes, the factory runs the secret scan on `origin/main..HEAD` in the card's
  worktree. It uses its own installed binary and the gitleaks config from `origin/main`, never the
  card's copy, so a card cannot weaken the scan that checks it. A hit, or a scan that cannot run,
  stops the card in Needs Zach; nothing is published.
- After a clean scan the gate's sign-off decision applies, as in any repo: what Zach asked for lands on its
  own, and what the system started comes to him first, except a scheduled run of a job he already approved. A
  landing runs `bin/land`, which scans again before its push, as does the pre-push hook. With no sign-off no person
  approves the landing: what keeps private data out is the gate, which grades public safety, and the scans. Landing a card at Ready for you
  scans again before the land message.
- The direct track is off (a first pass that says it took one is scanned and stops for Zach), and reviewers get a public-repo rule
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

## Board layout checks

Use the checked-in headless helper for this board's layout checks. It builds the current
`cmd/board-preview`, serves the real embedded page and API with fake Paseo/session inputs,
then captures **1280x800 desktop** and **390x844 phone** viewports. It never connects to the
live board or reads real card/state files. No pipeline runs. Supported buttons simulate changes to fabricated cards in memory; they never
invoke the real action pipeline, start agents, run Git, send messages or write production data.
No answer yet lists two fabricated requests from a sample dispatcher file in the preview's own
temporary state; their Dismiss runs the real dismissal there, never on that file.

First exclude local outputs, then install the pinned development dependency:

```sh
printf '\n.factory-scratch/\n.factory-evidence/\nnode_modules/\n' >> "$(git rev-parse --git-path info/exclude)"
npm ci --ignore-scripts --cache .factory-scratch/npm-cache
npm run board:screenshot
npm run board:screenshot -- --card=preview-zach
npm run board:screenshot -- --view=columns
```

The helper uses Playwright's default shared browser cache. If Chromium is missing, it prints
`npx playwright install chromium` and exits; it never downloads a browser automatically.
Provision a missing browser through the host's approved setup process, outside a card build.

Inspect both `.factory-evidence/desktop.png` and `phone.png`, plus their `.txt` page text. Both
open on For you; `--view=columns` shows the desktop kanban instead (the phone always shows For you).
When a For you card has questions, `phone-questions.png` also captures its visible choices at
390x844, closing the panel and scrolling to the question so the choices are not hidden below the
screen.
Each run replaces those files and `capture.json`; copy useful captures to distinct names before
another run. Evidence stays excluded from Git. `--out` chooses another excluded output folder.
An unknown view or card, an incomplete load, a page error, a failed request, or a phone page that
scrolls sideways fails the capture.
The helper owns and stops its preview server and browser, including on failure.

Fixtures use IDs `preview-<stage>` for agent stages: `route`, `building`, `gate`, `zach`, `landing`,
and `landed`. The Ideas fixture is `idea-preview`. The Needs Zach fixture
(`preview-zach`, Land / Change / Drop) is a change the gate sent to you for sign-off, with its reason;
`preview-ready` is finished work with Land / Change / Drop, its own synthetic request and changes, and an
optional question; `preview-action` is work that landed with a required approval (Done it / Something's wrong / Skip it). Every fixture session starts with a synthetic request, so a card opens on its ask.
`preview-unverified` shows a publication verification stop with Re-run, Reply and Drop.
`preview-direct-ask` is a one-pass result that stopped on a question with a due time (Answer / Mark done / Drop).
Their buttons are simulated through the same `choose` route the board posts, and move the card where the
real action does: Land or Yes to landing or building, No, Change or an answer where a real message
sends it (`answerLabels`), Drop to done. The page is served with `<html data-mode="preview">`, which
shows a fixed notice that the cards are samples and buttons change only the sample; the real board
never has it. Real API serialization and Ask/transcript parsing build the preview;
when a UI needs a new scenario, extend the synthetic inputs in `factory/preview.go`.
Run `npm run test:board` for browser regression checks.

For computer use, serve the same fake-data preview on the host's tailnet IP:

```sh
mkdir -p .factory-scratch
TMPDIR="$PWD/.factory-scratch" go build -o .factory-scratch/board-preview ./cmd/board-preview
.factory-scratch/board-preview -listen "$(tailscale ip -4):0"
```

Bind direct remote checks to the tailnet IP, never `0.0.0.0`. The command refuses wildcard
addresses. The command prints its address. Append `/?card=preview-zach` to open a card, and
`view=columns` to show the desktop kanban for that visit (it is not saved; the phone always shows
For you). Query selections apply once after loading, so closing a card keeps it closed during polling. Existing `#<id>`
links still work. Invalid query values keep the normal view without opening an unknown card.
Stop the server when finished; it removes its own temporary fixture directory.

The repo's `.factory.json` declares the same fake preview using the real `-listen` and `-scratch`
options. The factory supplies an explicit loopback `$HOST:$PORT`, isolated `$HOME`, and a private
preview link; its command refuses missing host/port values. Interactive buttons only change sample
cards for that preview process, and restarting restores the fixtures. New cards and agent launches
remain disabled. No real card or production service is used. The board's clock is frozen for these
fixtures in the browser as well as the server.

The factory trusts preview declarations from `origin/main`, so this declaration bootstraps with
screenshots before it lands. After landing, subsequent real Ready for you cards use the declared
live Preview. Verify that lifecycle on a later genuine Ready card; do not create a production card
just to test it.

**Keep computer use for changed interactive flows.** Screenshots verify layout, not approval,
Drop confirmation, or other actions. Read the current computer-use skill, click through changed
flows, read page text, and inspect screenshots. To verify actual action behavior beyond the preview simulation, build the current `cmd/factory` under `.factory-scratch/` and use the documented
`-dry-run -listen "$(tailscale ip -4):0"` mode. That mode reads real data but only logs writes;
never test against the live service. Plans and builds must declare this real-data dry run for build
approval. Code and landing reviewers may run it as a documented read-only check without a new approval.
Close your tabs and stop your servers. Report failed or skipped checks explicitly. Remove `.factory-scratch/` before finishing.

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
or a pending gate. Dropped, replaced, held and parked cards cannot publish.

An agent outside the card flow can also run `land-review -why "..."` (a link to the factory
binary) once its work is committed. It exits 0 on approve, 2 on changes, 3 when Zach must
decide. The same completed approval can be reused for the same commit and checkout.
An unavailable or rejecting review stops publication. The secret scan still runs on every
push. Agent pushes can publish only the checkout's reviewed HEAD.
Each landing attempt pins the fetched main commit before rebasing and scanning. If main
advances during review, a rejected push retries from a new fetched commit with another review;
shared remote-tracking refs cannot hide that change.
Before its long steps, an agent's landing command records its process ID and start time on the
agent. On the factory's host it waits while that exact command runs, for at most two hours, so an idle agent
or a progress note does not count as a failed landing. If recording the run fails, nothing lands.

### Host config

`~/.config/agent-factory/env` (the unit's `EnvironmentFile`; flags of the same names work too):

| Variable | Flag | Meaning |
| --- | --- | --- |
| `FACTORY_BOARD_URL` | `-board-url` | board link put in emails (none compiled in) |
| `FACTORY_DEFAULT_REPO` | `-default-repo` | `owner/repo` for cards with none given; errands are recorded there (required for cards with no repo) |
| `FACTORY_GUARDED_REPOS` | `-guarded` | comma-separated public repos (default this one) |
| `FACTORY_PRIVATE_RULES` | `-private-rules` | private scan patterns (default `~/.config/agent-factory/denylist`) |
| `FACTORY_LANES` | `-lanes` | per repo: board lane name, how it lands and idea caps, `{"owner/repo": {"name": "...", "lands": "...", "ideas": {"inFlight": 2, "daily": 5}}}` (default `~/.config/agent-factory/lanes.json`) |
| `FACTORY_IDLE_TTL` | `-idle-ttl` | park a Needs Zach card idle this long (default `3h`) |
| `FACTORY_IDLE_CAP` | `-idle-cap` | park the longest idle Needs Zach cards while more factory agents than this are idle (default 10) |
| `FACTORY_STAGE_HANDOFF` | `-stage-handoff` | `on` (default): each new stage of a card starts a fresh agent with a short handoff; `off`: resume the card's session |
| `FACTORY_INBOX_SOURCE` | | the email dispatcher's `source` label value on its agents (none: no dispatcher) |
| `FACTORY_INBOX_ANCHOR_ROLE` | | the dispatcher's anchor agent `role` label, hidden on the board |
| `FACTORY_UNANSWERED_PATH` | `-unanswered` | the dispatcher's JSON file of requests with no answer (none: the No answer yet list is empty) |
| `FACTORY_ZACH_EMAIL` | `-zach-email` | the address replies in a factory-started thread use once an email answer makes it the card's thread; a board card is not emailed (Needs Zach email) |
| `FACTORY_DIGEST_TO` | `-digest-to` | sends nothing: no daily digest is sent (Daily digest) |
| `FACTORY_PREVIEW_PORTS` | `-preview-ports` | `lo-hi` ports Ready for you previews run and are published on (default `7700-7799`) |
| `FACTORY_PREVIEW_MAX` | `-preview-max` | previews running at once (default 8) |
| `FACTORY_METRIC_HOSTS` | `-metric-hosts` | comma-separated SSH aliases whose CPU, load, RAM and disk the board's host icon shows besides this host (none: this host only) |
| | `-listen` | address to serve on (default `127.0.0.1:<port>`); `host:0` picks a free port |
| | `-dry-run` | real data, no changes: only paseo and git reads run, every write is logged to Activity, no Tick, no email, state copied to a temp dir that is deleted when it stops |
| `FACTORY_ARCHIVE_CLI` | | separate Paseo executable for archive writes (default: `paseo` on PATH; stable version >= 0.13.0 required) |
| `PATH` | | must reach `claude` (authenticated for the tool-free email relevance check), `paseo`, `node`, `git`, `gitleaks`, `tailscale` (previews) and `pdw` (logged in) |

Before deploying email relevance checking, verify that the factory service's PATH resolves
`claude` and that its service account can run an authenticated, tool-free structured check.
Missing access leaves free-text replies awaiting clarification; exact choices still work.
Do not install or authenticate tools as part of a build.

Cards can start in any checkout under `~/dev/<owner>/<repo>` (when that folder is missing, the
factory finds the checkout under `~/dev/*/*` whose `origin` git remote matches `owner/repo`).

### Computer use for agents

Builders get computer-use instructions on both tracks, including web research, and must click through
their own UI changes before the gate. The gate and landing reviewers get the same setup instructions
when a change touches a UI. Repo pickers, Scouts and the other gates do not get browser instructions.

Agents use the logged-in `pdw` CLI and read the current `openai-computer-use` skill for connection
names, tools and recovery steps. The browser runs remotely; a test server must use the execution
host's tailnet address. Open sessions only when needed, close owned tabs and stop test servers when
finished. Report unavailable access or gateway capacity honestly; reviewers may skip with a reason.
Computer-use access keeps the role's existing action approvals and read-only restrictions.

## Layout

- `cmd/factory`: the binary (board server, `factory scan`, `land-review`).
- `cmd/factory-deploy`, `deployer/`: the self-deployer; `deploy/`: the systemd units.
- `factory/policy.go`: prompts, verdicts, the automatic moves (`Tick`). `factory/gates.go`: the gate, its grades and the flow.
- `factory/guard.go`: guarded (public) repos. `factory/scan/`: the secret scanner.
- `factory/signoff.go`: who sees finished work before it lands. `.factory.json`: this repo's preview.
- `factory/route.go`: Repo Auto. `factory/agents.go`: the agent choices. `factory/reap.go`: the idle reap.
- `factory/board.go`, `collect.go`, `ask.go`, `ideas.go`, `inbox.go`, `actions.go`,
  `transcript.go`, `server.go`; the board and transcript pages are in `factory/web/`.
- `factory/transcript.go`, `transcript_providers.go`: the transcript page, the board's card summary,
  Needs Zach text and email, Close, Retry and gate reads use each agent's own session file
  (Claude `~/.claude/projects`, Codex `~/.codex/sessions`, Antigravity
  `~/.gemini/antigravity-cli/brain`, Muse `~/.local/share/muse/sessions`). With no readable file
  they say "history unavailable" and why; they never call `paseo logs`, which loads the agent in
  Paseo (resuming an archived one). Decision reads use the same session-file readers.
  `/api/transcript/{id}` sends pages of about 300 KB at whole lines: `tail=1`
  is the newest page, `before=N` the page before line N, `after=N` new lines to poll (`more` means
  read again now; a line still being written waits until it ends). Where each line starts is kept for
  the last 64 session files read, with their size and mtime, so a request scans only the bytes added
  since the last one for line ends. A shrink, an earlier mtime, a new mtime at the same size, another
  file at the path, or a line that no longer starts and ends at a newline rebuilds it from the start.
  Images carry a `ref` instead of their data and are fetched from `/api/transcript/{id}/image/{ref}` (PNG, JPEG, GIF or WebP, for
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
