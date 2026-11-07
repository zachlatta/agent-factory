package factory

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	askReply = "DECISION: Send the booth form with the larger table?\n" +
		"OVERVIEW: The form is filled in, with the larger table.\n" +
		"NEEDS FROM ZACH: Answer yes or name another size.\n" +
		"DUE: 2026-10-27T22:00:00-04:00\n\nThe form is ready.\nTRACK: direct"
	askDue        = "2026-10-28T02:00:00.000Z"
	doneReply     = "DECISION: None.\nOVERVIEW: The form went in.\nNEEDS FROM ZACH: Nothing.\n\nTRACK: direct"
	askWithCommas = "DECISION: Send the form, with the larger table, today?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Answer it.\n\nTRACK: direct"
)

func TestDirectAskReadsTheStop(t *testing.T) {
	for _, tc := range []struct {
		name, reply, title string
		asks               bool
	}{
		{"none and nothing", "DECISION: None.\nNEEDS FROM ZACH: Nothing.", "", false},
		{"lower case none", "DECISION: none\nNEEDS FROM ZACH: nothing", "", false},
		{"none with a rest", "DECISION: None. The form went in.\nNEEDS FROM ZACH: Nothing.", "", false},
		{"decision only", "DECISION: Send the form?\nOVERVIEW: Ready.", "Send the form?", true},
		{"decision with nothing needed", "DECISION: Send the form?\nNEEDS FROM ZACH: Nothing.", "Send the form?", true},
		{"needs only", "DECISION: None.\nNEEDS FROM ZACH: Approve the send.", "Approve the send.", true},
		{"nothing, then an ask", "DECISION: None.\nNEEDS FROM ZACH: Nothing. Please reply with the size.", "", false},
		{"nothing from you", "DECISION: None.\nNEEDS FROM ZACH: Nothing from you.", "", false},
		{"no action needed", "DECISION: None.\nNEEDS FROM ZACH: No action needed from you.", "", false},
		{"nothing, then a pointer", "DECISION: None.\nNEEDS FROM ZACH: Nothing. The answer is in your email.", "", false},
		{"bold nothing, then a rest", "DECISION: None.\nNEEDS FROM ZACH: **Nothing.** Details are below.", "", false},
		{"decision with nothing, then a rest", "DECISION: Send the form?\nNEEDS FROM ZACH: Nothing from you.", "Send the form?", true},
		{"item left for Zach with nothing, then a rest", "DECISION: None.\nNEEDS FROM ZACH: Nothing from you.\n\nREMAINING: Zach: sign the form", "sign the form", true},
		{"question block with nothing, then a rest", "DECISION: None.\nNEEDS FROM ZACH: Nothing from you.\n\nQUESTION: Which size?\n- Large (recommended)\n- Small", "Which size?", true},
		{"no needs line", "DECISION: None.\nOVERVIEW: The form went in.", "", false},
		{"question block with an overview", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nQUESTION: Which size?\n- Large (recommended)\n- Small", "Which size?", true},
		{"question block with no overview", "All set.\n\nQUESTION: Which size?\n- Large (recommended)\n- Small", "Which size?", true},
		{"item left for Zach", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nREMAINING: Zach: sign the form", "sign the form", true},
		{"no overview", "Replied to Zach with the answer and archived the thread.", "", false},
		{"empty", "", "", false},
		{"optional try after landing", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nREMAINING: Zach: try the page", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title, asks := directAsk(tc.reply)
			if asks != tc.asks || title != tc.title {
				t.Errorf("directAsk = %q, %v; want %q, %v", title, asks, tc.title, tc.asks)
			}
		})
	}
}

func TestDirectAskNoteIsCappedAndHasAFallback(t *testing.T) {
	long := directAskNote(strings.Repeat("word ", 100) + "\nmore")
	if len(long) > len(noteDirectAsk)+directAskNoteCap+4 || strings.Contains(long, "\n") {
		t.Errorf("note = %q", long)
	}
	a := card("c1", map[string]string{labelStage: "zach", labelNote: directAskNote("  ")})
	if got := askFor(Card{ID: "c1", Stage: "zach"}, a).Title; got != directAskFallback {
		t.Errorf("empty note: title = %q", got)
	}
}

func TestParseDue(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"2026-10-27T22:00:00-04:00", true},
		{"2026-10-27T22:00-04:00", true},
		{"2026-10-28T02:00:00Z", true},
		{"2026-10-27T22:00:00", false},
		{"2026-10-27", false},
		{"tomorrow at 10", false},
		{"", false},
	} {
		if _, ok := parseDue(tc.in); ok != tc.ok {
			t.Errorf("parseDue(%q) ok = %v", tc.in, ok)
		}
	}
}

