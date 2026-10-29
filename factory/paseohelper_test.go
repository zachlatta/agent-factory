package factory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFakePaseoHelper is not a test: it is the fake helper process the PaseoHelper tests start
// (the test binary run again with FAKE_HELPER set). It greets with FAKE_HELPER_VERSION, or refuses
// when FAKE_HELPER=wrong, logs each request line to FAKE_HELPER_LOG and answers by agent id:
// missing (a daemon error), crash (exits), hang (no answer), unsent, lost; others echo the request.
func TestFakePaseoHelper(t *testing.T) {
	mode := os.Getenv("FAKE_HELPER")
	if mode == "" {
		t.Skip("only runs as the fake helper")
	}
	logLine := func(s string) {
		f, err := os.OpenFile(os.Getenv("FAKE_HELPER_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, s)
			f.Close()
		}
	}
	logLine("start")
	if os.Getenv("PASEO_AGENT_ID") != "" || os.Getenv("PASEO_AGENT_CWD") != "" {
		logLine("agent-env")
	}
	if mode == "wrong" {
		fmt.Println(`{"ready":false,"error":"daemon 0.11.3 does not match CLI 0.11.4"}`)
		os.Exit(1)
	}
	fmt.Printf(`{"ready":true,"cli":%q,"daemon":%q}`+"\n", os.Getenv("FAKE_HELPER_VERSION"), os.Getenv("FAKE_HELPER_VERSION"))
	var mu sync.Mutex
	reply := func(v any) {
		b, _ := json.Marshal(v)
		mu.Lock()
		os.Stdout.Write(append(b, '\n'))
		mu.Unlock()
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		logLine(sc.Text())
		var req helperReq
		json.Unmarshal(sc.Bytes(), &req)
		switch req.Agent {
		case "missing":
			reply(helperReply{ID: req.ID, Error: "Error: Agent not found: missing\nUse \"paseo ls\" to list available agents"})
		case "crash":
			os.Exit(3)
		case "hang":
		case "unsent":
			reply(helperReply{ID: req.ID, Unsent: true, Lost: true, Error: "not connected"})
		case "lost":
			reply(helperReply{ID: req.ID, Lost: true, Error: "Error: Failed to send message: socket closed"})
		default:
			req.ID = 0
			b, _ := json.Marshal(req)
			reply(helperReply{ID: helperIDOf(sc.Bytes()), OK: true, Out: string(b)})
		}
	}
	os.Exit(0)
}

func helperIDOf(line []byte) int64 {
	var r struct{ ID int64 }
	json.Unmarshal(line, &r)
	return r.ID
}

// cliPaseo records the calls that fell back to the CLI.
type cliPaseo struct {
	mu    sync.Mutex
	calls [][]string
}

func (c *cliPaseo) Run(ctx context.Context, args ...string) (string, error) {
	markCall(ctx, callCLI)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, args)
	return "cli\n", nil
}

func (c *cliPaseo) all() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

type helperFixture struct {
	h      *PaseoHelper
	cli    *cliPaseo
	log    string
	pkg    string
	clock  *time.Time
	clockM *sync.Mutex
}

// newFakeHelper is a PaseoHelper that starts the fake helper in mode with version.
func newFakeHelper(t *testing.T, mode, version string) *helperFixture {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "cli")
	os.MkdirAll(pkgDir, 0o700)
	pkg := filepath.Join(pkgDir, "package.json")
	os.WriteFile(pkg, []byte(`{"name":"@getpaseo/cli","version":"`+version+`"}`), 0o600)
	fx := &helperFixture{cli: &cliPaseo{}, log: filepath.Join(dir, "helper.log"), pkg: pkg, clockM: &sync.Mutex{}}
	now := t0
	fx.clock = &now
	h := NewPaseoHelper(CLI{}, filepath.Join(dir, "state"))
	h.cli = fx.cli
	h.findCLI = func() (string, error) { return pkgDir, nil }
	h.now = func() time.Time { fx.clockM.Lock(); defer fx.clockM.Unlock(); return *fx.clock }
	h.command = func(script, cliDir string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFakePaseoHelper$")
		cmd.Env = append(os.Environ(), "FAKE_HELPER="+mode, "FAKE_HELPER_VERSION="+version, "FAKE_HELPER_LOG="+fx.log,
			"PASEO_AGENT_ID=parent", "GORACE=atexit_sleep_ms=0") // -race otherwise holds each exit for a second
		return cmd
	}
	fx.h = h
	t.Cleanup(h.Close)
	return fx
}

