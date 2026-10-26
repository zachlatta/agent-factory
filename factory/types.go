// Package factory is a Kanban board and review pipeline on top of Paseo.
//
// Paseo is the orchestrator and the database. Every card is a Paseo agent; its column is the
// agent's `factory.stage` label (or is derived from its status when unlabeled). Review gates are
// fresh Paseo agents. The factory reads agent state from Paseo's files, moves labels, and starts
// or messages agents through the paseo CLI.
package factory

import (
	"context"
	"time"
)

// Paseo runs one paseo CLI command and returns its stdout.
type Paseo interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Agent is one Paseo agent as read from ~/.paseo/agents/*/<id>.json.
type Agent struct {
	ID                string
	Host              string
	Title             string
	Provider          string
	Model             string
	Cwd               string
	Repo              string
	Project           string
	Branch            string
	WorkspaceID       string
	SessionID         string
	Status            string // running, idle, closed, ...
	CreatedAt         time.Time
	LastActivityAt    time.Time
	ArchivedAt        time.Time
	Labels            map[string]string
	RequiresAttention bool
	AttentionReason   string
	LastError         string
	Pending           string // the permission prompt it waits on (`paseo permit ls`), "" for none
}

// L returns the agent's factory.<k> label.
func (a Agent) L(k string) string { return a.Labels[labelPrefix+k] }

func (a Agent) archived() bool { return !a.ArchivedAt.IsZero() }

// stopped: the agent is not working and not archived, so the factory may act on it.
func (a Agent) stopped() bool { return (a.Status == "idle" || a.Status == "closed") && !a.archived() }

// Card is what the board shows: a top-level agent (or a waiting idea) with its sub-agents folded in.
type Card struct {
	ID             string    `json:"id"`
	Short          string    `json:"short"`
	Host           string    `json:"host"`
	Title          string    `json:"title"`
	Repo           string    `json:"repo"`
	Project        string    `json:"project"`
	Branch         string    `json:"branch"`
	Cwd            string    `json:"cwd"`
	Model          string    `json:"model"`
	Status         string    `json:"status"`
	Source         string    `json:"source"`
	Stage          string    `json:"stage"`
	Labelled       bool      `json:"labelled"`
	Flags          []string  `json:"flags"`
	Unread         bool      `json:"unread"`
	Rounds         Rounds    `json:"rounds"`
	Verdict        string    `json:"verdict"`
	Note           string    `json:"note"`
	Tier           string    `json:"tier"`
	Awaiting       string    `json:"awaiting"`
	Origin         string    `json:"origin"`
	Agent          string    `json:"agent"`   // AgentChoice id (factory.agent label)
	Routing        bool      `json:"routing"` // a repo picker (route.go), not the card's real work
	CreatedAt      string    `json:"createdAt"`
	LastActivityAt string    `json:"lastActivityAt"`
	ArchivedAt     string    `json:"archivedAt"`
	Children       []Child   `json:"children"`
	Reviewing      bool      `json:"reviewing"`
	Ask            *Ask      `json:"ask,omitempty"`  // Needs Zach only: what he is asked and how to answer
	Idea           *IdeaView `json:"idea,omitempty"` // waiting ideas, and cards started from one: what it proposes
	WorkspaceID    string    `json:"-"`
	Decided        bool      `json:"-"` // factory.zach-decided: reviews treat Zach's rulings as settled

	// set only for reviewer prompts: Zach's request and follow-ups from the session file
	Request *CardRequest `json:"-"`
}

type Rounds struct {
	Plan int `json:"plan"`
	Code int `json:"code"`
}

type Child struct {
	ID       string `json:"id"`
	Short    string `json:"short"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Archived bool   `json:"archived"`
	Role     string `json:"role"`
	Kind     string `json:"kind"`
	Verdict  string `json:"verdict"`
	// Blocked: running but not moving on its own ("waiting on permission", "stalled N min"); Permission: the prompt
	Blocked    string `json:"blocked,omitempty"`
	Permission string `json:"permission,omitempty"`
}

// Idea is a suggested card waiting to be started (by the factory within its caps, or by Zach).
type Idea struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Request   string    `json:"request"`
	Summary   string    `json:"summary"` // for Zach: what changes and what he gets, plain words
	Why       string    `json:"why"`
	DoneWhen  string    `json:"doneWhen"` // how to tell it worked
	Repo      string    `json:"repo"`
	By        string    `json:"by"`
	Tier      string    `json:"tier"`
	Agent     string    `json:"agent,omitempty"` // AgentChoice id to build it ("" = default)
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"` // waiting, started, dismissed
	Card      string    `json:"card,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	StartedBy string    `json:"startedBy,omitempty"` // auto or zach
	Note      string    `json:"note,omitempty"`      // Zach's changes typed when he started it
}

// IdeaView is the reviewable part of an idea on a card. The request (the builder's brief) is left
// out to keep /api/board small; the panel fetches it from /api/logs/<idea id>.
type IdeaView struct {
	ID        string `json:"id"`
	Summary   string `json:"summary"`
	Why       string `json:"why"`
	DoneWhen  string `json:"doneWhen"`
	By        string `json:"by"`
	CreatedAt string `json:"createdAt"`
	Status    string `json:"status"`
	Queue     int    `json:"queue,omitempty"` // waiting ideas: 1 = next to start
	Agent     string `json:"agent,omitempty"` // AgentChoice id it will be built with ("" = default)
	Note      string `json:"note,omitempty"`  // started ideas: Zach's changes before starting
}

// Body is the JSON body of every POST /api/<action>.
type Body struct {
	ID          string       `json:"id"`
	Stage       string       `json:"stage"`
	Kind        string       `json:"kind"`
	Message     string       `json:"message"` // send: the message; starting an idea: Zach's changes
	Attachments []Attachment `json:"attachments"`
	Request     string       `json:"request"`
	Repo        string       `json:"repo"`
	Title       string       `json:"title"`
	Why         string       `json:"why"`
	Summary     string       `json:"summary"`
	DoneWhen    string       `json:"doneWhen"`
	By          string       `json:"by"`
	Tier        string       `json:"tier"`
	Origin      string       `json:"origin"`
	Agent       string       `json:"agent"`      // AgentChoice id for a new card, an idea, or starting an idea
	Thread      string       `json:"thread"`     // new card: zrl-bot Gmail thread to report in; answer: the reply's thread
	MessageID   string       `json:"message_id"` // answer: Zach's reply
	Text        string       `json:"text"`       // answer: the reply's text
	Via         string       `json:"-"`          // "email": an answer by email (answer.go), not the board
}

type Attachment struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	DataBase64 string `json:"data_base64"`
}

func iso(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
