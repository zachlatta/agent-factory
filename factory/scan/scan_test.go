package scan

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Fixtures are built at run time from pieces, so this file itself holds nothing the scanner flags.
var (
	fakeToken = "gh" + "p_" + "L76BeHTg" + "TwaXzTj1" + "rkMWpQYR" + "hqngIyT2" + "MoWN" // made up
	tailnet   = "box." + "secret-tail" + ".ts" + ".net"
	homePath  = "/ho" + "me/alice/notes"
	email     = "alice" + "@" + "corp.example" + "co.uk"
	hexID     = "1a2b3c4d" + "5e6f7a8b"
	quote     = "Za" + "ch (20" + "26-01-02): ship it"
	noreply   = "1+bot" + "@users.noreply.github.com"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func needGitleaks(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gitleaks"); err != nil {
		t.Skip("gitleaks not installed")
	}
}

// repo makes a git repo with one clean commit by a noreply author.
func repo(t *testing.T) string {
	t.Helper()
	needGit(t)
	d := t.TempDir()
	run(t, d, "init", "-q", "-b", "main")
	run(t, d, "config", "user.email", noreply)
	run(t, d, "config", "user.name", "bot")
	write(t, d, "README.md", "hello\n")
	run(t, d, "add", ".")
	run(t, d, "commit", "-q", "-m", "init")
	return d
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rules(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Rule)
	}
	return out
}

func commitFile(t *testing.T, d, name, body string) {
	t.Helper()
	write(t, d, name, body)
	run(t, d, "add", ".")
	run(t, d, "commit", "-q", "-m", "change "+name)
}

func TestGenericRulesHit(t *testing.T) {
	cases := map[string]string{
		"tailnet-hostname": "url = https://" + tailnet + ":7443/",
		"home-path":        "dir " + homePath,
		"email":            "mail " + email,
		"hex-id-16":        "thread " + hexID,
		"dated-quote":      quote,
	}
	for rule, text := range cases {
		hit := false
		for _, r := range Generic {
			if r.Name == rule && matches(r, text) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("%s: no hit on its fixture", rule)
		}
	}
}

func TestAllowedTextPasses(t *testing.T) {
	for _, s := range []string{
		noreply, "ops" + "@example.com", "git" + "@github.com:org/repo.git",
		"Zach approves the plan", "Needs Zach", "waits on Zach (stuck)", "email to Zach",
		"commit " + strings.Repeat("ab", 20), // 40-char SHA
		"module github.com/zachlatta/agent-factory", "*.ts.net names are private",
	} {
		for _, r := range Generic {
			if matches(r, s) {
				t.Errorf("%q: unexpected %s hit", s, r.Name)
			}
		}
	}
}

func TestRangeFindsEveryRuleAndIntermediateCommits(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	base := strings.TrimSpace(run(t, d, "rev-parse", "HEAD"))
	commitFile(t, d, "a.txt", "url https://"+tailnet+"/\n"+homePath+"\n"+email+"\n"+hexID+"\n"+quote+"\n")
	commitFile(t, d, "cfg/token.txt", "token = \""+fakeToken+"\"\n")
	// removed again in a later commit: still published by the push, so still a hit
	os.Remove(filepath.Join(d, "cfg/token.txt"))
	run(t, d, "add", "-A")
	run(t, d, "commit", "-q", "-m", "remove")
	hits, err := Run(context.Background(), d, Options{Range: base + "..HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	got := rules(hits)
	for _, want := range []string{"tailnet-hostname", "home-path", "email", "hex-id-16", "dated-quote"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if !slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, "gitleaks:") }) {
		t.Errorf("gitleaks did not flag the token: %v", got)
	}
	for _, h := range hits {
		if h.Rule == "email" && (h.File != "a.txt" || h.Line != 3) {
			t.Errorf("email hit at %s:%d, want a.txt:3", h.File, h.Line)
		}
		if strings.Contains(h.String(), "alice") || strings.Contains(h.String(), fakeToken[:8]) {
			t.Errorf("hit leaks the matched text: %s", h)
		}
	}
}

