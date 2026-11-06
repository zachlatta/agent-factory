package factory

import (
	"bytes"
	"fmt"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
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
		"ok":                            {Choice: "land"},
		"yes":                           {Choice: "land"},
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
		labelCodeRounds: "1", labelReady: "1", labelTold: readyNote(1), labelReviewed: reviewedSHA})
	f, p := newTestFactory(t, c, reviewer("r2", "c1", "code"))
	headIs(f, reviewedSHA, nil)
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
	c := inThread(card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)}), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
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
	if got := composed(t, f, "c1").Subject; got != "Ready to try: card c1" {
		t.Errorf("subject = %v", got)
	}
	for _, want := range []string{"- The board groups cards by repo.", "- Each group folds.", "- Counts show per group.",
		"Try it: https://board.example:7701/", `Reply "yes" or "land"`, "https://board.example/#c1"} {
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
	c := inThread(card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)}), "t1")
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
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
	c := inThread(card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1",
		labelWriting: ago(5 * 60e9)}), "t1")
	c.Title = "Buttons in example/desk get clearer labels"
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
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
	if got := composed(t, f, "c1").Subject; strings.Contains(got, "example/desk") {
		t.Errorf("subject keeps the repo slug: %v", got)
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
		return card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReady: "1", labelZachThread: "nt1", labelReviewed: reviewedSHA})
	}
	reply := "CHANGED: Buttons say Land.\n\nQUESTION: Which colour for the button?\n- Blue (recommended)\n- Red\n"
	setup := func() (*Factory, *fakePaseo) {
		f, p, _ := mailFactory(t)
		headIs(f, reviewedSHA, nil)
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

func TestReadyDecisionWithoutTry(t *testing.T) {
	for _, state := range []string{"", previewNone, previewFailed, previewStarting, previewQueued, previewReady, previewSampled} {
		t.Run(state, func(t *testing.T) {
			f, _ := newTestFactory(t)
			p := Preview{Card: "c1", State: state}
			if state == previewReady {
				p.URL = "https://preview.example/"
			}
			if state == previewSampled {
				p.Sample = "Two checks passed."
			}
			if state != "" {
				if err := f.previews.put(p); err != nil {
					t.Fatal(err)
				}
			}
			ask := readyAsk("")
			ask.Overview = &Overview{Decision: "Use clearer buttons?"}
			f.fillReady(ask, "c1", "CHANGED: Clearer buttons.")
			hasTry := state == previewStarting || state == previewQueued || state == previewReady || state == previewSampled
			want := "Use clearer buttons?"
			if hasTry {
				want = readyTitle
			}
			if ask.Title != want {
				t.Fatalf("title = %q, want %q", ask.Title, want)
			}
			e := f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
			if !hasTry {
				if e.Subject != want {
					t.Errorf("subject = %q", e.Subject)
				}
				for _, body := range []string{e.Text, e.HTML, e.HTMLNoImage} {
					if !strings.Contains(body, want) || !strings.Contains(body, "Nothing is installed or changed until you say yes or land.") || !strings.Contains(body, "Reply") {
						t.Errorf("bad body: %s", body)
					}
				}
				if strings.Contains(ask.Label+ask.Why, "Try the preview") {
					t.Error("still asks to try")
				}
			}
		})
	}
}

func TestReadyEvidenceAndFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".factory-evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".factory-evidence", "valid.png"), b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".factory-evidence", "bad.png"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid.png", "bad.png", "missing.png", "../valid.png", ""} {
		for _, decision := range []string{"", "None.", "Use clearer buttons?"} {
			f, _ := newTestFactory(t)
			ask := readyAsk("")
			ask.Evidence = Evidence{After: name}
			ask.Overview = &Overview{Decision: decision}
			f.fillReady(ask, "c1", "", dir)
			want := "Approve this change?"
			if decision == "Use clearer buttons?" {
				want = decision
			}
			if name == "valid.png" {
				want = readyTitle
			}
			if ask.Title != want {
				t.Errorf("%q %q: title %q want %q", name, decision, ask.Title, want)
			}
		}
	}
}