func (fx *helperFixture) advance(d time.Duration) {
	fx.clockM.Lock()
	defer fx.clockM.Unlock()
	*fx.clock = fx.clock.Add(d)
}

// lines is the fake helper's log: "start" per start, then its request lines.
func (fx *helperFixture) lines() []string {
	b, _ := os.ReadFile(fx.log)
	return strings.Fields(strings.ReplaceAll(string(b), " ", "_"))
}

func (fx *helperFixture) starts() int {
	n := 0
	for _, l := range fx.lines() {
		if l == "start" {
			n++
		}
	}
	return n
}

func (fx *helperFixture) run(args ...string) (string, string, error) {
	var path string
	out, err := fx.h.Run(withCallPath(context.Background(), &path), args...)
	return out, path, err
}

func TestHelperRequestShapes(t *testing.T) {
	name := "New title"
	for _, c := range []struct {
		args []string
		want *helperReq
	}{
		{[]string{"inspect", "a1", "--json"}, &helperReq{Op: "inspect", Agent: "a1", JSON: true}},
		{[]string{"inspect", "a1"}, &helperReq{Op: "inspect", Agent: "a1"}},
		{[]string{"inspect", "a1", "--yaml"}, nil},
		{[]string{"agent", "update", "a1", "--label", "k=v", "--label", "x=y,z=w"}, &helperReq{Op: "update", Agent: "a1", Labels: []string{"k=v", "x=y,z=w"}}},
		{[]string{"agent", "update", "a1", "--name", name, "--label", "k=v"}, &helperReq{Op: "update", Agent: "a1", Name: &name, Labels: []string{"k=v"}}},
		{[]string{"agent", "update", "a1", "--thinking", "high"}, nil},
		{[]string{"agent", "update", "a1"}, nil},
		{[]string{"send", "--no-wait", "a1", "hello"}, &helperReq{Op: "send", Agent: "a1", Text: "hello"}},
		{[]string{"send", "--no-wait", "--image", "/p/1.png", "--image", "/p/2.png", "a1", "look"}, &helperReq{Op: "send", Agent: "a1", Text: "look", Images: []string{"/p/1.png", "/p/2.png"}}},
		{[]string{"send", "a1", "hello"}, nil},
		{[]string{"send", "--no-wait", "a1", "--not-a-message"}, nil},
		{[]string{"archive", "a1"}, &helperReq{Op: "archive", Agent: "a1"}},
		{[]string{"archive", "a1", "--force"}, &helperReq{Op: "archive", Agent: "a1", Force: true}},
		{[]string{"run", "-d", "x"}, nil},
		{[]string{"permit", "ls", "--json"}, nil},
		{[]string{"stop", "a1"}, nil},
	} {
		got, ok := helperRequest(c.args)
		if c.want == nil {
			if ok {
				t.Errorf("%q served by the helper as %+v; want the CLI", c.args, got)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, *c.want) {
			t.Errorf("%q = %+v, %v; want %+v", c.args, got, ok, *c.want)
		}
	}
}

func TestHelperServesTheFourVerbs(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.13.2")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"inspect", "a1", "--json"}, `{"id":0,"op":"inspect","agent":"a1","json":true}`},
		{[]string{"agent", "update", "a1", "--name", "T", "--label", "k=v"}, `{"id":0,"op":"update","agent":"a1","name":"T","labels":["k=v"]}`},
		{[]string{"send", "--no-wait", "--image", "/i.png", "a1", "hi"}, `{"id":0,"op":"send","agent":"a1","text":"hi","images":["/i.png"]}`},
		{[]string{"archive", "a1", "--force"}, `{"id":0,"op":"archive","agent":"a1","force":true}`},
	} {
		out, path, err := fx.run(c.args...)
		if err != nil || out != c.want || path != callHelper {
			t.Errorf("%q = %q (%s), %v; want %q from the helper", c.args, out, path, err, c.want)
		}
	}
	if out, path, _ := fx.run("permit", "ls", "--json"); out != "cli\n" || path != callCLI {
		t.Errorf("permit ls = %q (%s); want the CLI", out, path)
	}
	if got := fx.cli.all(); len(got) != 1 {
		t.Errorf("CLI calls %q; want only permit ls", got)
	}
	if fx.starts() != 1 {
		t.Errorf("helper started %d times; want once", fx.starts())
	}
	if st := fx.h.Status(); st.State != "running" || st.CLI != "0.13.2" || st.Daemon != "0.13.2" || st.Started == "" {
		t.Errorf("status %+v", st)
	}
}

