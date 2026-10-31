package factory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every ask the board can show, so askcheck can hold each one to the same rules. A new kind of ask
// goes here first: TestEveryAskBuilderIsChecked fails until it does.
func everyAsk() map[string]*Ask {
	zach := func(labels map[string]string) Agent {
		l := map[string]string{labelStage: "zach"}
		for k, v := range labels {
			l[k] = v
		}
		return card("c1", l)
	}
	running := func(a Agent, pending string, flags ...string) (Card, Agent) {
		a.Status, a.Pending = "running", pending
		return Card{ID: a.ID, Stage: "zach", Flags: flags}, a
	}
	asks := map[string]*Ask{}
	add := func(name string, c Card, a Agent) {
		if c.ID == "" {
			c = Card{ID: a.ID, Stage: "zach"}
		}
		asks[name] = askFor(c, a)
	}
	add("permission prompt", Card{}, func() Agent { _, a := running(zach(nil), "Bash"); return a }())
	c, a := running(zach(nil), "")
	add("writing", c, a)
	c, a = running(zach(nil), "", "stalled 40m")
	add("stalled", c, a)
	add("stuck: archived", Card{ID: "c1", Stage: "building"}, card("c1", map[string]string{labelStage: "building", labelStuck: stuckArchived}))
	add("stuck: agent error", Card{ID: "c1", Stage: "building"}, card("c1", map[string]string{labelStage: "building", labelStuck: stuckAgentError}))
	add("stuck: retries ran out", Card{ID: "c1", Stage: "review"}, card("c1", map[string]string{labelStage: "review", labelStuck: "review kept failing"}))
	add("history unavailable", Card{}, zach(map[string]string{labelNote: "history unavailable: x"}))
	add("repo", Card{}, zach(map[string]string{labelAwaiting: "repo", labelCandidates: "a/one b/two"}))
	add("plan", Card{}, zach(map[string]string{labelAwaiting: "plan", labelNote: "architecture: approve the plan to build"}))
	add("secret scan", Card{}, zach(map[string]string{labelNote: "secret scan failed: email in x.go:1"}))
	add("public repo", Card{}, zach(map[string]string{labelNote: "public repo: finished without review"}))
	add("ready for you", Card{}, zach(map[string]string{labelNote: "review passed: approve to land"}))
	add("reviewed commit not verified", Card{}, zach(map[string]string{labelNote: unverifiedReviewNote + "the commit moved"}))
	add("reviews did not settle", Card{}, zach(map[string]string{labelNote: arbiterStuckNote}))
	add("close suggested", Card{}, zach(map[string]string{labelVerdict: "close", labelNote: "reviewer suggests closing"}))
	add("work left: test for Zach", Card{}, zach(map[string]string{labelNote: noteWorkLeft + "Zach: open the usage page on your phone and check the table fits; agent: tidy the docs"}))
	add("work left: action for Zach", Card{}, zach(map[string]string{labelNote: noteWorkLeft + "Zach: log in to the admin page once"}))
	add("work left: agents only", Card{}, zach(map[string]string{labelNote: noteWorkLeft + "agent: push 2 not landed"}))
	add("landed: did it finish", Card{}, zach(map[string]string{labelNote: "landed; did not say what remains"}))
	add("landed other than reviewed", Card{}, zach(map[string]string{labelNote: landDiffNotePrefix + ": extra commit"}))
	add("landed other than reviewed, scan failed", Card{}, zach(map[string]string{labelNote: landDiffNotePrefix + ": extra commit" + landDiffScanMark + "failed"}))
	add("moved by hand", Card{}, zach(nil))
	open := card("c1", map[string]string{})
	open.RequiresAttention, open.AttentionReason = true, "question"
	add("waiting on you", Card{ID: "c1", Stage: "zach"}, open)
	add("finished", Card{ID: "c1", Stage: "zach"}, Agent{ID: "c1", Status: "idle", RequiresAttention: true, AttentionReason: "finished", Labels: map[string]string{}})
	return asks
}

// actions a choice can run: runChoice's own cases, then Act
var choiceActions = []string{"send", "approve", "pickrepo", "arbiter", "done", "close", "stop", "rerun", "resume"}

// Every ask says what kind of input it wants, and its buttons fit that kind: Yes / No only on a decision,
// the action buttons on required work, and no work that landed answered with a bare reply box.
func TestEveryAskIsLabelledAndFits(t *testing.T) {
	for name, ask := range everyAsk() {
		if ask == nil {
			t.Errorf("%s: no ask", name)
			continue
		}
		for _, fault := range askFaults(ask) {
			t.Errorf("%s: %s", name, fault)
		}
	}
}

