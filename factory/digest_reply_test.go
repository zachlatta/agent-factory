package factory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/agent-factory/factory/scan"
)

// digestWait is a card waiting for Zach to allow its build: Yes, No, Change and Drop, no questions.
func digestWait(id, title string, age time.Duration) Agent {
	a := planWait(id, map[string]string{labelSince: ago(age)})
	a.Title = title
	a.LastActivityAt = t0.Add(-age)
	return a
}

// sentDigest is a factory whose 6 PM digest went out in thread nt1, numbering agents newest first.
func sentDigest(t *testing.T, agents ...Agent) (*Factory, *fakePaseo, *fakeMail) {
	t.Helper()
	f, p, m := mailFactory(t, agents...)
	f.cfg.DigestTo = testZach
	loc, _ := time.LoadLocation("America/New_York")
	f.cfg.Location = loc
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, loc)
	f.Now = func() time.Time { return now }
	f.tickDigest()
	if len(digests(m)) != 1 {
		t.Fatalf("digests = %+v", digests(m))
	}
	return f, p, m
}

func digestReply(f *Factory, msg, text string) (map[string]string, error) {
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: msg, Text: text})
	if err != nil {
		return nil, err
	}
	return res.(map[string]string), nil
}

// replies are the mails sent after the digest went out.
func replyMails(m *fakeMail) []mailCall {
	var out []mailCall
	for _, s := range m.sends() {
		if s.Tool == "reply_to_message" {
			out = append(out, s)
		}
	}
	return out
}

func TestParseDigestReply(t *testing.T) {
	good := map[string]string{
		"yes 1 3, drop 2":        "1:yes 2:drop 3:yes",
		"approve 3 and 5":        "3:approve 5:approve",
		"1 yes, 2 drop":          "1:yes 2:drop",
		"1, 2 yes; 3 no.":        "1:yes 2:yes 3:no",
		"land 2":                 "2:land",
		"go ahead 1, drop it 2":  "1:go ahead 2:drop it",
		"YES 1\nDrop 2\n":        "1:yes 2:drop",
		"yes 1, yes 1":           "1:yes",
		"yes 1\n\nOn Mon wrote:": "1:yes",
		"yes 1\n> yes 2":         "1:yes",
		"yes 99":                 "99:yes",
	}
	for text, want := range good {
		got, ok := parseDigestReply(text)
		var s []string
		for _, a := range got {
			s = append(s, fmt.Sprintf("%d:%s", a.Number, a.Word))
		}
		if !ok || strings.Join(s, " ") != want {
			t.Errorf("%q = %v %v, want %s", text, s, ok, want)
		}
	}
	for _, text := range []string{"", "   ", "1 2", "approve all", "yes", "drop", "yes please 1",
		"please land the first one", "yes 1, drop 1", "yes 1, 2 drop", "1 yes 2", "yes 1.5", "1a", "done 1", "yes 12345",
		"> yes 1"} {
		if got, ok := parseDigestReply(text); ok {
			t.Errorf("%q parsed as %v", text, got)
		}
	}
}

// The numbered list comes first, newest first, one short line each; other asks stay unnumbered with
// their link, and no card is listed twice. The subject still counts every waiting card.

// A line stays short, and the title, which says which change it is, survives the cut.
func TestDigestLineKeepsTheTitleAndStaysShort(t *testing.T) {
	it := digestItem{Title: "Move the backup", Question: "Allow this build?"}
	if got := digestLine(it); got != "Move the backup: Allow this build?" {
		t.Errorf("line = %q", got)
	}
	it = digestItem{Title: strings.Repeat("title ", 30), Question: strings.Repeat("decision ", 30)}
	got := digestLine(it)
	if n := len([]rune(got)); n > digestLineCap {
		t.Errorf("line is %d runes: %q", n, got)
	}
	if !strings.HasPrefix(got, "title title") || !strings.Contains(got, ": decision decision") {
		t.Errorf("line = %q", got)
	}
}

// Asks a number must not answer stay unnumbered: questions (Yes would take the recommended options),
// a builder still writing, a board-only wait.

// A failed send removes the list with the rest of the commit; one that may have gone out binds no thread.

// A reply to a list older than the keep window is not recognised, even when no later digest
// pruned it: it falls through to the card path and acts on nothing.

// Lists older than the keep window go when a later digest is recorded.

