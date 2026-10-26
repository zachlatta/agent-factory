# agent-factory

A Kanban board and review pipeline for coding agents run by Paseo. You throw
any request at the start of the queue; an agent plans it, other agents review the plan and the
code, and the work lands on its own or waits for your approval, depending on how risky it is.

One Go binary. It reads Paseo's agent files every 15 seconds, applies the rules below, and calls
the `paseo` CLI for every move (labels on agents are the state: `factory.stage`, `factory.tier`
and so on, see `factory/labels.go`). It serves the board on `127.0.0.1:7478`; the deployer puts
it on the tailnet with `tailscale serve`.

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
  given by an idea or the email dispatcher are kept, and other titles are cut at a word.

| Tier | What | Gates | Lands |
| --- | --- | --- | --- |
| routine | scheduled runs of an approved job | none per run; surfaces only if blocked | itself |
| low | docs, copy, an isolated fix, tests, a small feature on existing patterns | plan review, code review | on its own |
| risky | prod data, deletes, migrations, money, auth/secrets, hosts/deploys/cron, messages to others, unattended jobs | plan review and code review both pass | on its own |
| architecture | new service/datastore/framework/dependency, shared interfaces, any refactor or cleanup, hard to undo or public | plan review, then a person approves the plan, then code review | on its own |

- Refactors and cleanups must cover the whole repo (or list what is left and why); a partial
  refactor is a blocking finding.
- Plans and build summaries name their deploy steps (if any). Builders make no live changes
  (install scripts, enabling or restarting services, resizing VMs, external settings) before code
  review: the code reviewer checks the steps, and the landing step runs and verifies them after pushing.
- Circuit breakers: 3 review rounds per gate, then a person (at the plan-review cap, a last round
  whose only blocking findings the reviewer marks with `SMALL FIX:` goes to Building with those
  fixes applied and verified by code review; design disagreements and large blockers go to Needs
  Zach); reviewers may answer `nits` or `close`; a reviewer that ends or disappears without a
  verdict is re-run once on its own before the card moves to Needs Zach (`factory.no-verdict`); a
  card that stops with nothing queued goes to Needs Zach after 2 minutes.
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
  left" or "Landed: did it finish?", and emailed like any other wait): reply to send the agent on, or
  mark it Done. REMAINING lists only this card's own request: another card's work, a problem the
  change did not cause (an expired login, an unrelated failing test) and follow-ups go in the summary
  or become ideas, so they never hold a finished card. A landing whose commits differ from the
  reviewed ones waits as "Landed something other than what was reviewed".
- Code and landing reviews of a change to frontend files (`factory/uireview.go`) include a UI check:
  the reviewer runs it on test data, served on this host's tailnet IP (`tailscale ip -4`, never
  `0.0.0.0`), and clicks through it with computer use. Its non-blocking "UI feedback:" block goes to
  the builder, who repeats it in the landing summary. In this repo, build the card's binary in /tmp
  and run `factory -dry-run -listen "$(tailscale ip -4):0"` (the log names the port). That meets the
  check's data rule: it reads the real cards, ideas and transcripts, and its buttons only log what
  they would do.
- Drop, Close and dragging to Done are final: the card is stopped and archived, and late verdicts
  for it are not forwarded. Its running reviewers are stopped and archived with it.
- A duplicate passes its request on. A reviewer (or the Fellow) that finds a duplicate names the
  card it duplicates on a `DUPLICATE OF: <card id>` line. When Zach closes the card at that close
  verdict, the factory keeps its request and the reviewer's notes (`steers.json`) and sends them to
  the kept card once it is planning or building; an idle one then gets the review for its stage
  again. While the kept card is in review or Needs Zach the steer waits; if it is landing, landed or
  dropped, the request becomes an idea titled "From closed duplicate <id>: ..." (`factory/steer.go`).

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

`/api/health` reports live, running, idle and parked agents, per kind and per card (a card's own
agent plus its reviewers and sub-agents), and the last reap; the `/host` page shows them.

