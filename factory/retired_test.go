package factory

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// retiredRunning is a reviewer retired (applied, verdict superseded) 20 minutes ago while its run
// record still says running, with no activity for half an hour.
func retiredRunning(id, of string) Agent {
	r := reviewer(id, of, "code")
	r.Labels[labelPrefix+labelApplied] = "1"
	r.Labels[labelPrefix+labelVerdict] = "superseded"
	r.Labels[labelPrefix+labelRetired] = iso(t0.Add(-20 * time.Minute))
	r.CreatedAt = t0.Add(-time.Hour)
	r.LastUserMessageAt = r.CreatedAt.Add(600 * time.Millisecond) // its prompt
	r.Status = "running"
	r.LastActivityAt = t0.Add(-30 * time.Minute)
	return r
}

// legacyRetired is retiredRunning as an older factory left it: no factory.retired marker.
func legacyRetired(id, of string) Agent {
	r := retiredRunning(id, of)
	delete(r.Labels, labelPrefix+labelRetired)
	return r
}

// storeSession writes agent id's Claude session file with one user text line at each of userAt,
// each followed by an assistant reply.
func storeSession(t *testing.T, f *Factory, id string, userAt ...time.Time) {
	t.Helper()
	var lines []string
	for i, at := range userAt {
		ts := at.UTC().Format(time.RFC3339Nano)
		lines = append(lines,
			fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"message %d"}}`, ts, i),
			fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"text","text":"reply %d"}]}}`, ts, i))
	}
	writeFile(t, f.cfg.Home+"/.claude/projects/-test/"+testSession(id)+".jsonl", strings.Join(lines, "\n")+"\n")
}

// refreshed is a with the labels Paseo was asked to set on it, as the next Refresh reads them.
func refreshed(a Agent, p *fakePaseo) Agent { return withLabels(a, p.labels(a.ID)) }

// forced counts `paseo archive <id> --force` calls.
func (p *fakePaseo) forced(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if len(c) == 3 && c[0] == "archive" && c[1] == id && c[2] == "--force" {
			n++
		}
	}
	return n
}

func TestRetiredReviewerWithStaleRunIsArchived(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "review"})
	r := reviewer("rev1", "card1", "code")
	r.Status = "running"
	f, p := newTestFactory(t, c, r)
	p.status["rev1"], p.stuck["rev1"] = "running", true // its process ended; the record still says running
	if err := f.retireReviews("card1"); err != nil {
		t.Fatal(err)
	}
	if !p.called("stop", "rev1") {
		t.Errorf("a retired reviewer is stopped first: %v", p.verbs("rev1"))
	}
	if !p.archived["rev1"] || p.forced("rev1") != 1 {
		t.Fatalf("a retired reviewer no stop settles is archived with --force: %v", p.verbs("rev1"))
	}
	if l := p.labels("rev1"); l[labelRetired] != iso(t0) || l[labelVerdict] != "superseded" {
		t.Errorf("retiring marks when, with the verdict: %v", l)
	}
	if v := p.verbs("card1"); len(v) != 0 {
		t.Errorf("retiring its reviewer leaves the card alone: %v", v)
	}
}

func TestPlainArchiveNeverForcesActiveWork(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "building"})
	f, p := newTestFactory(t, c)
	p.status["card1"], p.stuck["card1"] = "running", true
	if err := f.archiveAgent("card1"); err == nil {
		t.Fatal("an unsettled stop should fail the archive")
	}
	if p.forced("card1") != 0 || p.archived["card1"] {
		t.Errorf("only a retired reviewer is archived with --force: %v", p.verbs("card1"))
	}
}