func TestParseOverviewKeepsDueOutOfNeeds(t *testing.T) {
	_, ov := ParseOverview("DECISION: Send it?\nNEEDS FROM ZACH: Answer it.\nDUE: 2026-10-27T22:00:00-04:00\n\nBody")
	if ov == nil || ov.Needs != "Answer it." || ov.Due != "2026-10-27T22:00:00-04:00" {
		t.Fatalf("overview = %+v", ov)
	}
}

// directCard is a card in its one-pass errand whose last reply is reply, on main with only its work item.
func directCard(t *testing.T, labels map[string]string, reply string) (*Factory, *fakePaseo, *fakeMail, Agent) {
	t.Helper()
	l := map[string]string{labelStage: "building", labelSource: "board", labelSince: ago(10 * time.Minute)}
	for k, v := range labels {
		l[k] = v
	}
	c := card("c1", l)
	c.Title = "Fill in the booth form"
	c.Cwd = t.TempDir()
	f, p, m := mailFactory(t, c)
	f.GitOut = gitStub(directOK)
	storeLog(t, f, p, "c1", reply)
	return f, p, m, c
}

// relabel plays the next Refresh: the board reads the labels the factory just set on c.
func relabel(f *Factory, p *fakePaseo, c Agent) {
	c.Labels = maps.Clone(c.Labels)
	for k, v := range p.labels(c.ID) {
		c.Labels[labelPrefix+k] = v
	}
	f.SetAgents([]Agent{c})
}

