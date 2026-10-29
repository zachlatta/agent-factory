package factory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFactoryBin stands in for the installed factory binary: it logs each call, passes `scan`, and
// answers `land-review` call n with the nth code in exits (default 0), first running mutate-<n> if
// present. The comment carries the markers bin/scan and bin/land look for.
const fakeFactoryBin = `#!/bin/sh
# agent-factory/factory/scan /api/land-review
case "$1" in
scan) echo "scan $*" >>"$FAKE/log"; exit 0 ;;
land-review)
	n=$(( $(cat "$FAKE/n" 2>/dev/null || echo 0) + 1 ))
	echo "$n" >"$FAKE/n"
	echo "land-review $(git rev-parse HEAD)" >>"$FAKE/log"
	[ -f "$FAKE/mutate-$n" ] && sh "$FAKE/mutate-$n" >/dev/null 2>&1
	code=$(cut -d' ' -f"$n" "$FAKE/exits" 2>/dev/null)
	exit "${code:-0}" ;;
esac
exit 9
`

// scriptRepo is a clone (wt) of a bare origin carrying this repo's bin/land, bin/scan and pre-push
// hook, another clone (other) that pushes main too, and the fake binary in fake.
type scriptRepo struct {
	t               *testing.T
	root, wt, other string
	fake, origin    string
	env             []string
}

func newScriptRepo(t *testing.T) *scriptRepo {
	t.Helper()
	root := t.TempDir()
	s := &scriptRepo{t: t, root: root, wt: filepath.Join(root, "wt"), other: filepath.Join(root, "other"),
		fake: filepath.Join(root, "fake"), origin: filepath.Join(root, "up.git")}
	os.MkdirAll(filepath.Join(root, "home"), 0o755)
	os.MkdirAll(s.fake, 0o755)
	os.WriteFile(filepath.Join(s.fake, "factory"), []byte(fakeFactoryBin), 0o755)
	os.WriteFile(filepath.Join(s.fake, "go"), []byte("#!/bin/sh\necho \"go $*\" >>\"$FAKE/log\"\n"), 0o755)
	s.env = append(os.Environ(), "HOME="+filepath.Join(root, "home"), "FAKE="+s.fake,
		"FACTORY_BIN="+filepath.Join(s.fake, "factory"), "PATH="+s.fake+":"+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@users.noreply.github.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@users.noreply.github.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"PASEO_AGENT_ID=")
	s.git(root, "init", "-q", "--bare", "-b", "main", s.origin)
	s.git(root, "clone", "-q", s.origin, s.wt)
	for _, f := range []string{"bin/land", "bin/scan", ".githooks/pre-push"} {
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(filepath.Join(s.wt, f)), 0o755)
		os.WriteFile(filepath.Join(s.wt, f), b, 0o755)
	}
	os.WriteFile(filepath.Join(s.wt, ".gitleaks.toml"), nil, 0o644)
	s.git(s.wt, "checkout", "-q", "-b", "main")
	s.git(s.wt, "add", ".")
	s.git(s.wt, "commit", "-q", "-m", "base")
	s.git(s.wt, "push", "-q", "origin", "main")
	s.git(s.wt, "config", "core.hooksPath", ".githooks")
	s.git(root, "clone", "-q", s.origin, s.other)
	return s
}