func TestRetiredReviewerRetryBacksOffThenSucceeds(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "review"})
	r := reviewer("rev1", "card1", "code")
	r.Status = "running"
	f, p := newTestFactory(t, c, r)
	p.status["rev1"], p.stuck["rev1"], p.forceErr["rev1"] = "running", true, 1
	if err := f.retireReviewer(r, "superseded"); err != nil {
		t.Fatal(err)
	}
	if p.archived["rev1"] {
		t.Fatal("the fake should fail the first archive")
	}
	if l := p.labels("rev1"); l[labelRetired] != iso(t0) || l[labelRetireTries] != "1" || l[labelRetireAt] != iso(t0) {
		t.Fatalf("a failed archive is counted on the reviewer: %v", l)
	}
	f.SetAgents([]Agent{c, refreshed(r, p)})
	before := len(p.verbs("rev1"))

	f.Now = func() time.Time { return t0.Add(time.Minute) }
	f.reap(t0.Add(time.Minute))
	if n := len(p.verbs("rev1")); n != before {
		t.Fatalf("no retry before the backoff ends: %v", p.verbs("rev1")[before:])
	}

	f.Now = func() time.Time { return t0.Add(retireWaits[0]) }
	f.reap(t0.Add(retireWaits[0]))
	if !p.archived["rev1"] {
		t.Fatalf("the reap retries once the backoff ends: %v", p.verbs("rev1")[before:])
	}
	if !p.called("stop", "rev1") || p.forced("rev1") != 2 {
		t.Errorf("the retry stops, then forces: %v", p.verbs("rev1"))
	}
	if f.lastReap == nil || !slices.Contains(f.lastReap.Archived, short("rev1")) {
		t.Errorf("the reap reports the archive: %+v", f.lastReap)
	}
	if v := p.verbs("card1"); len(v) != 0 {
		t.Errorf("the card is left alone: %v", v)
	}
}

func TestRetiredReviewerRetryAfterRestart(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "zach"})
	r := retiredRunning("rev1", "card1")
	r.Labels[labelPrefix+labelRetireTries] = "2"
	r.Labels[labelPrefix+labelRetireAt] = iso(t0)
	f, p := newTestFactory(t, c, r)
	p.status["rev1"], p.stuck["rev1"] = "running", true

	f.Now = func() time.Time { return t0.Add(retireWaits[1] - time.Second) }
	f.reap(f.Now())
	if len(p.verbs("rev1")) != 0 {
		t.Fatalf("a restart keeps the stored backoff: %v", p.verbs("rev1"))
	}
	f.Now = func() time.Time { return t0.Add(retireWaits[1]) }
	f.reap(f.Now())
	if !p.archived["rev1"] {
		t.Fatalf("after a restart the reap retries when the stored backoff ends: %v", p.verbs("rev1"))
	}

	// a reviewer retired before any try was counted (or by an older factory) is tried at once
	old := retiredRunning("rev2", "card1")
	f2, p2 := newTestFactory(t, c, old)
	p2.status["rev2"], p2.stuck["rev2"] = "running", true
	f2.reap(t0)
	if !p2.archived["rev2"] {
		t.Fatalf("an uncounted retired reviewer is tried on the first reap: %v", p2.verbs("rev2"))
	}
}

func TestRetiredReviewerRetriesAreBounded(t *testing.T) {
	c := card("card1", map[string]string{labelStage: "landed"})
	c.ArchivedAt = t0.Add(-time.Hour)
	r := retiredRunning("rev1", "card1")
	f, p := newTestFactory(t, c, r)
	p.status["rev1"], p.stuck["rev1"], p.forceErr["rev1"] = "running", true, 1000

	var at []time.Time
	for now := t0; now.Before(t0.Add(48 * time.Hour)); now = now.Add(time.Minute) {
		f.Now = func() time.Time { return now }
		n := p.forced("rev1")
		f.reap(now)
		if p.forced("rev1") > n {
			at = append(at, now)
		}
	}
	if len(at) != retireMaxTries {
		t.Fatalf("tries = %d (%v), want %d", len(at), at, retireMaxTries)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < retireWaits[i-1] {
			t.Errorf("try %d came %s after the last, want at least %s", i+1, gap, retireWaits[i-1])
		}
	}
	if p.archived["rev1"] || p.called("delete", "rev1") {
		t.Error("a failed recovery never deletes the reviewer")
	}

	// the count survives a restart, and the board says it needs a hand
	f2, p2 := newTestFactory(t, c, refreshed(r, p))
	p2.status["rev1"], p2.stuck["rev1"] = "running", true
	f2.reap(t0.Add(72 * time.Hour))
	if len(p2.verbs("rev1")) != 0 {
		t.Errorf("no tries after the cap, even after a restart: %v", p2.verbs("rev1"))
	}
	got, _ := f2.card("card1")
	if !slices.Contains(got.Flags, retireStuckFlag) {
		t.Errorf("flags %v should include %q", got.Flags, retireStuckFlag)
	}
}

