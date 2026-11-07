package factory

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc writes a /proc where process pid holds the client end of a loopback connection from
// clientPort to serverPort, under parent ppid whose environment names agent.
func fakeProc(t *testing.T, pid, ppid string, clientPort, serverPort int, agent string) string {
	t.Helper()
	root := t.TempDir()
	hexPort := func(p int) string { return fmt.Sprintf("%04X", p) }
	row := "   1: 0100007F:" + hexPort(clientPort)[len(hexPort(clientPort))-4:] + " 0100007F:" + hexPort(serverPort)[len(hexPort(serverPort))-4:] +
		" 01 00000000:00000000 00:00000000 00000000  1000        0 4242 1 0 20 4 30 10 -1\n"
	writeFile(t, filepath.Join(root, "net", "tcp"), "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+row)
	writeFile(t, filepath.Join(root, "net", "tcp6"), "  sl  local_address rem_address\n")
	for _, p := range []string{pid, ppid} {
		if err := os.MkdirAll(filepath.Join(root, p, "fd"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("socket:[4242]", filepath.Join(root, pid, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, pid, "status"), "Name:\tcurl\nPPid:\t"+ppid+"\n")
	writeFile(t, filepath.Join(root, pid, "environ"), "PATH=/bin\x00")
	writeFile(t, filepath.Join(root, ppid, "status"), "Name:\tbash\nPPid:\t1\n")
	writeFile(t, filepath.Join(root, ppid, "environ"), "PATH=/bin\x00PASEO_AGENT_ID="+agent+"\x00")
	return root
}

func TestCallerAgentFindsTheAgentBehindALoopbackRequest(t *testing.T) {
	root := fakeProc(t, "500", "400", 51000, 7478, "sched-agent")
	if got := callerAgentID(root, "127.0.0.1:51000", "127.0.0.1:7478"); got != "sched-agent" {
		t.Errorf("caller = %q", got)
	}
	if got := callerAgentID(root, "127.0.0.1:51001", "127.0.0.1:7478"); got != "" {
		t.Errorf("another connection = %q", got)
	}
	if got := callerAgentID(root, "192.0.2.1:51000", "192.0.2.2:7478"); got != "" {
		t.Errorf("only loopback requests are traced: %q", got)
	}
}

func TestScheduledAgentFilesAScheduledRun(t *testing.T) {
	sched := card("sched-agent", nil)
	sched.Labels = map[string]string{"paseo.schedule-id": "sched-1"}
	sched.Status = "running"
	f, p := newTestFactory(t, sched)
	f.procRoot = fakeProc(t, "500", "400", 51000, 7478, "sched-agent")
	body, _ := json.Marshal(Body{Request: "Run the nightly job"})
	req := httptest.NewRequest("POST", "/api/new", strings.NewReader(string(body)))
	req.Header.Set("X-Factory", "1")
	req.RemoteAddr = "127.0.0.1:51000"
	req = req.WithContext(withLocalAddr(req.Context(), "127.0.0.1:7478"))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if runs := p.runsWithLabel(labelArg(labelOrigin, "schedule")); len(runs) != 1 {
		t.Fatalf("a scheduled agent's card must be a scheduled run: %v", p.calls)
	}
}

func TestAClaimedScheduleIsNotTrusted(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "Run the job", Schedule: "nightly"}); err != nil {
		t.Fatal(err)
	}
	if runs := p.runsWithLabel(labelArg(labelOrigin, "schedule")); len(runs) != 0 {
		t.Fatalf("only the calling agent can make a scheduled run: %v", p.calls)
	}
}

func TestOnlyARunningScheduledAgentCounts(t *testing.T) {
	sched := card("sched-agent", nil)
	sched.Labels = map[string]string{"paseo.schedule-id": "sched-1"}
	sched.Status = "closed"
	f, _ := newTestFactory(t, sched)
	if got := f.callerSchedule("sched-agent"); got != "" {
		t.Errorf("a stopped agent's schedule = %q", got)
	}
	sched.Status = "running"
	f.SetAgents([]Agent{sched})
	if got := f.callerSchedule("sched-agent"); got != "sched-1" {
		t.Errorf("schedule = %q", got)
	}
	if got := f.callerSchedule("nobody"); got != "" {
		t.Errorf("unknown agent = %q", got)
	}
}

// On a real Linux host, a connection from this test process traces back to this process's agent.
func TestCallerAgentOnARealSocket(t *testing.T) {
	want := os.Getenv("PASEO_AGENT_ID")
	if want == "" {
		t.Skip("not run inside a Paseo agent")
	}
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skip("no /proc")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			c.Read(buf)
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := callerAgentID("/proc", conn.LocalAddr().String(), ln.Addr().String()); got != want {
		t.Errorf("caller = %q, want this process's agent", got)
	}
}