func TestFinishDirectWaitsWhenTheReplyAsks(t *testing.T) {
	f, p, m, c := directCard(t, nil, askReply)
	c.Labels[gmailThreadLabel] = "t1"
	f.SetAgents([]Agent{c})
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelTrack] != "direct" || l[labelNote] != noteDirectAsk+"Send the booth form with the larger table?" ||
		l[labelSince] != iso(t0) || l[labelDue] != askDue {
		t.Fatalf("labels = %v", l)
	}
	if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
		t.Errorf("review started: %v", runs)
	}
	relabel(f, p, c)
	f.Tick()
	relabel(f, p, c)
	f.Tick()
	sends := m.sends()
	if len(sends) != 1 || composed(t, f, "c1").Subject != "Send the booth form with the larger table?" {
		t.Fatalf("sends = %+v", sends)
	}
	text := sends[0].Args["text"].(string)
	for _, want := range []string{"Due ", `"done" to mark it done`, `"drop" to stop it`} {
		if !strings.Contains(text, want) {
			t.Errorf("email lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "DUE:") {
		t.Errorf("email shows the raw line:\n%s", text)
	}
}

func TestFinishDirectLandsWhenTheReplyAsksNothing(t *testing.T) {
	for name, reply := range map[string]string{
		"nothing":               doneReply,
		"nothing, then a rest":  "DECISION: None.\nOVERVIEW: Answered.\nNEEDS FROM ZACH: Nothing. The answer is in your email.\n\nTRACK: direct",
		"no action, then words": "DECISION: None.\nOVERVIEW: Answered.\nNEEDS FROM ZACH: No action needed from you.\n\nTRACK: direct",
	} {
		t.Run(name, func(t *testing.T) {
			f, p, m, c := directCard(t, map[string]string{labelDue: askDue}, reply)
			f.Tick()
			relabel(f, p, c)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != "landed" || l[labelNote] != "done in one pass" || l[labelDue] != "" {
				t.Fatalf("labels = %v", l)
			}
			if s := m.sends(); len(s) != 0 {
				t.Errorf("sends = %+v", s)
			}
		})
	}
}

func TestFinishDirectChecksComeBeforeTheAsk(t *testing.T) {
	t.Run("not on main", func(t *testing.T) {
		f, p, _, _ := directCard(t, nil, askReply)
		f.Git = func(_ context.Context, _ string, args ...string) error {
			if args[0] == "merge-base" {
				return errors.New("exit status 1")
			}
			return nil
		}
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != "" || l[labelNote] != "landing its result again" {
			t.Fatalf("labels = %v", l)
		}
	})
	t.Run("outside its work item", func(t *testing.T) {
		f, p, _, _ := directCard(t, nil, askReply)
		f.GitOut = gitStub(withAnswers(directOK, "show --name-only --format= aaa", "bin/land\n"))
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != "zach" || !strings.HasPrefix(l[labelNote], "direct card changed files") {
			t.Fatalf("labels = %v", l)
		}
	})
	t.Run("public repo", func(t *testing.T) {
		f, p, _, c := directCard(t, nil, askReply)
		c.Repo = "pub/repo"
		f.cfg.Guarded = []string{"pub/repo"}
		f.SetAgents([]Agent{c})
		f.Tick()
		if l := p.labels("c1"); l[labelStage] == "landed" || strings.HasPrefix(l[labelNote], noteDirectAsk) {
			t.Fatalf("labels = %v", l)
		}
	})
}

func TestDirectAskShape(t *testing.T) {
	ask := everyAsk()["direct ask"]
	if ask.Kind != askQuestion || ask.BoardOnly || !mailable(ask) || ask.landed {
		t.Fatalf("ask = %+v", ask)
	}
	if got := choiceIDs(ask); !slices.Equal(got, []string{"answer", "done", "drop"}) || !ask.Choices[0].Primary {
		t.Fatalf("choices = %+v", ask.Choices)
	}
	if goChoice(ask) != nil {
		t.Errorf("no word stands for an approval here: %+v", goChoice(ask))
	}
	for word, want := range map[string]string{"done": "done", "drop": "drop", "yes": "answer", "the larger one": "answer"} {
		if got := parseAnswer(word, ask); got.Choice != want {
			t.Errorf("%q = %+v, want %s", word, got, want)
		}
	}
}

// The note is a label, cut and with commas changed; what Zach reads is the agent's own wording.
func TestDirectAskTitleComesFromTheOverview(t *testing.T) {
	f, p, m, a := directCard(t, nil, askWithCommas)
	a.Labels[gmailThreadLabel] = "t1"
	f.SetAgents([]Agent{a})
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	f.Tick()
	relabel(f, p, a)
	if note := p.labels("c1")[labelNote]; !strings.HasPrefix(note, noteDirectAsk) {
		t.Fatalf("note = %q", note)
	}
	c, _ := f.card("c1")
	ask := f.replyAsk(c, f.agent("c1"))
	want := "Send the form, with the larger table, today?"
	if ask.Title != want {
		t.Errorf("title = %q", ask.Title)
	}
	f.Tick()
	if s := m.sends(); len(s) != 1 || composed(t, f, "c1").Subject != want {
		t.Errorf("sends = %+v", s)
	}
}

func TestDirectAskCanAskAgain(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelTrack: "direct",
		labelNote: directAskNote("Send it?"), labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.GitOut = gitStub(directOK)
	if _, err := f.Act("send", Body{ID: "c1", Message: "which form?"}); err != nil {
		t.Fatal(err)
	}
	stopsAfterReply(t, f, p, c, "DECISION: Send the second form?\nOVERVIEW: It is ready.\nNEEDS FROM ZACH: Answer it.\n\nTRACK: direct")
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != noteDirectAsk+"Send the second form?" {
		t.Errorf("labels = %v", l)
	}
}

func TestMarkDoneAndDropCloseADirectAsk(t *testing.T) {
	for _, tc := range []struct {
		name, choice string
		parked       bool
	}{
		{"mark done", "done", false},
		{"mark done parked", "done", true},
		{"drop", "drop", false},
		{"drop parked", "drop", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelTrack: "direct",
				labelNote: directAskNote("Send it?"), labelSince: ago(10 * time.Minute)})
			if tc.parked {
				c.Labels[labelPrefix+labelParked] = iso(t0)
				c.ArchivedAt = t0
			}
			f, p, _ := mailFactory(t, c)
			storeLog(t, f, p, "c1", askReply)
			if _, err := f.choose(Body{ID: "c1", Choice: tc.choice}); err != nil {
				t.Fatal(err)
			}
			if len(p.sent("c1")) != 0 {
				t.Errorf("sent = %v", p.sent("c1"))
			}
			want := map[string]string{"done": "marked done by Zach", "drop": "closed by Zach"}[tc.choice]
			if l := p.labels("c1"); l[labelNote] != want || l[labelParked] != "" {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestDirectAskCountsOneAsk(t *testing.T) {
	a := fresh("c1")
	a.Labels[labelPrefix+labelSince] = iso(t0)
	a.Cwd = t.TempDir()
	f, p := newTestFactory(t, a)
	f.GitOut = gitStub(withAnswers(directOK,
		"reflog show --max-count=2001 --format=%H%x09%gs HEAD", "aaa\tcommit: work\n",
		"show -s --format=%cI aaa", t0.Add(time.Minute).Format(time.RFC3339)+"\n"))
	clockAt(f, t0.Add(10*time.Minute))
	a.LastActivityAt = t0.Add(5 * time.Minute)
	f.SetAgents([]Agent{a})
	storeLog(t, f, p, "c1", askReply)
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" {
		t.Fatalf("labels = %v", l)
	}
	sync := func(at time.Time) {
		for k, v := range p.labels("c1") {
			a.Labels[labelPrefix+k] = v
		}
		clockAt(f, at)
		f.SetAgents([]Agent{a})
	}
	sync(t0.Add(11 * time.Minute))
	if ct := timing(t, f, "c1"); ct.asks() != 1 || ct.LandedAt.IsZero() {
		t.Fatalf("after the stop: asks = %d, landed %v", ct.asks(), ct.LandedAt)
	}
	sync(t0.Add(20 * time.Minute))
	if _, err := f.Act("send", Body{ID: "c1", Message: "yes"}); err != nil {
		t.Fatal(err)
	}
	sync(t0.Add(30 * time.Minute))
	a.LastActivityAt = t0.Add(29 * time.Minute)
	f.SetAgents([]Agent{a})
	storeLog(t, f, p, "c1", doneReply)
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landed" {
		t.Fatalf("labels = %v", l)
	}
	sync(t0.Add(31 * time.Minute))
	ct := timing(t, f, "c1")
	if ct.asks() != 1 || ct.LandedAt.IsZero() {
		t.Errorf("asks = %d, stages %v, landed %v", ct.asks(), stages(ct), ct.LandedAt)
	}
}

func TestDirectAskFromAnArchivedAgentShowsAndEmailsOnce(t *testing.T) {
	archived := func(t *testing.T) (*Factory, *fakePaseo, *fakeMail, Agent) {
		f, p, m, c := directCard(t, map[string]string{labelArchiveWatch: "1"}, askReply)
		c.Labels[gmailThreadLabel] = "t1"
		m.threads["t1"] = []threadMsg{{ID: "m1"}}
		c.ArchivedAt = t0
		f.SetAgents([]Agent{c})
		return f, p, m, c
	}
	t.Run("shows, parked, and mailed once", func(t *testing.T) {
		f, p, m, a := archived(t)
		f.Tick()
		l := p.labels("c1")
		if l[labelStage] != "zach" || l[labelParked] == "" {
			t.Fatalf("labels = %v", l)
		}
		relabel(f, p, a)
		c, ok := f.card("c1")
		if !ok || c.Stage != "zach" || c.Parked == "" || c.Ask == nil || c.Ask.Kind != askQuestion {
			t.Fatalf("card = %+v", c)
		}
		if s := BuildSections(f.boardCards(), t0, nil); len(s.ForYou) != 1 {
			t.Fatalf("For you = %+v", s.ForYou)
		}
		f.Tick()
		relabel(f, p, a)
		f.Tick()
		sends := m.sends()
		if len(sends) != 1 || composed(t, f, "c1").Subject != "Send the booth form with the larger table?" {
			t.Fatalf("sends = %+v", sends)
		}
	})
	t.Run("a board card shows and parks with no email", func(t *testing.T) {
		f, p, m, c := directCard(t, map[string]string{labelArchiveWatch: "1"}, askReply)
		c.ArchivedAt = t0
		f.SetAgents([]Agent{c})
		f.Tick()
		relabel(f, p, c)
		f.Tick()
		if s := BuildSections(f.boardCards(), t0, nil); len(s.ForYou) != 1 {
			t.Fatalf("For you = %+v", s.ForYou)
		}
		if s := m.sends(); len(s) != 0 {
			t.Fatalf("board card emailed Zach: %+v", s)
		}
	})
	t.Run("a failed send is tried again", func(t *testing.T) {
		f, p, m, a := archived(t)
		m.errs["reply_to_message"] = errors.New("mail down")
		f.Tick()
		relabel(f, p, a)
		f.Tick()
		if s := m.sends(); len(s) == 0 {
			t.Fatalf("no send tried")
		}
		tried := len(m.sends())
		delete(m.errs, "reply_to_message")
		relabel(f, p, a)
		f.Tick()
		if len(m.sends()) != tried {
			t.Fatalf("retried before mailRetryAfter")
		}
		clockAt(f, t0.Add(mailRetryAfter+time.Minute))
		relabel(f, p, a)
		f.Tick()
		if len(m.sends()) != tried+1 {
			t.Fatalf("not retried: %d after %d", len(m.sends()), tried)
		}
	})
	t.Run("a reply wakes it on the direct track", func(t *testing.T) {
		f, p, _, a := archived(t)
		f.Tick()
		relabel(f, p, a)
		if _, err := f.Act("send", Body{ID: "c1", Message: "yes"}); err != nil {
			t.Fatal(err)
		}
		l := p.labels("c1")
		if l[labelStage] != "building" || l[labelParked] != "" || l[labelNote] != "answering Zach" {
			t.Fatalf("labels = %v", l)
		}
	})
}

// reminderCard waits on Zach since since, with a due time, in a thread Zach started.
func reminderCard(t *testing.T, since, due time.Time) (*Factory, *fakePaseo, *fakeMail, Agent) {
	t.Helper()
	c := inThread(card("c1", map[string]string{labelStage: "zach", labelSource: "board", labelTrack: "direct",
		labelNote: directAskNote("Send it?"), labelSince: iso(since), labelDue: iso(due), labelTold: directAskNote("Send it?")}), "t1")
	c.Title = "Fill in the booth form"
	f, p, m := mailFactory(t, c)
	m.threads["t1"] = []threadMsg{{ID: "m1"}}
	storeLog(t, f, p, "c1", "DECISION: Send it?\nOVERVIEW: Ready.\nNEEDS FROM ZACH: Answer it.\n\nTRACK: direct")
	return f, p, m, c
}

func TestDueReminderWindow(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	for _, tc := range []struct {
		name       string
		since, now time.Time
		sends      int
	}{
		{"before the window", t0, due.Add(-6*time.Hour - time.Second), 0},
		{"at the window", t0, due.Add(-6 * time.Hour), 1},
		{"inside the window", t0, due.Add(-time.Hour), 1},
		{"at the deadline", t0, due, 0},
		{"past the deadline", t0, due.Add(time.Hour), 0},
		{"asked inside the window", due.Add(-5 * time.Hour), due.Add(-time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, m, c := reminderCard(t, tc.since, due)
			clockAt(f, tc.now)
			f.mailDueReminders(f.agents(), tc.now)
			relabel(f, p, c)
			f.mailDueReminders(f.agents(), tc.now)
			sends := m.sends()
			if len(sends) != tc.sends {
				t.Fatalf("sends = %+v", sends)
			}
			if tc.sends == 1 {
				if sends[0].Tool != "reply_to_message" || p.labels("c1")[labelDueTold] != iso(due) {
					t.Errorf("sent %+v, labels %v", sends[0], p.labels("c1"))
				}
			}
		})
	}
}

func TestTickSendsTheDueReminder(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	f, p, m, c := reminderCard(t, t0, due)
	clockAt(f, due.Add(-time.Hour))
	f.Tick()
	relabel(f, p, c)
	f.Tick()
	var reminders int
	for _, s := range m.sends() {
		if text, _ := s.Args["text"].(string); strings.HasPrefix(text, "Reminder: ") {
			reminders++
		}
	}
	if reminders != 1 || p.labels("c1")[labelDueTold] != iso(due) {
		t.Fatalf("reminders = %d, sends %+v, labels %v", reminders, m.sends(), p.labels("c1"))
	}
}

func TestDueReminderOncePerDueTime(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	f, p, m, c := reminderCard(t, t0, due)
	now := due.Add(-time.Hour)
	clockAt(f, now)
	f.mailDueReminders(f.agents(), now)
	if len(m.sends()) != 1 {
		t.Fatalf("sends = %+v", m.sends())
	}
	sync := func(extra map[string]string) {
		for k, v := range p.labels("c1") {
			c.Labels[labelPrefix+k] = v
		}
		for k, v := range extra {
			c.Labels[labelPrefix+k] = v
		}
		f.SetAgents([]Agent{c})
	}
	sync(map[string]string{labelSince: iso(t0.Add(time.Hour))}) // asked again, same time
	f.mailDueReminders(f.agents(), now)
	if len(m.sends()) != 1 {
		t.Fatalf("same due time reminded again: %+v", m.sends())
	}
	later := due.Add(24 * time.Hour)
	sync(map[string]string{labelDue: iso(later)}) // asked again, a new time
	now = later.Add(-time.Hour)
	clockAt(f, now)
	f.mailDueReminders(f.agents(), now)
	if len(m.sends()) != 2 {
		t.Fatalf("new due time not reminded: %+v", m.sends())
	}
}

func TestDueReminderReachesParkedCardsOnly(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	now := due.Add(-time.Hour)
	t.Run("parked", func(t *testing.T) {
		f, _, m, c := reminderCard(t, t0, due)
		c.Labels[labelPrefix+labelParked] = iso(t0)
		c.ArchivedAt = t0
		f.SetAgents([]Agent{c})
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if len(m.sends()) != 1 {
			t.Fatalf("sends = %+v", m.sends())
		}
	})
	for name, change := range map[string]func(*Agent){
		"dropped":  func(c *Agent) { c.ArchivedAt = t0 },
		"answered": func(c *Agent) { c.Labels[labelPrefix+labelStage] = "plan" },
		"other stop": func(c *Agent) {
			c.Labels[labelPrefix+labelNote] = "direct card changed files outside its work item without review: x"
		},
		"marked landed": func(c *Agent) { c.Labels[labelPrefix+labelStage] = "landed" },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, m, c := reminderCard(t, t0, due)
			change(&c)
			f.SetAgents([]Agent{c})
			clockAt(f, now)
			f.mailDueReminders(f.agents(), now)
			if len(m.sends()) != 0 {
				t.Fatalf("sends = %+v", m.sends())
			}
		})
	}
	t.Run("passive", func(t *testing.T) {
		f, _, m, _ := reminderCard(t, t0, due)
		f.cfg.DryRun = true
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if len(m.sends()) != 0 {
			t.Fatalf("sends = %+v", m.sends())
		}
	})
	t.Run("replaced predecessor", func(t *testing.T) {
		f, _, m, c := reminderCard(t, t0, due)
		c.Labels[labelPrefix+labelReplacedBy] = "c2"
		next := card("c2", map[string]string{labelStage: "zach", labelSource: "board", labelReplaces: "c1",
			labelNote: directAskNote("Send it?"), labelSince: iso(t0), labelDue: iso(due), labelDueTold: iso(due)})
		f.SetAgents([]Agent{c, next})
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if len(m.sends()) != 0 {
			t.Fatalf("sends = %+v", m.sends())
		}
	})
	t.Run("a failed send is tried again", func(t *testing.T) {
		f, p, m, _ := reminderCard(t, t0, due)
		m.errs["reply_to_message"] = errors.New("mail down")
		clockAt(f, now)
		f.mailDueReminders(f.agents(), now)
		if p.labels("c1")[labelDueTold] != "" {
			t.Fatalf("labels = %v", p.labels("c1"))
		}
		tried := len(m.sends())
		delete(m.errs, "reply_to_message")
		retry := now.Add(mailRetryAfter + time.Minute)
		clockAt(f, retry)
		f.mailDueReminders(f.agents(), retry)
		if len(m.sends()) != tried+1 || p.labels("c1")[labelDueTold] != iso(due) {
			t.Fatalf("sends = %+v, labels %v", m.sends(), p.labels("c1"))
		}
	})
}