// An unknown number and a word the card does not offer are refused; the rest of the reply still acts.

// Land keeps its gates: a secret scan hit stops it, and the reply says why.
func TestDigestReplyLandStillScans(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land", labelVerdict: "code approve",
		labelRoundsRequest: "1", labelReviewed: reviewedSHA, labelSince: ago(time.Hour)})
	a.Repo = "zachlatta/agent-factory"
	f, p, m := sentDigest(t, a)
	headIs(f, reviewedSHA, nil)
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		return []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, nil
	}
	if d, _ := f.readDigest(); len(d.Lists) != 1 || len(d.Lists[0].Items) != 1 {
		t.Fatalf("not numbered: %+v", d.Lists)
	}
	res, err := digestReply(f, "m1", "yes 1")
	if err != nil || res["refused"] != "1" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if l := p.labels("c1"); l[labelStage] == "landing" {
		t.Errorf("landed past a scan hit: %v", l)
	}
	r := replyMails(m)
	if len(r) != 1 || !strings.Contains(r[0].Args["text"].(string), "secret scan failed") {
		t.Errorf("replies = %+v", r)
	}
}

// A later digest with a different order never shifts what an earlier digest's numbers mean.

// Anything unparsed changes nothing, and says so once.

// A failed reply send is logged and not repeated; the answers stand.

// A thread that no digest holds still goes to the card path.

// Land works on a finished card: it goes through the card's own choice, and the reply says where it went.
func TestDigestReplyLandsAFinishedCard(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: "review passed: approve to land", labelVerdict: "code approve",
		labelRoundsRequest: "1", labelReviewed: reviewedSHA, labelSince: ago(time.Hour)})
	f, p, m := sentDigest(t, a)
	headIs(f, reviewedSHA, nil)
	res, err := digestReply(f, "m1", "land 1")
	if err != nil || res["done"] != "1" {
		t.Fatalf("res = %v, err = %v; replies %+v", res, err, replyMails(m))
	}
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelAnswered] != "m1" {
		t.Errorf("labels = %v", l)
	}
	if r := replyMails(m); len(r) != 1 || !strings.Contains(r[0].Args["text"].(string), "1. Land: card c1 (now shipping)") {
		t.Errorf("replies = %+v", r)
	}
}

// A dry run records nothing in digest.json, as it stores no thread for a card's answer.

// A card that moved on is refused: gone, past Zach, left and came back, replaced, or changed.

// Questions that appear after the digest, with the same line and choices, must not let a bare Yes take
// the recommended options: the numbering check runs again at reply time.

// An unknown number and a word the card does not offer are refused; the rest of the reply still acts.

// A later digest with a different order never shifts what an earlier digest's numbers mean.

// A failed reply send is logged and not repeated; the answers stand.

// Asks a number must not answer stay unnumbered: questions (Yes would take the recommended options),
// a builder still writing, a board-only wait.
func TestDigestLeavesAsksOutOfTheNumberedList(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	writing := digestWait("c1", "Still writing", time.Hour)
	writing.Labels[labelPrefix+labelWriting] = iso(now)
	running := digestWait("c2", "Still running", time.Hour)
	running.Status = "running"
	questions := digestWait("c3", "Has questions", time.Hour)
	f, p, _ := mailFactory(t, writing, running, questions)
	storeLog(t, f, p, "c3", "Brief.\n\nQUESTION: Which store?\n- Postgres (recommended)\n- SQLite\n")
	f.cfg.DigestTo, f.cfg.Location = testZach, time.UTC
	f.Now = func() time.Time { return now }
	f.tickDigest()
	d, _ := f.readDigest()
	if len(d.Lists) != 1 || len(d.Lists[0].Items) != 0 {
		t.Fatalf("lists = %+v", d.Lists)
	}
}