func TestHelperWrongVersionFallsBackAndRetriesLater(t *testing.T) {
	fx := newFakeHelper(t, "wrong", "0.11.4")
	for range 2 {
		if out, path, err := fx.run("inspect", "a1", "--json"); err != nil || out != "cli\n" || path != callCLI {
			t.Fatalf("inspect = %q (%s), %v; want the CLI", out, path, err)
		}
	}
	if fx.starts() != 1 {
		t.Errorf("started %d times within the retry wait; want once", fx.starts())
	}
	st := fx.h.Status()
	if st.State != "down" || !strings.Contains(st.LastError, "does not match") {
		t.Errorf("status %+v", st)
	}
	fx.advance(helperRetry + time.Second)
	fx.run("inspect", "a1", "--json")
	if fx.starts() != 2 {
		t.Errorf("started %d times after the retry wait; want twice", fx.starts())
	}
	if st := fx.h.Status(); st.Restarts != 1 {
		t.Errorf("restarts = %d; want 1", st.Restarts)
	}
}

func TestHelperThatCannotStartFallsBack(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	fx.h.findCLI = func() (string, error) { return "", fmt.Errorf("no paseo") }
	if out, _, err := fx.run("send", "--no-wait", "a1", "hi"); err != nil || out != "cli\n" {
		t.Errorf("send = %q, %v; want the CLI", out, err)
	}
	if st := fx.h.Status(); st.State != "down" || !strings.Contains(st.LastError, "no paseo") {
		t.Errorf("status %+v", st)
	}
}

func TestHelperCrashFallsBackExceptForSend(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	if out, path, err := fx.run("inspect", "crash", "--json"); err != nil || out != "cli\n" || path != callCLI {
		t.Errorf("inspect after a crash = %q (%s), %v; want the CLI", out, path, err)
	}
	waitFor(t, "the crashed helper marked down", func() bool { return fx.h.Status().State == "down" })
	fx.advance(helperRetry + time.Second)
	_, _, err := fx.run("send", "--no-wait", "crash", "hello")
	if err == nil || !strings.Contains(err.Error(), "may have been sent") || !strings.HasPrefix(err.Error(), "paseo send --no-wait: ") {
		t.Errorf("send after a crash: %v; want an error that it may have been sent", err)
	}
	for _, c := range fx.cli.all() {
		if c[0] == "send" {
			t.Errorf("a send the helper took was sent again through the CLI: %q", c)
		}
	}
}

func TestHelperSendsThroughTheCLIOnlyWhenUnsent(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	if out, path, err := fx.run("send", "--no-wait", "unsent", "hello"); err != nil || out != "cli\n" || path != callCLI {
		t.Errorf("unsent send = %q (%s), %v; want the CLI", out, path, err)
	}
	if _, _, err := fx.run("send", "--no-wait", "lost", "hello"); err == nil || !strings.Contains(err.Error(), "may have been sent") {
		t.Errorf("send whose connection dropped: %v; want an error", err)
	}
	if out, _, err := fx.run("inspect", "lost", "--json"); err != nil || out != "cli\n" {
		t.Errorf("inspect whose connection dropped = %q, %v; want the CLI", out, err)
	}
	if got := fx.cli.all(); len(got) != 2 || got[0][0] != "send" || got[1][0] != "inspect" {
		t.Errorf("CLI calls %q; want the unsent send and the inspect", got)
	}
}

func TestHelperTimeoutStopsItAndFallsBack(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	fx.h.timeout = 300 * time.Millisecond
	if out, _, err := fx.run("agent", "update", "hang", "--label", "k=v"); err != nil || out != "cli\n" {
		t.Errorf("update after a timeout = %q, %v; want the CLI", out, err)
	}
	waitFor(t, "the hung helper stopped", func() bool { return fx.h.Status().State == "down" })
}

