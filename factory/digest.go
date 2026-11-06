package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// No daily digest is sent (tickDigest is not called); this file keeps the shipped ledger and the
// numbering of digests sent earlier. The digest as built: one email at digestHour in Config.Location, to Config.DigestTo,
// with what waits on Zach, what shipped today, what is in progress, what is stuck and which requests
// got no answer, one line per card with its board link. It is built from the board's Sections
// (sections.go) by the factory itself: no agent turn. digest.json in the state directory keeps the day
// last sent, written before the send, so a restart never sends a day twice; a factory that was down at
// digestHour still sends that evening. A day with nothing to report sends nothing. A failed send is
// tried again after mailRetryAfter; one that may have gone out is not. Dry and read-only runs never send.
// It is opt-in: with no DigestTo it is off and never falls back to ZachEmail. The operator sets
// DigestTo once the chosen delivery is ready (README: normal, or quiet with a recipient filter), and
// the first digest goes out that evening (catch-up).
// Shipped lists every finished card no digest has listed yet, whatever the day, so a card that lands
// after 6 PM, or whose landing is confirmed after a digest went out, is in the next one. No answer yet
// is the board's list as it stands (the dispatcher's requests not dismissed, unanswered.go), so a
// dismissed request leaves both. Each pass records the board's finished cards in digest.json's ledger (recordShipped) and a
// digest marks the lines it lists, so a card is listed once and never dropped: selection is by card,
// never by a time cutoff. The ledger keeps a line ledgerKeep, past the board's Done window, so a card
// still on the board is not recorded again; the board's Shipped today reads today's lines all day.
// The asked-often line is the exception: it reads the timing store, since a dropped card has no ledger
// line, and lists the cards asked manyAsksMin times or more that reached Done after the last digest's
// exact selection time (digest.json's asksCutoff, whenever that send happened), or today for the first.
// The digest opens with what is due, then a numbered list of the waiting asks one word answers (digest_reply.go), stored
// in digest.json with the thread the send returned, so a reply's numbers mean what that digest showed.

const digestHour = 18 // 6 PM

// manyAsksMin is how many asks put a finished card on the digest's asked-often line.
const manyAsksMin = 3

// digestSubject starts every digest's subject; a recipient-side mail filter and the email dispatcher,
// which sends replies to it to /api/answer, match on it (README).
const digestSubject = "Factory today: "

type digestFile struct {
	Sent    string       `json:"sent"`              // the local day (2006-01-02) of the last digest
	Shipped []ledgerLine `json:"shipped,omitempty"` // finished cards, kept after they leave the board
	// AsksCutoff is when the last digest selected its asked-often cards, not always digestHour: it can
	// send late, or retry.
	AsksCutoff time.Time `json:"asksCutoff,omitzero"`
	// Lists is what each sent digest numbered, kept digestListKeep for replies to it.
	Lists []digestList `json:"lists,omitempty"`
}

// ledgerLine is a finished card's line and whether a digest has listed it.
type ledgerLine struct {
	SectionLine
	Listed bool `json:"listed,omitempty"`
}

func (f *Factory) digestPath() string { return filepath.Join(f.cfg.StateDir, "digest.json") }

// readDigest is digest.json; a missing file is empty.
func (f *Factory) readDigest() (digestFile, error) {
	var d digestFile
	data, err := os.ReadFile(f.digestPath())
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(data, &d)
}

// digestState is the local day the last digest was sent for, "" for none.
func (f *Factory) digestState() (string, error) {
	d, err := f.readDigest()
	return d.Sent, err
}

func (f *Factory) writeDigest(d digestFile) error {
	data, _ := json.Marshal(d)
	tmp := f.digestPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.digestPath())
}

// recordShipped adds the board's finished cards to the ledger in digest.json, drops lines older
// than ledgerKeep, and drops digest lists older than digestListKeep (no digest is sent now, so
// nothing else prunes them).
func (f *Factory) recordShipped() {
	if f.passive() {
		return
	}
	now := f.Now()
	f.digestMu.Lock()
	defer f.digestMu.Unlock()
	d, err := f.readDigest()
	if err != nil {
		f.logf("daily digest: reading %s: %v", filepath.Base(f.digestPath()), err)
		return
	}
	keep := func(l ledgerLine) bool { return !parseTime(l.At).Before(now.Add(-ledgerKeep)) }
	merged, added := recordLines(d.Shipped, Finished(f.boardCards()))
	kept := filterLedger(merged, keep)
	d.Shipped = kept
	lists := keptLists(d.Lists, now)
	pruned := len(lists) != len(d.Lists)
	d.Lists = lists
	if !added && !pruned && len(kept) == len(merged) {
		return // nothing new: no write
	}
	if err := f.writeDigest(d); err != nil {
		f.logf("daily digest: %v", err)
	}
}