func TestCleanRangePasses(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	base := strings.TrimSpace(run(t, d, "rev-parse", "HEAD"))
	commitFile(t, d, "doc.md", "Zach approves the plan. Contact ops"+"@example.com.\n")
	hits, err := Run(context.Background(), d, Options{Range: base + "..HEAD"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits %v, err %v", hits, err)
	}
}

func TestPrivateRules(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	base := strings.TrimSpace(run(t, d, "rev-parse", "HEAD"))
	commitFile(t, d, "x.go", "// runs on "+"bigbox"+" every night\n")
	without, err := Run(context.Background(), d, Options{Range: base + "..HEAD"})
	if err != nil || len(without) != 0 {
		t.Fatalf("without private rules: hits %v, err %v", without, err)
	}
	pf := filepath.Join(t.TempDir(), "denylist")
	os.WriteFile(pf, []byte("# hosts\n\\b"+"big"+"box\\b\n"), 0o600)
	priv, err := LoadPrivate(pf)
	if err != nil {
		t.Fatal(err)
	}
	with, err := Run(context.Background(), d, Options{Range: base + "..HEAD", Private: priv})
	if err != nil {
		t.Fatal(err)
	}
	if got := rules(with); !slices.Equal(got, []string{"private-2"}) {
		t.Fatalf("got %v, want [private-2] (named by line, not by value)", got)
	}
}

func TestLoadPrivateFailsClosed(t *testing.T) {
	if _, err := LoadPrivate(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: no error")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, []byte("# nothing\n"), 0o600)
	if _, err := LoadPrivate(empty); err == nil {
		t.Error("file with no rules: no error")
	}
}

func TestAuthorMustBeNoreply(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	base := strings.TrimSpace(run(t, d, "rev-parse", "HEAD"))
	run(t, d, "-c", "user.email=me"+"@corp.example", "commit", "-q", "--allow-empty", "-m", "oops")
	hits, err := Run(context.Background(), d, Options{Range: base + "..HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rules(hits); !slices.Contains(got, "author-email-not-noreply") || !slices.Contains(got, "committer-email-not-noreply") {
		t.Fatalf("got %v", got)
	}
}

func TestStagedChecksIndexAndIdentity(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	write(t, d, "b.txt", "see https://"+tailnet+"\n")
	run(t, d, "add", ".")
	hits, err := Run(context.Background(), d, Options{Staged: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := rules(hits); !slices.Equal(got, []string{"tailnet-hostname"}) {
		t.Fatalf("got %v", got)
	}
	run(t, d, "config", "user.email", "me"+"@corp.example")
	hits, _ = Run(context.Background(), d, Options{Staged: true})
	if got := rules(hits); !slices.Contains(got, "author-email-not-noreply") {
		t.Fatalf("staged identity not checked: %v", got)
	}
}

func TestTreeScansFilesAndHistory(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	write(t, d, "notes/"+hexID+".md", "fine\n")
	write(t, d, "c.txt", "token = \""+fakeToken+"\"\n")
	run(t, d, "add", ".")
	hits, err := Run(context.Background(), d, Options{Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	got := rules(hits)
	if !slices.Contains(got, "hex-id-16") || !slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, "gitleaks:") }) {
		t.Fatalf("got %v", got)
	}
}

// A change cannot hide its own secret with inline allow comments or a .gitleaksignore file.
func TestIgnoresInlineAllowAndIgnoreFile(t *testing.T) {
	needGitleaks(t)
	d := repo(t)
	base := strings.TrimSpace(run(t, d, "rev-parse", "HEAD"))
	commitFile(t, d, "d.txt", "token = \""+fakeToken+"\" # gitleaks"+":allow\n")
	commitFile(t, d, ".gitleaksignore", "d.txt:generic-api-key:1\n")
	commitFile(t, d, ".gitleaks.toml", "[allowlist]\npaths = ['''.*''']\n")
	hits, err := Run(context.Background(), d, Options{Range: base + "..HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rules(hits), func(r string) bool { return strings.HasPrefix(r, "gitleaks:") }) {
		t.Fatalf("allow comment or ignore file hid the token: %v", rules(hits))
	}
}

func TestMissingGitleaksFailsClosed(t *testing.T) {
	d := repo(t)
	_, err := Run(context.Background(), d, Options{Tree: true, Gitleaks: "gitleaks-not-installed-here"})
	if err == nil || !regexp.MustCompile(`gitleaks not found`).MatchString(err.Error()) {
		t.Fatalf("err %v", err)
	}
}

func TestExactlyOneMode(t *testing.T) {
	if _, err := Run(context.Background(), t.TempDir(), Options{}); err == nil {
		t.Error("no mode: no error")
	}
	if _, err := Run(context.Background(), t.TempDir(), Options{Tree: true, Staged: true}); err == nil {
		t.Error("two modes: no error")
	}
}