func TestHelperDaemonErrorIsReturnedAsTheCLIs(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	_, path, err := fx.run("inspect", "missing", "--json")
	want := `paseo inspect missing: Error: Agent not found: missing` + "\n" + `Use "paseo ls" to list available agents`
	if err == nil || err.Error() != want || path != callHelper {
		t.Errorf("err = %v (%s); want %q", err, path, want)
	}
	if got := fx.cli.all(); len(got) != 0 {
		t.Errorf("a daemon error ran the CLI too: %q", got)
	}
}

func TestHelperArchivesOnlyAtTheFixedVersion(t *testing.T) {
	old := newFakeHelper(t, "ok", "0.11.4")
	if out, _, _ := old.run("archive", "a1"); out != "cli\n" {
		t.Errorf("archive with a 0.11.4 helper = %q; want the CLI", out)
	}
	if out, _, _ := old.run("inspect", "a1", "--json"); out == "cli\n" {
		t.Errorf("inspect with a 0.11.4 helper went to the CLI")
	}
	fixed := newFakeHelper(t, "ok", "0.13.0")
	if out, path, _ := fixed.run("archive", "a1"); path != callHelper || !strings.Contains(out, `"op":"archive"`) {
		t.Errorf("archive with a 0.13.0 helper = %q (%s); want the helper", out, path)
	}
}

func TestHelperRestartsWhenTheCLIChanges(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	fx.run("inspect", "a1", "--json")
	later := time.Now().Add(time.Hour)
	os.Chtimes(fx.pkg, later, later)
	if out, path, _ := fx.run("inspect", "a1", "--json"); path != callHelper {
		t.Errorf("inspect after a CLI upgrade = %q (%s); want a new helper", out, path)
	}
	if fx.starts() != 2 {
		t.Errorf("started %d times; want a restart for the new CLI", fx.starts())
	}
}

func TestHelperDropsAgentEnvAndClosesOnStop(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	fx.run("inspect", "a1", "--json")
	if slices.Contains(fx.lines(), "agent-env") {
		t.Error("the helper got PASEO_AGENT_ID or PASEO_AGENT_CWD")
	}
	p := fx.h.proc
	fx.h.Close()
	if p.alive() {
		t.Error("helper still running after Close")
	}
	if out, _, _ := fx.run("inspect", "a1", "--json"); out != "cli\n" {
		t.Errorf("inspect after Close = %q; want the CLI", out)
	}
	if st := fx.h.Status(); st.State != "closed" {
		t.Errorf("status %+v", st)
	}
}

func TestPaseoCallsCountHelperAndCLI(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: t.TempDir()}, fx.h)
	f.Now = func() time.Time { return t0 }
	f.paseo("inspect", "a1", "--json")
	f.paseo("agent", "update", "a1", "--label", "k=v")
	f.paseo("permit", "ls", "--json")
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	c := h.PaseoCalls
	if c.Calls != 3 || c.HelperCalls != 2 || c.CLICalls != 1 || c.ByVerb["inspect"].Helper != 1 ||
		c.ByVerb["permit ls"].CLI != 1 || c.Helper == nil || c.Helper.State != "running" {
		t.Errorf("paseoCalls = %+v helper %+v", c, c.Helper)
	}
}

func TestDryRunBlocksWritesBeforeTheHelper(t *testing.T) {
	fx := newFakeHelper(t, "ok", "0.11.4")
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: t.TempDir(), DryRun: true}, fx.h)
	f.paseo("send", "--no-wait", "a1", "hello")
	f.paseo("agent", "update", "a1", "--label", "k=v")
	f.paseo("archive", "a1")
	if out, err := f.paseo("inspect", "a1", "--json"); err != nil || !strings.Contains(out, `"op":"inspect"`) {
		t.Errorf("dry-run inspect = %q, %v; want the helper", out, err)
	}
	for _, l := range fx.lines() {
		if strings.Contains(l, `"op":"send"`) || strings.Contains(l, `"op":"update"`) || strings.Contains(l, `"op":"archive"`) {
			t.Errorf("a dry run's write reached the helper: %s", l)
		}
	}
	if got := fx.cli.all(); len(got) != 0 {
		t.Errorf("a dry run's write reached the CLI: %q", got)
	}
}

// The tests below run the real paseohelper.mjs with node against a fake @getpaseo/cli package,
// whose connectToDaemon returns a fake client that logs each call.

