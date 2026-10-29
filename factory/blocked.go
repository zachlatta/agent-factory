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
	"no cd earlier in the same command (run cd as its own command if needed). Never run cd X && git or any compound command combining cd and git; use git -C X instead. rm outside your worktree, such as in /tmp, asks too. " +
	"A factory -dry-run deletes its own temp state when you stop it. " +
	"Wait on your original tool task handle or a verified exact process ID. Do not poll pgrep -f: " +
	"the watcher can match itself or other agents. A watcher timeout is not a failed command; " +
	"read the original result before retrying."

// scratchSafetyRule applies to every agent using the card's shared worktree.
const scratchSafetyRule = "Never run commands from inside .factory-scratch/. Keep your working directory at the worktree root. " +
	"Any authorized scratch removal must run from the worktree root using its literal absolute path; " +
	"never use a relative path, variable, substitution, glob, or ~."

// scratchClean deletes a worktree's .factory-scratch/ without rm: rm -r asks for permission even
// inside the worktree. -X removes only ignored files, and the folder is ignored (.git/info/exclude).
const scratchClean = "git -C <worktree root> clean -fdX -- .factory-scratch/"

// scratchCleanupRule assigns cleanup only to the card owner, never reviewers or sub-agents.
const scratchCleanupRule = scratchSafetyRule + " Before landing or finishing, delete .factory-scratch/ if present with " +
	scratchClean + " (rm -r asks for permission even inside the worktree). " +
	"This keeps ignored files from blocking worktree retirement."

// worktreeStorageRule includes shell safety without assigning cleanup of shared scratch.
// These constants are formatted into newCardPrompt: no % in them.
const worktreeStorageRule = shellRule + "\n\n" +
	"Worktree, not /tmp: do all your work in this worktree, never in /tmp or another temp directory. " +
	"Put real deliverables in tracked paths (for work items in work/, create work/YYYY-MM-DD-short-name/ first and keep " +
	"the scripts and outputs that produced the deliverable inside it). Put transient scratch (JSON payloads, downloads, " +
	"one-off helper scripts) in a .factory-scratch/ folder in the worktree, listed in .git/info/exclude " +
	"(git rev-parse --git-path info/exclude) before first use; do not commit or force-add .factory-scratch/."

// worktreeRule is for the card owner; its helpers use storage and safety rules only.
const worktreeRule = worktreeStorageRule + " " + scratchCleanupRule

// permissionWait reports whether agent a is stopped on a permission prompt: one Paseo lists for it
// (Agent.Pending), or Paseo's own attention flag for it. The factory refuses these prompts itself
// (denyPrompts).
func permissionWait(a Agent) bool {
	return a.Pending != "" || (a.RequiresAttention && a.AttentionReason == "permission")
}