func (s *scriptRepo) git(dir string, args ...string) string {
	s.t.Helper()
	out, err := s.cmd(dir, "git", args...)
	if err != nil {
		s.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func (s *scriptRepo) cmd(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir, c.Env = dir, s.env
	out, err := c.CombinedOutput()
	return string(out), err
}

// commit writes path in dir and commits it, returning the commit.
func (s *scriptRepo) commit(dir, path, content string) string {
	s.t.Helper()
	os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644)
	s.git(dir, "add", path)
	s.git(dir, "commit", "-q", "-m", "change "+path)
	return s.git(dir, "rev-parse", "HEAD")
}

func (s *scriptRepo) agent() { s.env = append(s.env, "PASEO_AGENT_ID=agent1") }
func (s *scriptRepo) exits(codes string) {
	os.WriteFile(filepath.Join(s.fake, "exits"), []byte(codes), 0o644)
}
func (s *scriptRepo) mutate(n, sh string) {
	os.WriteFile(filepath.Join(s.fake, "mutate-"+n), []byte(sh), 0o644)
}
func (s *scriptRepo) land() (string, error) { return s.cmd(s.wt, "sh", "bin/land") }
func (s *scriptRepo) main() string          { return s.git(s.root, "--git-dir", s.origin, "rev-parse", "main") }

// log is the fake binary's calls, one per line.
func (s *scriptRepo) log() []string {
	b, _ := os.ReadFile(filepath.Join(s.fake, "log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// reviews is the HEAD each land-review call saw, in order.
func (s *scriptRepo) reviews() []string {
	var out []string
	for _, l := range s.log() {
		if h, ok := strings.CutPrefix(l, "land-review "); ok {
			out = append(out, h)
		}
	}
	return out
}

func (s *scriptRepo) scans() []string {
	var out []string
	for _, l := range s.log() {
		if strings.HasPrefix(l, "scan ") {
			out = append(out, l)
		}
	}
	return out
}

func TestLandReviewsTheRebasedHeadBeforePushing(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	reviewed := s.commit(s.wt, "a.txt", "mine\n")
	s.commit(s.other, "b.txt", "theirs\n")
	s.git(s.other, "push", "-q", "origin", "main")
	out, err := s.land()
	if err != nil {
		t.Fatalf("land: %v\n%s", err, out)
	}
	head := s.git(s.wt, "rev-parse", "HEAD")
	if head == reviewed || s.main() != head {
		t.Fatalf("main %s, HEAD %s, pre-rebase %s", s.main(), head, reviewed)
	}
	// bin/land asks for the rebased candidate, and the pre-push hook checks the same one
	if r := s.reviews(); len(r) != 2 || r[0] != head || r[1] != head {
		t.Errorf("reviews = %v, want the rebased %s twice", r, head)
	}
	if sc := s.scans(); len(sc) != 2 || !strings.Contains(sc[0], "origin/main..HEAD") || !strings.Contains(sc[1], ".."+head) {
		t.Errorf("scans = %v", sc)
	}
}

func TestLandStopsWhenTheReviewDoesNotApprove(t *testing.T) {
	for _, tc := range []struct{ name, exits string }{
		{"changes", "2"}, {"zach decides", "3"}, {"service down or refused", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptRepo(t)
			s.agent()
			s.exits(tc.exits)
			before := s.main()
			s.commit(s.wt, "a.txt", "mine\n")
			out, err := s.land()
			if err == nil || !strings.Contains(out, "did not approve") {
				t.Fatalf("land: %v\n%s", err, out)
			}
			if s.main() != before || len(s.reviews()) != 1 || strings.Contains(out, "retrying") {
				t.Errorf("pushed or retried: main %s reviews %v\n%s", s.main(), s.reviews(), out)
			}
		})
	}
}

func TestLandFailsClosedWithoutTheLandReviewCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  func(s *scriptRepo) string
	}{
		{"missing", func(s *scriptRepo) string { return filepath.Join(s.root, "nowhere", "factory") }},
		{"too old", func(s *scriptRepo) string {
			p := filepath.Join(s.fake, "old")
			os.WriteFile(p, []byte("#!/bin/sh\n# agent-factory/factory/scan\nexit 0\n"), 0o755)
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptRepo(t)
			s.agent()
			s.env = append(s.env, "FACTORY_BIN="+tc.bin(s))
			before := s.main()
			s.commit(s.wt, "a.txt", "mine\n")
			out, err := s.land()
			if err == nil || !strings.Contains(out, "no land-review command") || s.main() != before {
				t.Errorf("land: %v main moved %v\n%s", err, s.main() != before, out)
			}
			// a direct push is refused by the hook the same way
			out, err = s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main")
			if err == nil || !strings.Contains(out, "no land-review command") || s.main() != before {
				t.Errorf("push: %v\n%s", err, out)
			}
		})
	}
}

func TestLandStopsWhenTheCheckoutChangesDuringReview(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	before := s.main()
	s.commit(s.wt, "a.txt", "mine\n")
	s.mutate("1", "echo sneaky >c.txt && git add c.txt && git commit -q -m sneaky")
	out, err := s.land()
	if err == nil || !strings.Contains(out, "changed during the landing review") || s.main() != before {
		t.Errorf("land: %v\n%s", err, out)
	}
}

func TestLandDoesNotRetryAPushTheHookRefused(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	s.exits("0 2") // bin/land's review approves; the hook's review of the same HEAD does not
	before := s.main()
	s.commit(s.wt, "a.txt", "mine\n")
	out, err := s.land()
	if err == nil || !strings.Contains(out, "not retrying") || s.main() != before || len(s.reviews()) != 2 {
		t.Errorf("land: %v reviews %v\n%s", err, s.reviews(), out)
	}
}

func TestLandRetriesARaceWithAFreshReview(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	first := s.commit(s.wt, "a.txt", "mine\n")
	// while bin/land's first review runs, another agent lands on main
	s.mutate("1", "cd '"+s.other+"' && echo theirs >b.txt && git add b.txt && git commit -q -m theirs && git push -q origin main")
	out, err := s.land()
	if err != nil {
		t.Fatalf("land: %v\n%s", err, out)
	}
	head := s.git(s.wt, "rev-parse", "HEAD")
	r := s.reviews()
	if s.main() != head || len(r) != 4 || r[0] != first || r[2] != head || r[3] != head || head == first {
		t.Errorf("main %s HEAD %s reviews %v\n%s", s.main(), head, r, out)
	}
}

func TestLandAsAPersonKeepsTheScanOnlyPath(t *testing.T) {
	s := newScriptRepo(t)
	head := s.commit(s.wt, "a.txt", "mine\n")
	out, err := s.land()
	if err != nil || s.main() != head {
		t.Fatalf("land: %v\n%s", err, out)
	}
	if len(s.reviews()) != 0 || len(s.scans()) != 2 {
		t.Errorf("log = %v", s.log())
	}
}

func TestPrePushChecksEveryPushedRef(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	before := s.main()
	older := s.commit(s.wt, "a.txt", "one\n")
	head := s.commit(s.wt, "b.txt", "two\n")
	// pushing anything but HEAD is refused, and every ref is still scanned
	out, err := s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main", older+":refs/heads/side")
	if err == nil || !strings.Contains(out, "refs/heads/side") || s.main() != before || len(s.reviews()) != 0 {
		t.Errorf("mixed push: %v reviews %v\n%s", err, s.reviews(), out)
	}
	if sc := s.scans(); len(sc) != 2 || !strings.Contains(strings.Join(sc, "\n"), ".."+older) ||
		!strings.Contains(strings.Join(sc, "\n"), ".."+head) {
		t.Errorf("scans = %v", sc)
	}
	// a review that does not approve refuses the push
	s.exits("2")
	if out, err := s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main"); err == nil || s.main() != before {
		t.Errorf("rejected review pushed: %v\n%s", err, out)
	}
	// an approved HEAD goes out, to any branch
	os.Remove(filepath.Join(s.fake, "exits"))
	if out, err := s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main", "HEAD:refs/heads/side"); err != nil || s.main() != head {
		t.Fatalf("approved push: %v\n%s", err, out)
	}
	if r := s.reviews(); len(r) != 2 || r[1] != head {
		t.Errorf("reviews = %v", r)
	}
	// deleting a ref publishes nothing: no scan, no review
	n := len(s.log())
	if out, err := s.cmd(s.wt, "git", "push", "-q", "origin", ":refs/heads/side"); err != nil || len(s.log()) != n {
		t.Errorf("delete: %v log %v\n%s", err, s.log()[n:], out)
	}
	// a person's push of any ref is scanned, not reviewed
	s.env = append(s.env, "PASEO_AGENT_ID=")
	if out, err := s.cmd(s.wt, "git", "push", "-q", "origin", older+":refs/heads/old"); err != nil || len(s.reviews()) != 2 {
		t.Errorf("person push: %v reviews %v\n%s", err, s.reviews(), out)
	}
	if sc := s.scans(); !strings.Contains(sc[len(sc)-1], ".."+older) {
		t.Errorf("person push not scanned: %v", sc)
	}
}

func TestPrePushRefusesWhenHeadMovesDuringReview(t *testing.T) {
	s := newScriptRepo(t)
	s.agent()
	before := s.main()
	s.commit(s.wt, "a.txt", "mine\n")
	// the review sees (and approves) a HEAD other than the one the push sends
	s.mutate("1", "echo sneaky >c.txt && git add c.txt && git commit -q -m sneaky")
	out, err := s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main")
	if err == nil || !strings.Contains(out, "changed during the landing review") || s.main() != before {
		t.Errorf("push: %v\n%s", err, out)
	}
	// and a review that leaves the checkout dirty
	s.mutate("2", "echo scratch >d.txt")
	out, err = s.cmd(s.wt, "git", "push", "-q", "origin", "HEAD:main")
	if err == nil || !strings.Contains(out, "changed during the landing review") || s.main() != before {
		t.Errorf("dirty push: %v\n%s", err, out)
	}
}
