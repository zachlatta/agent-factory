package factory

import (
	"strings"
	"testing"
)

func TestParseChanged(t *testing.T) {
	text := "DECISION: land?\n\nCHANGED: The board groups cards by repo.\n- CHANGED: Emails are shorter.\n" +
		"**CHANGED:** Buttons say Land.\nCHANGED: a fourth line\nrest"
	got := ParseChanged(text)
	want := []string{"The board groups cards by repo.", "Emails are shorter.", "Buttons say Land."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ParseChanged = %q", got)
	}
}

func TestReadyReplyWords(t *testing.T) {
	for text, want := range map[string]answer{
		"Land":                          {Choice: "land"},
		"land it.":                      {Choice: "land"},
		"drop":                          {Choice: "drop"},
		"Drop it":                       {Choice: "drop"},
		"close":                         {Choice: "drop"},
		"cancel":                        {Choice: "drop"},
		"ok":                            {Choice: "change", Message: "ok"},
		"yes":                           {Choice: "change", Message: "yes"},
		"make the button blue":          {Choice: "change", Message: "make the button blue"},
		"land\n\nOn Mon someone wrote:": {Choice: "land"},
	} {
		if got := parseAnswer(text, readyAsk("")); got != want {
			t.Errorf("%q -> %+v, want %+v", text, got, want)
		}
	}
}

func TestReadyForYouRoundsGetOneEmailEach(t *testing.T) {
	// a change request after the first Ready for you: the next code review pass is a new stop and a new email
	c := card("c1", map[string]string{labelStage: "review", labelTier: "architecture", labelPlanRounds: "1",
		labelCodeRounds: "1", labelReady: "1", labelTold: readyNote(1)})
	f, p := newTestFactory(t, c, reviewer("r2", "c1", "code"))
	storeLog(t, f, p, "r2", "TIER: architecture\nVERDICT: approve")
	f.ApplyVerdict(f.agent("r2"))
	l := p.labels("c1")
	if l[labelNote] != readyNote(2) || l[labelReady] != "2" || l[labelStage] != "zach" {
		t.Errorf("labels = %v", l)
	}
	if lineageTold(f.byID(), f.agent("c1")) == waitNote(f.agent("c1")) {
		t.Error("the second Ready for you must not count as already emailed")
	}
}

func TestReadyEmail(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)})
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", "DECISION: Land this?\nOVERVIEW: The board is grouped.\nNEEDS FROM ZACH: try it\n\n"+
		"CHANGED: The board groups cards by repo.\nCHANGED: Each group folds.\nCHANGED: Counts show per group.\n\nrisks")
	if err := f.previews.put(Preview{Card: "c1", Port: 7701, URL: "https://board.example:7701/", State: previewReady}); err != nil {
		t.Fatal(err)
	}
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	a := sends[0].Args
	text, html := a["text"].(string), a["html"].(string)
	if a["subject"] != "Ready to try: card c1" {
		t.Errorf("subject = %v", a["subject"])
	}
	for _, want := range []string{"- The board groups cards by repo.", "- Each group folds.", "- Counts show per group.",
		"Try it: https://board.example:7701/", `Reply "land"`, "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	for _, want := range []string{`href="https://board.example:7701/"`, ">Preview</a>", "<li>Each group folds.</li>"} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	for _, not := range []string{"example/desk", "c1)", "Needs you"} {
		if strings.Contains(text, not) {
			t.Errorf("text has %q:\n%s", not, text)
		}
	}
	if l := p.labels("c1"); l[labelTold] != readyNote(1) {
		t.Errorf("labels = %v", l)
	}
}

func TestReadyEmailWaitsForThePreview(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)})
	f, p, m := mailFactory(t, c)
	f.runner = newFakeRunner()
	storeLog(t, f, p, "c1", "CHANGED: a\nCHANGED: b\nCHANGED: c")
	f.previews.put(Preview{Card: "c1", State: previewStarting})
	f.mailZach(f.agent("c1"), mustCard(t, f, "c1"))
	if len(m.sends()) != 0 {
		t.Fatal("emailed before the preview settled")
	}
	f.previews.put(Preview{Card: "c1", State: previewFailed, Log: "boom"})
	f.mailZach(f.agent("c1"), mustCard(t, f, "c1"))
	sends := m.sends()
	if len(sends) != 1 || !strings.Contains(sends[0].Args["text"].(string), "The preview didn't start.") {
		t.Fatalf("sends = %+v", sends)
	}
}

func TestReadyEmailAnswer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelSource: "board"})
	c.Labels[gmailThreadLabel] = "th1"
	f, _, _ := mailFactory(t, c)
	got, err := f.answerByEmail(Body{Thread: "th1", MessageID: "m1", Text: "Land it"})
	if err != nil || got["action"] != "approve" {
		t.Fatalf("answer = %v, %v", got, err)
	}
}

