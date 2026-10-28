package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePaseo records every paseo CLI call and answers `run`, `inspect` and `archive` from
// canned data; `send` unarchives, as Paseo does. inspect reports archived[id] and status[id] (default: not archived, idle).
type fakePaseo struct {
	mu         sync.Mutex // guards the fields below; background refreshes call Run concurrently
	lsCalls    atomic.Int32
	lsBlock    chan struct{} // when non-nil, `provider ls` waits for it to close
	calls      [][]string
	runs       int
	archived   map[string]bool
	status     map[string]string
	busy       map[string]int // archive fails with "currently running" this many more times
	runErr     error          // `paseo run` fails with this
	inspectErr error
	providers  map[string]string // provider ls status by provider id; unset ones are "available"
	fixed      map[string]bool   // providers a `provider diagnostic` turns available
	lsErr      error             // `provider ls` fails with this
	cwd        map[string]string // inspect reports this Cwd
	unknown    map[string]bool   // inspect fails: no such agent
	permits    string            // `permit ls --json` output (default "[]")
	permitErr  error             // `permit ls` fails with this
}

func (p *fakePaseo) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "provider" && args[1] == "ls" {
		p.lsCalls.Add(1)
		if p.lsBlock != nil {
			<-p.lsBlock
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(args) > 1 && args[0] == "agent" && args[1] == "update" {
		// like Paseo: --label takes comma-separated pairs, so a comma in a value splits it
		for i := 2; i+1 < len(args); i++ {
			if args[i] == "--label" {
				if _, v, _ := strings.Cut(args[i+1], "="); strings.Contains(v, ",") {
					return "", fmt.Errorf("paseo agent update: Error: Invalid label format: %s", v[strings.LastIndex(v, ",")+1:])
				}
			}
			if args[i] == "--name" && strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("paseo agent update: Error: Name cannot be empty")
			}
		}
	}
	p.calls = append(p.calls, args)
	switch args[0] {
	case "run":
		if p.runErr != nil {
			return "", p.runErr
		}
		p.runs++
		if quietRunLabels(args)[labelPrefix+labelRole] == quietAnchorRole {
			p.cwd[fmt.Sprintf("new-agent-%d", p.runs)] = argAfter(args, "--cwd")
		}
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
		at := ""
		if p.archived[args[1]] {
			at = t0.Format(time.RFC3339)
		}
		return fmt.Sprintf(`{"Id": %q, "Status": %q, "Archived": %t, "ArchivedAt": %q, "Cwd": %q}`, args[1], st, p.archived[args[1]], at,
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
	case "permit":
		if p.permitErr != nil {
			return "", p.permitErr
		}
		if p.permits == "" {
			return "[]", nil
		}
		return p.permits, nil
	case "stop":
		p.status[args[1]] = "idle"
	case "archive":
		if p.busy[args[1]] > 0 {
			p.busy[args[1]]--
			return "", fmt.Errorf("paseo archive %s: Error: Agent %s is currently running", args[1], args[1])
		}
		p.archived[args[1]] = true
	case "send":
		p.archived[args[len(args)-2]] = false // like Paseo: a send unarchives the agent
	}
	return "", nil
}

// countCalls counts recorded calls whose first two words match ("" matches any word).
// Safe while a background refresh is running; the other accessors are not.
func (p *fakePaseo) countCalls(a, b string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if len(c) >= 2 && (a == "" || c[0] == a) && (b == "" || c[1] == b) {
			n++
		}
	}
	return n
}

// waitFor polls cond until it holds or the deadline passes. For background-refresh tests.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// verbs returns the paseo verbs called on id, in order (`agent update` counts as "update").
func (p *fakePaseo) verbs(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	defer p.mu.Unlock()
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

// names returns the names set on id with `agent update <id> --name`, in order.
func (p *fakePaseo) names(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if len(c) < 3 || c[0] != "agent" || c[1] != "update" || c[2] != id {
			continue
		}
		for i := 3; i+1 < len(c); i += 2 {
			if c[i] == "--name" {
				out = append(out, c[i+1])
			}
		}
	}
	return out
}