// A failed send removes the list with the rest of the commit; one that may have gone out binds no thread.
func TestDigestListFollowsTheSend(t *testing.T) {
	f, _, m := mailFactory(t, digestWait("c1", "A build", time.Hour))
	f.cfg.DigestTo, f.cfg.Location = testZach, time.UTC
	f.Now = func() time.Time { return time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC) }
	m.errs["send_email"] = fmt.Errorf("refused")
	f.tickDigest()
	if d, _ := f.readDigest(); len(d.Lists) != 0 {
		t.Errorf("a failed send kept its list: %+v", d.Lists)
	}
	m.errs["send_email"] = fmt.Errorf("send_email: %w", ErrMailMaybeSent)
	f.digestRetry = time.Time{}
	f.tickDigest()
	d, _ := f.readDigest()
	if len(d.Lists) != 1 || d.Lists[0].Thread != "" {
		t.Fatalf("lists = %+v", d.Lists)
	}
	if _, err := digestReply(f, "m1", "yes 1"); err == nil || !strings.Contains(err.Error(), ErrNoCard.Error()) {
		t.Errorf("a digest with no known thread must fall through: %v", err)
	}
}

// A reply to a list older than the keep window is not recognised, even when no later digest
// pruned it: it falls through to the card path and acts on nothing.
func TestDigestReplyRefusesAStaleList(t *testing.T) {
	f, p, m := sentDigest(t, digestWait("c1", "A build", time.Hour))
	then := f.Now()
	f.Now = func() time.Time { return then.Add(31 * 24 * time.Hour) }
	if _, err := digestReply(f, "m1", "yes 1"); err == nil || !strings.Contains(err.Error(), ErrNoCard.Error()) {
		t.Fatalf("a 31-day-old list must not be recognised: %v", err)
	}
	if l := p.labels("c1"); l[labelStage] == "building" || l[labelAnswered] != "" || f.agent("c1").L(labelStage) != "zach" {
		t.Errorf("the stale reply acted: %v (stage %q)", l, f.agent("c1").L(labelStage))
	}
	if r := replyMails(m); len(r) != 0 {
		t.Errorf("the stale reply was answered in its thread: %+v", r)
	}
}

// Lists older than the keep window go when a later digest is recorded.
func TestDigestListsAgeOut(t *testing.T) {
	f, _, _ := sentDigest(t, digestWait("c1", "A build", time.Hour))
	then := f.Now()
	f.Now = func() time.Time { return then.Add(31*24*time.Hour + 2*time.Hour) } // past 6 PM whatever the clock change
	f.tickDigest()
	d, _ := f.readDigest()
	if len(d.Lists) != 1 || d.Lists[0].Day == then.Format("2006-01-02") {
		t.Errorf("lists = %+v", d.Lists)
	}
}

// A card that moved on is refused: gone, past Zach, left and came back, replaced, or changed.

// Questions that appear after the digest, with the same line and choices, must not let a bare Yes take
// the recommended options: the numbering check runs again at reply time.

func TestDigestReplyRefusesAReplacedCard(t *testing.T) {
	old := digestWait("c1", "A build", time.Hour)
	f, _, m := sentDigest(t, old)
	next := digestWait("c2", "A build", time.Hour)
	next.Labels[labelPrefix+labelReplaces] = "c1"
	f.SetAgents([]Agent{old, next})
	res, err := digestReply(f, "m1", "yes 1")
	if err != nil || res["refused"] != "1" {
		t.Fatalf("res = %v, err = %v; mail %+v", res, err, replyMails(m))
	}
}

// An unknown number and a word the card does not offer are refused; the rest of the reply still acts.

// land is not a word a Yes ask offers.
func TestDigestReplyRefusesAWordTheCardDoesNotOffer(t *testing.T) {
	f, p, _ := sentDigest(t, digestWait("c1", "A build", time.Hour))
	res, err := digestReply(f, "m1", "land 1")
	if err != nil || res["refused"] != "1" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("sent = %v", p.sent("c1"))
	}
}

// A later digest with a different order never shifts what an earlier digest's numbers mean.

// A repeated POST, even after a restart, changes nothing and sends nothing.
func TestDigestReplyRepeatedDoesNothing(t *testing.T) {
	f, p, m := sentDigest(t, digestWait("c1", "A build", time.Hour), digestWait("c2", "Other build", 2*time.Hour))
	if _, err := digestReply(f, "m1", "yes 1"); err != nil {
		t.Fatal(err)
	}
	sent := len(p.sent("c1"))
	f.replies.done = nil // a restart forgets the in-memory marks
	res, err := digestReply(f, "m1", "yes 1, drop 2")
	if err != nil || res["action"] != "duplicate" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if len(p.sent("c1")) != sent || p.called("archive", "c2") {
		t.Errorf("the repeat acted: %v", p.labels("c2"))
	}
	if n := len(replyMails(m)); n != 1 {
		t.Errorf("replies = %d, want 1", n)
	}
}