func mustCard(t *testing.T, f *Factory, id string) Card {
	t.Helper()
	c, ok := f.card(id)
	if !ok {
		t.Fatalf("no card %s", id)
	}
	return c
}

func TestReadyEmailShowsQuestionsAndLettersAnswerThem(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)})
	c.Title = "Buttons in example/desk get clearer labels"
	f, p, m := mailFactory(t, c)
	storeLog(t, f, p, "c1", "CHANGED: Buttons say Land.\n\nQUESTION: Which colour for the button?\n- Blue (recommended)\n- Red\n")
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %+v", sends)
	}
	text, html := sends[0].Args["text"].(string), sends[0].Args["html"].(string)
	for _, want := range []string{"1. Which colour for the button?", "A. Blue (recommended)", "B. Red", "1A"} {
		if !strings.Contains(text, want) || !strings.Contains(html, strings.ReplaceAll(want, "(", "(")) {
			t.Errorf("email lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(sends[0].Args["subject"].(string), "example/desk") {
		t.Errorf("subject keeps the repo slug: %v", sends[0].Args["subject"])
	}
	ask := readyAsk("")
	ask.Questions = []Question{{Text: "Which colour for the button?", Options: []string{"Blue (recommended)", "Red"}}}
	if got := parseAnswer("1b", ask); got.Choice != "change" || !strings.Contains(got.Message, "→ Red") {
		t.Errorf("1b -> %+v", got)
	}
	if got := parseAnswer("b", ask); got.Choice != "change" || !strings.Contains(got.Message, "→ Red") {
		t.Errorf("b -> %+v", got)
	}
	if got := parseAnswer("land", ask); got.Choice != "land" {
		t.Errorf("land -> %+v", got)
	}
}

// A Ready for you card with no preview link always says why on the board, in the email's words where
// the email has them.
func TestReadyWithoutPreviewSaysWhy(t *testing.T) {
	for _, st := range []string{"", previewNone, previewSampled, previewStarting, previewQueued, previewFailed} {
		r := &Ready{State: st}
		if previewReason(r) == "" || (previewWaitText(r) != "" && previewReason(r) != previewWaitText(r)) {
			t.Errorf("state %q: no reason given for the missing preview", st)
		}
	}
	f, _ := newTestFactory(t)
	ask := readyAsk("")
	f.fillReady(ask, "c1", "CHANGED: one thing")
	if ask.Ready.Preview != "" || ask.Ready.Wait == "" {
		t.Errorf("no preview record: %+v", ask.Ready)
	}
}

// On Ready for you, the board and email answer alike: a letter and a tapped option reach the builder
// as Change with the same answer, and "land" and Land both land it.
func TestReadyBoardAndEmailParity(t *testing.T) {
	ready := func() Agent {
		return card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelZachThread: "nt1"})
	}
	reply := "CHANGED: Buttons say Land.\n\nQUESTION: Which colour for the button?\n- Blue (recommended)\n- Red\n"
	setup := func() (*Factory, *fakePaseo) {
		f, p, _ := mailFactory(t)
		storeLog(t, f, p, "c1", "[User] go\n"+reply)
		f.SetAgents([]Agent{ready()})
		return f, p
	}
	// email: "b"
	f, p := setup()
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "b"}); err != nil {
		t.Fatal(err)
	}
	email := p.sent("c1")
	// board: the tapped option, sent through Change as the board page does
	g, q := setup()
	if _, err := g.Act("choose", Body{ID: "c1", Choice: "change", Message: "Zach's answers:\n1. Which colour for the button?\n   → Red"}); err != nil {
		t.Fatal(err)
	}
	board := q.sent("c1")
	if len(email) != 1 || len(board) != 1 || !strings.Contains(email[0], "→ Red") || !strings.Contains(board[0], "→ Red") {
		t.Fatalf("email sent %v, board sent %v", email, board)
	}
	if p.labels("c1")[labelStage] != q.labels("c1")[labelStage] {
		t.Errorf("email moved the card to %q, board to %q", p.labels("c1")[labelStage], q.labels("c1")[labelStage])
	}
	// land
	f, p = setup()
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m3", Text: "land"}); err != nil {
		t.Fatal(err)
	}
	g, q = setup()
	if _, err := g.Act("choose", Body{ID: "c1", Choice: "land"}); err != nil {
		t.Fatal(err)
	}
	if p.labels("c1")[labelStage] != "landing" || q.labels("c1")[labelStage] != "landing" {
		t.Errorf("land: email %q, board %q", p.labels("c1")[labelStage], q.labels("c1")[labelStage])
	}
}
