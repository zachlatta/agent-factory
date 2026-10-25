// Package scan checks git changes for secrets and private details before they are published.
//
// Three checks run on the lines a change adds: gitleaks (credentials), the generic patterns below
// (hostnames, home paths, emails, long hex ids, dated quotes), and private patterns from a file
// outside the repo (the real host and tailnet names, which a public pattern list would publish).
// Every commit's author and committer email must be a GitHub noreply address.
//
// The factory runs this in-process from its installed binary with the gitleaks config from
// origin/main, so a change cannot weaken the scan that checks it. Hits name the rule and the place,
// never the matched text.
package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Rule is one pattern that must not appear in an added line.
type Rule struct {
	Name string
	Re   *regexp.Regexp
}

// Generic rules name no real host, tailnet or person, so they can live in a public repo.
var Generic = []Rule{
	{"tailnet-hostname", regexp.MustCompile(`(?i)[a-z0-9-]+\.ts\.net\b`)},
	{"home-path", regexp.MustCompile(`/(home|Users)/[a-z]`)},
	{"email", emailRe},
	{"hex-id-16", regexp.MustCompile(`\b[0-9a-f]{16}\b`)},
	{"dated-quote", regexp.MustCompile(`Zach[,]? [(]?20[0-9]{2}-[0-9]{2}`)},
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	// allowedEmail: addresses fine to publish (GitHub noreply, examples, git remotes).
	allowedEmail = regexp.MustCompile(`(?i)^([A-Za-z0-9._%+-]+@users\.noreply\.github\.com|[A-Za-z0-9._%+-]+@example\.(com|org|net)|git@github\.com)$`)
	noreplyRe    = regexp.MustCompile(`(?i)^[A-Za-z0-9._%+-]+@users\.noreply\.github\.com$`)
)

