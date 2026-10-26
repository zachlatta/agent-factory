package factory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fresh is a board card created just now, in plan.
func fresh(id string) Agent {
	a := card(id, map[string]string{labelStage: "plan", labelSource: "board"})
	a.CreatedAt = t0.Add(-30 * time.Second)
	return a
}

// moved is a with its column set to stage, moved at since ("" = no since label).
func moved(a Agent, stage, since string) Agent {
	l := map[string]string{}
	for k, v := range a.Labels {
		l[k] = v
	}
	l[labelPrefix+labelStage] = stage
	if since != "" {
		l[labelPrefix+labelSince] = since
	}
	a.Labels = l
	return a
}

func stages(t CardTiming) []string {
	var out []string
	for _, e := range t.Events {
		out = append(out, e.Stage)
	}
	return out
}

func timing(t *testing.T, f *Factory, id string) CardTiming {
	t.Helper()
	ct, ok := f.timings.get(id)
	if !ok {
		t.Fatalf("%s not timed", id)
	}
	return ct
}

func TestTimingRecordsStagesFromAcceptedReads(t *testing.T) {
	a := fresh("c1")
	f, _ := newTestFactory(t, a)
	ct := timing(t, f, "c1")
	if ct.Partial || len(ct.Events) != 1 || !ct.Events[0].At.Equal(a.CreatedAt) || !ct.RequestedAt.Equal(a.CreatedAt) ||
		ct.RequestedFrom != "created" || ct.Source != "board" {
		t.Fatalf("first sight = %+v", ct)
	}
	// a move with a since label is dated by it; the same read again adds nothing
	f.Now = func() time.Time { return t0.Add(10 * time.Minute) }
	b := moved(a, "plan-review", t0.Add(9*time.Minute).Format(time.RFC3339))
	f.SetAgents([]Agent{b})
	f.SetAgents([]Agent{b})
	// Needs Zach sets no since: it is dated when the factory saw it
	f.Now = func() time.Time { return t0.Add(20 * time.Minute) }
	f.SetAgents([]Agent{moved(b, "zach", "")})
	ct = timing(t, f, "c1")
	if got := strings.Join(stages(ct), " "); got != "plan plan-review zach" {
		t.Fatalf("stages = %s", got)
	}
	if !ct.Events[1].At.Equal(t0.Add(9*time.Minute)) || !ct.Events[2].At.Equal(t0.Add(20*time.Minute)) {
		t.Errorf("times = %+v", ct.Events)
	}
	f.Now = func() time.Time { return t0.Add(50 * time.Minute) }
	f.SetAgents([]Agent{moved(b, "building", t0.Add(50*time.Minute).Format(time.RFC3339))})
	if z := timing(t, f, "c1").zach(t0.Add(time.Hour)); z != 30*time.Minute {
		t.Errorf("zach = %v", z)
	}
	f.mu.Lock()
	v := f.cards[0].Timing
	f.mu.Unlock()
	if v == nil || len(v.Stages) != 4 || v.ZachSecs != 1800 || v.Stages[0].Secs != 570 {
		t.Errorf("view = %+v", v)
	}
}

func TestTimingNeedsNoPaseoCall(t *testing.T) {
	a := fresh("c1")
	f, p := newTestFactory(t, a)
	before := len(p.calls)
	f.SetAgents([]Agent{moved(a, "building", ago(-time.Minute))})
	f.Now = func() time.Time { return t0.Add(time.Hour) }
	f.SetAgents([]Agent{moved(a, "review", "")})
	f.SetAgents([]Agent{moved(a, "landed", "")})
	if len(p.calls) != before {
		t.Errorf("paseo calls: %v", p.calls[before:])
	}
}