// ledgerKeep is how long the ledger keeps a line: longer than any card stays on the board as Done,
// and the most a failing digest catches up on.
const ledgerKeep = 7 * 24 * time.Hour

// recordLines is ledger with the lines of board it lacks, unlisted. A card that finished again later
// (a new Done stay) replaces its line, unlisted. It reports whether it changed anything.
func recordLines(ledger []ledgerLine, board []SectionLine) ([]ledgerLine, bool) {
	out := append([]ledgerLine{}, ledger...)
	at := map[string]int{}
	for i, l := range out {
		at[l.ID] = i
	}
	changed := false
	for _, l := range board {
		i, ok := at[l.ID]
		switch {
		case !ok:
			at[l.ID] = len(out)
			out = append(out, ledgerLine{SectionLine: l})
		case parseTime(l.At).After(parseTime(out[i].At)):
			out[i] = ledgerLine{SectionLine: l}
		default:
			continue
		}
		changed = true
	}
	return out, changed
}

func filterLedger(ls []ledgerLine, keep func(ledgerLine) bool) []ledgerLine {
	var out []ledgerLine
	for _, l := range ls {
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

// lines is the section lines of ls that keep accepts, newest first.
func lines(ls []ledgerLine, keep func(ledgerLine) bool) []SectionLine {
	var out []SectionLine
	for _, l := range ls {
		if keep(l) {
			out = append(out, l.SectionLine)
		}
	}
	sortNewest(out)
	return out
}

// finishedToday is the ledger's lines that finished on now's local day, for the board.
func (f *Factory) finishedToday(now time.Time) []SectionLine {
	d, err := f.readDigest()
	if err != nil {
		return nil
	}
	return lines(d.Shipped, func(l ledgerLine) bool { return sameDay(parseTime(l.At), now, f.cfg.Location) })
}

// digestSections is the board's Sections at now with Shipped replaced by the finished cards no digest
// has listed (in the ledger d or on the board), and No answer yet the board's own. The first digest
// ever lists only the day's. A card that is on neither when this reads them is left unlisted for the
// next digest.
func (f *Factory) digestSections(d digestFile, now time.Time) Sections {
	f.mu.Lock()
	cards, srcs := f.cards, append([]NoAnswerSource{}, f.noAnswer...)
	f.mu.Unlock()
	s := BuildSections(cards, now, f.cfg.Location)
	due := func(l ledgerLine) bool {
		return !l.Listed && (d.Sent != "" || sameDay(parseTime(l.At), now, f.cfg.Location))
	}
	ledShipped, _ := recordLines(d.Shipped, Finished(cards))
	s.Shipped = lines(ledShipped, due)
	for _, src := range srcs {
		s.NoAnswer = append(s.NoAnswer, src(now)...)
	}
	after := d.AsksCutoff
	if after.IsZero() {
		y, m, day := now.In(f.cfg.Location).Date()
		after = time.Date(y, m, day, 0, 0, 0, 0, f.cfg.Location)
	}
	s.ManyAsks = f.timings.manyAsks(after, now)
	f.numberDecisions(&s)
	return s
}

// commitDigest records, before the send, that day's digest went out listing s, selected at selected:
// its lines are marked listed (added to the ledger if a pass had not recorded them yet) and list, the
// numbers of a digest with something to send, is stored. The first digest ever also marks the ledger's
// earlier lines it left out, so no later digest lists them. It returns the undo for a send that surely
// failed, which unmarks them, removes the list and restores the cutoff for the retry.
func (f *Factory) commitDigest(day string, s Sections, selected time.Time, list *digestList) (undo func() error, err error) {
	f.digestMu.Lock()
	defer f.digestMu.Unlock()
	d, err := f.readDigest()
	if err != nil {
		return nil, err
	}
	prevSent, prevCutoff := d.Sent, d.AsksCutoff
	if prevSent == "" {
		s.Shipped = append(append([]SectionLine{}, s.Shipped...), unlisted(d.Shipped)...)
	}
	d.Sent, d.AsksCutoff = day, selected
	d.Shipped = markListed(d.Shipped, s.Shipped, true)
	d.Lists = keptLists(d.Lists, selected)
	if list != nil {
		d.Lists = append(d.Lists, *list)
	}
	if err := f.writeDigest(d); err != nil {
		return nil, err
	}
	return func() error {
		f.digestMu.Lock()
		defer f.digestMu.Unlock()
		cur, err := f.readDigest()
		if err != nil {
			return err
		}
		cur.Sent, cur.AsksCutoff = prevSent, prevCutoff
		cur.Shipped = markListed(cur.Shipped, s.Shipped, false)
		cur.Lists = slices.DeleteFunc(cur.Lists, func(l digestList) bool { return l.Day == day })
		return f.writeDigest(cur)
	}, nil
}

// unlisted is the lines of ledger no digest has listed.
func unlisted(ledger []ledgerLine) []SectionLine {
	return lines(ledger, func(l ledgerLine) bool { return !l.Listed })
}

// markListed is ledger with the cards of ls marked listed (or not); a card of ls the ledger lacks is
// added. Lines with no card are not recorded.
func markListed(ledger []ledgerLine, ls []SectionLine, listed bool) []ledgerLine {
	var cards []SectionLine
	for _, l := range ls {
		if l.ID != "" {
			cards = append(cards, l)
		}
	}
	out, _ := recordLines(ledger, cards)
	ids := map[string]bool{}
	for _, l := range cards {
		ids[l.ID] = true
	}
	for i := range out {
		if ids[out[i].ID] {
			out[i].Listed = listed
		}
	}
	return out
}

// mergeLines is a plus the lines of b whose card a lacks, newest first.
func mergeLines(a, b []SectionLine) []SectionLine {
	have := map[string]bool{}
	for _, l := range a {
		have[l.ID] = true
	}
	out := append([]SectionLine{}, a...)
	for _, l := range b {
		if !have[l.ID] {
			out = append(out, l)
			have[l.ID] = true
		}
	}
	sortNewest(out)
	return out
}

// setDigestStatus records the digest's last state for /api/health.
func (f *Factory) setDigestStatus(s string) {
	f.mu.Lock()
	f.digestStatus = s
	f.mu.Unlock()
}

// tickDigest is not called: no digest is sent. It stays until the mail machinery is deleted;
// the shipped ledger in this file (recordShipped) is live and feeds the board.
func (f *Factory) tickDigest() {
	to := f.cfg.DigestTo
	if to == "" {
		f.setDigestStatus("off: no FACTORY_DIGEST_TO")
		return
	}
	if f.passive() || f.Mail == nil {
		return
	}
	now := f.Now().In(f.cfg.Location)
	if now.Hour() < digestHour {
		return
	}
	day := now.Format("2006-01-02")
	d, err := f.readDigest()
	if err != nil {
		f.logf("daily digest: reading %s: %v", filepath.Base(f.digestPath()), err)
		return
	}
	f.mu.Lock()
	retry := f.digestRetry
	f.mu.Unlock()
	if d.Sent == day || now.Before(retry) {
		return
	}
	s := f.digestSections(d, now)
	e, ok := f.digestEmail(s)
	// recorded first: one digest a day even if the factory stops mid-send
	var list *digestList
	if ok {
		list = &digestList{Day: day, At: now, Items: s.Decisions}
	}
	undo, err := f.commitDigest(day, s, now, list)
	if err != nil {
		f.logf("daily digest: %v", err)
		return
	}
	if !ok {
		f.setDigestStatus("nothing to report " + day)
		f.logf("daily digest: nothing to report today")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
	defer cancel()
	out, err := f.sendMail(ctx, "", "send_email", map[string]any{"to": []string{to}, "subject": e.Subject, "text": e.Text, "html": e.HTML})
	switch {
	case errors.Is(err, ErrMailMaybeSent):
		f.setDigestStatus("possibly sent " + day + " (not resent)")
		f.logf("daily digest possibly sent: %v", err)
	case err != nil:
		f.mu.Lock()
		f.digestRetry = f.Now().Add(mailRetryAfter)
		f.mu.Unlock()
		f.setDigestStatus("send failed " + day + ", retrying")
		f.logf("daily digest not sent: %v", errors.Join(err, undo()))
	default:
		f.setDigestStatus("sent " + day)
		f.logf("daily digest sent: %s", e.Subject)
		if err := f.bindDigestThread(day, out); err != nil {
			f.logf("daily digest: replies to it will not be recognised: %v", err)
		}
	}
}

// digestEmail is the digest of s, and false when there is nothing to report.
func (f *Factory) digestEmail(s Sections) (email, bool) {
	type part struct {
		head, count string
		lines       []SectionLine
		clock       bool // show when it shipped
	}
	loc := f.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	// what has a deadline leads, and is not listed again under Waiting on you
	var due, waiting []SectionLine
	for _, l := range s.ForYou {
		if at := parseTime(l.Due); !at.IsZero() {
			l.State = dueText(at, f.Now(), loc) + " · " + l.State
			due = append(due, l)
			continue
		}
		waiting = append(waiting, l)
	}
	parts := []part{
		{"Due", "due", due, false},
		{"Waiting on you", "for you", waiting, false},
		{"Shipped", "shipped", s.Shipped, true},
		{"In progress", "in progress", s.Working, false},
		{"Stuck", "stuck", s.Stuck, false},
		{"No answer yet", "no answer", s.NoAnswer, false},
	}
	var counts []string
	for _, p := range parts {
		n := len(p.lines)
		if p.count == "for you" {
			n += len(s.Decisions)
		}
		if n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, p.count))
		}
	}
	if n := len(s.ManyAsks); n > 0 {
		counts = append(counts, fmt.Sprintf("%d asked %d+ times", n, manyAsksMin))
	}
	if len(counts) == 0 {
		return email{}, false
	}
	e := html.EscapeString
	var t, h strings.Builder
	h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;max-width:640px">`)
	for _, p := range parts {
		if p.count == "for you" { // the numbered asks follow what is due
			f.writeDecisions(&t, &h, s.Decisions)
		}
		if len(p.lines) == 0 {
			continue
		}
		fmt.Fprintf(&t, "%s\n", p.head)
		fmt.Fprintf(&h, `<h3 style="margin:16px 0 4px;font-size:15px">%s</h3><ul style="margin:0;padding-left:20px">`, e(p.head))
		for _, l := range p.lines {
			title, state := plainText(l.Title), plainText(l.State)
			if title == "" {
				title = "(untitled)"
			}
			if p.clock && l.At != "" {
				if at := parseTime(l.At); !at.IsZero() {
					state = at.In(loc).Format("3:04 PM")
					if !sameDay(at, f.Now(), loc) {
						state = at.In(loc).Format("Mon 3:04 PM")
					}
				}
			}
			link := ""
			if l.ID != "" {
				link = f.cardLink(l.ID)
			}
			line := "- " + title
			if state != "" && !strings.EqualFold(state, title) {
				line += ": " + state
			}
			if link != "" {
				line += " " + link
			}
			t.WriteString(line + "\n")
			item := e(title)
			if link != "" {
				item = `<a href="` + e(link) + `" style="color:#0969da">` + item + `</a>`
			}
			if state != "" && !strings.EqualFold(state, title) {
				item += `<span style="color:#59636e"> · ` + e(state) + `</span>`
			}
			h.WriteString("<li>" + item + "</li>")
		}
		t.WriteString("\n")
		h.WriteString("</ul>")
	}
	if len(s.ManyAsks) > 0 {
		cards := make([]string, len(s.ManyAsks))
		for i, c := range s.ManyAsks {
			title := plainText(c.Title)
			if title == "" {
				title = "(untitled)"
			}
			cards[i] = fmt.Sprintf("%s (%d)", title, c.Asks)
		}
		line := fmt.Sprintf("Asked you %d or more times: %s", manyAsksMin, strings.Join(cards, ", "))
		t.WriteString(line + "\n\n")
		h.WriteString(`<p>` + e(line) + `</p>`)
	}
	if n := len(s.Routine); n > 0 {
		fmt.Fprintf(&t, "Also %d routine runs today.\n", n)
		fmt.Fprintf(&h, `<p style="color:#59636e">Also %d routine runs today.</p>`, n)
	}
	if board := strings.TrimRight(f.cfg.BoardURL, "#"); board != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", board)
		fmt.Fprintf(&h, `<p><a href="%s" style="display:inline-block;background:#0969da;color:#ffffff;text-decoration:none;`+
			`font-weight:600;padding:10px 18px;border-radius:6px">Open the board</a></p>`, e(board))
	}
	h.WriteString("</div>")
	return email{Subject: digestSubject + strings.Join(counts, ", "), Text: t.String(), HTML: h.String()}, true
}