// A later stop of another kind must not show or remind about the due time of an earlier ask.
func TestDueBelongsToTheDirectAsk(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: directAskNote("Send it?"), labelDue: askDue})
	c := BuildBoard([]Agent{a}, nil, t0)[0]
	if c.Due != askDue {
		t.Errorf("due = %q", c.Due)
	}
	if c.Overdue {
		t.Errorf("overdue before the deadline")
	}
	if !BuildBoard([]Agent{a}, nil, parseTime(askDue).Add(time.Minute))[0].Overdue {
		t.Errorf("not overdue after the deadline")
	}
	a.Labels[labelPrefix+labelNote] = "direct card scope not verified: x"
	if c := BuildBoard([]Agent{a}, nil, t0)[0]; c.Due != "" {
		t.Errorf("due on another stop = %q", c.Due)
	}
	a.Labels[labelPrefix+labelNote] = directAskNote("Send it?")
	a.Labels[labelPrefix+labelDue] = "not a time"
	if c := BuildBoard([]Agent{a}, nil, t0)[0]; c.Due != "" {
		t.Errorf("due from a bad label = %q", c.Due)
	}
}

func TestBadDueLineStillStops(t *testing.T) {
	f, p, _, _ := directCard(t, nil, strings.Replace(askReply, "2026-10-27T22:00:00-04:00", "2026-10-27T22:00:00", 1))
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelDue] != "" {
		t.Fatalf("labels = %v", l)
	}
}