func TestTimingIgnoresOlderReads(t *testing.T) {
	a := fresh("c1")
	f, _ := newTestFactory(t, a)
	f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
	newer := moved(a, "plan-review", t0.Add(4*time.Minute).Format(time.RFC3339))
	seqOld, seqNew := f.nextRead(), f.nextRead()
	if !f.setAgents([]Agent{newer}, seqNew, false) {
		t.Fatal("newer read refused")
	}
	f.Now = func() time.Time { return t0.Add(6 * time.Minute) }
	if f.setAgents([]Agent{moved(a, "zach", "")}, seqOld, false) {
		t.Fatal("older read accepted")
	}
	if got := strings.Join(stages(timing(t, f, "c1")), " "); got != "plan plan-review" {
		t.Errorf("stages = %s", got)
	}
	// the store's own guard, for reads that reach it out of order
	cards := BuildBoard([]Agent{a}, nil, t0)
	if f.timings.observe(cards, []Agent{a}, seqOld, t0.Add(7*time.Minute), nil) {
		t.Error("an older seq changed timings")
	}
}

func TestTimingSameReadRebuildRecordsArchive(t *testing.T) {
	a := moved(fresh("c1"), "landing", ago(time.Minute))
	f, p := newTestFactory(t, a)
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	if _, err := f.Act("archive", Body{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_ = p
	ct := timing(t, f, "c1")
	if l := ct.last(); l.Stage != "landed" || !l.At.Equal(t0.Add(time.Minute)) {
		t.Errorf("events = %+v", ct.Events)
	}
}

func TestTimingSurvivesRestart(t *testing.T) {
	a := fresh("c1")
	f, p := newTestFactory(t, a)
	f.Now = func() time.Time { return t0.Add(5 * time.Minute) }
	f.SetAgents([]Agent{moved(a, "plan-review", t0.Add(4*time.Minute).Format(time.RFC3339))})
	for range 5 {
		f.nextRead() // the old run read many times; the new one starts again at 0
	}
	f.SetAgents([]Agent{moved(a, "plan-review", t0.Add(4*time.Minute).Format(time.RFC3339))})
	if b, err := os.ReadFile(filepath.Join(f.cfg.StateDir, "timings.json")); err != nil || strings.Contains(strings.ToLower(string(b)), "seq") {
		t.Fatalf("file = %s %v (no read number saved)", b, err)
	}

	g := New(Config{Host: "host1", Home: f.cfg.Home, StateDir: f.cfg.StateDir}, p)
	g.Now = func() time.Time { return t0.Add(30 * time.Minute) }
	g.GitOut = f.GitOut
	// unchanged column: no duplicate
	g.SetAgents([]Agent{moved(a, "plan-review", t0.Add(4*time.Minute).Format(time.RFC3339))})
	if got := strings.Join(stages(timing(t, g, "c1")), " "); got != "plan plan-review" {
		t.Fatalf("after restart stages = %s", got)
	}
	// a move made while the factory was down is dated by its since; the next one is still recorded
	g.SetAgents([]Agent{moved(a, "building", t0.Add(20*time.Minute).Format(time.RFC3339))})
	g.Now = func() time.Time { return t0.Add(40 * time.Minute) }
	g.SetAgents([]Agent{moved(a, "review", t0.Add(39*time.Minute).Format(time.RFC3339))})
	ct := timing(t, g, "c1")
	if got := strings.Join(stages(ct), " "); got != "plan plan-review building review" {
		t.Fatalf("stages = %s", got)
	}
	if !ct.Events[2].At.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("down-time move at %v", ct.Events[2].At)
	}
}

func TestTimingStoreSkipsUnchangedAndPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timings.json")
	now := t0
	s := newTimingStore(path, func() time.Time { return now }, time.UTC)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("written with nothing changed")
	}
	s.cards["old"] = &CardTiming{Events: []TimingEvent{{Stage: "landed", At: t0.Add(-31 * 24 * time.Hour)}}}
	s.cards["new"] = &CardTiming{Events: []TimingEvent{{Stage: "landed", At: t0.Add(-time.Hour)}}}
	s.dirty = true
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	r := newTimingStore(path, func() time.Time { return now }, time.UTC)
	if _, ok := r.cards["old"]; ok || r.cards["new"] == nil {
		t.Errorf("loaded = %v", r.cards)
	}
}