// blockedWhy includes failed reviewers and running children that cannot move on their own: a child
// waits on a permission prompt, or it has had no activity for over stallMinutes while no sub-agent
// of its own works (subagentActive). "" when neither.
func blockedWhy(agents []Agent, a Agent, now time.Time) string {
	if a.Status == "error" && a.L(labelRole) == "reviewer" && !a.archived() {
		return "stopped with an error"
	}
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

// reviewerBlocked includes errored reviewers and running reviewers that need someone to step
// in: on a permission prompt for at least stuckAfter since its last activity, or stalled (with no
// working sub-agent of its own).
func reviewerBlocked(agents []Agent, r Agent, now time.Time) bool {
	if r.Status == "error" && !r.archived() {
		return true
	}
	if r.Status != "running" || r.archived() {
		return false
	}
	idle := now.Sub(r.LastActivityAt)
	return (permissionWait(r) && idle >= stuckAfter) || (idle > stallMinutes*time.Minute && !subagentActive(agents, r.ID, now))
}

// permitEvery: `permit ls` runs at most this often. Between reads each agent keeps the prompt the
// last read found; Refreshes, board actions and watcher passes in between make no call.
const permitEvery = 15 * time.Second

// addPending fills each agent's Pending from `paseo permit ls --json`, one call for all agents,
// made only when some agent is running, and at most every permitEvery unless an agent's file says
// it waits on permission. A failed read leaves Pending empty: the stall checks still catch a
// blocked agent, later.
func (f *Factory) addPending(agents []Agent) {
	prev, prevIDs := map[string]string{}, map[string][]string{}
	for _, a := range f.agents() {
		prev[a.ID], prevIDs[a.ID] = a.Pending, a.PendingIDs
	}
	running, flagged := false, false
	for _, a := range agents {
		live := a.Status == "running" && !a.archived()
		running = running || live
		// a prompt Paseo flags that the last read did not see
		flagged = flagged || (live && a.RequiresAttention && a.AttentionReason == "permission" && prev[a.ID] == "")
	}
	if !running {
		return
	}
	now := f.Now()
	f.mu.Lock()
	due := f.permitAt.IsZero() || now.Sub(f.permitAt) >= permitEvery || now.Before(f.permitAt)
	f.mu.Unlock()
	if !due && !flagged {
		for i := range agents {
			agents[i].Pending, agents[i].PendingIDs = prev[agents[i].ID], prevIDs[agents[i].ID]
		}
		return
	}
	out, err := f.paseo("permit", "ls", "--json")
	var perms []struct{ ID, AgentID, Name, Description string }
	if err == nil {
		err = json.Unmarshal([]byte(out), &perms)
	}
	if err != nil {
		f.logf("pending permissions not read: %v", err)
		return
	}
	f.mu.Lock()
	f.permitAt = now
	f.mu.Unlock()
	by, ids := map[string]string{}, map[string][]string{}
	for _, p := range perms {
		ids[p.AgentID] = append(ids[p.AgentID], p.ID)
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
		agents[i].Pending, agents[i].PendingIDs = by[agents[i].ID], ids[agents[i].ID]
	}
}

// denyRule is what the factory tells an agent when it refuses its permission prompt.
const denyRule = "The factory answers every permission prompt with no, and nobody else will answer it. " +
	"Write rm only with literal absolute paths (no glob, variable, substitution or ~, and no cd earlier in the same " +
	"command), never combine cd and git in one command (use git -C), and keep your work in your worktree. " +
	"Do this step another way, or stop and say in your summary what you need."

// denyPrompts refuses every permission prompt of the factory's own agents (cards, reviewers, Scouts,
// pickers and their sub-agents), once per prompt, so none waits for a person. It never allows a
// prompt and never answers one of an agent the factory did not start. Dry and read-only runs only
// read.
func (f *Factory) denyPrompts(agents []Agent) {
	if f.passive() {
		return
	}
	managed, live := quietManaged(agents), map[string]bool{}
	defer func() { // forget prompts that are gone
		f.mu.Lock()
		for k := range f.denied {
			if !live[k] {
				delete(f.denied, k)
			}
		}
		f.mu.Unlock()
	}()
	for _, a := range agents {
		if !managed[a.ID] || a.archived() || a.Status != "running" || a.Pending == "" {
			continue
		}
		ids := a.PendingIDs
		if len(ids) == 0 {
			ids = []string{""} // an older Paseo without prompt ids: refuse all of the agent's
		}
		for _, id := range ids {
			key := a.ID + "/" + id
			if id == "" {
				key += a.Pending
			}
			live[key] = true
			f.mu.Lock()
			done := f.denied[key]
			f.denied[key] = true
			f.mu.Unlock()
			if done {
				continue
			}
			args := []string{"permit", "deny", a.ID, id, "--message", denyRule}
			if id == "" {
				args = []string{"permit", "deny", a.ID, "--all", "--message", denyRule}
			}
			if _, err := f.paseo(args...); err != nil {
				f.mu.Lock()
				delete(f.denied, key) // try again next pass
				f.mu.Unlock()
				f.logf("%s: refusing its permission prompt: %v", short(a.ID), err)
				continue
			}
			f.logf("%s: refused its permission prompt (%s)", short(a.ID), trunc(a.Pending, 120))
		}
	}
}

// supersedeReviewers retires card id's unapplied reviewers before new work replaces their
// review. Archived reviewers need the applied marker too: their verdicts can still be read.
func (f *Factory) supersedeReviewers(id string) error {
	byID := f.byID()
	for _, r := range f.agents() {
		if r.L(labelRole) != "reviewer" || !sameCard(byID, r.L(labelReviewOf), id) || r.L(labelApplied) != "" {
			continue
		}
		if err := f.retireReviewer(r, "superseded"); err != nil {
			return err
		}
	}
	return nil
}

// retireReviewer records verdict on reviewer r (superseded, or "" for one given up on) and archives it.
func (f *Factory) retireReviewer(r Agent, verdict string) error {
	if err := f.setLabels(r.ID, map[string]string{labelApplied: "1", labelVerdict: verdict}); err != nil {
		return err
	}
	f.noteLens(r.ID, lensResult{done: true, verdict: verdict})
	if r.archived() {
		return nil
	}
	if err := f.archiveAgent(r.ID); err != nil {
		f.logf("reviewer %s retired but not archived: %v", short(r.ID), err)
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