const fakeClientJS = `import { appendFileSync, readFileSync } from "node:fs";
const log = (m) => appendFileSync(process.env.FAKE_DAEMON_LOG, JSON.stringify(m) + "\n");
const agents = JSON.parse(readFileSync(process.env.FAKE_DAEMON_AGENTS, "utf8"));
export async function connectToDaemon({ target }) {
  log({ connect: target.home });
  let state = { status: "connected" };
  const listeners = new Set();
  const set = (s, notify = true) => { state = s; if (notify) for (const l of listeners) l(s); };
  return {
    getLastServerInfoMessage: () => ({ version: process.env.FAKE_DAEMON_VERSION }),
    get isConnected() { return state.status === "connected"; },
    subscribeConnectionStatus(l) { listeners.add(l); l(state); return () => listeners.delete(l); },
    async close() { set({ status: "disposed" }); },
    async fetchAgent({ agentId }) {
      log({ fetch: agentId });
      if (agentId === "drop") { set({ status: "disconnected" }); throw new Error("socket closed"); }
      if (agentId === "quietdrop") { set({ status: "disconnected" }, false); throw new Error("socket closed"); }
      if (agentId === "broken") throw new Error("boom");
      const a = agents[agentId] ?? Object.values(agents).find((x) => x.id === agentId);
      return a ? { agent: a, project: null } : null;
    },
    async updateAgent(id, updates) { log({ update: id, updates }); },
    async sendAgentMessage(id, text, opts) {
      if (id === "drop") { set({ status: "disconnected" }); throw new Error("socket closed"); }
      log({ send: id, text, images: opts?.images ?? null });
    },
    async archiveAgent(id) { log({ archive: id }); return { archivedAt: "2026-10-29T19:00:00.000Z" }; },
  };
}
`

const fakeTargetJS = `export function selectDaemonTarget(options, env = process.env) {
  return { kind: "instance", home: env.FAKE_DAEMON_HOME };
}
`

type nodeHelper struct {
	*helperFixture
	daemonLog string
}

// newNodeHelper runs the real helper script against a fake CLI package of version cli whose
// daemon reports version daemon.
func newNodeHelper(t *testing.T, cli, daemon string) *nodeHelper {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	fx := newFakeHelper(t, "ok", cli)
	pkgDir := filepath.Dir(fx.pkg)
	os.MkdirAll(filepath.Join(pkgDir, "dist", "utils"), 0o700)
	os.WriteFile(filepath.Join(pkgDir, "dist", "utils", "client.js"), []byte(fakeClientJS), 0o600)
	os.WriteFile(filepath.Join(pkgDir, "dist", "utils", "daemon-target.js"), []byte(fakeTargetJS), 0o600)
	agents := map[string]any{
		"a1": map[string]any{"id": "a1-full-id", "title": "Card one", "provider": "claude", "model": "default",
			"runtimeInfo": map[string]any{"model": "claude-opus-5-5"}, "effectiveThinkingOptionId": "medium",
			"status": "idle", "archivedAt": nil, "currentModeId": "bypassPermissions", "cwd": "/w/a1",
			"createdAt": "2026-10-29T10:00:00.000Z", "updatedAt": "2026-10-29T11:00:00.000Z",
			"lastUsage":          map[string]any{"inputTokens": 10, "outputTokens": 5},
			"capabilities":       map[string]any{"supportsStreaming": true},
			"availableModes":     []any{map[string]any{"id": "plan", "label": "Plan", "extra": 1}},
			"pendingPermissions": []any{map[string]any{"id": "p1", "name": "Bash"}, map[string]any{"id": "p2"}},
			"labels":             map[string]any{"paseo.worktree": "wt", "paseo.parent-agent-id": "parent-id"}},
		"bare":     map[string]any{"id": "bare", "provider": "codex", "status": "running", "cwd": "/w/b", "createdAt": "c", "updatedAt": "u"},
		"busy":     map[string]any{"id": "busy-agent", "status": "running", "labels": map[string]any{}},
		"archived": map[string]any{"id": "archived-agent", "status": "idle", "archivedAt": "2026-10-28T00:00:00.000Z"},
	}
	b, _ := json.Marshal(agents)
	dir := filepath.Dir(pkgDir)
	agentsFile, daemonLog := filepath.Join(dir, "agents.json"), filepath.Join(dir, "daemon.log")
	os.WriteFile(agentsFile, b, 0o600)
	fx.h.command = func(script, cliDir string) *exec.Cmd {
		cmd := exec.Command("node", script, cliDir)
		cmd.Env = append(os.Environ(), "FAKE_DAEMON_VERSION="+daemon, "FAKE_DAEMON_LOG="+daemonLog,
			"FAKE_DAEMON_AGENTS="+agentsFile, "FAKE_DAEMON_HOME="+dir)
		return cmd
	}
	return &nodeHelper{fx, daemonLog}
}