func TestTimingReadOnlyWritesNothing(t *testing.T) {
	p := &fakePaseo{logs: map[string]string{}}
	dir := t.TempDir()
	f := New(Config{Host: "host1", Home: t.TempDir(), StateDir: dir, ReadOnly: true}, p)
	f.Now = func() time.Time { return t0 }
	f.SetAgents([]Agent{fresh("c1")})
	if _, ok := f.timings.get("c1"); !ok {
		t.Error("not timed in memory")
	}
	if _, err := os.Stat(filepath.Join(dir, "timings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("read-only factory wrote timings")
	}
}

func TestTimingOnlyFactoryCards(t *testing.T) {
	app := Agent{ID: "app1", Status: "running", CreatedAt: t0, LastActivityAt: t0, Labels: map[string]string{}}
	sched := Agent{ID: "s1", Status: "idle", CreatedAt: t0, Labels: map[string]string{"paseo.schedule-id": "x"}}
	scout := card("sc1", map[string]string{labelRole: "scout"})
	picker := card("p1", map[string]string{labelStage: "plan", labelRouting: "1"})
	rev := reviewer("r1", "c1", "plan")
	doneBefore := card("d1", map[string]string{labelStage: "landed"}) // finished before timing saw it
	mailCard := card("e1", map[string]string{labelStage: "plan", labelSource: "email"})
	mailCard.Labels[gmailThreadLabel] = "th1"
	f, _ := newTestFactory(t, app, sched, scout, picker, rev, mailCard, doneBefore)
	// the unlabelled session moves between Building and Needs Zach as it works
	app.Status = "idle"
	app.RequiresAttention, app.AttentionReason = true, "finished"
	f.SetAgents([]Agent{app, sched, scout, picker, rev, mailCard, doneBefore})
	for _, id := range []string{"app1", "s1", "sc1", "p1", "r1", "d1"} {
		if _, ok := f.timings.get(id); ok {
			t.Errorf("%s timed", id)
		}
	}
	ct := timing(t, f, "e1")
	if !ct.Partial || ct.Source != "email" || ct.Thread != "th1" {
		t.Errorf("email card = %+v", ct)
	}
}

func TestTimingRequestedAt(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	created := time.Date(2026, 10, 26, 5, 3, 0, 0, time.UTC)
	newYear := time.Date(2027, 1, 1, 5, 10, 0, 0, time.UTC) // just after midnight on 1 Jan in New York
	for _, tc := range []struct {
		name, source, prompt string
		created              time.Time
		want                 time.Time
		from                 string
	}{
		{"email", "email", "Intro\n- Gmail thread ID: x\n- Date: Sun, 25 Oct 2026 21:33:59 -0400\nZach's email:\nhi\n(end of Zach's email)",
			created, time.Date(2026, 10, 26, 1, 33, 59, 0, time.UTC), "email-date"},
		{"voice", "voice", "- Recorded: Mon Oct 26, 1:01 AM (America/New_York), 31 seconds\n",
			created, time.Date(2026, 10, 26, 5, 1, 0, 0, time.UTC), "voice-recorded"},
		{"voice over new year", "voice", "- Recorded: Thu Dec 31, 11:58 PM (America/New_York), 9 seconds\n",
			newYear, time.Date(2027, 1, 1, 4, 58, 0, 0, time.UTC), "voice-recorded"},
		{"bad date", "email", "- Date: yesterday\n", created, created, "created"},
		{"no line", "email", "Zach's email:\nhi\n(end of Zach's email)", created, created, "created"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := card("c1", map[string]string{labelStage: "plan", labelSource: tc.source})
			a.CreatedAt = tc.created
			r := CardRequest{Read: true}
			r.AskedKind, r.Asked = askedOf(tc.prompt)
			got, from := requestedAt(a, map[string]CardRequest{"c1": r}, ny, tc.created)
			if !got.Equal(tc.want) || from != tc.from {
				t.Errorf("got %v %s, want %v %s", got, from, tc.want, tc.from)
			}
		})
	}
	// an email card whose first prompt is not read yet waits
	a := card("c1", map[string]string{labelStage: "plan", labelSource: "email"})
	a.CreatedAt = t0.Add(-time.Minute)
	if got, _ := requestedAt(a, nil, ny, t0); !got.IsZero() {
		t.Errorf("unread prompt gave %v", got)
	}
	// ... until askedWait: then its creation
	if got, from := requestedAt(a, nil, ny, t0.Add(askedWait)); !got.Equal(a.CreatedAt) || from != "created" {
		t.Errorf("long unread prompt gave %v %s", got, from)
	}
}

