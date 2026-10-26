package factory

import (
	"encoding/json"
	"fmt"
	"time"
)

// shellRule keeps agents off the commands Claude Code asks permission for even in bypass mode:
// nobody answers the prompt, so the agent hangs. It is formatted into newCardPrompt: no % in it.
const shellRule = "Shell commands: Claude Code asks for permission before some rm commands even in bypass mode, and " +
	"nobody responds to it, so you would hang. rm takes only literal absolute paths: no glob, $VAR, $(...) or ~, and " +
	"no cd earlier in the same command (use git -C or absolute paths, or run cd as its own command)."

// worktreeRule keeps card agents in their own worktree instead of /tmp, with transient scratch
// isolated in .factory-scratch/ (excluded from Git via .git/info/exclude) and deleted before the
// card lands or finishes so ignored files never stay on disk or block worktree retirement.
const worktreeRule = "Worktree, not /tmp: do all your work in this worktree, never in /tmp or another temp directory. " +
	"Put real deliverables in tracked paths (for work items in work/, create work/YYYY-MM-DD-short-name/ first and keep " +
	"the scripts and outputs that produced the deliverable inside it). Put transient scratch (JSON payloads, downloads, " +
	"one-off helper scripts) in a .factory-scratch/ folder in the worktree, listed in .git/info/exclude " +
	"(git rev-parse --git-path info/exclude) before first use; do not commit or force-add .factory-scratch/, and delete " +
	".factory-scratch/ before the card lands or finishes so ignored files do not block worktree retirement."

// permissionWait reports whether agent a is stopped on a permission prompt: one Paseo lists for it
// (Agent.Pending), or Paseo's own attention flag for it. The factory never answers these prompts.
func permissionWait(a Agent) bool {
	return a.Pending != "" || (a.RequiresAttention && a.AttentionReason == "permission")
}

// blockedWhy says why a running child agent (reviewer or sub-agent) will not move on its own: it
// waits on a permission prompt, or it has had no activity for over stallMinutes while no sub-agent
// of its own works (subagentActive). "" when neither.
func blockedWhy(agents []Agent, a Agent, now time.Time) string {
	if a.Status != "running" || a.archived() {
		return ""
	}
	if permissionWait(a) {
		return "waiting on permission"
	}
	if idle := now.Sub(a.LastActivityAt); idle > stallMinutes*time.Minute && !subagentActive(agents, a.ID, now) {
		return fmt.Sprintf("stalled %d min", int(idle.Minutes()))
	}
	return ""
}

// reviewerBlocked reports whether a running reviewer will give no verdict without someone stepping
// in: on a permission prompt for at least stuckAfter since its last activity, or stalled (with no
// working sub-agent of its own).
func reviewerBlocked(agents []Agent, r Agent, now time.Time) bool {
	if r.Status != "running" || r.archived() {
		return false
	}
	idle := now.Sub(r.LastActivityAt)
	return (permissionWait(r) && idle >= stuckAfter) || (idle > stallMinutes*time.Minute && !subagentActive(agents, r.ID, now))
}

// addPending fills each agent's Pending from `paseo permit ls --json`, one call for all agents,
// made only when some agent is running. A failed read leaves Pending empty: the stall checks still
// catch a blocked agent, later.
func (f *Factory) addPending(agents []Agent) {
	running := false
	for _, a := range agents {
		running = running || (a.Status == "running" && !a.archived())
	}
	if !running {
		return
	}
	out, err := f.paseo("permit", "ls", "--json")
	var perms []struct{ AgentID, Name, Description string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &perms)
	}
	if err != nil {
		f.logf("pending permissions not read: %v", err)
		return
	}
	by := map[string]string{}
	for _, p := range perms {
		if by[p.AgentID] != "" {
			continue
		}
		s := p.Name
		if p.Description != "" && p.Description != "-" {
			s += ": " + p.Description
		}
		by[p.AgentID] = trunc(s, 300)
	}
	for i := range agents {
		agents[i].Pending = by[agents[i].ID]
	}
}

// supersedeReviewers retires card id's reviewers whose verdict was not applied, before a re-run
// starts a fresh one. Each is marked applied first, so a verdict it gives later never moves the
// card, then stopped and archived.
func (f *Factory) supersedeReviewers(id string) error {
	for _, r := range f.agents() {
		if r.L(labelRole) != "reviewer" || r.L(labelReviewOf) != id || r.archived() || r.L(labelApplied) != "" {
			continue
		}
		if err := f.setLabels(r.ID, map[string]string{labelApplied: "1", labelVerdict: "superseded"}); err != nil {
			return err
		}
		if err := f.archiveAgent(r.ID); err != nil {
			f.logf("reviewer %s superseded but not archived: %v", short(r.ID), err)
		}
	}
	return nil
}

// blockedReviewer is the first reviewer on card c that blockedWhy flagged, if any.
func blockedReviewer(c Card) (Child, bool) {
	for _, k := range c.Children {
		if k.Role == "reviewer" && k.Blocked != "" {
			return k, true
		}
	}
	return Child{}, false
}