func TestReadyApprovalEmailWordCap(t *testing.T) {
	f, _ := newTestFactory(t)
	ask := readyAsk("")
	ask.Overview = &Overview{Decision: "Use clearer buttons?"}
	f.fillReady(ask, "c1", "CHANGED: "+strings.Repeat("clearer ", 100))
	e := f.readyEmail(Agent{}, Card{}, ask)
	if n := wordCount(e.Text); n > mailWordCap {
		t.Fatalf("email has %d words: %s", n, e.Text)
	}
}

// noBlankLines fails on what a whole-line email never has: an empty bullet or list item, or a cut.
func noBlankLines(t *testing.T, e email) {
	t.Helper()
	for _, l := range strings.Split(e.Text, "\n") {
		if strings.TrimSpace(l) == "-" {
			t.Errorf("empty bullet:\n%s", e.Text)
		}
	}
	for _, h := range []string{e.HTML, e.HTMLNoImage} {
		if strings.Contains(h, "<li></li>") || strings.Contains(h, "<ul></ul>") {
			t.Errorf("empty list: %s", h)
		}
	}
	if strings.Contains(e.Text+e.HTML, "…") {
		t.Errorf("cut text:\n%s", e.Text)
	}
}

func bodyWords(e email) int { return wordCount(strings.Split(e.Text, "Open the board:")[0]) }

func TestReadyEmailBulletsAreWholeOrCounted(t *testing.T) {
	short := "Buttons say what they do."
	long := strings.Repeat("clearer ", 30) + "buttons."
	for _, tc := range []struct {
		name    string
		changed []string
		preview bool
		shown   []string
		more    string
	}{
		{"all fit", []string{short, "Colours match the page."}, false, []string{short, "Colours match the page."}, ""},
		{"all fit with a preview", []string{short}, true, []string{short}, ""},
		{"the long one drops", []string{short, long}, false, []string{short}, "1 more on the board."},
		{"a later short one still fits", []string{long, short}, false, []string{short}, "1 more on the board."},
		{"none fit", []string{long, long, long}, false, nil, "3 changes are on the board."},
		{"one does not fit", []string{long}, false, nil, "1 change is on the board."},
		{"preview path has the cap too", []string{long, long, short}, true, []string{short}, "2 more on the board."},
		{"a line stripped to nothing", []string{"`factory/ready.go`", short}, false, []string{short}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			if tc.preview {
				if err := f.previews.put(Preview{Card: "c1", State: previewReady, URL: "https://preview.example/"}); err != nil {
					t.Fatal(err)
				}
			}
			ask := readyAsk("")
			ask.Overview = &Overview{Decision: "Use clearer buttons?"}
			var reply []string
			for _, l := range tc.changed {
				reply = append(reply, "CHANGED: "+l)
			}
			f.fillReady(ask, "c1", strings.Join(reply, "\n"))
			e := f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
			noBlankLines(t, e)
			if n := bodyWords(e); n > mailWordCap {
				t.Errorf("%d words:\n%s", n, e.Text)
			}
			for _, l := range tc.shown {
				if !strings.Contains(e.Text, "- "+l+"\n") || !strings.Contains(e.HTML, "<li>"+l+"</li>") {
					t.Errorf("lacks %q:\n%s", l, e.Text)
				}
			}
			if got := strings.Count(e.Text, "\n- "); got != len(tc.shown) {
				t.Errorf("%d bullets:\n%s", got, e.Text)
			}
			if (len(tc.shown) > 0) != strings.Contains(e.Text, "What changed:") {
				t.Errorf("heading without bullets, or bullets without it:\n%s", e.Text)
			}
			if tc.more != "" && (!strings.Contains(e.Text, tc.more) || !strings.Contains(e.HTML, tc.more)) {
				t.Errorf("lacks %q:\n%s", tc.more, e.Text)
			}
			if tc.more == "" && strings.Contains(e.Text, "on the board.") {
				t.Errorf("count with nothing dropped:\n%s", e.Text)
			}
		})
	}
}

