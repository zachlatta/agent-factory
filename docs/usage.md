# Usage product contracts

Usage answers three questions, in order:

1. What is the last known usage for each subscription?
2. Where did usage go?
3. Can I trust these numbers?

The default view starts with last known subscription readings. Decisions and
consumption summaries follow. Source records and counting rules
are available through progressive disclosure. The phone page has no horizontal
page scroll; wide source tables scroll inside their own region.

| Contract | Acceptance criteria |
| --- | --- |
| Understand usage and act | A compact overview of every subscription comes first, with weekly and five-hour usage, remaining allowance, reset information and observation age. Capacity advice follows the readings. Consumption has a period, a daily chart, ranked consumers, and model/role summaries. Advanced filters and source rows start closed. |
| Know subscription headroom | Show every subscription and account together, including old and unavailable readings. Use explicit percent-used and percent-left labels, provider windows, reset dates, account emails, active accounts and observation age. Unknown, stale, estimated and elapsed-reset readings are visibly distinct. |
| Choose a provider | Recommend only an available provider with a recent, non-estimated reading and comfortable remaining windows. Compare weekly pace where reset times are known. Never switch accounts. |
| Count consistently | Sum disjoint normalized rows. Input includes cached input; total is input plus output. Cache and reasoning remain subsets. Deduplicate provider records and session references. |
| Identify consuming work | Rank cards with reviews and delegated work included. List work without a card by agent. Clicking a group filters summaries and opens source records. |
| Explain review and repeated work | Show reviewer token share, known repeated Building share, visits and review rounds. Show the token share without stage attribution and each reason. Never invent missing stage history. |
| Explain completeness | Show source issues independently of filters, their implications and next steps. Link issues to affected rows. Counts of missing sessions do not estimate missing tokens. |
| Distinguish readings from estimates | A passed reset does not change an observed percentage. Such a reading is unsuitable for capacity advice. Automatic decisions ignore only elapsed windows; known unexpired exhaustion still blocks the provider. A current low weekly window continues to hold ideas after a short-window reset. Failed readings do not hold ideas. No-usage estimates are labeled and excluded from recommendations. |
| Handle freshness and failure | Keep the last successful token snapshot, visibly date it and retry. Preserve the newest successful allowance, including per-account readings, when a refresh fails; retain a failure indicator on the affected account. Preserve current account identity when restoring its counters. Bound browser fetches. |
| Inspect evidence | Show exact counts, subset counts, source status and attribution reasons per row. Open retained main-session transcripts, including archived agents, without loading or resuming agents. Missing transcripts have an explicit unavailable state. |
| Compare consumption over time | Offer Today, seven or thirty complete UTC days, and all retained history. Compare complete periods to the preceding equal period. Show daily exact counts and uncertain-date exclusions. Label bounded-scan totals as partial and withhold percentage comparisons until collection finishes. Do not present a partial day as a completed-period change. |
| Separate quota and token scopes | Subscription quota spans devices. Token summaries cover retained local records. Historical accounts and bills cannot be inferred from token volume. |

## Date and attribution limits

A row's `day` is the UTC day containing its entire recorded token interval. An
initial cumulative reading has no known start. An interval crossing midnight has
no single known day. Both remain in retained-history totals with a `timeReason`,
and are excluded from dated summaries rather than prorated. Rows stay disjoint
when split by day. Their counts still sum to the snapshot totals.

`stageReason` explains unknown stage attribution. `rework` marks an interval in a
Building visit with an earlier recorded Building visit. It does not estimate
unrecorded rework. Reviewer shares use recorded agent roles, including panel
reviewers, even when the stage is unknown.

Comparisons describe the files retained at collection time. Cleanup can change
both periods. Collection gaps and unknown dates remain visible; neither a zero
bar nor a finished scan proves complete historical activity.

## Evidence and validation

The usage snapshot contains counters and accounting metadata, never transcript
text or filesystem paths. The separate evidence route reads a bounded page of a
retained main transcript only after an explicit navigation. Only agents in the
current accounting inventory qualify. Evidence reads use the published snapshot
inventory and do not wait for a collection in progress. It provides no actions or agent writes. Transcript support is independent of
token-counter support, so an unsupported counter row may still offer evidence.
Built-in sub-agent transcript files are counted under their owner but are not
shown in the main-session evidence view.

Go tests cover normalization, duplication, stage/date boundaries, conservation,
progressive scan budgets, failure retention, live-meter parsing and archived
evidence. Browser tests cover the real page and API on synthetic preview data,
period changes, ranking filters, issue drill-down, pagination, escaping, failure
recovery, read-only evidence and phone overflow.

Run `npm run board:screenshot -- --view=usage` for desktop and phone captures of
the synthetic usage dashboard. The ordinary board screenshot checks still apply.
The preview contains current and stale allowances, missing/unsupported sources,
dated history, review work, repeated building and archived transcript evidence.
