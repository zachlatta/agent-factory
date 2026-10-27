package factory

import (
	"net/http"
	"sort"
	"time"
)

// TimingSession links an observed native transcript session to a card's step. ObservedAt
// dates the association, not an execution boundary. Earlier links survive session and step
// changes so Factory can attribute individual transcript turns without reading prompt text.
type TimingSession struct {
	AgentID    string    `json:"agentId"`
	ParentID   string    `json:"parentId,omitempty"`
	Device     string    `json:"device"`
	Provider   string    `json:"provider"`
	SessionID  string    `json:"sessionId"`
	Model      string    `json:"model,omitempty"`
	Role       string    `json:"role"`
	ReviewKind string    `json:"reviewKind,omitempty"`
	Stage      string    `json:"stage"`
	CreatedAt  time.Time `json:"createdAt,omitzero"` // agent creation, not native session creation
	ObservedAt time.Time `json:"observedAt"`
}

// observeSessions runs under the timing store's lock, on accepted board reads only. Status,
// activity timestamps and elapsed seconds are deliberately absent: unchanged polls write nothing.
func (t *CardTiming) observeSessions(c Card, agents []Agent, byID map[string]Agent, owners map[string]string, now time.Time) bool {
	last := make(map[string]TimingSession)
	for _, l := range t.Sessions {
		last[l.AgentID] = l
	}
	changed := false
	for _, a := range agents {
		isFellow := a.ID == c.ID || (a.L(labelRole) == "" && sameCard(byID, a.ID, c.ID))
		if !isFellow && owners[a.ID] != c.ID {
			continue
		}
		id := nativeSessionID(a.SessionID)
		if id == "" || a.Provider == "" {
			continue // identity can arrive on a later read
		}
		prev, seen := last[a.ID]
		role, parent := a.L(labelRole), a.Labels["paseo.parent-agent-id"]
		if isFellow {
			role, parent = "fellow", ""
		} else if role == "reviewer" {
			parent = a.L(labelReviewOf)
		} else if role == "" {
			role = "subagent"
		}
		stage, kind := sessionStep(a, c.Stage, byID)
		if seen && (a.gone() || c.Stage == "landed") {
			// Preserve the work's step, but still accept a late native session identity.
			stage, kind = prev.Stage, prev.ReviewKind
		}
		l := TimingSession{AgentID: a.ID, ParentID: parent, Device: a.Host, Provider: a.Provider,
			SessionID: id, Model: a.Model, Role: role, ReviewKind: kind, Stage: stage,
			CreatedAt: a.CreatedAt, ObservedAt: now}
		if seen {
			l.ObservedAt = prev.ObservedAt
			if l == prev {
				continue
			}
			l.ObservedAt = now
		}
		t.Sessions = append(t.Sessions, l)
		last[a.ID], changed = l, true
	}
	return changed
}

// A reviewer and its descendants belong to that review step even when the card moves on.
// Other agents follow the card's observed step. Bound parent traversal like cardOwners.
func sessionStep(a Agent, stage string, byID map[string]Agent) (string, string) {
	for range 10 {
		if a.L(labelRole) == "reviewer" {
			kind := a.L(labelReviewKind)
			switch kind {
			case "plan":
				return "plan-review", kind
			case "code":
				return "review", kind
			}
			return stage, kind
		}
		parent, ok := byID[a.Labels["paseo.parent-agent-id"]]
		if !ok {
			break
		}
		a = parent
	}
	return stage, ""
}

// TimingExport is the read-only GET /api/timings snapshot of recorded card timings and
// transcript session links. It contains workflow facts and transcript identities only;
// mailbox identifiers and transcript bodies stay internal.
type TimingExport struct {
	Cards []TimingExportCard `json:"cards"`
}

type TimingExportCard struct {
	ID            string          `json:"id"`
	Repo          string          `json:"repo,omitempty"`
	Source        string          `json:"source"`
	Tier          string          `json:"tier,omitempty"`
	Partial       bool            `json:"partial,omitempty"`
	RequestedAt   time.Time       `json:"requestedAt,omitzero"`
	RequestedFrom string          `json:"requestedFrom,omitempty"`
	LandedAt      time.Time       `json:"landedAt,omitzero"`
	RepliedAt     time.Time       `json:"repliedAt,omitzero"`
	NoReply       bool            `json:"noReply,omitempty"`
	DoneAt        time.Time       `json:"doneAt,omitzero"`
	Events        []TimingEvent   `json:"events"`
	Rounds        []TimingRound   `json:"rounds,omitempty"`
	Sessions      []TimingSession `json:"sessions,omitempty"`
}

func (s *TimingStore) export() TimingExport {
	all := s.all()
	out := TimingExport{Cards: make([]TimingExportCard, 0, len(all))}
	for id, t := range all {
		out.Cards = append(out.Cards, TimingExportCard{ID: id, Repo: t.Repo, Source: t.Source, Tier: t.Tier,
			Partial: t.Partial, RequestedAt: t.RequestedAt, RequestedFrom: t.RequestedFrom,
			LandedAt: t.LandedAt, RepliedAt: t.RepliedAt, NoReply: t.NoReply, DoneAt: t.doneAt(),
			Events: t.Events, Rounds: t.Rounds, Sessions: t.Sessions})
	}
	sort.Slice(out.Cards, func(i, j int) bool { return out.Cards[i].ID < out.Cards[j].ID })
	return out
}

func (f *Factory) handleTimings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, f.timings.export())
}