### Repo and agent per card

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
- At most 3 running at once per agent; no secrets in their prompts. A card's sub-agents edit only
  the files it assigns them and never commit; a reviewer's sub-agents are read-only. An agent's own
  sub-agent tool (Claude's Agent tool, say) is allowed too.
- On the board, sub-agents fold into their card at any depth (a reviewer's own sub-agents too),
  with their model. A sub-agent on a permission prompt or stalled flags the card as other children
  do; a card agent or reviewer waiting on a working sub-agent is not counted as stalled; and
  "sub-agent still running" flags one left running after the agent that started it stopped.

### Recorded token usage

`GET /api/usage` serves recorded tokens from retained local Paseo sessions, including
archived agents. The first request starts a background scan and returns an empty snapshot
with `refreshing: true`; later requests serve the cached snapshot while a refresh runs,
at most once every five minutes. It makes no agent turns or changes to sessions.

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
entries without agent metadata and scan limits. Scans are bounded to 2,000 metadata files,
32 MiB per session, 128 MiB of session data and 20 seconds per refresh; newer agents are
read first within the metadata-file cap and limit-skipped sessions are counted separately
from errors. Partial readings
are flagged. A process remembers previously observed sources and reports their disappearance
on subsequent complete scans. Restarting forgets that inventory. Totals can shrink after
cleanup; they describe retained local records, not every device's activity or a subscription
bill. Historical subscription account attribution is unknown. No prompts, session contents
or filesystem paths are returned. A separate usage page will consume this data feed.

### Ideas

Any agent can suggest work with `POST /api/idea` (header `X-Factory: 1`; `repo`, `title`,
`summary`, `why`, `doneWhen`, `request`, optional `tier`). The factory starts the oldest waiting
idea, at most 2 in flight and 5 a day; a person can Start or Dismiss any idea. Titles that repeat
a waiting, dismissed or recently started idea are rejected. "Suggest ideas" on the board starts a
read-only Scout that submits up to 3, guided by the dialog's prompt; with Repo Auto, a picker first
chooses the repo from that prompt, as for new cards. The text in “What should it do?” is passed to the Scout
as guidance; leave it empty for general suggestions. A running Scout shows in the Ideas column, then in Done.
Every Fellow and reviewer is told how to suggest out-of-scope work it finds as an idea (at most 2 per
card) instead of doing it in its card; agents started before this rule do not have it.

### Email and voice-memo cards

An email dispatcher (not in this repo) can start agents as cards with labels
`factory.source=email|voice` and `gmail-thread=<id>`. Those cards follow the same tracks and
report back in their email thread when they land.

### Needs Zach overview

Agents start every message where they stop (plan, build or rework summary, review, brief, handoff,
result) with three lines: `DECISION:`, `OVERVIEW:` and `NEEDS FROM ZACH:` (`factory/overview.go`).
The factory takes the first such block (at least 2 of the 3 keys) out of the message shown on a
Needs Zach card and puts it in the ask's `overview`. The board shows it in full on top of the card,
with the agent's whole message open under it, and the email leads with it. A message with no block
shows as before.

### Needs Zach email

Every card that waits in Needs Zach is emailed to Zach once per wait reason, by the factory itself
through the zrl-bot mailbox (`pdw call zrl_bot_email_control__*`, `factory/mail.go`). The email
carries what the board's ask shows: the title and why, the card and repo, the full last message of
the builder or reviewer (the plan, brief or findings), its questions numbered with lettered
options, the words he can reply with, and an Open board button that opens the card.

- Inbox cards answer in their own thread. Other cards start one thread to `FACTORY_ZACH_EMAIL` on
  their first wait (`factory.zach-thread`); later waits reply in it.
- `factory.told` holds the wait already emailed; it is cleared when the card leaves Needs Zach.
  A wait where the builder was just asked for a summary or brief (`factory.writing`) is emailed
  once it has written it (at most 10 minutes later).
- Not emailed: cards moved to Needs Zach by hand, finished cards, and cards with no thread when
  `FACTORY_ZACH_EMAIL` is unset. A failed send is retried after 5 minutes; a timed-out one is
  not, since it may have gone out.
- `/api/health` reports the last mailbox check as `mail`; it never fails the health check.

Replies: the email dispatcher checks the sender, then posts his reply to
`POST /api/answer` (`thread`, `message_id`, `text`; 404 when no card has the thread, 409 when none
of its cards waits, or when two or more do: then the reply could be for either, so the dispatcher
passes it on as a follow-up). Quoted text and signatures are cut first. Only a whole reply from these lists
acts:

| Reply | Does |
| --- | --- |
| `approve`, `approved`, `yes`, `y`, `ok`, `okay`, `lgtm`, `go`, `go ahead`, `ship it` | the ask's Yes button, when it is approve or recommend |
| `drop`, `drop it`, `close`, `close it`, `cancel` | Drop, when the ask has it |
| `1B 2A` (only numbers and letters of the email's questions) | answers them, with the Yes button when it approves |

Anything else goes to the agent as his instructions, written as a follow-up email block. Each
message is applied once (`factory.answered`), and the factory confirms what it did in one line in
the thread. An emailed approve of a public-repo landing still runs the secret scan first.

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
- **Landed**: the commit time of the card's HEAD when the factory verifies the landing; a card
  that lands again keeps the last one.
- **Replied** (email and voice cards): 5 minutes after Done, one read of the card's thread takes
  the last message the mailbox sent after he asked and by Done plus 5 minutes. The factory's own
  emails (Needs Zach notes, answer confirmations) are recorded when sent and never count.
- **Partial**: a card first seen past its start (in flight when timing began, or created while the
  factory was down) has an incomplete history.

The card panel shows it as one Timeline line. `GET /api/metrics?days=7` (1 to 30) gives the
median and p90, in seconds, of end to end (asked to replied, else to landed) and of the time in
Needs Zach, by source (`board`, `email`, `voice`, `idea`), by tier, and by both. It counts cards
that reached Done in the window and landed; dropped or closed cards and partial ones are only
counted in `excluded`.

#### Transcript links for warehouse analysis

The same timing records keep the repo and a history of observed agent-to-session links for the
Fellow, reviewers and nested Paseo sub-agents. A link contains the Paseo agent and immediate
parent, device, provider, native transcript session id, model, role, review kind and card step.
Session ids use the same decoding as the transcript reader, including plugin providers.

`GET /api/timings` is a read-only snapshot for a host-side warehouse uploader. It returns
`{"cards":[...]}`, sorted by card id, from the existing timing store. Each card includes `id`,
`repo`, `source`, `tier`, `partial`, `requestedAt`, `requestedFrom`, `landedAt`, `repliedAt`,
`doneAt`, `events`, `rounds` and `sessions` when available. It excludes titles, folders, transcript
bodies, mailbox thread and message ids, and reply-check bookkeeping. Like the other board APIs,
it uses the server's existing access boundary; it must not be published on the public internet.

The warehouse should upsert these snapshots by card id and use the exported request, landing and
reply times directly, so its card turnaround agrees with Factory's own metrics. It can join each
link's `provider`, `device` and native `sessionId` to its transcript archive after mapping Paseo
provider names to the archive's source names. Do not match sessions by title or folder.

A link's `observedAt` dates when Factory saw that association, not when a turn began or ended.
`createdAt` is the Paseo agent's creation, not necessarily the transcript session's creation.
Links are added on changes to the session, step or attribution metadata; unchanged polls and
export reads write nothing. Reviewers and their descendants keep their review step. Archived
children keep their previous step. Earlier links survive session changes, restarts and agents
leaving the board until the card timing record expires under the existing 30-day rule.

Older records may lack a repo or session links. First observations can miss earlier sessions or
step changes; export absence is not a deletion signal. Warehouse analysis must report unmatched
sessions and incomplete coverage. Agent duration should be measured per transcript turn, from
the prompt to its final assistant event, with missing boundaries counted explicitly. Session
span is not uninterrupted work, and overlapping agents' durations are not card elapsed time.
Warehouse ingestion and average/median queries by repo, source, agent and step are a separate
warehouse change; this export makes no warehouse calls and does not change `/api/metrics`.

## Public repos: review and secret scan

Repos listed in `FACTORY_GUARDED_REPOS` (default: this one) are public, so everything that lands
in them is published. For their cards:

- After code review passes, the factory runs the secret scan on `origin/main..HEAD` in the card's
  worktree. It uses its own installed binary and the gitleaks config from `origin/main`, never the
  card's copy, so a card cannot weaken the scan that checks it. A hit, or a scan that cannot run,
  stops the card in Needs Zach; nothing is published.
- A clean scan lands the card under the usual tier rules, with `bin/land`, which scans again
  before its push, as does the pre-push hook. No person approves the landing: what keeps private
  data out is the code reviewer, told the repo is public, and the scans. This holds for every
  card, including ideas, Scouts and scheduled runs, so those can publish with no person reading
  them. Architecture-tier cards and cards with no plan review still wait for Zach, as in any repo;
  approving one scans again before the land message.
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

### Landing reviews for other agents

An agent outside the card flow (a scheduled job, say) can ask for a review before it lands:
`land-review -why "..."` (a link to the factory binary) once its work is committed. It exits 0 on
approve, 2 on changes, 3 when Zach must decide.

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
| `FACTORY_METRIC_HOSTS` | `-metric-hosts` | comma-separated SSH aliases whose CPU, load, RAM and disk the board's host icon shows besides this host (none: this host only) |
| | `-listen` | address to serve on (default `127.0.0.1:<port>`); `host:0` picks a free port |
| | `-dry-run` | real data, no changes: only paseo and git reads run, every write is logged to Activity, no Tick, no email, state copied to a temp dir |
| `PATH` | | must reach `paseo`, `node`, `git`, `gitleaks` and `pdw` (logged in) |

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
- `factory/policy.go`: tiers, prompts, verdicts, the automatic moves (`Tick`).
- `factory/guard.go`: guarded (public) repos. `factory/scan/`: the secret scanner.
- `factory/route.go`: Repo Auto. `factory/agents.go`: the agent choices. `factory/reap.go`: the idle reap.
- `factory/board.go`, `collect.go`, `ask.go`, `ideas.go`, `inbox.go`, `actions.go`,
  `transcript.go`, `server.go`; the board and transcript pages are in `factory/web/`.
- `factory/transcript.go`, `transcript_providers.go`: the transcript page reads each agent's own
  session file (Claude `~/.claude/projects`, Codex `~/.codex/sessions`, Antigravity
  `~/.gemini/antigravity-cli/brain`, Muse `~/.local/share/muse/sessions`), and `paseo logs` only
  when there is none.
- `factory/hostmetrics*.go`, `hostfleet.go`: host load for the header icon and `/host`. Each
  `FACTORY_METRIC_HOSTS` alias gets one read-only SSH stream (`ssh -o BatchMode=yes`, a fixed
  script on stdin, Linux or macOS), shared by all viewers, run only while someone polls
  `/api/hosts` and stopped a minute after the last poll.
- `factory/delegate.go`: the sub-agent prompt section and the sub-agent checks.
- Tests sit beside the code, with a fake `paseo` (`factory/fake_test.go`).

## License

MIT.
