package factory

import (
	"strings"
	"testing"
	"time"
)

func TestParseDuplicate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Same work as the reaper card.\nDUPLICATE OF: abc1234\nVERDICT: close", "abc1234"},
		{"- **DUPLICATE OF:** `abc1234`", "abc1234"},
		{"DUPLICATE OF: <card id>\nDUPLICATE OF: def5678", "def5678"},
		{"duplicate of: abc1234", "abc1234"},
		{"This looks like a duplicate of abc1234.", ""},
		{"no line", ""},
	}
	for _, c := range cases {
		if got := ParseDuplicate(c.in); got != c.want {
			t.Errorf("ParseDuplicate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

const (
	dupID  = "dup1234aaaa"
	keepID = "keep123bbbb"
)

// dupFactory has a card at a close verdict whose plan reviewer names the kept card, and the kept
// card with the given stage and status.
func dupFactory(t *testing.T, stage, status string, keepLabels map[string]string) (*Factory, *fakePaseo) {
	t.Helper()
	dup := card(dupID, map[string]string{labelStage: "zach", labelVerdict: "close", labelNote: "reviewer suggests closing"})
	dup.Title = "Archive idle agents"
	rev := reviewer("rev1", dupID, "plan")
	l := map[string]string{labelStage: stage, labelSource: "board"}
	for k, v := range keepLabels {
		l[k] = v
	}
	keep := card(keepID, l)
	keep.Status = status
	f, p := newTestFactory(t, dup, rev, keep)
	p.logs["rev1"] = "The kept card covers only builders and reviewers, not scouts or pickers.\n" +
		"DUPLICATE OF: keep123\nTIER: low\nVERDICT: close"
	return f, p
}

func closeDup(t *testing.T, f *Factory) {
	t.Helper()
	if _, err := f.Act("close", Body{ID: dupID}); err != nil {
		t.Fatal(err)
	}
}

func TestCloseDuplicateSteersRunningKeptCard(t *testing.T) {
	f, p := dupFactory(t, "building", "running", nil)
	closeDup(t, f)
	if got := p.labels(dupID)[labelNote]; got != "closed by Zach: duplicate of keep123" {
		t.Errorf("closed card note = %q", got)
	}
	if !p.archived[dupID] {
		t.Error("the duplicate is still dropped")
	}
	if len(f.steers.all()) != 1 {
		t.Fatalf("steer not recorded: %v", f.steers.all())
	}
	f.tickSteers()
	f.tickSteers()
	msgs := p.sent(keepID)
	if len(msgs) != 1 {
		t.Fatalf("kept card got %d messages, want 1: %q", len(msgs), msgs)
	}
	msg := msgs[0]
	fus := followUpsOf(msg)
	if len(fus) != 1 || fus[0].From != "dup1234" || fus[0].Text != "Archive idle agents" {
		t.Fatalf("follow-up block = %+v\n%s", fus, msg)
	}
	if strings.Contains(fus[0].Text, "scouts") {
		t.Error("the reviewer's notes are inside the block shown as Zach's words")
	}
	if !strings.Contains(msg, "an agent's notes, not Zach's words):\nThe kept card covers only builders") ||
		strings.Contains(msg, "VERDICT: close") {
		t.Errorf("reviewer notes missing or carry the trailer:\n%s", msg)
	}
	if len(f.steers.all()) != 0 {
		t.Error("a delivered steer is cleared")
	}
	if got := p.labels(keepID); got[labelAutoReview] != "" || got[labelStage] != "" {
		t.Errorf("a running kept card's labels are left alone: %v", got)
	}
}

func TestSteerToIdleKeptCardQueuesItsOwnReview(t *testing.T) {
	for _, c := range []struct{ stage, source, review string }{
		{"plan", "idea", "plan"},
		{"plan", "board", "plan"},
		{"building", "board", "code"},
	} {
		f, p := dupFactory(t, c.stage, "idle", map[string]string{labelSource: c.source})
		closeDup(t, f)
		f.tickSteers()
		got := p.labels(keepID)
		if len(p.sent(keepID)) != 1 || got[labelStage] != c.stage || got[labelAutoReview] != c.review || got[labelSince] == "" {
			t.Errorf("%s/%s: sent %d, labels %v", c.stage, c.source, len(p.sent(keepID)), got)
		}
	}
}

func TestSteerWaitsWhileKeptCardIsReviewedOrWaitsOnZach(t *testing.T) {
	for _, l := range []map[string]string{
		{labelStage: "plan-review"}, {labelStage: "review"}, {labelStage: "zach"},
		{labelStage: "plan", labelRouting: "1"}, // still picking its repo
	} {
		f, p := dupFactory(t, l[labelStage], "idle", l)
		closeDup(t, f)
		f.tickSteers()
		if len(p.sent(keepID)) != 0 || len(f.steers.all()) != 1 {
			t.Errorf("%v: sent %q, steers %d", l, p.sent(keepID), len(f.steers.all()))
		}
		keep := f.agent(keepID)
		keep.Labels = map[string]string{labelPrefix + labelStage: "building"}
		f.SetAgents([]Agent{keep})
		f.tickSteers()
		if len(p.sent(keepID)) != 1 || len(f.steers.all()) != 0 {
			t.Errorf("%v then building: sent %d, steers %d", l, len(p.sent(keepID)), len(f.steers.all()))
		}
	}
}

func TestSteerToFinishedKeptCardBecomesAnIdea(t *testing.T) {
	for _, stage := range []string{"landing", "landed", "gone"} {
		f, p := dupFactory(t, stage, "idle", nil)
		closeDup(t, f)
		if stage == "gone" {
			keep := f.agent(keepID)
			keep.ArchivedAt = t0
			f.SetAgents([]Agent{keep})
		}
		f.tickSteers()
		if len(p.sent(keepID)) != 0 {
			t.Errorf("%s: a finished kept card gets no message", stage)
		}
		ideas := f.ideas.List()
		if len(ideas) != 1 {
			t.Fatalf("%s: ideas = %+v", stage, ideas)
		}
		i := ideas[0]
		if i.Title != "From closed duplicate dup1234: Archive idle agents" || i.By != "factory (closed duplicate dup1234)" ||
			!strings.Contains(i.Request, "not Zach's words") || !strings.Contains(i.Why, "keep123") || i.Repo != "example/desk" {
			t.Errorf("%s: idea = %+v", stage, i)
		}
		if len(f.steers.all()) != 0 {
			t.Errorf("%s: steer kept after filing the idea", stage)
		}
	}
}

func TestCloseWithoutDuplicateVerdictDoesNotSteer(t *testing.T) {
	// an older plan reviewer said duplicate, Zach kept going, and now drops it at a code review
	f, p := dupFactory(t, "building", "running", nil)
	dup := f.agent(dupID)
	dup.Labels[labelPrefix+labelVerdict] = "code changes"
	f.SetAgents([]Agent{dup, f.agent("rev1"), f.agent(keepID)})
	closeDup(t, f)
	if len(f.steers.all()) != 0 || p.labels(dupID)[labelNote] != "closed by Zach" {
		t.Errorf("Drop it on a non-close ask steered: %v %v", f.steers.all(), p.labels(dupID))
	}

	for name, text := range map[string]string{
		"no line":      "Not needed.\nVERDICT: close",
		"unknown card": "DUPLICATE OF: zzz9999\nVERDICT: close",
		"itself":       "DUPLICATE OF: dup1234\nVERDICT: close",
	} {
		f, p := dupFactory(t, "building", "running", nil)
		p.logs["rev1"] = text
		closeDup(t, f)
		if len(f.steers.all()) != 0 || p.labels(dupID)[labelNote] != "closed by Zach" || !p.archived[dupID] {
			t.Errorf("%s: steers %v, labels %v", name, f.steers.all(), p.labels(dupID))
		}
	}
}

func TestCloseUsesNewestReviewerOrTheCardsOwnLine(t *testing.T) {
	f, p := dupFactory(t, "building", "running", nil)
	old := reviewer("rev0", dupID, "plan")
	old.CreatedAt = t0.Add(-30 * time.Minute)
	f.SetAgents(append(f.agents(), old))
	p.logs["rev0"] = "DUPLICATE OF: zzz9999\nVERDICT: close"
	closeDup(t, f)
	if st := f.steers.all()[dupID]; st.Keep != keepID {
		t.Errorf("newest reviewer's line wins: %+v", st)
	}

	f, p = dupFactory(t, "building", "running", nil)
	p.logs["rev1"] = "Same as the reaper card.\nVERDICT: close"
	p.logs[dupID] = "[Assistant] Card keep123 already does this.\nDUPLICATE OF: keep123"
	closeDup(t, f)
	if st := f.steers.all()[dupID]; st.Keep != keepID {
		t.Errorf("the Fellow's own line counts at a close verdict: %+v", st)
	}
}

func TestDuplicateFollowUpInReviewPromptAndPeers(t *testing.T) {
	msg := steerMsg(Steer{From: "dup1234", Title: "x", Request: CardRequest{Base: "Reap scouts too."},
		Notes: "agent text"}, t0)
	fus := followUpsOf(msg)
	if len(fus) != 1 || fus[0].Text != "Reap scouts too." || fus[0].From != "dup1234" {
		t.Fatalf("parsed %+v", fus)
	}
	if got := followUpsOf(dupPrefix + "dup1234 (" + t0.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "):\nno end line"); len(got) != 0 {
		t.Errorf("a header with no end line is no block: %+v", got)
	}
	c := Card{ID: "c1", Request: &CardRequest{Base: "Reap idle agents.", FollowUps: fus}}
	pr := ReviewPrompt(c, "plan", 7478, "unknown")
	if !strings.Contains(pr, "From closed duplicate dup1234 ("+fus[0].Date+"): adds to the request, does not override it:\nReap scouts too.") ||
		strings.Contains(pr, "agent text") {
		t.Errorf("review prompt:\n%s", pr)
	}
	line, list := c.Request.brief()
	if !strings.Contains(line, "from closed duplicate dup1234: Reap scouts too.") || len(list) != 1 {
		t.Errorf("peers brief = %q %q", line, list)
	}
}
