package factory

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"
)

// Stages are the board's columns, in order: id, name, help text.
var Stages = [][3]string{
	{"idea", "Ideas", "Suggested by agents, and Scouts looking for them. Ideas start on their own, a few at a time."},
	{"plan", "Plan", "Agent writes a plan. No code yet."},
	{"plan-review", "Plan review", "A different agent tries to find fault with the plan."},
	{"building", "Building", "Agent is working."},
	{"review", "Code review", "A different agent checks the work before it lands."},
	{"zach", "Needs Zach", "Waiting on you: a question, a preview to try, or an approval."},
	{"landing", "Landing", "Approved. Agent is landing it."},
	{"landed", "Done", "Landed, answered by email, or finished and seen."},
}

func validStage(s string) bool {
	for _, st := range Stages {
		if st[0] == s {
			return true
		}
	}
	return false
}

const stallMinutes = 20 // running with no activity this long = stalled

// Derive is an agent's column: its factory.stage label first, else from its Paseo status.
//
// An unlabeled agent that stopped only needs Zach if it waits on him (permission, question) or
// finished and he has not looked yet. Email agents already answered him by email, scheduled runs
// are routine, and closed sessions have nothing left to do, so those count as done.
func Derive(a Agent) string {
	if a.gone() {
		return "landed"
	}
	if st := a.L(labelStage); validStage(st) {
		return st
	}
	if a.Status == "running" {
		return "building"
	}
	reason := ""
	if a.RequiresAttention {
		reason = a.AttentionReason
	}
	if reason != "" && reason != "finished" {
		return "zach"
	}
	if inboxAgent(a) || a.Labels["paseo.schedule-id"] != "" || a.Status == "closed" {
		return "landed"
	}
	if reason == "finished" {
		return "zach"
	}
	return "landed"
}

// The email dispatcher's labels are host config (FACTORY_INBOX_SOURCE, FACTORY_INBOX_ANCHOR_ROLE): its
// agents carry source=<InboxSource>, and its idle anchor agent role=<InboxAnchorRole>, which the
// board hides. Empty: no dispatcher.
var InboxSource, InboxAnchorRole string

func inboxAgent(a Agent) bool { return InboxSource != "" && a.Labels["source"] == InboxSource }

func source(a Agent) string {
	switch {
	case a.L(labelOrigin) == "idea":
		return "idea"
	case a.L(labelRole) == "scout":
		return "scout"
	case inboxAgent(a):
		return "email"
	case a.Labels["voice-memo"] != "":
		return "voice memo"
	case a.Labels["paseo.schedule-id"] != "":
		return "schedule"
	case a.L(labelSource) == "board":
		return "board"
	}
	return "app"
}