// The overview stands in for missing CHANGED lines, split into sentences so each is whole.
func TestReadyEmailOverviewStandsInSentenceBySentence(t *testing.T) {
	f, _ := newTestFactory(t)
	ask := readyAsk("")
	ask.Overview = &Overview{Decision: "Use clearer buttons?",
		Overview: "Buttons say what they do, e.g. Land. Then " + strings.Repeat("long ", 40) + "sentence. Colours match."}
	f.fillReady(ask, "c1", "")
	e := f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
	noBlankLines(t, e)
	for _, want := range []string{"- Buttons say what they do, e.g. Land.\n", "- Colours match.\n", "1 more on the board."} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("lacks %q:\n%s", want, e.Text)
		}
	}
}

// A line with no letter or digit left is not printed as if it said something.
func TestReadyEmailSkipsLinesStrippedToPunctuation(t *testing.T) {
	f, _ := newTestFactory(t)
	ask := readyAsk("")
	f.fillReady(ask, "c1", "CHANGED: `factory/ready.go`.\nCHANGED: Buttons are clearer.\nCHANGED: `factory/mail.go` (recommended)\n")
	e := f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
	noBlankLines(t, e)
	if n := strings.Count(e.Text, "\n- "); n != 1 || !strings.Contains(e.Text, "- Buttons are clearer.\n") {
		t.Errorf("%d bullets:\n%s", n, e.Text)
	}
	if strings.Contains(e.HTML, "<li>.</li>") || strings.Count(e.HTML, "<li>") != 1 {
		t.Errorf("html: %s", e.HTML)
	}

	ask = readyAsk("")
	ask.Overview = &Overview{Decision: "Use clearer buttons?", Overview: "`factory/ready.go`."}
	f.fillReady(ask, "c1", "")
	e = f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
	if strings.Contains(e.Text, "What changed:") || strings.Contains(e.Text, "\n- ") {
		t.Errorf("overview stripped to a stop:\n%s", e.Text)
	}
}

// A long decision stays whole on the board and in the first line; only the subject is cut.
func TestReadyLongDecisionIsCutOnlyInTheSubject(t *testing.T) {
	const long = "Should the phone page also show the cost of each agent run and the model that ran it before you land?"
	if len([]rune(long)) <= subjectCap {
		t.Fatal("the decision must be over the subject cap")
	}
	f, _ := newTestFactory(t, card("c1", nil))
	ask := readyAsk("")
	ask.Overview = &Overview{Decision: long, Overview: "Adds costs."}
	f.fillReady(ask, "c1", "CHANGED: Costs show.\n")
	if ask.Overview.Decision != long || ask.Title != long {
		t.Errorf("board decision %q, title %q", ask.Overview.Decision, ask.Title)
	}
	e := f.readyEmail(Agent{}, Card{Title: "Costs on the phone page"}, ask)
	if !strings.HasPrefix(e.Text, long+"\n") {
		t.Errorf("first line:\n%s", e.Text)
	}
	if !strings.Contains(e.HTML, long) {
		t.Errorf("html: %s", e.HTML)
	}
	if n := len([]rune(e.Subject)); n > subjectCap || !strings.HasSuffix(e.Subject, "…") {
		t.Errorf("subject (%d runes) = %q", n, e.Subject)
	}

	if err := f.previews.put(Preview{Card: "c1", State: previewReady, URL: "https://preview.example/"}); err != nil {
		t.Fatal(err)
	}
	ask = readyAsk("")
	ask.Overview = &Overview{Decision: long, Overview: "Adds costs."}
	f.fillReady(ask, "c1", "")
	if ask.Overview.Decision != long {
		t.Errorf("board decision with a preview %q", ask.Overview.Decision)
	}
}

