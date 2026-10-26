package factory

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Sub-agents. A card's agent or a reviewer may hand parts of its work to other Paseo agents on any
// provider here. Run from the agent's own shell, `paseo run` sets paseo.parent-agent-id from
// PASEO_AGENT_ID and uses the caller's workspace, so the sub-agent shares the card's worktree and
// folds into its card on the board (BuildBoard). Paseo archives an agent's children when it is
// archived, but a card is archived only once Done, so the prompt tells every agent to archive its
// own sub-agents before it stops.

// maxSubagents is how many sub-agents one agent may have running at once.
const maxSubagents = 6

// subagentRunningFlag marks a card whose agent (or reviewer) stopped while a sub-agent it started runs on.
const subagentRunningFlag = "sub-agent still running"

// delegateOption is one agent a sub-agent can run on, with its remaining usage.
type delegateOption struct {
	Choice AgentChoice
	Usage  string
}

// delegateRule is the prompt section on sub-agents for a card's agent, or (reviewer) a reviewer's
// read-only version. It lists the providers Paseo's cached list has available; the default agent is
// listed even when the list is empty or unknown, as resolveAgent starts it then too. It reads the
// provider and usage caches only and never starts a refresh: starting a card refreshes the provider
// list (resolveAgent), and the board's agent picker the usage.
func (f *Factory) delegateRule(reviewer bool) string {
	now := f.Now()
	f.usageMu.Lock()
	usages := make(map[string]AgentUsage, len(f.usage.byProvider))
	for k, v := range f.usage.byProvider {
		usages[k] = withAge(v, now)
	}
	f.usageMu.Unlock()
	var opts []delegateOption
	for _, a := range AgentChoices {
		if st, _ := f.cachedProviderStatus(a.providerID()); st != "available" && a.ID != defaultAgent {
			continue
		}
		u := usages[a.providerID()].Summary
		if u == "" {
			u = "usage unknown"
		}
		opts = append(opts, delegateOption{a, u})
	}
	return delegateText(opts, reviewer)
}

// delegateText renders the sub-agent section. It is appended to prompts, never formatted, so it
// may hold %.
func delegateText(opts []delegateOption, reviewer bool) string {
	var b strings.Builder
	b.WriteString("\nSub-agents. Zach allows you to hand parts of this work to sub-agents, including models from other " +
		"providers (for a second opinion, a parallel search, or a model better at a task). On factory cards this " +
		"overrides the start-here rules on subagents and on Opus-only launches. Your own sub-agent tool (such as " +
		"Claude's Agent tool) is fine too. To start a Paseo sub-agent, run one of these from your shell (it prints the " +
		"sub-agent's id), with the prompt as the last argument:\n")
	for _, o := range opts {
		args := o.Choice.runArgs() // "run", then its flags
		cmd := "paseo run -d -q " + strings.Join(args[1:], " ") + ` --title "<task in a few words>" "<prompt>"`
		fmt.Fprintf(&b, "- %s (%s; %s", o.Choice.Label, o.Choice.Plan, o.Usage)
		if o.Choice.Note != "" {
			b.WriteString("; " + o.Choice.Note)
		}
		b.WriteString("):\n  " + cmd + "\n")
	}
	b.WriteString(`Then:
- Wait with ` + "`paseo wait <id> --timeout 100`" + `, one call at a time, until it reports the agent idle. Each call
  returns within 100 seconds (inside Claude Code's 120-second shell limit); a timed-out wait is not a
  failure, just run it again.
- Read its answer with ` + "`paseo logs <id> --filter text --tail 1`" + `, then archive it: ` + "`paseo archive <id>`" + `.
- Before you end any turn (a plan, a summary, a rework, a verdict), list the sub-agents you started with
  ` + "`paseo ls -g -q --label paseo.parent-agent-id=$PASEO_AGENT_ID`" + ` and run ` + "`paseo archive <id>`" + ` for each,
  so nothing you started keeps running after you stop.
`)
	fmt.Fprintf(&b, "- At most %d sub-agents running at once. Prefer a provider with usage left. Never put secrets "+
		"(tokens, keys, passwords) in a sub-agent's prompt; a provider marked \"sends ...\" receives what you give it "+
		"and what it reads.\n", maxSubagents)
	if reviewer {
		b.WriteString("- Your sub-agents are read-only, like you: start each prompt with \"You are a read-only sub-agent " +
			"of a factory reviewer. Edit nothing; do not commit, push, land, start agents, send email or message " +
			"anyone. End with your answer.\" Their findings are yours to check before you use them. Starting and archiving them is allowed besides posting " +
			"ideas.\n")
	} else {
		b.WriteString("- A sub-agent works in this card's worktree, never in /tmp: put transient scratch in .factory-scratch/ " +
			"listed in .git/info/exclude before first use. Start each prompt with \"You are a sub-agent of a " +
			"factory card. Work in this worktree, never in /tmp (put transient scratch in .factory-scratch/ listed in " +
			".git/info/exclude); edit only the files you assign it (or nothing); do not commit, push, land, start agents, " +
			"send email or message anyone. End with your answer.\" Edit only the files you assign it, never the same " +
			"file in two sub-agents at once. You review its work and make every commit yourself.\n")
	}
	return b.String()
}

// subagentActive reports whether agent id has a running sub-agent (or one of theirs) with activity
// within stallMinutes: an agent waiting on such a sub-agent is not stalled.
func subagentActive(agents []Agent, id string, now time.Time) bool {
	seen := map[string]bool{id: true}
	for frontier := []string{id}; len(frontier) > 0; {
		var next []string
		for _, a := range agents {
			p := a.Labels["paseo.parent-agent-id"]
			if p == "" || seen[a.ID] || !slices.Contains(frontier, p) || a.archived() {
				continue
			}
			seen[a.ID] = true
			if a.Status == "running" && now.Sub(a.LastActivityAt) <= stallMinutes*time.Minute {
				return true
			}
			next = append(next, a.ID)
		}
		frontier = next
	}
	return false
}