func TestForYouShowsDueCardsFirstSoonestFirst(t *testing.T) {
	zach := func(id, due string) Agent {
		a := card(id, map[string]string{labelStage: "zach", labelNote: directAskNote("Send " + id + "?"), labelDue: due})
		a.Title = id
		return a
	}
	later, sooner := iso(t0.Add(48*time.Hour)), iso(t0.Add(24*time.Hour))
	plain := card("plain", map[string]string{labelStage: "zach", labelNote: readyNote(1)})
	plain.Title = "plain"
	cards := BuildBoard([]Agent{plain, zach("later", later), zach("sooner", sooner)}, nil, t0)
	s := BuildSections(cards, t0, nil)
	var got []string
	for _, l := range s.ForYou {
		got = append(got, l.ID)
	}
	if !slices.Equal(got, []string{"sooner", "later", "plain"}) {
		t.Fatalf("order = %v", got)
	}
	if s.ForYou[0].Due != sooner || s.ForYou[2].Due != "" {
		t.Errorf("lines = %+v", s.ForYou)
	}
}

func TestDigestPutsDueFirst(t *testing.T) {
	f, m, now := digestFactory(t, 18, 0)
	due := now.Add(4 * time.Hour)
	asked := card("c9", map[string]string{labelStage: "zach", labelSource: "board",
		labelNote: directAskNote("Send the booth form?"), labelDue: iso(due)})
	asked.Title = "Fill in the booth form"
	f.SetAgents(append(f.agents(), asked))
	f.tickDigest()
	d := digests(m)
	if len(d) != 1 {
		t.Fatalf("digests = %+v", d)
	}
	text := d[0].Args["text"].(string)
	if subj := d[0].Args["subject"].(string); !strings.HasPrefix(subj, "Factory today: 1 due, 1 for you") {
		t.Errorf("subject = %q", subj)
	}
	if !strings.HasPrefix(text, "Due\n") {
		t.Errorf("Due is not first:\n%s", text)
	}
	if !strings.Contains(text, "Wed Oct 28, 10:00 PM") {
		t.Errorf("the due time is not local:\n%s", text)
	}
	if strings.Count(text, "Fill in the booth form") != 1 {
		t.Errorf("listed twice:\n%s", text)
	}
	if !strings.Contains(d[0].Args["html"].(string), ">Due<") {
		t.Errorf("html lacks the part: %s", d[0].Args["html"])
	}
	if i, j := strings.Index(text, "Fill in the booth form"), strings.Index(text, "Answer by reply"); i < 0 || j < 0 || i > j {
		t.Errorf("order:\n%s", text)
	}
}