func TestReadyEmailQuestionsAreWholeOrSendHimToTheBoard(t *testing.T) {
	longOption := strings.Repeat("wait ", 70) + "for the fix."
	for _, tc := range []struct {
		name  string
		qs    []Question
		whole bool
	}{
		{"options print in full", []Question{{Text: "Which one?", Options: []string{"Keep the old layout until the new one has been tried on a phone (recommended)", "I will switch every page over by hand and tell you when that is done"}}}, true},
		{"more than four options", []Question{{Text: "Which?", Options: []string{"One", "Two", "Three", "Four", "Five", "Six"}}}, true},
		{"two questions", []Question{{Text: "First?", Options: []string{"One", "Two"}}, {Text: "Second?", Options: []string{"Three", "Four"}}}, true},
		{"over the allowance", []Question{{Text: "Which one?", Options: []string{longOption, "Other"}}}, false},
		{"over the allowance across questions", []Question{{Text: "First?", Options: []string{strings.Repeat("a ", 35)}}, {Text: "Second?", Options: []string{strings.Repeat("b ", 35)}}}, false},
		{"an option strips to nothing", []Question{{Text: "Which one?", Options: []string{"`factory/ready.go`", "Other"}}}, false},
		{"a question strips to nothing", []Question{{Text: "`factory/ready.go`", Options: []string{"One", "Two"}}}, false},
		{"an option strips to punctuation", []Question{{Text: "Which one?", Options: []string{"`factory/ready.go`.", "Other"}}}, false},
		{"an option strips to its mark", []Question{{Text: "Which one?", Options: []string{"`factory/ready.go` (recommended)", "Other"}}}, false},
		{"a question strips to punctuation", []Question{{Text: "`factory/ready.go`?", Options: []string{"One", "Two"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			ask := readyAsk("")
			ask.Overview = &Overview{Decision: "Use clearer buttons?"}
			ask.Questions = tc.qs
			f.fillReady(ask, "c1", "CHANGED: Buttons say what they do.")
			e := f.readyEmail(Agent{}, Card{Title: "Clearer buttons"}, ask)
			noBlankLines(t, e)
			if tc.whole {
				for _, q := range tc.qs {
					for j, o := range q.Options {
						if !strings.Contains(e.Text, letter(j)+". "+o) {
							t.Errorf("lacks option %q:\n%s", o, e.Text)
						}
					}
				}
				if !strings.Contains(e.Text, "Letters like 1A") {
					t.Errorf("no letters:\n%s", e.Text)
				}
				return
			}
			want := "1 question is too long for email. Open the board to answer."
			if len(tc.qs) == 2 {
				want = "2 questions are too long for email. Open the board to answer."
			}
			if !strings.Contains(e.Text, want) || !strings.Contains(e.HTML, want) {
				t.Errorf("lacks %q:\n%s", want, e.Text)
			}
			if strings.Contains(e.Text, "Letters") || strings.Contains(e.Text, "A. ") {
				t.Errorf("offers letters:\n%s", e.Text)
			}
			if strings.Contains(e.Text, `"yes"`) || strings.Contains(e.Text, `"land"`) || strings.Contains(e.HTML, "yes") {
				t.Errorf("offers a one-word yes on questions it withheld:\n%s", e.Text)
			}
			if n := bodyWords(e); n > mailWordCap {
				t.Errorf("%d words:\n%s", n, e.Text)
			}
		})
	}
}

func TestReadyDecisionNamesTheChange(t *testing.T) {
	for _, tc := range []struct{ name, decision, title, want string }{
		{"bare", "Land this change?", "Clearer board buttons", "Land: Clearer board buttons?"},
		{"with a recommendation", "Land this change? I recommend yes.", "Clearer board buttons", "Land: Clearer board buttons?"},
		{"in markdown", "**Land this change?**", "Clearer board buttons", "Land: Clearer board buttons?"},
		{"empty", "", "Clearer board buttons", "Land: Clearer board buttons?"},
		{"none", "None.", "Clearer board buttons", "Land: Clearer board buttons?"},
		{"specific", "Use clearer buttons?", "Clearer board buttons", "Use clearer buttons?"},
		{"no title", "Land this change?", "", "Approve this change?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := card("c1", nil)
			a.Title = tc.title
			f, _ := newTestFactory(t, a)
			ask := readyAsk("")
			ask.Overview = &Overview{Decision: tc.decision}
			f.fillReady(ask, "c1", "CHANGED: Buttons say what they do.")
			if ask.Title != tc.want {
				t.Errorf("board title = %q, want %q", ask.Title, tc.want)
			}
			e := f.readyEmail(a, Card{Title: tc.title}, ask)
			if e.Subject != tc.want || !strings.HasPrefix(e.Text, tc.want+"\n") {
				t.Errorf("subject %q, first line of:\n%s", e.Subject, e.Text)
			}
			if tc.decision != "" && ask.Overview.Decision != tc.want {
				t.Errorf("the board reads %q, want %q", ask.Overview.Decision, tc.want)
			}
		})
	}
}

// The board reads the decision first, on a card with a preview too, and the cached overview stays as written.
func TestReadyBoardDecisionNamesTheChangeWithAPreview(t *testing.T) {
	a := card("c1", nil)
	a.Title = "Clearer board buttons"
	f, _ := newTestFactory(t, a)
	if err := f.previews.put(Preview{Card: "c1", State: previewReady, URL: "https://preview.example/"}); err != nil {
		t.Fatal(err)
	}
	cached := &Overview{Decision: "Land this change?"}
	ask := readyAsk("")
	ask.Overview = cached
	f.fillReady(ask, "c1", "CHANGED: Buttons say what they do.")
	if ask.Overview.Decision != "Land: Clearer board buttons?" {
		t.Errorf("the board reads %q", ask.Overview.Decision)
	}
	if cached.Decision != "Land this change?" {
		t.Errorf("the cached overview changed: %q", cached.Decision)
	}
	specific := readyAsk("")
	specific.Overview = &Overview{Decision: "Use clearer buttons?"}
	f.fillReady(specific, "c1", "")
	if specific.Overview.Decision != "Use clearer buttons?" {
		t.Errorf("a specific decision changed: %q", specific.Overview.Decision)
	}
}

func TestReadyEmailNeverPrintsACutTitle(t *testing.T) {
	const cut = "Approval emails name the change and never show blank or…"
	const whole = "Approval emails name the change and never show blank"
	for _, tc := range []struct {
		name    string
		preview bool
		subject string
	}{
		{"with a preview", true, "Ready to try: " + whole},
		{"nothing to try", false, "Land: " + whole + "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := card("c1", nil)
			a.Title = cut
			f, _ := newTestFactory(t, a)
			if tc.preview {
				if err := f.previews.put(Preview{Card: "c1", State: previewReady, URL: "https://preview.example/"}); err != nil {
					t.Fatal(err)
				}
			}
			ask := readyAsk("")
			ask.Overview = &Overview{Decision: "Land this change?"}
			f.fillReady(ask, "c1", "CHANGED: Buttons say what they do.")
			e := f.readyEmail(a, Card{Title: cut}, ask)
			if e.Subject != tc.subject || !strings.HasPrefix(e.Text, tc.subject+"\n") {
				t.Errorf("subject %q:\n%s", e.Subject, e.Text)
			}
			if strings.Contains(e.Text, "…") || strings.Contains(e.HTML, "…") {
				t.Errorf("cut text:\n%s", e.Text)
			}
		})
	}
}

