package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The daily digest: one email at digestHour in Config.Location, to Config.DigestTo,
// with what waits on Zach, what shipped today, what is in progress, what is stuck and which requests
// got no answer, one line per card with its board link. It is built from the board's Sections
// (sections.go) by the factory itself: no agent turn. digest.json in the state directory keeps the day
// last sent, written before the send, so a restart never sends a day twice; a factory that was down at
// digestHour still sends that evening. A day with nothing to report sends nothing. A failed send is
// tried again after mailRetryAfter; one that may have gone out is not. Dry and read-only runs never send.
// It is opt-in: with no DigestTo it is off and never falls back to ZachEmail. The operator sets
// DigestTo once the chosen delivery is ready (README: normal, or quiet with a recipient filter), and
// the first digest goes out that evening (catch-up).
// Done cards leave the board after Config.Window, so each pass also records the day's shipped cards in
// digest.json (recordShipped); Sections lists them all day, and the digest has every one.

const digestHour = 18 // 6 PM

// digestSubject starts every digest's subject; a recipient-side mail filter matches on it (README).
const digestSubject = "Factory today: "

type digestFile struct {
	Sent    string        `json:"sent"`              // the local day (2006-01-02) of the last digest
	Day     string        `json:"day,omitempty"`     // the local day Shipped is for
	Shipped []SectionLine `json:"shipped,omitempty"` // that day's shipped cards, kept after they leave the board
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

func (f *Factory) setDigestState(day string) error {
	f.digestMu.Lock()
	defer f.digestMu.Unlock()
	d, err := f.readDigest()
	if err != nil {
		return err
	}
	d.Sent = day
	return f.writeDigest(d)
}

func (f *Factory) writeDigest(d digestFile) error {
	data, _ := json.Marshal(d)
	tmp := f.digestPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.digestPath())
}

// recordShipped adds today's shipped cards on the board to the day's ledger in digest.json.
func (f *Factory) recordShipped() {
	if f.passive() {
		return
	}
	day := f.Now().In(f.cfg.Location).Format("2006-01-02")
	shipped := BuildSections(f.boardCards(), f.Now(), f.cfg.Location).Shipped
	f.digestMu.Lock()
	defer f.digestMu.Unlock()
	d, err := f.readDigest()
	if err != nil {
		f.logf("daily digest: reading %s: %v", filepath.Base(f.digestPath()), err)
		return
	}
	changed := d.Day != day
	if changed {
		d.Day, d.Shipped = day, nil
	}
	n := len(d.Shipped)
	d.Shipped = mergeLines(d.Shipped, shipped)
	if !changed && len(d.Shipped) == n {
		return // nothing new: no write
	}
	if err := f.writeDigest(d); err != nil {
		f.logf("daily digest: %v", err)
	}
}

// shippedToday is the day's ledger of shipped cards (recordShipped), or nil.
func (f *Factory) shippedToday(now time.Time) []SectionLine {
	d, err := f.readDigest()
	if err != nil || d.Day != now.In(f.cfg.Location).Format("2006-01-02") {
		return nil
	}
	return d.Shipped
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

// tickDigest sends today's digest once it is digestHour, unless it was sent already.
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
	prev, err := f.digestState()
	if err != nil {
		f.logf("daily digest: reading %s: %v", filepath.Base(f.digestPath()), err)
		return
	}
	f.mu.Lock()
	retry := f.digestRetry
	f.mu.Unlock()
	if prev == day || now.Before(retry) {
		return
	}
	e, ok := f.digestEmail(f.sections())
	// recorded first: one digest a day even if the factory stops mid-send
	if err := f.setDigestState(day); err != nil {
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
	_, err = f.sendMail(ctx, "", "send_email", map[string]any{"to": []string{to}, "subject": e.Subject, "text": e.Text, "html": e.HTML})
	switch {
	case errors.Is(err, ErrMailMaybeSent):
		f.setDigestStatus("possibly sent " + day + " (not resent)")
		f.logf("daily digest possibly sent: %v", err)
	case err != nil:
		f.mu.Lock()
		f.digestRetry = f.Now().Add(mailRetryAfter)
		f.mu.Unlock()
		f.setDigestStatus("send failed " + day + ", retrying")
		f.logf("daily digest not sent: %v", errors.Join(err, f.setDigestState(prev)))
	default:
		f.setDigestStatus("sent " + day)
		f.logf("daily digest sent: %s", e.Subject)
	}
}

// digestEmail is the digest of s, and false when there is nothing to report.
func (f *Factory) digestEmail(s Sections) (email, bool) {
	type part struct {
		head, count string
		lines       []SectionLine
		clock       bool // show when it shipped
	}
	parts := []part{
		{"Waiting on you", "for you", s.ForYou, false},
		{"Shipped today", "shipped", s.Shipped, true},
		{"In progress", "in progress", s.Working, false},
		{"Stuck", "stuck", s.Stuck, false},
		{"No answer yet", "no answer", s.NoAnswer, false},
	}
	var counts []string
	for _, p := range parts {
		if len(p.lines) > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", len(p.lines), p.count))
		}
	}
	if len(counts) == 0 {
		return email{}, false
	}
	loc := f.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	e := html.EscapeString
	var t, h strings.Builder
	h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;max-width:640px">`)
	for _, p := range parts {
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