// Anything unparsed changes nothing, and says so once.
func TestDigestReplyUnparsedChangesNothing(t *testing.T) {
	for _, text := range []string{"1 2", "approve all", "what is the first one about?", "", "yes 1, drop 1"} {
		t.Run(text, func(t *testing.T) {
			f, p, m := sentDigest(t, digestWait("c1", "A build", time.Hour), digestWait("c2", "Other", 2*time.Hour))
			before := fmt.Sprint(p.labels("c1"), p.labels("c2"))
			res, err := digestReply(f, "m1", text)
			if err != nil || res["action"] != "digest-unparsed" {
				t.Fatalf("res = %v, err = %v", res, err)
			}
			if after := fmt.Sprint(p.labels("c1"), p.labels("c2")); after != before || len(p.sent("c1"))+len(p.sent("c2")) != 0 {
				t.Errorf("changed: %s -> %s", before, after)
			}
			r := replyMails(m)
			if len(r) != 1 || !strings.Contains(r[0].Args["text"].(string), "Nothing changed") {
				t.Errorf("replies = %+v", r)
			}
		})
	}
}

// A failed reply send is logged and not repeated; the answers stand.

// A dry run records nothing in digest.json, as it stores no thread for a card's answer.
func TestDigestReplyInDryRunRecordsNothing(t *testing.T) {
	f, _, _ := sentDigest(t, digestWait("c1", "A build", time.Hour))
	f.cfg.DryRun = true
	if _, err := digestReply(f, "m1", "what is this?"); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.readDigest(); len(d.Lists[0].Handled) != 0 {
		t.Errorf("handled = %v", d.Lists[0].Handled)
	}
}

// A card that moved on is refused: gone, past Zach, left and came back, replaced, or changed.
func TestDigestReplyRefusesACardThatMovedOn(t *testing.T) {
	cases := map[string]func(f *Factory, a Agent) Agent{
		"past Zach": func(_ *Factory, a Agent) Agent { a.Labels[labelPrefix+labelStage] = "building"; return a },
		"came back": func(_ *Factory, a Agent) Agent { a.Labels[labelPrefix+labelSince] = ago(time.Minute); return a },
		"archived":  func(_ *Factory, a Agent) Agent { a.ArchivedAt = t0; return a },
		"writing": func(f *Factory, a Agent) Agent {
			a.Labels[labelPrefix+labelWriting] = iso(f.Now())
			return a
		},
		"different ask": func(_ *Factory, a Agent) Agent {
			a.Labels[labelPrefix+labelAwaiting] = ""
			a.Labels[labelPrefix+labelNote] = readyNote(1)
			return a
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, p, m := sentDigest(t, digestWait("c1", "A build", time.Hour))
			f.SetAgents([]Agent{change(f, f.agent("c1"))})
			res, err := digestReply(f, "m1", "yes 1")
			if err != nil || res["done"] != "0" || res["refused"] != "1" {
				t.Fatalf("res = %v, err = %v", res, err)
			}
			if len(p.sent("c1")) != 0 || p.labels("c1")[labelStage] == "building" {
				t.Errorf("a refused number acted: %v %v", p.sent("c1"), p.labels("c1"))
			}
			r := replyMails(m)
			if len(r) != 1 || !strings.Contains(r[0].Args["text"].(string), "Refused") {
				t.Errorf("replies = %+v", r)
			}
		})
	}
}

// Questions that appear after the digest, with the same line and choices, must not let a bare Yes take
// the recommended options: the numbering check runs again at reply time.
func TestDigestReplyRefusesAskThatGainedQuestions(t *testing.T) {
	f, p, _ := sentDigest(t, digestWait("c1", "A build", time.Hour))
	storeLog(t, f, p, "c1", "Brief.\n\nQUESTION: Which store?\n- Postgres (recommended)\n- SQLite\n")
	res, err := digestReply(f, "m1", "yes 1")
	if err != nil || res["refused"] != "1" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if p.labels("c1")[labelStage] == "building" {
		t.Errorf("Yes took the recommended options: %v", p.labels("c1"))
	}
}

// An unknown number and a word the card does not offer are refused; the rest of the reply still acts.

// A later digest with a different order never shifts what an earlier digest's numbers mean.

// A failed reply send is logged and not repeated; the answers stand.