func TestRetiredReviewerRetriesPerReapAreCapped(t *testing.T) {
	agents := []Agent{card("card1", map[string]string{labelStage: "zach"})}
	for _, id := range []string{"rev1", "rev2", "rev3"} {
		agents = append(agents, retiredRunning(id, "card1"))
	}
	f, p := newTestFactory(t, agents...)
	for _, id := range []string{"rev1", "rev2", "rev3"} {
		p.status[id], p.stuck[id] = "running", true
	}
	f.reap(t0)
	n := 0
	for _, id := range []string{"rev1", "rev2", "rev3"} {
		if p.called("stop", id) {
			n++
		}
	}
	if n != retirePerReap {
		t.Errorf("retired reviewers tried in one reap = %d, want %d", n, retirePerReap)
	}
}

func TestReapNeverStopsCurrentReviewsOrActiveWork(t *testing.T) {
	busy := card("card1", map[string]string{labelStage: "building"})
	busy.Status = "running"
	current := reviewer("rev1", "card2", "code") // its verdict still counts
	current.Status, current.LastActivityAt = "running", t0.Add(-time.Hour)
	applied := reviewer("rev2", "card2", "code") // applied with a real verdict: not retired
	applied.Status = "running"
	applied.Labels[labelPrefix+labelApplied] = "1"
	applied.Labels[labelPrefix+labelVerdict] = "approve"
	unapplied := reviewer("rev3", "card2", "code") // a verdict label alone does not retire it
	unapplied.Status = "running"
	unapplied.Labels[labelPrefix+labelVerdict] = "superseded"
	land := reviewer("rev4", "card2", "land")
	land.Status = "running"
	f, p := newTestFactory(t, busy, card("card2", map[string]string{labelStage: "review"}), current, applied, unapplied, land)
	for _, id := range []string{"card1", "rev1", "rev2", "rev3", "rev4"} {
		p.status[id] = "running"
	}
	for now := t0; now.Before(t0.Add(24 * time.Hour)); now = now.Add(10 * time.Minute) {
		f.Now = func() time.Time { return now }
		f.reap(now)
	}
	for _, id := range []string{"card1", "card2", "rev1", "rev2", "rev3", "rev4"} {
		if p.called("stop", id) || p.called("archive", id) {
			t.Errorf("%s is current or active work: %v", id, p.verbs(id))
		}
	}
}

func TestRetiredReviewerCleanupLeavesDoneCardDone(t *testing.T) {
	gone := card("card1", map[string]string{labelStage: "landed"})
	gone.ArchivedAt = t0.Add(-time.Hour)
	fresh := card("card2", map[string]string{labelStage: "landed"}) // in its follow-up grace
	f, p := newTestFactory(t, gone, fresh, retiredRunning("rev1", "card1"), retiredRunning("rev2", "card2"))
	for _, id := range []string{"rev1", "rev2"} {
		p.status[id], p.stuck[id] = "running", true
	}
	f.reap(t0)
	if !p.archived["rev1"] || !p.archived["rev2"] {
		t.Fatalf("retired reviewers of Done cards are archived: %v %v", p.verbs("rev1"), p.verbs("rev2"))
	}
	for _, id := range []string{"card1", "card2"} {
		if v := p.verbs(id); len(v) != 0 {
			t.Errorf("%s is not touched: %v", id, v)
		}
		c, _ := f.card(id)
		if c.Stage != "landed" || c.Reviewing {
			t.Errorf("%s stays Done: %+v", id, c)
		}
	}
	if c, _ := f.card("card1"); c.ArchivedAt == "" {
		t.Error("an archived Done card stays archived")
	}
}

func TestBoardDoesNotCountRetiredReviews(t *testing.T) {
	given := retiredRunning("rev2", "card1") // given up on: retired with no verdict
	given.Labels[labelPrefix+labelVerdict] = ""
	f, _ := newTestFactory(t, card("card1", map[string]string{labelStage: "zach"}), retiredRunning("rev1", "card1"), given)
	c, _ := f.card("card1")
	if c.Reviewing {
		t.Error("a retired reviewer that still runs is not reviewing")
	}
	for _, fl := range c.Flags {
		if strings.HasPrefix(fl, "reviewer ") {
			t.Errorf("a retired reviewer does not block the card: %v", c.Flags)
		}
	}
	for _, k := range c.Children {
		if k.Blocked != "" {
			t.Errorf("child %s marked blocked: %q", k.Short, k.Blocked)
		}
	}
	agents := f.agents()
	if reviewPending(agents, "card1", t0) {
		t.Error("a retired review is not pending")
	}
	if r, ok := blockedReviewerOf(agents, "card1", t0); ok {
		t.Errorf("a retired reviewer is not replaced again: %s", r.ID)
	}

	// control: a current reviewer that runs or stalls still counts
	live := reviewer("rev3", "card1", "code")
	live.Status, live.LastActivityAt = "running", t0.Add(-30*time.Minute)
	f2, _ := newTestFactory(t, card("card1", map[string]string{labelStage: "review"}), live)
	c2, _ := f2.card("card1")
	if !c2.Reviewing || !slices.ContainsFunc(c2.Flags, func(s string) bool { return strings.HasPrefix(s, "reviewer stalled") }) {
		t.Errorf("a current stalled reviewer is reviewing and flagged: %+v", c2)
	}
	if _, ok := blockedReviewerOf(f2.agents(), "card1", t0); !ok {
		t.Error("a current stalled reviewer is still recovered")
	}
}