func TestDueText(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	due := time.Date(2026, 10, 28, 22, 0, 0, 0, loc)
	if got := dueText(due, due.Add(-time.Hour), loc); got != "Due Wed Oct 28, 10:00 PM" {
		t.Errorf("before = %q", got)
	}
	if got := dueText(due, due.Add(time.Hour), loc); got != "Overdue: was due Wed Oct 28, 10:00 PM" {
		t.Errorf("after = %q", got)
	}
}

func TestDueReminderEmailIsPlain(t *testing.T) {
	due := t0.Add(48 * time.Hour)
	f, _, m, _ := reminderCard(t, t0, due)
	now := due.Add(-time.Hour)
	clockAt(f, now)
	f.mailDueReminders(f.agents(), now)
	s := m.sends()
	if len(s) != 1 {
		t.Fatalf("sends = %+v", s)
	}
	text := s[0].Args["text"].(string)
	for _, want := range []string{"Send it?", "Due ", `"done"`, `"drop"`, "https://board.example/#c1"} {
		if !strings.Contains(text, want) {
			t.Errorf("reminder lacks %q:\n%s", want, text)
		}
	}
}

func TestNewCardPromptTeachesDue(t *testing.T) {
	for _, want := range []string{"DUE: 2035-", "waits for him", "time zone"} {
		if !strings.Contains(newCardPrompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestReplyToDirectAskStaysDirect(t *testing.T) {
	for _, tc := range []struct {
		name, source, via string
	}{
		{"board", "board", ""},
		{"board card answered by email", "board", "email"},
		{"inbox", "email", "email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "zach", labelSource: tc.source, labelTrack: "direct",
				labelNote: directAskNote("Send it?"), labelDue: askDue, labelSince: ago(10 * time.Minute)})
			c.Labels["gmail-thread"] = "t1"
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.GitOut = gitStub(directOK)
			if _, err := f.Act("send", Body{ID: "c1", Message: "yes, the larger one", Via: tc.via}); err != nil {
				t.Fatal(err)
			}
			l := p.labels("c1")
			if l[labelStage] != "building" || l[labelNote] != "answering Zach" || l[labelAutoReview] != gateCheck {
				t.Fatalf("after the reply: %v", l)
			}
			sent := p.sent("c1")
			if len(sent) != 1 || !strings.Contains(sent[0], "yes, the larger one") || !strings.Contains(sent[0], "TRACK: direct") {
				t.Fatalf("sent = %v", sent)
			}
			stopsAfterReply(t, f, p, c, "Sent it.\n\n"+doneReply)
			f.Tick()
			if runs := p.runsWithLabel(labelArg(labelReviewOf, "c1")); len(runs) != 0 {
				t.Fatalf("review started: %v", runs)
			}
			if l := p.labels("c1"); l[labelStage] != "landed" || l[labelDue] != "" {
				t.Errorf("labels = %v", l)
			}
		})
	}
}