// askApproveWords are the words an ask's own label or title uses when its main button approves.
var askApproveWords = regexp.MustCompile(`(?i)\b(land|ship|allow|approve|build|start|pick)\b`)

// askFaults is what is wrong with ask by the rules above, or nothing.
func askFaults(ask *Ask) []string {
	var out []string
	if _, ok := kindLabels[ask.Kind]; !ok {
		out = append(out, fmt.Sprintf("kind %q is not a known kind", ask.Kind))
	}
	if strings.TrimSpace(ask.Label) == "" || strings.EqualFold(ask.Label, "Needs your input") {
		out = append(out, fmt.Sprintf("label %q says nothing about the kind of input", ask.Label))
	}
	if len(ask.Choices) == 0 {
		out = append(out, "no choices")
	}
	for i, ch := range ask.Choices {
		if (ch.ID == "yes" || ch.ID == "no") && ask.Kind != askDecision {
			out = append(out, fmt.Sprintf("%s button on a %s ask", ch.Label, ask.Kind))
		}
		if !slices.Contains(choiceActions, ch.action) {
			out = append(out, fmt.Sprintf("choice %q runs %q, which nothing handles", ch.Label, ch.action))
		}
		if ch.Primary && ch.Text && ask.landed {
			out = append(out, fmt.Sprintf("work landed and its main answer is a reply box (%q)", ch.Label))
		}
		if ch.Primary && i > 0 && ask.Choices[0].Primary {
			out = append(out, "two main buttons")
		}
		if (ch.ID == "land" || ch.Label == "Land") && ask.Kind != askReady {
			out = append(out, fmt.Sprintf("a Land button on a %s ask", ask.Kind))
		}
		if ch.Primary && ch.action == "approve" && !askApproveWords.MatchString(ask.Label+" "+ask.Title) {
			out = append(out, fmt.Sprintf("its main button %q approves, but the ask (%q) asks for something else", ch.Label, ask.Label))
		}
	}
	if ask.landed && !strings.HasPrefix(ask.Label, "Work landed") && !strings.HasPrefix(ask.Label, "Landing") &&
		!strings.HasPrefix(ask.Label, "Finished") {
		out = append(out, fmt.Sprintf("landed work labelled %q", ask.Label))
	}
	if ask.Kind == askAction {
		if got := choiceIDs(ask); !slices.Equal(got, []string{"works", "broken", "skip"}) || !ask.Choices[0].Primary || !ask.Choices[1].Text {
			out = append(out, fmt.Sprintf("try choices %v", ask.Choices))
		}
	}
	return out
}

// The secret-scan stop asks for a fix: Tell it to fix, Scan again & ship, Drop it. Its old buttons (Land /
// Change / Drop under a "fix it" label) are what askFaults catches.
func TestSecretScanAskButtons(t *testing.T) {
	ask := everyAsk()["secret scan"]
	want := []Choice{
		{ID: "fix", Label: "Tell it to fix", Primary: true, Text: true, action: "send"},
		{ID: "scan", Label: "Scan again & ship", action: "approve"},
		{ID: "drop", Label: "Drop it", action: "close"},
	}
	if len(ask.Choices) != len(want) {
		t.Fatalf("secret scan choices %+v", ask.Choices)
	}
	for i, w := range want {
		c := ask.Choices[i]
		if c.ID != w.ID || c.Label != w.Label || c.Primary != w.Primary || c.Text != w.Text || c.action != w.action {
			t.Errorf("secret scan button %d = %+v, want %+v", i, c, w)
		}
	}
	old := &Ask{Label: ask.Label, Kind: askReady, Title: ask.Title, Actions: ask.Actions}
	finishAsk(old)
	if len(askFaults(old)) == 0 {
		t.Errorf("the old secret-scan buttons %v pass askFaults", choiceIDs(old))
	}
	// an emailed approve scans again, as the board button does, and the reply words say so
	for _, w := range []string{"yes", "approve", "ship it"} {
		if got := parseAnswer(w, ask); got.Choice != "scan" {
			t.Errorf("reply %q = %+v, want scan", w, got)
		}
	}
	if got := parseAnswer("remove the address", ask); got.Choice != "fix" {
		t.Errorf("a written reply = %+v, want fix", got)
	}
	if words := replyWords(ask); !slices.Contains(words, replyWord{"yes", "scan again & ship"}) {
		t.Errorf("reply words %v do not offer scanning again", words)
	}
}

func choiceLabels(ask *Ask) []string {
	var out []string
	for _, c := range ask.Choices {
		out = append(out, c.Label)
	}
	return out
}