func TestRetiredReviewerInConversationIsNeverStopped(t *testing.T) {
	quick := retiredRunning("rev1", "card1") // messaged seconds after it was retired
	quick.LastUserMessageAt = parseTime(quick.L(labelRetired)).Add(10 * time.Second)
	unknown := retiredRunning("rev2", "card1") // no last-message time
	unknown.LastUserMessageAt = time.Time{}
	before := retiredRunning("rev3", "card1") // its last message came before it was retired: still eligible
	f, p := newTestFactory(t, card("card1", map[string]string{labelStage: "zach"}), quick, unknown)
	for _, id := range []string{"rev1", "rev2", "rev3"} {
		p.status[id], p.stuck[id] = "running", true
	}
	for now := t0; now.Before(t0.Add(24 * time.Hour)); now = now.Add(10 * time.Minute) {
		f.Now = func() time.Time { return now }
		f.reap(now)
	}
	for _, r := range []Agent{quick, unknown} {
		if err := f.archiveRetired(r); err == nil {
			t.Errorf("%s: a running conversation is not archived", r.ID)
		}
		if p.called("stop", r.ID) || p.forced(r.ID) != 0 || p.archived[r.ID] {
			t.Errorf("%s: a reviewer in a conversation is never stopped or forced: %v", r.ID, p.verbs(r.ID))
		}
	}
	if err := f.archiveRetired(before); err != nil || p.forced("rev3") != 1 {
		t.Errorf("a message before retirement does not protect it: %v %v", err, p.verbs("rev3"))
	}
}

func TestLegacyRetiredReviewerUsesItsSession(t *testing.T) {
	created := t0.Add(-time.Hour)
	prompt := created.Add(1800 * time.Millisecond) // the session line lands after Paseo records the send
	legacy := func(id string) Agent {
		r := legacyRetired(id, "card1")
		r.CreatedAt, r.LastUserMessageAt = created, created.Add(600*time.Millisecond)
		return r
	}
	onlyPrompt := legacy("rev1")
	quick := legacy("rev2") // a human follow-up seconds after the prompt
	quick.LastUserMessageAt = prompt.Add(4 * time.Second)
	pending := legacy("rev3") // a message Paseo sent that the session has not recorded yet
	pending.LastUserMessageAt = prompt.Add(time.Second)
	noSession := legacy("rev4") // retirement boundary unknown and no session to check
	f, p := newTestFactory(t, card("card1", map[string]string{labelStage: "landed"}), onlyPrompt, quick, pending, noSession)
	storeSession(t, f, "rev1", prompt)
	storeSession(t, f, "rev2", prompt, prompt.Add(4*time.Second))
	storeSession(t, f, "rev3", prompt)
	for _, id := range []string{"rev1", "rev2", "rev3", "rev4"} {
		p.status[id], p.stuck[id] = "running", true
	}
	f.reap(t0)
	f.reap(t0.Add(time.Hour))
	if !p.archived["rev1"] || p.forced("rev1") != 1 {
		t.Fatalf("an older retirement whose session holds only its prompt is cleaned up: %v", p.verbs("rev1"))
	}
	if v := p.verbs("rev1"); len(v) == 0 || v[0] != "update" || p.labels("rev1")[labelRetired] != iso(t0) {
		t.Errorf("its checked boundary is stored once, before the stop: %v %v", v, p.labels("rev1"))
	}
	for _, id := range []string{"rev2", "rev3", "rev4"} {
		if v := p.verbs(id); len(v) != 0 {
			t.Errorf("%s: an older retirement that may be a conversation is left alone: %v", id, v)
		}
	}
	for _, r := range []Agent{quick, pending, noSession} {
		f.archiveRetired(r)
		if p.called("stop", r.ID) || p.forced(r.ID) != 0 {
			t.Errorf("%s: retiring it never interrupts it: %v", r.ID, p.verbs(r.ID))
		}
	}
}