// BuildBoard turns agents (and waiting ideas) into cards. Reviewers and sub-agents fold into
// their parent card; the email dispatcher's anchor agent is hidden.
func BuildBoard(agents []Agent, ideas []Idea, now time.Time) []Card {
	byID := map[string]bool{}
	anchors := map[string]bool{}
	for _, a := range agents {
		byID[a.ID] = true
		if InboxAnchorRole != "" && a.Labels["role"] == InboxAnchorRole {
			anchors[a.ID] = true
		}
	}
	children := map[string][]Agent{}
	var tops []Agent
	owner := cardOwners(agents, anchors)
	for _, a := range agents {
		switch o, folded := owner[a.ID]; {
		case anchors[a.ID]:
		case folded:
			children[o] = append(children[o], a)
		default:
			tops = append(tops, a)
		}
	}
	running := map[string]bool{} // agents running now, for the sub-agent still running flag
	for _, a := range agents {
		running[a.ID] = a.Status == "running" && !a.archived()
	}
	// Ideas behind cards: started ones by their card, waiting ones in start order (oldest first).
	fromIdea := map[string]*IdeaView{}
	var waiting []Idea
	for _, i := range ideas {
		switch {
		case i.Status == "started" && i.Card != "":
			fromIdea[i.Card] = ideaView(i, 0)
		case i.Status == "waiting":
			waiting = append(waiting, i)
		}
	}
	sort.SliceStable(waiting, func(a, b int) bool { return waiting[a].CreatedAt.Before(waiting[b].CreatedAt) })
	out := make([]Card, 0, len(tops))
	for _, a := range tops {
		kids := children[a.ID]
		sort.Slice(kids, func(i, j int) bool { return kids[i].CreatedAt.Before(kids[j].CreatedAt) })
		c := Card{ID: a.ID, Short: short(a.ID), Host: a.Host, Title: a.Title, Repo: a.Repo, Project: a.Project,
			Branch: a.Branch, Cwd: a.Cwd, Model: a.Model, Status: a.Status, Source: source(a), Stage: Derive(a),
			Labelled: a.L(labelStage) != "", Flags: []string{}, Verdict: a.L(labelVerdict), Note: a.L(labelNote),
			Tier: a.L(labelTier), Awaiting: a.L(labelAwaiting), Origin: a.L(labelOrigin), Agent: a.L(labelAgent),
			Rounds:    Rounds{Plan: atoi(a.L(labelPlanRounds)), Code: atoi(a.L(labelCodeRounds))},
			CreatedAt: iso(a.CreatedAt), LastActivityAt: iso(a.LastActivityAt), ArchivedAt: iso(a.ArchivedAt),
			Children: []Child{}, WorkspaceID: a.WorkspaceID, Decided: a.L(labelDecided) != "", PlanFix: a.L(labelPlanFix)}
		if a.parked() {
			c.ArchivedAt, c.Parked = "", iso(a.ArchivedAt)
			c.Flags = append(c.Flags, parkedFlag)
		}
		if c.Title == "" {
			c.Title = "(untitled)"
		}
		if c.Repo == "" {
			c.Repo = "(no repo)"
		}
		if a.L(labelRouting) != "" {
			c.Routing, c.Repo = true, autoRepo
			if c.Stage == "plan" {
				c.Flags = append(c.Flags, "picking a repo")
			}
		}
		if c.Source == "scout" && !a.archived() {
			c.Stage, c.Note = "idea", "looking for ideas" // beside the ideas it submits; Done once archived
		}
		if c.Source == "schedule" && c.Tier == "" {
			c.Tier = "routine"
		}
		idle := now.Sub(a.LastActivityAt)
		if a.Status == "running" && a.Pending != "" && !a.archived() {
			if !(a.RequiresAttention && a.AttentionReason == "permission") { // that flag says it below
				c.Flags = append(c.Flags, "waiting on permission")
			}
			if c.Labelled {
				c.Stage = "zach" // shown only, like a stall: nobody else answers the prompt
			}
		}
		if a.Status == "running" && idle > stallMinutes*time.Minute && !subagentActive(agents, a.ID, now) {
			c.Flags = append(c.Flags, fmt.Sprintf("stalled %d min", int(idle.Minutes())))
			if c.Labelled && !a.archived() {
				c.Stage = "zach" // shown only: its labels keep the column, and it returns when activity resumes
			}
		}
		c.Unread = unread(a)
		if a.RequiresAttention && a.AttentionReason != "finished" {
			f := "needs attention"
			if a.AttentionReason != "" {
				f += ": " + a.AttentionReason
			}
			c.Flags = append(c.Flags, f)
		}
		if a.LastError != "" && !a.archived() {
			c.Flags = append(c.Flags, "error")
		}
		if c.Stage == "landed" && !a.archived() {
			c.Flags = append(c.Flags, "not archived")
		}
		if c.WaitsOn = a.L(labelWaitsOn); c.WaitsOn != "" && c.Stage != "landed" {
			c.Flags = append(c.Flags, waitFlag(agents, a))
		}
		if r := a.L(labelAutoReview); r != "" {
			c.Flags = append(c.Flags, r+" review queued")
		}
		for _, k := range kids {
			role := k.L(labelRole)
			if role == "" {
				role = "subagent"
			}
			ch := Child{ID: k.ID, Short: short(k.ID), Title: k.Title, Model: k.Model, Status: k.Status,
				Archived: k.archived(), Role: role, Kind: k.L(labelReviewKind), Verdict: k.L(labelVerdict),
				CreatedAt: iso(k.CreatedAt), LastActivityAt: iso(k.LastActivityAt)}
			if ch.Model == "" {
				ch.Model = k.Provider
			}
			if ch.Blocked = blockedWhy(agents, k, now); ch.Blocked != "" {
				ch.Permission = k.Pending
				c.Flags = append(c.Flags, role+" "+ch.Blocked)
			} else if role == "subagent" && running[k.ID] && !running[k.Labels["paseo.parent-agent-id"]] &&
				!slices.Contains(c.Flags, subagentRunningFlag) {
				// the agent that started it stopped: it should have archived it first (delegateRule)
				c.Flags = append(c.Flags, subagentRunningFlag)
			}
			c.Children = append(c.Children, ch)
			if role == "reviewer" && k.Status == "running" && !k.archived() {
				c.Reviewing = true
			}
		}
		c.Ask = askFor(c, a)
		c.Idea = fromIdea[a.ID]
		out = append(out, c)
	}
	for n, i := range waiting {
		out = append(out, Card{ID: i.ID, Short: i.ID, Host: "", Title: i.Title, Repo: i.Repo, Status: "waiting",
			Source: "idea", Stage: "idea", Labelled: true, Flags: []string{}, Note: "from " + i.By, Tier: i.Tier,
			Origin: "idea", CreatedAt: iso(i.CreatedAt), LastActivityAt: iso(i.CreatedAt), Children: []Child{},
			Idea: ideaView(i, n+1)})
	}
	return out
}