func TestReadyEmailNamesTheChangeFromTheCardTitle(t *testing.T) {
	f, _ := newTestFactory(t)
	ask := readyAsk("")
	ask.Overview = &Overview{Decision: "Land this change?"}
	f.fillReady(ask, "c1", "CHANGED: Buttons say what they do.")
	e := f.readyEmail(Agent{}, Card{Title: "Clearer board buttons"}, ask)
	if e.Subject != "Land: Clearer board buttons?" || !strings.HasPrefix(e.Text, e.Subject+"\n") {
		t.Errorf("subject %q:\n%s", e.Subject, e.Text)
	}
}

// Every agent that can ask Zach a question is told the limit emails hold it to.
func TestQuestionRuleStatesTheEmailLimit(t *testing.T) {
	if want := fmt.Sprintf("%d words", mailQuestionWordCap); !strings.Contains(questionRule, want) {
		t.Errorf("lacks %q: %s", want, questionRule)
	}
}

func TestReadyMsgAsksForANamedDecision(t *testing.T) {
	m := readyMsg("you asked for it")
	for _, want := range []string{"never a bare", "at most 8 words"} {
		if !strings.Contains(m, want) {
			t.Errorf("lacks %q: %s", want, m)
		}
	}
	if strings.Contains(m, "60 words") {
		t.Errorf("states the question limit, which questionRule owns: %s", m)
	}
	if strings.Contains(m, "<land this change?") {
		t.Errorf("still asks for the bare form: %s", m)
	}
}