// Required landed actions keep action buttons; optional NEEDS text never replaces an existing ask.
func TestLandedActionsKeepTheirButtons(t *testing.T) {
	asks := everyAsk()
	wantActionLabels := []string{"Done it, mark done", "Something's wrong", "Skip it"}
	if a := asks["work left: test for Zach"]; a.Kind != askAction || a.Label != "Work landed. Do this:" ||
		!strings.Contains(a.Title, "open the usage page") || !strings.Contains(a.Why, "tidy the docs") ||
		!slices.Equal(choiceLabels(a), wantActionLabels) {
		t.Errorf("Zach test item: %+v", a)
	}
	if a := asks["work left: action for Zach"]; a.Kind != askAction || a.Label != "Work landed. Do this:" ||
		!slices.Equal(choiceLabels(a), wantActionLabels) {
		t.Errorf("Zach action item: %+v", a)
	}
	if a := asks["work left: agents only"]; a.Kind != askProblem || !slices.Equal(choiceIDs(a), []string{"finish", "done", "drop"}) ||
		a.Choices[1].action != "done" || !strings.Contains(a.Choices[0].says, "push 2 not landed") {
		t.Errorf("agent-only work left: %+v", a)
	}
	for _, name := range []string{"landed: did it finish", "finished", "work left: agents only"} {
		ask := everyAsk()[name]
		ask.Overview = &Overview{Decision: "None.", Needs: "Try the new button on your phone and say whether it works."}
		refineAsk(ask)
		if ask.Kind == askAction {
			t.Errorf("%s: optional NEEDS changed the ask", name)
		}
		if ask.Overview.Decision != "" {
			t.Errorf("%s: headline %q", name, ask.Overview.Decision)
		}
	}
	for _, needs := range []string{"Nothing.", "Approve the plan.", ""} {
		ask := everyAsk()["landed: did it finish"]
		ask.Overview = &Overview{Needs: needs}
		refineAsk(ask)
		if ask.Kind == askAction {
			t.Errorf("NEEDS %q replaced the existing ask", needs)
		}
	}
	// a landing the factory wants checked keeps its label and buttons: the agent's "try it" cannot
	// hide that the landing failed, was pushed without review or is not what was reviewed
	for _, name := range []string{"public repo", "landed other than reviewed", "landed other than reviewed, scan failed"} {
		ask := everyAsk()[name]
		label, ids := ask.Label, choiceIDs(ask)
		ask.Overview = &Overview{Needs: "Try the new button on your phone and say whether it works."}
		refineAsk(ask)
		if ask.Kind == askAction || ask.Label != label || !slices.Equal(choiceIDs(ask), ids) {
			t.Errorf("%s with a test in NEEDS became kind %q label %q choices %v", name, ask.Kind, ask.Label, choiceIDs(ask))
		}
	}
	// not landed: a test in NEEDS does not turn a plan approval into a test
	plan := asks["plan"]
	plan.Overview = &Overview{Needs: "Try the preview, then approve."}
	refineAsk(plan)
	if plan.Kind != askDecision {
		t.Errorf("plan became %q", plan.Kind)
	}
}

// A "None" headline is dropped while the card asks for something, and its remainder kept as the overview.
func TestNoneHeadlineIsCleaned(t *testing.T) {
	ask := everyAsk()["work left: test for Zach"]
	shared := &Overview{Decision: "None. The usage page fits a phone.", Overview: "The usage table wraps.",
		Needs: "Open the usage page on your phone and reply saying whether the table fits."}
	ask.Overview = shared
	refineAsk(ask)
	ov := ask.Overview
	if ov.Decision != "" || !strings.HasPrefix(ov.Overview, "The usage page fits a phone.") || !strings.HasPrefix(ov.Needs, "Open the usage page") {
		t.Errorf("overview = %+v", ov)
	}
	if ask.Title != "Do it, then mark it done" {
		t.Errorf("headline %q repeats or misses the thing to try", ask.Title)
	}
	if subj := emailQuestion(Agent{}, ask, nil); subj != "Do it, then mark it done" {
		t.Errorf("email subject = %q", subj)
	}
	if shared.Decision != "None. The usage page fits a phone." {
		t.Error("refineAsk changed the cached overview")
	}
	// when the summary already contains the remainder of a "None. …" headline, do not repeat it
	dup := everyAsk()["work left: test for Zach"]
	dup.Overview = &Overview{
		Decision: "None. The usage page now fits a phone screen.",
		Overview: "The usage page now fits a phone screen. Long rows wrap cleanly.",
		Needs:    "Open the usage page on your phone and reply saying whether the table fits.",
	}
	refineAsk(dup)
	if dup.Overview.Decision != "" || dup.Overview.Overview != "The usage page now fits a phone screen. Long rows wrap cleanly." {
		t.Errorf("duplicate remainder repeated: %+v", dup.Overview)
	}
	for in, want := range map[string]string{
		"None needed now.":                "",
		"Nothing for you to decide yet.":  "",
		"N/A":                             "",
		"Approve this plan?":              "Approve this plan?",
		"Nonetheless, approve the merge?": "Nonetheless, approve the merge?",
	} {
		a := &Ask{Title: "t", Overview: &Overview{Decision: in}}
		cleanOverview(a)
		if a.Overview.Decision != want {
			t.Errorf("decision %q -> %q, want %q", in, a.Overview.Decision, want)
		}
	}
	// a NEEDS that says nothing falls back to the item left for Zach
	ask = everyAsk()["work left: test for Zach"]
	ask.Overview = &Overview{Needs: "None."}
	refineAsk(ask)
	if !strings.HasPrefix(ask.Overview.Needs, "open the usage page") {
		t.Errorf("needs = %q", ask.Overview.Needs)
	}
}