func TestTimingRecordsReviewRounds(t *testing.T) {
	c := moved(fresh("c1"), "review", ago(5*time.Minute))
	c.Labels[labelPrefix+labelTier] = "low"
	c.Labels[labelPrefix+labelPlanRounds] = "1"
	f, p := newTestFactory(t, c, reviewer("r1", "c1", "code"))
	p.logs["r1"] = "VERDICT: approve"
	f.ApplyVerdict(f.agent("r1"))
	ct := timing(t, f, "c1")
	if len(ct.Rounds) != 1 {
		t.Fatalf("rounds = %+v", ct.Rounds)
	}
	r := ct.Rounds[0]
	if r.Kind != "code" || r.N != 1 || r.Verdict != "approve" || !r.Start.Equal(t0.Add(-5*time.Minute)) || !r.End.Equal(t0) {
		t.Errorf("round = %+v", r)
	}
}

func TestTimingRecordsLandingCommit(t *testing.T) {
	commit := "2026-10-25T19:40:00-04:00"
	landedLog := map[string]string{"log -1 --format=%cI HEAD": commit + "\n"}
	t.Run("landing", func(t *testing.T) {
		c := moved(fresh("c1"), "landing", ago(10*time.Minute))
		c.Cwd = t.TempDir()
		f, p := newTestFactory(t, c)
		f.Git = func(context.Context, string, ...string) error { return nil }
		f.GitOut = gitStub(landedLog)
		p.logs["c1"] = "Landed.\nREMAINING: none"
		f.Tick()
		if got := timing(t, f, "c1").LandedAt; !got.Equal(parseTime(commit)) {
			t.Errorf("landedAt = %v", got)
		}
	})
	t.Run("direct", func(t *testing.T) {
		c := fresh("c1")
		c.Labels[labelPrefix+labelSince] = ago(10 * time.Minute)
		c.Cwd = t.TempDir()
		f, p := newTestFactory(t, c)
		p.logs["c1"] = "Answered and landed the work item.\nTRACK: direct"
		f.GitOut = gitStub(directOK, landedLog)
		f.Git = func(context.Context, string, ...string) error { return nil }
		f.Tick()
		if l := p.labels("c1"); l[labelStage] != "landed" {
			t.Fatalf("labels = %v", l)
		}
		if got := timing(t, f, "c1").LandedAt; !got.Equal(parseTime(commit)) {
			t.Errorf("landedAt = %v", got)
		}
	})
}

// replyCard is an email card that landed at t0-20m and reached Done at t0-10m, so the reply check
// is due at t0.
func replyCard(t *testing.T, msgs []threadMsg, mails ...FactoryMail) (*Factory, *fakeMail) {
	t.Helper()
	c := card("e1", map[string]string{labelStage: "landed", labelSource: "email"})
	c.Labels[gmailThreadLabel] = "th1"
	f, _, m := mailFactory(t)
	m.threads["th1"] = msgs
	f.timings.cards["e1"] = &CardTiming{Source: "email", Thread: "th1", RequestedAt: t0.Add(-2 * time.Hour),
		LandedAt: t0.Add(-20 * time.Minute), Mails: mails,
		Events: []TimingEvent{{Stage: "plan", At: t0.Add(-2 * time.Hour)}, {Stage: "landed", At: t0.Add(-10 * time.Minute)}}}
	return f, m
}

func msg(id string, at time.Time, sent bool) threadMsg {
	m := threadMsg{ID: id, Date: at.Format(time.RFC1123Z)}
	if sent {
		m.LabelIDs = []string{"SENT"}
	}
	return m
}

func gets(m *fakeMail) int {
	n := 0
	for _, c := range m.calls {
		if c.Tool == "get_thread" {
			n++
		}
	}
	return n
}

