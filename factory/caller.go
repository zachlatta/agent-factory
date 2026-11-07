package factory

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A card filed by a scheduled run of an approved job needs no sign-off. The factory does not take a
// caller's word for that: for a request over loopback it finds the process holding the client end of
// the connection in /proc, walks up its parents to the nearest PASEO_AGENT_ID, and asks whether that
// agent is a running scheduled run (paseo.schedule-id). Anything it cannot trace is not a scheduled run.

// callerAgentID is the Paseo agent behind a loopback request from remote to local, or "".
func callerAgentID(procRoot, remote, local string) string {
	rh, rp, err := net.SplitHostPort(remote)
	if err != nil {
		return ""
	}
	lh, lp, err := net.SplitHostPort(local)
	if err != nil || !net.ParseIP(rh).IsLoopback() || !net.ParseIP(lh).IsLoopback() {
		return ""
	}
	cport, err1 := strconv.Atoi(rp)
	sport, err2 := strconv.Atoi(lp)
	if err1 != nil || err2 != nil {
		return ""
	}
	inode := socketInode(procRoot, cport, sport)
	if inode == "" {
		return ""
	}
	pid := socketOwner(procRoot, inode)
	for range 32 { // up the process tree to the nearest agent
		if pid == "" || pid == "0" || pid == "1" {
			return ""
		}
		if id := envValue(filepath.Join(procRoot, pid, "environ"), "PASEO_AGENT_ID"); id != "" {
			return id
		}
		pid = parentPID(procRoot, pid)
	}
	return ""
}

// socketInode is the inode of the client socket from cport to sport in /proc/net/tcp or tcp6.
func socketInode(procRoot string, cport, sport int) string {
	for _, name := range []string{"tcp", "tcp6"} {
		raw, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			fs := strings.Fields(line)
			if len(fs) < 10 || hexPort(fs[1]) != cport || hexPort(fs[2]) != sport {
				continue
			}
			return fs[9]
		}
	}
	return ""
}

func hexPort(addr string) int {
	_, p, ok := strings.Cut(addr, ":")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(p, 16, 32)
	if err != nil {
		return -1
	}
	return int(n)
}

// socketOwner is the process holding socket inode, or "".
func socketOwner(procRoot, inode string) string {
	want := "socket:[" + inode + "]"
	pids, _ := os.ReadDir(procRoot)
	for _, p := range pids {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(procRoot, p.Name(), "fd"))
		if err != nil {
			continue // another user's process, or gone
		}
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join(procRoot, p.Name(), "fd", fd.Name())); err == nil && l == want {
				return p.Name()
			}
		}
	}
	return ""
}

func parentPID(procRoot, pid string) string {
	raw, err := os.ReadFile(filepath.Join(procRoot, pid, "status"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "PPid:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func envValue(path, key string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// callerSchedule is the schedule of agent id when it is a running scheduled run, or "".
func (f *Factory) callerSchedule(id string) string {
	if id == "" {
		return ""
	}
	a, ok := f.agentOK(id)
	if !ok || a.Status != "running" {
		return ""
	}
	return a.Labels["paseo.schedule-id"]
}

// requestSchedule is the schedule behind request r, traced through the calling process.
func (f *Factory) requestSchedule(r *http.Request) string {
	local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if local == nil {
		return ""
	}
	return f.callerSchedule(callerAgentID(f.procRoot, r.RemoteAddr, local.String()))
}

// withLocalAddr sets the server address a request arrived on, as net/http does, for tests.
func withLocalAddr(ctx context.Context, addr string) context.Context {
	a, _ := net.ResolveTCPAddr("tcp", addr)
	return context.WithValue(ctx, http.LocalAddrContextKey, net.Addr(a))
}