// A decision whose agent asked QUESTION blocks says so.
func TestQuestionsLabel(t *testing.T) {
	ask := everyAsk()["waiting on you"]
	ask.Questions = []Question{{Text: "Where?", Options: []string{"a", "b"}}}
	refineAsk(ask)
	if ask.Label != "Answer the agent's questions" {
		t.Errorf("label %q", ask.Label)
	}
}

var askLiteral = regexp.MustCompile(`&Ask\{`)
var askTitleLiteral = regexp.MustCompile(`&Ask\{[^}]*?Title:\s*"([^"]+)"`)

// askBuilders is how many places in factory/ build an Ask. Adding one means adding its ask to everyAsk
// (so it gets a label and fitting buttons), then this number.
const askBuilders = 22

// Every place that builds an ask is held to the rules above: a new one fails here until it is checked.
func TestEveryAskBuilderIsChecked(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	var titles []string
	n := 0
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		b, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(b), "\n", " ")
		n += len(askLiteral.FindAllString(src, -1))
		for _, m := range askTitleLiteral.FindAllStringSubmatch(src, -1) {
			titles = append(titles, m[1])
		}
	}
	if n != askBuilders {
		t.Errorf("%d places build an Ask, askcheck knows %d: add the new ask to everyAsk, then set askBuilders", n, askBuilders)
	}
	asks := everyAsk()
	for _, title := range titles {
		found := false
		for _, a := range asks {
			if strings.HasPrefix(a.Title, title) || strings.HasPrefix(title, a.Title) {
				found = true
			}
		}
		if !found {
			t.Errorf("the ask %q is not in everyAsk", title)
		}
	}
}

// A landed card with a required action answers to its action buttons on the board and by email,
// and they do what they say.
func TestActionAskChoicesRun(t *testing.T) {
	reply := "DECISION: None. The usage page fits a phone.\nOVERVIEW: The usage table wraps.\n" +
		"NEEDS FROM ZACH: Open the usage page on your phone and say whether the table fits.\n\nREMAINING: none"
	landed := func() Agent {
		return card("c1", map[string]string{labelStage: "zach", labelNote: noteWorkLeft + "Zach: approve the account setting", labelZachThread: "nt1"})
	}
	f, p, _ := mailFactory(t, landed())
	storeLog(t, f, p, "c1", reply)
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "works"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Errorf("Done it: labels %v", l)
	}
	f, p, _ = mailFactory(t, landed())
	storeLog(t, f, p, "c1", reply)
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "broken", Message: "It still pinged me"}); err != nil {
		t.Fatal(err)
	}
	if s := p.sent("c1"); len(s) != 1 || !strings.Contains(s[0], "It still pinged me") {
		t.Errorf("It's broken: sent %v", s)
	}
	f, p, _ = mailFactory(t, landed())
	storeLog(t, f, p, "c1", reply)
	if _, err := f.Act("choose", Body{ID: "c1", Choice: "skip"}); err != nil {
		t.Fatal(err)
	}
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Errorf("Skip it: labels %v", l)
	}
	for text, stage := range map[string]string{"it works": "landed", "Done.": "landed"} {
		f, p, _ = mailFactory(t, landed())
		storeLog(t, f, p, "c1", reply)
		if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: text}); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if l := p.labels("c1"); l[labelStage] != stage {
			t.Errorf("email %q: labels %v", text, l)
		}
	}
}
