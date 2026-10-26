package factory

import (
	"fmt"
	"sort"
	"time"
)

// Stages are the board's columns, in order: id, name, help text.
var Stages = [][3]string{
	{"idea", "Ideas", "Suggested by agents, and Scouts looking for them. Ideas start on their own, a few at a time."},
	{"plan", "Plan", "Agent writes a plan. No code yet."},
	{"plan-review", "Plan review", "A different agent tries to find fault with the plan."},
	{"building", "Building", "Agent is working."},
	{"review", "Code review", "A different agent checks the work before it lands."},
	{"zach", "Needs Zach", "Waiting on you: a question, an architecture plan, or an approval."},
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
	for _, a := range agents {
		switch parent := a.Labels["paseo.parent-agent-id"]; {
		case anchors[a.ID]:
		case a.L(labelRole) == "reviewer":
			children[a.L(labelReviewOf)] = append(children[a.L(labelReviewOf)], a)
		case parent != "" && byID[parent] && !anchors[parent]:
			children[parent] = append(children[parent], a)
		default:
			tops = append(tops, a)
		}
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
			Children: []Child{}, WorkspaceID: a.WorkspaceID, Decided: a.L(labelDecided) != ""}
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
		if a.Status == "running" && idle > stallMinutes*time.Minute {
			c.Flags = append(c.Flags, fmt.Sprintf("stalled %d min", int(idle.Minutes())))
			if c.Labelled && !a.archived() {
				c.Stage = "zach" // shown only: its labels keep the column, and it returns when activity resumes
			}
		}
		c.Unread = a.RequiresAttention && a.AttentionReason == "finished"
		if a.RequiresAttention && !c.Unread {
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
		if r := a.L(labelAutoReview); r != "" {
			c.Flags = append(c.Flags, r+" review queued")
		}
		for _, k := range kids {
			role := k.L(labelRole)
			if role == "" {
				role = "subagent"
			}
			ch := Child{ID: k.ID, Short: short(k.ID), Title: k.Title, Status: k.Status,
				Archived: k.archived(), Role: role, Kind: k.L(labelReviewKind), Verdict: k.L(labelVerdict)}
			if ch.Blocked = blockedWhy(k, now); ch.Blocked != "" {
				ch.Permission = k.Pending
				c.Flags = append(c.Flags, role+" "+ch.Blocked)
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

func ideaView(i Idea, queue int) *IdeaView {
	return &IdeaView{ID: i.ID, Summary: i.Summary, Why: i.Why, DoneWhen: i.DoneWhen, By: i.By,
		CreatedAt: iso(i.CreatedAt), Status: i.Status, Queue: queue, Agent: i.Agent, Note: i.Note}
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
