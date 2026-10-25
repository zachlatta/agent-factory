package factory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakePaseo records every paseo CLI call and answers `logs`, `run`, `inspect` and `archive` from
// canned data. inspect reports archived[id] and status[id] (default: not archived, idle).
type fakePaseo struct {
	calls      [][]string
	logs       map[string]string
	runs       int
	archived   map[string]bool
	status     map[string]string
	busy       map[string]int // archive fails with "currently running" this many more times
	inspectErr error
	providers  map[string]string // provider ls status by provider id; unset ones are "available"
	fixed      map[string]bool   // providers a `provider diagnostic` turns available
	lsErr      error             // `provider ls` fails with this
	cwd        map[string]string // inspect reports this Cwd
	unknown    map[string]bool   // inspect fails: no such agent
}

func (p *fakePaseo) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "agent" && args[1] == "update" {
		// like Paseo: --label takes comma-separated pairs, so a comma in a value splits it
		for i := 2; i+1 < len(args); i++ {
			if args[i] == "--label" {
				if _, v, _ := strings.Cut(args[i+1], "="); strings.Contains(v, ",") {
					return "", fmt.Errorf("paseo agent update: Error: Invalid label format: %s", v[strings.LastIndex(v, ",")+1:])
				}
			}
		}
	}
	p.calls = append(p.calls, args)
	switch args[0] {
	case "logs":
		return p.logs[args[1]], nil
	case "run":
		p.runs++
		return fmt.Sprintf("new-agent-%d\n", p.runs), nil
	case "inspect":
		if p.inspectErr != nil {
			return "", p.inspectErr
		}
		if p.unknown[args[1]] {
			return "", fmt.Errorf("paseo inspect: agent %s not found", args[1])
		}
		st := p.status[args[1]]
		if st == "" {
			st = "idle"
		}
		return fmt.Sprintf(`{"Id": %q, "Status": %q, "Archived": %t, "Cwd": %q}`, args[1], st, p.archived[args[1]],
			p.cwd[args[1]]), nil
	case "provider":
		switch args[1] {
		case "ls":
			if p.lsErr != nil {
				return "", p.lsErr
			}
			var rows []string
			for _, id := range []string{"claude", "codex", "antigravity", "muse"} {
				st := p.providers[id]
				if st == "" {
					st = "available"
				}
				rows = append(rows, fmt.Sprintf(`{"provider": %q, "label": %q, "status": %q}`, id, id, st))
			}
			return "[" + strings.Join(rows, ",") + "]", nil
		case "diagnostic":
			if p.fixed[args[2]] {
				p.providers[args[2]] = "available"
			}
			return "Status: Ready", nil
		}
	case "archive":
		if p.busy[args[1]] > 0 {
			p.busy[args[1]]--
			return "", fmt.Errorf("paseo archive %s: Error: Agent %s is currently running", args[1], args[1])
		}
		p.archived[args[1]] = true
	}
	return "", nil
}

// verbs returns the paseo verbs called on id, in order (`agent update` counts as "update").
func (p *fakePaseo) verbs(id string) []string {
	var out []string
	for _, c := range p.calls {
		switch {
		case len(c) >= 3 && c[0] == "agent" && c[2] == id:
			out = append(out, c[1])
		case len(c) >= 2 && c[1] == id:
			out = append(out, c[0])
		}
	}
	return out
}

// labels merges every `agent update <id> --label k=v` sent for id (factory. prefix removed).
func (p *fakePaseo) labels(id string) map[string]string {
	out := map[string]string{}
	for _, c := range p.calls {
		if len(c) < 3 || c[0] != "agent" || c[1] != "update" || c[2] != id {
			continue
		}
		for i := 3; i+1 < len(c); i += 2 {
			if c[i] == "--label" {
				k, v, _ := strings.Cut(c[i+1], "=")
				out[strings.TrimPrefix(k, labelPrefix)] = v
			}
		}
	}
	return out
}

// sent returns the messages sent to id with `paseo send`.
func (p *fakePaseo) sent(id string) []string {
	var out []string
	for _, c := range p.calls {
		if c[0] == "send" && len(c) >= 2 && c[len(c)-2] == id {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func (p *fakePaseo) called(verb, id string) bool {
	for _, c := range p.calls {
		if c[0] == verb && len(c) > 1 && c[1] == id {
			return true
		}
	}
	return false
}

// runsWithLabel returns `paseo run` calls carrying the given label.
func (p *fakePaseo) runsWithLabel(label string) [][]string {
	var out [][]string
	for _, c := range p.calls {
		if c[0] != "run" {
			continue
		}
		for i := range c {
			if c[i] == "--label" && i+1 < len(c) && c[i+1] == label {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// the email dispatcher's labels, as a host would configure them
func init() { InboxSource, InboxAnchorRole = "mail-bot", "mail-anchor" }

var t0 = time.Date(2026, 10, 25, 20, 0, 0, 0, time.UTC)

func newTestFactory(t *testing.T, agents ...Agent) (*Factory, *fakePaseo) {
	t.Helper()
	p := &fakePaseo{logs: map[string]string{}, archived: map[string]bool{}, status: map[string]string{},
		busy: map[string]int{}, providers: map[string]string{}, fixed: map[string]bool{},
		cwd: map[string]string{}, unknown: map[string]bool{}}
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: t.TempDir(), DefaultRepo: "example/desk"}, p)
	f.Now = func() time.Time { return t0 }
	f.Sleep = func(time.Duration) {}
	f.GitOut = func(context.Context, string, ...string) (string, error) { return "", fmt.Errorf("no git in tests") }
	f.SetAgents(agents)
	return f, p
}

func card(id string, labels map[string]string) Agent {
	l := map[string]string{}
	for k, v := range labels {
		l[labelPrefix+k] = v
	}
	return Agent{ID: id, Host: "host1", Title: "card " + id, Cwd: "/w/" + id, Repo: "example/desk",
		Status: "idle", Labels: l, CreatedAt: t0.Add(-time.Hour), LastActivityAt: t0.Add(-time.Minute)}
}

// ago is an RFC 3339 time d before t0, for factory.since labels.
func ago(d time.Duration) string { return t0.Add(-d).Format(time.RFC3339) }

func reviewer(id, of, kind string) Agent {
	return Agent{ID: id, Host: "host1", Title: "review", Status: "idle", CreatedAt: t0.Add(-5 * time.Minute),
		LastActivityAt: t0.Add(-time.Minute),
		Labels:         map[string]string{labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewOf: of, labelPrefix + labelReviewKind: kind}}
}