func TestTimingReplySent(t *testing.T) {
	ask := t0.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name  string
		msgs  []threadMsg
		mails []FactoryMail
		want  time.Time
	}{
		{"one-pass answer before its commit", []threadMsg{msg("z", ask, false), msg("a", t0.Add(-25*time.Minute), true)},
			nil, t0.Add(-25 * time.Minute)},
		{"build reply after landing", []threadMsg{msg("z", ask, false), msg("w", t0.Add(-90*time.Minute), true),
			msg("a", t0.Add(-15*time.Minute), true)}, nil, t0.Add(-15 * time.Minute)},
		{"factory notes by id and by time", []threadMsg{msg("z", ask, false), msg("a", t0.Add(-60*time.Minute), true),
			msg("n1", t0.Add(-30*time.Minute), true), msg("n2", t0.Add(-12*time.Minute), true)},
			[]FactoryMail{{ID: "n1", At: t0.Add(-29 * time.Minute)}, {At: t0.Add(-12*time.Minute + 30*time.Second)}},
			t0.Add(-60 * time.Minute)},
		{"approval confirmation is the only later message", []threadMsg{msg("z", ask, false),
			msg("zz", t0.Add(-40*time.Minute), false), msg("ok", t0.Add(-39*time.Minute), true)},
			[]FactoryMail{{ID: "ok", At: t0.Add(-39 * time.Minute)}}, time.Time{}},
		{"later unrelated message", []threadMsg{msg("z", ask, false), msg("x", t0.Add(-4*time.Minute), true)},
			nil, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, m := replyCard(t, tc.msgs, tc.mails...)
			f.checkReplies(t0)
			f.checkReplies(t0.Add(time.Hour))
			ct := timing(t, f, "e1")
			if !ct.RepliedAt.Equal(tc.want) || !ct.ReplyChecked {
				t.Errorf("repliedAt = %v checked %v, want %v", ct.RepliedAt, ct.ReplyChecked, tc.want)
			}
			if n := gets(m); n != 1 {
				t.Errorf("thread reads = %d", n)
			}
		})
	}
}

func TestTimingReplyCheckSkips(t *testing.T) {
	f, m := replyCard(t, nil)
	f.checkReplies(t0.Add(-6 * time.Minute)) // Done only 4 minutes ago
	f.timings.cards["b1"] = &CardTiming{Source: "board", LandedAt: t0.Add(-time.Hour),
		Events: []TimingEvent{{Stage: "landed", At: t0.Add(-time.Hour)}}}
	f.timings.update("e1", func(t *CardTiming) { t.Thread = "" })
	f.checkReplies(t0)
	if n := gets(m); n != 0 {
		t.Errorf("thread reads = %d", n)
	}
	f, m = replyCard(t, nil)
	f.cfg.DryRun = true
	f.checkReplies(t0)
	f.cfg.DryRun = false
	f.Mail = nil
	f.checkReplies(t0)
	if n := gets(m); n != 0 {
		t.Errorf("thread reads = %d", n)
	}
	// a failed read is tried again later, a few times
	f, m = replyCard(t, nil)
	m.errs["get_thread"] = errors.New("down")
	for i := range 5 {
		f.checkReplies(t0.Add(time.Duration(i) * 10 * time.Minute))
	}
	if n := gets(m); n != replyTries {
		t.Errorf("thread reads = %d", n)
	}
}

func TestFactoryEmailsAreRecorded(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "zach", labelNote: "stuck", labelStuckFrom: "building"})
	f, p, _ := mailFactory(t, c)
	p.logs["c1"] = "What now?"
	f.Tick()
	ct := timing(t, f, "c1")
	if len(ct.Mails) != 1 || ct.Mails[0].ID != "nm1" || !ct.Mails[0].At.Equal(t0) {
		t.Errorf("mails = %+v", ct.Mails)
	}
	f.confirmAnswer(f.agent("c1"), "m2", "Approved: landing now.")
	if ct := timing(t, f, "c1"); len(ct.Mails) != 2 {
		t.Errorf("confirmation not recorded: %+v", ct.Mails)
	}
}