func TestDigestPutsDueAboveTheNumberedList(t *testing.T) {
	asked := card("c9", map[string]string{labelStage: "zach", labelSource: "board",
		labelNote: directAskNote("Send the booth form?"), labelDue: "2026-10-28T22:00:00-04:00"})
	asked.Title = "Fill in the booth form"
	f, _, m := sentDigest(t, digestWait("c1", "Newer build", time.Hour), asked)
	text := digests(m)[0].Args["text"].(string)
	if !strings.HasPrefix(text, "Due\n- Fill in the booth form") {
		t.Errorf("Due is not first:\n%s", text)
	}
	html := digests(m)[0].Args["html"].(string)
	due, list := strings.Index(html, ">Due<"), strings.Index(html, ">Answer by reply<")
	if due < 0 || list < 0 || due > list {
		t.Errorf("Due is not above the numbered list: %s", html)
	}
	if !strings.Contains(text, "1. Newer build:") {
		t.Errorf("the one-word ask lost its number:\n%s", text)
	}
	d, _ := f.readDigest()
	if len(d.Lists) != 1 || len(d.Lists[0].Items) != 1 || d.Lists[0].Items[0].Card != "c1" {
		t.Errorf("a question that needs words got a number: %+v", d.Lists)
	}
	if subj := digests(m)[0].Args["subject"].(string); !strings.HasPrefix(subj, "Factory today: 1 due, 1 for you") {
		t.Errorf("subject = %q", subj)
	}
}