// cardOwners maps each agent that folds into a card to that card's id: a reviewer to the card it
// reviews, a sub-agent (paseo.parent-agent-id) to its parent's card, at any depth, so a reviewer's
// own sub-agents fold into the card too. Top-level agents are not in the map. A sub-agent whose
// parent is not listed (or is an anchor) is top-level; a reviewer is folded even when its card is not.
func cardOwners(agents []Agent, anchors map[string]bool) map[string]string {
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	up := func(a Agent) (string, bool) {
		if a.L(labelRole) == "reviewer" {
			return a.L(labelReviewOf), true
		}
		p := a.Labels["paseo.parent-agent-id"]
		if _, ok := byID[p]; p != "" && ok && !anchors[p] {
			return p, true
		}
		return "", false
	}
	out := map[string]string{}
	for _, a := range agents {
		o, ok := up(a)
		if !ok || anchors[a.ID] {
			continue
		}
		for depth := 0; depth < 10; depth++ { // bounded: a label loop cannot hang the board
			next, ok := byID[o]
			if !ok {
				break
			}
			p, ok := up(next)
			if !ok {
				break
			}
			o = p
		}
		out[a.ID] = o
	}
	return out
}

// unread: the agent finished a turn Zach has not opened. Paseo never clears its finished attention
// (Zach reads cards here, not in Paseo), so factory.seen marks his last look and a newer message to
// the agent starts a new turn. lastActivityAt cannot be the clock: every label write, factory.seen
// included, moves it. A running card has nothing new to read yet.
func unread(a Agent) bool {
	if !a.RequiresAttention || a.AttentionReason != "finished" || a.Status == "running" {
		return false
	}
	seen, err := strconv.ParseInt(a.L(labelSeen), 10, 64)
	if err != nil {
		return true
	}
	last := a.LastUserMessageAt
	if last.IsZero() {
		last = a.CreatedAt
	}
	return last.Unix() > seen
}

func ideaView(i Idea, queue int) *IdeaView {
	return &IdeaView{ID: i.ID, Summary: i.Summary, Why: i.Why, DoneWhen: i.DoneWhen, By: i.By,
		CreatedAt: iso(i.CreatedAt), Status: i.Status, Queue: queue, Agent: i.Agent, Thinking: i.Thinking, Note: i.Note}
}

func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

func atoi(s string) int {
	n := 0
	fmt.Sscan(s, &n)
	return n
}
