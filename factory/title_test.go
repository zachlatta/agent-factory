package factory

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseTitle(t *testing.T) {
	long := "Show the remaining usage for each agent provider on the board header and the picker"
	for _, c := range []struct{ in, want string }{
		{"Plan...\nTITLE: Cards get short titles\nTIER: low", "Cards get short titles"},
		{"**TITLE:** Cards get short titles", "Cards get short titles"},
		{"TITLE: **Cards get short titles**", "Cards get short titles"},
		{"TITLE: `Cards get short titles`", "Cards get short titles"},
		{`TITLE: "Cards get short titles"`, "Cards get short titles"},
		{"- TITLE: Cards get short titles", "Cards get short titles"},
		{"> **Title:** _Cards get short titles_", "Cards get short titles"},
		{"no title here\nTIER: low", ""},
		{"TITLE: <the outcome in at most 60 characters, plain words, no file names>", ""},
		{"TITLE:   \nTIER: low", ""},
		{"TITLE: First idea\nmore\nTITLE: Second idea", "Second idea"},
		// a repeated prompt placeholder after the real title does not hide it
		{"TITLE: Real one\nTITLE: <the outcome in at most 60 characters>", "Real one"},
	} {
		if got := ParseTitle(c.in); got != c.want {
			t.Errorf("ParseTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := ParseTitle("TITLE: " + long)
	if utf8.RuneCountInString(got) > 60 || !strings.HasSuffix(got, "…") || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("long title = %q", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") || !strings.HasPrefix(long[len(strings.TrimSuffix(got, "…")):], " ") {
		t.Errorf("long title not cut at a word: %q", got)
	}
}

func TestNewCardTitleCutAtWord(t *testing.T) {
	f, p := newTestFactory(t)
	req := "For the factory new card thing, have it automatically find the best project for me\nmore"
	if _, err := f.Act("new", Body{Request: req, Repo: "example/desk"}); err != nil {
		t.Fatal(err)
	}
	r := runArgs(p)[0]
	title := argAfter(r, "--title")
	if utf8.RuneCountInString(title) > 70 || !strings.HasSuffix(title, "…") || !strings.HasPrefix(req, strings.TrimSuffix(title, "…")) {
		t.Errorf("title = %q", title)
	}
	if want := "For the factory new card thing, have it automatically find the best…"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	for _, a := range r {
		if strings.HasPrefix(a, labelPrefix+labelTitled+"=") {
			t.Errorf("derived title marked as given: %v", r)
		}
	}
}

func TestNewCardKeepsGivenTitle(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "long dictated request", Repo: "example/desk", Title: "Ideas keep their title"}); err != nil {
		t.Fatal(err)
	}
	r := runArgs(p)[0]
	if argAfter(r, "--title") != "Ideas keep their title" || !contains(r, labelArg(labelTitled, "given")) {
		t.Errorf("given title not kept and marked: %v", r)
	}
}

func TestAutoRepoCardTitles(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		given       bool
	}{{"derived", "", false}, {"given", "Dispatcher title", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTestFactory(t)
			withRepos(t, f, "example/desk", "example/ops")
			res, err := f.Act("new", Body{Request: "Rotate the backup key", Repo: "auto", Title: tc.title})
			if err != nil {
				t.Fatal(err)
			}
			id := res.(map[string]string)["id"]
			f.SetAgents([]Agent{picker(id, nil)})
			p.logs[id] = "[User] x\nREPO: example/ops"
			f.Tick()
			runs := runArgs(p)
			if len(runs) != 2 {
				t.Fatalf("runs = %v", runs)
			}
			r := runs[1]
			if argAfter(r, "--cwd") != filepath.Join(f.cfg.Home, "dev", "example/ops") {
				t.Fatalf("card not started in ops: %v", r)
			}
			want := "Rotate the backup key"
			if tc.given {
				want = tc.title
			}
			if argAfter(r, "--title") != want || contains(r, labelArg(labelTitled, "given")) != tc.given {
				t.Errorf("routed card title/label wrong: %v", r)
			}
		})
	}
}

func TestScoutTitleCutAtWord(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk", "example/ops")
	if _, err := f.Act("scout", Body{Request: "Look at how the factory handles the board on phones and suggest fixes", Repo: "auto"}); err != nil {
		t.Fatal(err)
	}
	title := argAfter(runArgs(p)[0], "--title")
	if want := "Scout: Look at how the factory handles the board on phones and…"; title != want {
		t.Errorf("scout picker title = %q, want %q", title, want)
	}
}

// planStop is a board card in Plan that stopped after acting, with reply as its last message.
func planStop(t *testing.T, labels map[string]string, reply string) (*Factory, *fakePaseo) {
	t.Helper()
	l := map[string]string{labelStage: "plan", labelSource: "board", labelSince: ago(10 * time.Minute)}
	for k, v := range labels {
		l[k] = v
	}
	f, p := newTestFactory(t, card("c1", l))
	p.logs["c1"] = "[Assistant] " + reply
	return f, p
}

func TestPlannerNamesCard(t *testing.T) {
	f, p := planStop(t, nil, "Plan: ...\nTITLE: **Cards get short titles**\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 1 || got[0] != "Cards get short titles" {
		t.Errorf("renames = %v", got)
	}
	if l := p.labels("c1"); l[labelTitled] != "planner" {
		t.Errorf("labels = %v", l)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 1 {
		t.Errorf("plan review not started after the rename: %v", runs)
	}
}

func TestPlannerNamesDirectCard(t *testing.T) {
	f, p := planStop(t, nil, "Done: report sent.\nTITLE: Weekly report sent\nTRACK: direct")
	f.Tick()
	if got := p.names("c1"); len(got) != 1 || got[0] != "Weekly report sent" {
		t.Errorf("renames = %v", got)
	}
}

func TestPlannerRenamesOnce(t *testing.T) {
	f, p := planStop(t, map[string]string{labelTitled: "planner"}, "Revised plan\nTITLE: Another name\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("renamed twice: %v", got)
	}
}

func TestGivenTitleKept(t *testing.T) {
	f, p := planStop(t, map[string]string{labelTitled: "given"}, "Plan\nTITLE: Another name\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("given title renamed: %v", got)
	}
}

func TestInboxCardNotRenamed(t *testing.T) {
	f, p := planStop(t, map[string]string{labelSource: "email"}, "Plan\nTITLE: Another name\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("inbox card renamed: %v", got)
	}
}

func TestPlanWithoutTitleKeepsTitle(t *testing.T) {
	f, p := planStop(t, nil, "Plan with no title line\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("renames = %v", got)
	}
	if l := p.labels("c1"); l[labelTitled] != "kept" {
		t.Errorf("labels = %v", l)
	}
}

func TestSameTitleNotRenamed(t *testing.T) {
	f, p := planStop(t, nil, "Plan\nTITLE: card c1\nTIER: low")
	f.Tick()
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("renames = %v", got)
	}
	if l := p.labels("c1"); l[labelTitled] != "kept" {
		t.Errorf("labels = %v", l)
	}
}

func TestQuestionStopDoesNotUseUpRename(t *testing.T) {
	f, p := planStop(t, nil, "Before I plan: which host should this run on?\nTITLE: Too early")
	f.nameCard(f.agent("c1"), "Before I plan: which host should this run on?\nTITLE: Too early")
	if got := p.names("c1"); len(got) != 0 {
		t.Errorf("renamed on a question: %v", got)
	}
	if l := p.labels("c1"); l[labelTitled] != "" {
		t.Errorf("titled set on a question: %v", l)
	}
}
