package factory

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
)

// The always-ask list: a change Zach did not ask for lands after the gates unless it changes the
// factory's own gates or rules, adds a service, or is architecture. Part of that check is mechanical:
// which files the diff touches, against builtinAlwaysAsk and the repo's own .factory.json patterns.
// The patterns are read from origin/main, never from the card's copy, so a card cannot loosen the
// check that applies to it; a card that edits .factory.json is always asked.

// repoConfigFile is the repo's factory settings at its root.
const repoConfigFile = ".factory.json"

// repoConfig is the part of .factory.json this file reads.
type repoConfig struct {
	// AlwaysAsk lists path patterns (`*` within a name, `**` across directories) whose change
	// always comes to Zach before it lands.
	AlwaysAsk []string `json:"alwaysAsk"`
}

// builtinAlwaysAsk applies in every repo: agent rules and gates, new services and new dependencies.
var builtinAlwaysAsk = []string{
	// gates and rules
	"**/AGENTS.md", "**/CLAUDE.md", ".githooks/**", ".github/workflows/**", ".gitleaks.toml", "**/.gitleaksignore",
	repoConfigFile,
	// services
	"deploy/**", "**/*.service", "**/*.timer", "**/Dockerfile", "**/Dockerfile.*", "**/docker-compose*.yml",
	"**/docker-compose*.yaml", "**/compose.yml", "**/compose.yaml", "**/Procfile",
	// dependencies
	"**/go.mod", "**/package.json", "**/pyproject.toml", "**/requirements*.txt", "**/Gemfile", "**/Cargo.toml",
}

// globMatch reports whether name matches pattern: `**` matches any number of directories (including
// none), and every other segment follows path.Match.
func globMatch(pattern, name string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchParts(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for i := 0; i <= len(n); i++ {
				if matchParts(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, err := path.Match(p[0], n[0]); err != nil || !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}

// alwaysAskHits returns the changed files that match patterns, in diff order.
func alwaysAskHits(files, patterns []string) []string {
	var hits []string
	for _, file := range files {
		for _, p := range patterns {
			if globMatch(p, file) {
				hits = append(hits, file)
				break
			}
		}
	}
	return hits
}

// alwaysAskFiles is the mechanical half of the always-ask check for the worktree at cwd: a reason
// naming the files it hits, or "" when it hits none. Anything it cannot read asks.
func (f *Factory) alwaysAskFiles(cwd string) string {
	if cwd == "" {
		return "the factory could not check which files it changes"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := f.GitOut(ctx, cwd, "diff", "--no-renames", "--name-only", "origin/main...HEAD")
	if err != nil {
		return "the factory could not check which files it changes"
	}
	patterns := builtinAlwaysAsk
	if raw, err := f.GitOut(ctx, cwd, "show", "origin/main:"+repoConfigFile); err == nil {
		var rc repoConfig
		if json.Unmarshal([]byte(raw), &rc) != nil {
			return "the factory could not read the repo's " + repoConfigFile
		}
		patterns = append(append([]string{}, builtinAlwaysAsk...), rc.AlwaysAsk...)
	} // no file on main: the built-in patterns only
	hits := alwaysAskHits(strings.Fields(out), patterns)
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 3 {
		hits = append(hits[:3], "…")
	}
	return "it changes the factory's rules, a service or a dependency (" + strings.Join(hits, " ") + ")"
}

var alwaysAskRe = regexp.MustCompile(`(?im)^[ \t>*_#-]*ALWAYS[ \t]+ASK[*_]*\s*:[ \t]*(.*)$`)

// alwaysAskRule asks code reviewers for their half of the always-ask check.
const alwaysAskRule = `ALWAYS ASK: <none, or why Zach must see this before it lands: architecture he did not ask for,
a change to the factory's own gates or rules, or a new service>`

// ParseAlwaysAsk returns the reviewer's last ALWAYS ASK line without markdown: "none", the reason, or
// "" when there is none. The prompt's <placeholder> is skipped.
func ParseAlwaysAsk(text string) string {
	m := alwaysAskRe.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		s := strings.Trim(strings.TrimSpace(strings.ReplaceAll(m[i][1], "`", "")), "*_\"' ")
		if s == "" || strings.HasPrefix(s, "<") {
			continue
		}
		lead := strings.ToLower(strings.TrimRight(strings.Fields(s)[0], ".;:,"))
		switch lead {
		case "none", "no", "n/a", "nothing":
			return "none"
		}
		return s
	}
	return ""
}

// factoryStarted reports whether the factory itself started card a (an idea, a Scout or a scheduled
// run) rather than Zach asking for it. Anything else counts as his ask.
func factoryStarted(a Agent) bool {
	switch source(a) {
	case "idea", "scout", "schedule":
		return true
	}
	return false
}

// finishedWorkReason says why card t, whose code review passed, comes to Zach before it lands, or ""
// when it lands on its own. review is the passing reviewer's (or arbiter's) final message; asked is a
// reason the arbiter already gave.
func (f *Factory) finishedWorkReason(t Agent, tier, review, asked string) string {
	var why []string
	add := func(s string) {
		if s != "" {
			why = append(why, s)
		}
	}
	add(asked)
	if !factoryStarted(t) {
		add("you asked for it")
	} else if t.L(labelPlanRounds) == "" {
		add("it had no plan review")
	}
	if tier == "architecture" {
		add("it is an architecture change")
	}
	switch r := ParseAlwaysAsk(review); r {
	case "none":
	case "":
		add("the reviewer did not say whether it needs you")
	default:
		add("the reviewer says: " + r)
	}
	add(f.alwaysAskFiles(t.Cwd))
	return strings.Join(why, "; ")
}