func (n *nodeHelper) daemonCalls() []string {
	b, _ := os.ReadFile(n.daemonLog)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestNodeHelperInspectMatchesTheCLI(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.11.4")
	out, path, err := n.run("inspect", "a1", "--json")
	want := `{
  "Id": "a1-full-id",
  "Name": "Card one",
  "Provider": "claude",
  "Model": "claude-opus-5-5",
  "Thinking": "medium",
  "Status": "idle",
  "Archived": false,
  "ArchivedAt": null,
  "Mode": "bypassPermissions",
  "Cwd": "/w/a1",
  "CreatedAt": "2026-10-29T10:00:00.000Z",
  "UpdatedAt": "2026-10-29T11:00:00.000Z",
  "LastUsage": {
    "InputTokens": 10,
    "OutputTokens": 5,
    "CachedTokens": 0,
    "CostUsd": 0
  },
  "Capabilities": {
    "Streaming": true,
    "Persistence": false,
    "DynamicModes": false,
    "McpServers": false
  },
  "AvailableModes": [
    {
      "id": "plan",
      "label": "Plan"
    }
  ],
  "PendingPermissions": [
    {
      "id": "p1",
      "tool": "Bash"
    },
    {
      "id": "p2",
      "tool": "unknown"
    }
  ],
  "Worktree": "wt",
  "ParentAgentId": "parent-id"
}
`
	if err != nil || out != want || path != callHelper {
		t.Errorf("inspect = %s (%s), %v; want\n%s", out, path, err, want)
	}
	out, _, _ = n.run("inspect", "bare", "--json")
	var bare map[string]any
	json.Unmarshal([]byte(out), &bare)
	if bare["Name"] != "-" || bare["Model"] != "-" || bare["Thinking"] != "auto" || bare["Mode"] != "default" ||
		bare["LastUsage"] != nil || bare["AvailableModes"] != nil || bare["Worktree"] != nil {
		t.Errorf("bare inspect = %s", out)
	}
	_, _, err = n.run("inspect", "nope", "--json")
	if err == nil || !strings.HasPrefix(err.Error(), "paseo inspect nope: {\n  \"error\": {\n    \"code\": \"AGENT_NOT_FOUND\",\n    \"message\": \"Agent not found: nope\"") {
		t.Errorf("missing agent: %v", err)
	}
	_, _, err = n.run("inspect", "broken")
	if err == nil || err.Error() != "paseo inspect broken: Error: Failed to inspect agent: boom" {
		t.Errorf("daemon error: %v", err)
	}
	if st := n.h.Status(); st.State != "running" || st.CLI != "0.11.4" || st.Daemon != "0.11.4" {
		t.Errorf("status %+v", st)
	}
	if got := n.cli.all(); len(got) != 0 {
		t.Errorf("CLI ran: %q", got)
	}
}

func TestNodeHelperRefusesAnotherDaemonVersion(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.12.0")
	if out, _, _ := n.run("inspect", "a1", "--json"); out != "cli\n" {
		t.Errorf("inspect = %q; want the CLI", out)
	}
	if st := n.h.Status(); st.State != "down" || st.LastError != "daemon 0.12.0 does not match CLI 0.11.4" {
		t.Errorf("status %+v", st)
	}
}

func TestNodeHelperUpdateParsesLabelsLikeTheCLI(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.11.4")
	if _, _, err := n.run("agent", "update", "a1", "--name", " New name ", "--label", "a=1, b=2", "--label", "c=x=y"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.daemonCalls(), `{"update":"a1-full-id","updates":{"name":"New name","labels":{"a":"1","b":"2","c":"x=y"}}}`) {
		t.Errorf("daemon calls %q", n.daemonCalls())
	}
	_, _, err := n.run("agent", "update", "a1", "--label", "novalue")
	if err == nil || err.Error() != "paseo agent update: Error: Invalid label format: novalue\nLabels must be in key=value format" {
		t.Errorf("invalid label: %v", err)
	}
	_, _, err = n.run("agent", "update", "a1", "--name", " ")
	if err == nil || !strings.Contains(err.Error(), "Name cannot be empty") {
		t.Errorf("empty name: %v", err)
	}
	if got := n.cli.all(); len(got) != 0 {
		t.Errorf("CLI ran: %q", got)
	}
}

func TestNodeHelperSendsImages(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.11.4")
	img := filepath.Join(t.TempDir(), "shot.png")
	os.WriteFile(img, []byte("png"), 0o600)
	if _, _, err := n.run("send", "--no-wait", "--image", img, "a1", "look at this"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(n.daemonCalls(), `{"send":"a1","text":"look at this","images":[{"data":"cG5n","mimeType":"image/png"}]}`) {
		t.Errorf("daemon calls %q", n.daemonCalls())
	}
	_, _, err := n.run("send", "--no-wait", "--image", filepath.Join(t.TempDir(), "gone.png"), "a1", "x")
	if err == nil || !strings.Contains(err.Error(), "ENOENT") {
		t.Errorf("missing image: %v", err)
	}
	if got := n.cli.all(); len(got) != 0 {
		t.Errorf("CLI ran: %q", got)
	}
}

func TestNodeHelperArchiveChecks(t *testing.T) {
	n := newNodeHelper(t, "0.13.0", "0.13.0")
	if _, _, err := n.run("archive", "busy"); err == nil || err.Error() != "paseo archive busy: Error: Agent busy-ag is currently running\n"+
		"Use --force to archive a running agent (it will interrupt the active run), or stop it first with: paseo agent stop. Use paseo agent delete to hard-delete it." {
		t.Errorf("running agent: %v", err)
	}
	if _, _, err := n.run("archive", "archived"); err == nil || !strings.Contains(err.Error(), "Agent archive is already archived") {
		t.Errorf("archived agent: %v", err)
	}
	if _, _, err := n.run("archive", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing agent: %v", err)
	}
	out, path, err := n.run("archive", "busy", "--force")
	if err != nil || path != callHelper || !strings.Contains(out, `"archivedAt": "2026-10-29T19:00:00.000Z"`) {
		t.Errorf("forced archive = %q (%s), %v", out, path, err)
	}
	if !slices.Contains(n.daemonCalls(), `{"archive":"busy-agent"}`) {
		t.Errorf("daemon calls %q", n.daemonCalls())
	}
}

func TestNodeHelperConnectionLoss(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.11.4")
	// the connection drops quietly: the next send is answered unsent, so the CLI sends it
	if out, _, _ := n.run("inspect", "quietdrop", "--json"); out != "cli\n" {
		t.Errorf("inspect on a lost connection = %q; want the CLI", out)
	}
	if out, _, _ := n.run("send", "--no-wait", "a1", "hello"); out != "cli\n" {
		t.Errorf("send on a lost connection = %q; want the CLI", out)
	}
	n.h.Close()

	// the connection drops during a send: it is not sent again, and the helper exits
	n = newNodeHelper(t, "0.11.4", "0.11.4")
	n.run("inspect", "a1", "--json")
	if _, _, err := n.run("send", "--no-wait", "drop", "hello"); err == nil || !strings.Contains(err.Error(), "may have been sent") {
		t.Errorf("send that lost the connection: %v", err)
	}
	waitFor(t, "the helper exited after the connection dropped", func() bool { return n.h.Status().State == "down" })
	if got := n.cli.all(); len(got) != 0 {
		t.Errorf("CLI ran: %q", got)
	}
}

func TestNodeHelperExitsWhenItsInputCloses(t *testing.T) {
	n := newNodeHelper(t, "0.11.4", "0.11.4")
	n.run("inspect", "a1", "--json")
	n.h.mu.Lock()
	p := n.h.proc
	n.h.mu.Unlock()
	// a request read just before the input closed is still answered
	ch := make(chan helperReply, 1)
	p.mu.Lock()
	p.pending[99] = ch
	p.mu.Unlock()
	p.stdin.Write([]byte(`{"id":99,"op":"inspect","agent":"a1","json":true}` + "\n"))
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Error("the helper kept running after its input closed")
	}
	select {
	case rep := <-ch:
		if !rep.OK || !strings.Contains(rep.Out, `"Id": "a1-full-id"`) {
			t.Errorf("last answer %+v", rep)
		}
	default:
		t.Error("the request sent before the input closed was not answered")
	}
}