// sent returns the messages sent to id with `paseo send`.
func (p *fakePaseo) sent(id string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if c[0] == "send" && len(c) >= 2 && c[len(c)-2] == id {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func (p *fakePaseo) called(verb, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		if c[0] == verb && len(c) > 1 && c[1] == id {
			return true
		}
	}
	return false
}

// runsWithLabel returns `paseo run` calls carrying the given label.
func (p *fakePaseo) runsWithLabel(label string) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p := &fakePaseo{archived: map[string]bool{}, status: map[string]string{},
		busy: map[string]int{}, providers: map[string]string{}, fixed: map[string]bool{},
		cwd: map[string]string{}, unknown: map[string]bool{}}
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: t.TempDir(), DefaultRepo: "example/desk"}, p)
	f.Now = func() time.Time { return t0 }
	// Most lifecycle tests begin with an already provisioned quiet anchor. Anchor bootstrap and
	// recovery tests clear this cache explicitly and exercise the production path with fake Paseo.
	f.quiet = quietState{id: "quiet-anchor", checked: t0}
	p.cwd["quiet-anchor"] = filepath.Join(f.cfg.StateDir, "quiet-anchor")
	f.Sleep = func(time.Duration) {}
	// no git, except an empty diff for the always-ask file check (alwaysask.go), so it hits nothing
	f.GitOut = gitStub(cleanDiff)
	f.Usage = func(context.Context, string) AgentUsage { return unknownUsage() }

	managed := quietManaged(agents)
	for i, a := range agents {
		if !managed[a.ID] {
			continue
		}
		a.Labels = maps.Clone(a.Labels)
		if a.Labels[parentAgentLabel] == "" {
			a.Labels[parentAgentLabel] = "quiet-anchor"
		}
		if a.L(labelQuiet) == "" {
			a.Labels[labelPrefix+labelQuiet] = "quiet-anchor"
		}
		agents[i] = a
	}
	f.SetAgents(agents)
	return f, p
}

// cleanDiff answers the always-ask file check (alwaysask.go): the card changes no file on the list.
var cleanDiff = map[string]string{"diff --no-renames --name-only origin/main...HEAD": ""}

// idea is card, started by the factory from an idea: its finished work lands without Zach unless the
// always-ask check hits.
func idea(id string, labels map[string]string) Agent {
	a := card(id, labels)
	a.Labels[labelPrefix+labelOrigin] = "idea"
	return a
}

func card(id string, labels map[string]string) Agent {
	l := map[string]string{}
	for k, v := range labels {
		l[labelPrefix+k] = v
	}
	return Agent{ID: id, Host: "host1", Title: "card " + id, Cwd: "/w/" + id, Repo: "example/desk",
		Provider: "claude", SessionID: testSession(id),
		Status: "idle", Labels: l, CreatedAt: t0.Add(-time.Hour), LastActivityAt: t0.Add(-time.Minute)}
}

// ago is an RFC 3339 time d before t0, for factory.since labels.
func ago(d time.Duration) string { return t0.Add(-d).Format(time.RFC3339) }

func reviewer(id, of, kind string) Agent {
	return Agent{ID: id, Host: "host1", Title: "review", Status: "idle", CreatedAt: t0.Add(-5 * time.Minute),
		Provider: "claude", SessionID: testSession(id),
		LastActivityAt: t0.Add(-time.Minute),
		Labels:         map[string]string{labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewOf: of, labelPrefix + labelReviewKind: kind}}
}

// testSession is the Claude session id card and reviewer give agent id; storeLog writes its file.
func testSession(id string) string { return "s-" + strings.NewReplacer("/", "-", ".", "-").Replace(id) }

var logTag = regexp.MustCompile(`^\[(User|Thought|[A-Z][A-Za-z_ -]{1,30})\]\s?(.*)$`)

// storeLog gives agent id the history text, in `paseo logs --filter text` shape ("[User] …",
// "[Thought] …", "[Tool] …" one-line entries, other lines the agent's text), as a Claude
// session file at testSession(id).
func storeLog(t *testing.T, f *Factory, _ *fakePaseo, id, text string) {
	t.Helper()
	type entry struct{ typ, block, text string }
	var es []entry
	for _, line := range strings.Split(text, "\n") {
		if m := logTag.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "Assistant":
				es = append(es, entry{"assistant", "text", m[2]})
			case "User":
				es = append(es, entry{"user", "", m[2]})
			case "Thought":
				es = append(es, entry{"assistant", "thinking", m[2]})
			default:
				es = append(es, entry{"assistant", "tool_use", m[1]})
			}
			continue
		}
		// Lines after a [User] line are the reply.
		if n := len(es); n > 0 && es[n-1].block == "text" {
			es[n-1].text += "\n" + line
			continue
		}
		es = append(es, entry{"assistant", "text", line})
	}
	var lines []string
	for n, e := range es {
		var msg map[string]any
		switch e.block {
		case "":
			msg = map[string]any{"content": e.text}
		case "thinking":
			msg = map[string]any{"content": []any{map[string]any{"type": "thinking", "thinking": e.text}}}
		case "tool_use":
			msg = map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprint("u", n), "name": e.text, "input": map[string]any{}}}}
		default:
			msg = map[string]any{"content": []any{map[string]any{"type": "text", "text": e.text}}}
		}
		b, _ := json.Marshal(map[string]any{"type": e.typ, "message": msg})
		lines = append(lines, string(b))
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-test/"+testSession(id)+".jsonl", strings.Join(lines, "\n")+"\n")
}