// Hit is one finding. Text is never included.
type Hit struct {
	Rule   string `json:"rule"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Commit string `json:"commit,omitempty"`
}

func (h Hit) String() string {
	s := h.Rule
	if h.File != "" {
		s += " " + h.File
		if h.Line > 0 {
			s += ":" + strconv.Itoa(h.Line)
		}
	}
	if h.Commit != "" {
		s += " (" + shortSHA(h.Commit) + ")"
	}
	return s
}

// Options says what to scan. Exactly one of Range, Staged or Tree.
type Options struct {
	Range  string // "A..B": every commit in the range, intermediate ones included
	Staged bool   // the index, plus the identity the next commit will use
	Tree   bool   // every tracked file and every commit's identity (first push)

	Private        []Rule // from LoadPrivate
	GitleaksConfig string // config file for gitleaks; "" = gitleaks' default rules
	Gitleaks       string // binary; "" = "gitleaks" on PATH
}

// Run scans the repo at dir. A non-nil error means the scan could not run: treat it as a failure.
func Run(ctx context.Context, dir string, o Options) ([]Hit, error) {
	n := 0
	for _, b := range []bool{o.Range != "", o.Staged, o.Tree} {
		if b {
			n++
		}
	}
	if n != 1 {
		return nil, errors.New("scan: give exactly one of a range, staged or tree")
	}
	rules := append(append([]Rule{}, Generic...), o.Private...)

	var hits []Hit
	added, err := addedLines(ctx, dir, o)
	if err != nil {
		return nil, err
	}
	for _, l := range added {
		for _, r := range rules {
			if matches(r, l.text) {
				hits = append(hits, Hit{Rule: r.Name, File: l.file, Line: l.line, Commit: l.commit})
			}
		}
	}
	idHits, err := identities(ctx, dir, o)
	if err != nil {
		return nil, err
	}
	hits = append(hits, idHits...)
	gl, err := gitleaks(ctx, dir, o)
	if err != nil {
		return nil, err
	}
	return append(hits, gl...), nil
}

func matches(r Rule, s string) bool {
	if r.Name != "email" {
		return r.Re.MatchString(s)
	}
	for _, m := range emailRe.FindAllString(s, -1) {
		if !allowedEmail.MatchString(m) {
			return true
		}
	}
	return false
}

// LoadPrivate reads one regular expression per line (blank lines and # comments skipped). Each
// becomes a rule named private-<line number>, so a report never repeats the private value.
func LoadPrivate(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %v", path, i+1, err)
		}
		out = append(out, Rule{Name: "private-" + strconv.Itoa(i+1), Re: re})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no rules", path)
	}
	return out, nil
}

type line struct {
	file, commit, text string
	line               int
}

func addedLines(ctx context.Context, dir string, o Options) ([]line, error) {
	if o.Tree {
		return treeLines(ctx, dir)
	}
	var args []string
	if o.Staged {
		args = []string{"diff", "--cached", "-U0", "--no-color", "--no-ext-diff", "--text"}
	} else {
		// -m: merge commits show their changes too; --text: binary files are scanned as text
		args = []string{"log", "-p", "-m", "-U0", "--no-color", "--no-ext-diff", "--text", "--format=commit %H", o.Range}
	}
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

// parseDiff returns the added lines of `git diff -U0` or `git log -p -U0 --format="commit %H"`.
func parseDiff(out string) []line {
	var res []line
	commit, file, n := "", "", 0
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		t := sc.Text()
		switch {
		case strings.HasPrefix(t, "commit ") && len(t) == 47:
			commit = t[7:]
		case strings.HasPrefix(t, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(t[4:], "b/"), "a/")
		case strings.HasPrefix(t, "@@ "):
			n = hunkStart(t)
		case strings.HasPrefix(t, "+"):
			res = append(res, line{file: file, commit: commit, text: t[1:], line: n})
			n++
		}
	}
	return res
}

var hunkRe = regexp.MustCompile(`\+(\d+)`)

func hunkStart(h string) int {
	m := hunkRe.FindStringSubmatch(h)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func treeLines(ctx context.Context, dir string) ([]line, error) {
	out, err := git(ctx, dir, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var res []line
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		res = append(res, line{file: f, text: f}) // the path itself
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		for i, t := range strings.Split(string(b), "\n") {
			res = append(res, line{file: f, line: i + 1, text: t})
		}
	}
	return res, nil
}

// identities checks author and committer emails: commits in the range (or all of history for a
// tree scan), or the identity the next commit will use (staged).
func identities(ctx context.Context, dir string, o Options) ([]Hit, error) {
	var hits []Hit
	check := func(commit, who, email string) {
		if !noreplyRe.MatchString(email) {
			hits = append(hits, Hit{Rule: who + "-email-not-noreply", Commit: commit})
		}
	}
	if o.Staged {
		for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
			id, err := git(ctx, dir, "var", v)
			if err != nil {
				return nil, err
			}
			who := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(v, "GIT_"), "_IDENT"))
			check("", who, identEmail(id))
		}
		return hits, nil
	}
	rng := o.Range
	if o.Tree {
		if _, err := git(ctx, dir, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
			return nil, nil // no commits yet
		}
		rng = "HEAD"
	}
	out, err := git(ctx, dir, "log", "--format=%H%x00%ae%x00%ce", rng)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\x00")
		if len(f) != 3 {
			continue
		}
		check(f[0], "author", f[1])
		check(f[0], "committer", f[2])
	}
	return hits, nil
}

func identEmail(ident string) string {
	i, j := strings.Index(ident, "<"), strings.Index(ident, ">")
	if i < 0 || j < i {
		return ""
	}
	return ident[i+1 : j]
}

// gitleaks runs gitleaks over the same changes. Inline gitleaks:allow comments and .gitleaksignore
// files are ignored: a change could add either to hide its own secret.
func gitleaks(ctx context.Context, dir string, o Options) ([]Hit, error) {
	bin := o.Gitleaks
	if bin == "" {
		bin = "gitleaks"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("gitleaks not found (%s): install it before scanning", bin)
	}
	cfg := o.GitleaksConfig
	if cfg == "" {
		// an empty config that only extends the defaults, so a .gitleaks.toml in the scanned
		// tree is never picked up on its own
		f, err := os.CreateTemp("", "gitleaks-*.toml")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		f.WriteString("[extend]\nuseDefault = true\n")
		f.Close()
		cfg = f.Name()
	}
	noIgnore, err := os.MkdirTemp("", "gitleaks-noignore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(noIgnore)
	report := filepath.Join(noIgnore, "report.json")
	args := []string{"--no-banner", "--redact", "--ignore-gitleaks-allow", "-i", noIgnore, "-c", cfg,
		"-f", "json", "-r", report, "--exit-code", "3", "-l", "error"}
	switch {
	case o.Tree:
		args = append([]string{"dir"}, append(args, dir)...)
	case o.Staged:
		args = append([]string{"git", "--pre-commit", "--staged"}, append(args, dir)...)
	default:
		args = append([]string{"git", "--log-opts=" + o.Range}, append(args, dir)...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 3) {
		return nil, fmt.Errorf("gitleaks: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	b, rerr := os.ReadFile(report)
	if rerr != nil {
		return nil, fmt.Errorf("gitleaks report: %v", rerr)
	}
	var found []struct {
		RuleID    string
		File      string
		StartLine int
		Commit    string
	}
	if err := json.Unmarshal(b, &found); err != nil {
		return nil, fmt.Errorf("gitleaks report: %v", err)
	}
	if err != nil && len(found) == 0 {
		return nil, errors.New("gitleaks reported leaks but the report is empty")
	}
	var hits []Hit
	for _, g := range found {
		file := g.File
		if rel, err := filepath.Rel(dir, file); err == nil && !strings.HasPrefix(rel, "..") {
			file = rel
		}
		hits = append(hits, Hit{Rule: "gitleaks:" + g.RuleID, File: file, Line: g.StartLine, Commit: g.Commit})
	}
	return hits, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