func TestMetrics(t *testing.T) {
	now := t0
	done := func(ago time.Duration) TimingEvent { return TimingEvent{Stage: "landed", At: now.Add(-ago)} }
	card := func(source, tier string, e2e, zach time.Duration, at time.Duration) CardTiming {
		end := now.Add(-at)
		start := end.Add(-e2e)
		ev := []TimingEvent{{Stage: "plan", At: start}}
		if zach > 0 {
			ev = append(ev, TimingEvent{Stage: "zach", At: start.Add(time.Minute)}, TimingEvent{Stage: "building", At: start.Add(time.Minute + zach)})
		}
		ev = append(ev, done(at))
		return CardTiming{Source: source, Tier: tier, RequestedAt: start, LandedAt: end, Events: ev}
	}
	all := map[string]CardTiming{}
	for i := 1; i <= 10; i++ {
		all["e"+string(rune('0'+i-1))] = card("email", "low", time.Duration(i)*time.Hour, time.Duration(i)*time.Minute, time.Hour)
	}
	all["b1"] = card("board", "risky", 30*time.Minute, 0, 2*time.Hour)
	all["old"] = card("board", "risky", time.Hour, 0, 8*24*time.Hour) // outside the window
	dropped := card("board", "low", time.Hour, 0, time.Hour)
	dropped.LandedAt = time.Time{}
	all["drop"] = dropped
	partial := card("email", "low", time.Hour, 0, time.Hour)
	partial.Partial = true
	all["part"] = partial
	replied := card("voice", "", time.Hour, 0, time.Hour)
	replied.RepliedAt = replied.LandedAt.Add(10 * time.Minute)
	all["v1"] = replied

	m := metrics(all, now, 7)
	if m.Cards != 12 || m.Excluded.NotLanded != 1 || m.Excluded.Partial != 1 {
		t.Fatalf("counts = %d %+v", m.Cards, m.Excluded)
	}
	byKey := map[string]MetricGroup{}
	for _, g := range m.BySource {
		byKey[g.Source] = g
	}
	if g := byKey["email"]; g.N != 10 || g.EndToEnd.Median != 5*3600 || g.EndToEnd.P90 != 9*3600 ||
		g.ZachWait.Median != 300 || g.ZachWait.P90 != 540 {
		t.Errorf("email = %+v", g)
	}
	if g := byKey["voice"]; g.EndToEnd.Median != 70*60 {
		t.Errorf("voice (to the reply) = %+v", g)
	}
	if len(m.ByTier) != 3 || m.ByTier[0].Tier != "low" || m.ByTier[1].Tier != "none" {
		t.Errorf("by tier = %+v", m.ByTier)
	}
	if len(m.BySourceTier) != 3 {
		t.Errorf("by source and tier = %+v", m.BySourceTier)
	}
	if q := quantiles([]int{4, 1, 3, 2}); q.Median != 2 || q.P90 != 4 {
		t.Errorf("quantiles = %+v", q)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f, _ := newTestFactory(t)
	f.timings.cards["c1"] = &CardTiming{Source: "board", Tier: "low", RequestedAt: t0.Add(-10 * 24 * time.Hour),
		LandedAt: t0.Add(-9 * 24 * time.Hour), Events: []TimingEvent{{Stage: "landed", At: t0.Add(-9 * 24 * time.Hour)}}}
	h := f.Handler()
	for _, tc := range []struct {
		q          string
		code, days int
		cards      int
	}{{"", 200, 7, 0}, {"?days=10", 200, 10, 1}, {"?days=99", 200, 30, 1}, {"?days=x", 400, 0, 0}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics"+tc.q, nil))
		if rec.Code != tc.code {
			t.Errorf("%s: code %d", tc.q, rec.Code)
			continue
		}
		if tc.code != 200 {
			continue
		}
		var m Metrics
		json.Unmarshal(rec.Body.Bytes(), &m)
		if m.Days != tc.days || m.Cards != tc.cards {
			t.Errorf("%s: %+v", tc.q, m)
		}
	}
}
