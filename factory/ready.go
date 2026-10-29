package factory

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Ready for you: the one stop where Zach sees finished work before it lands. The builder writes
// three CHANGED: lines and any before/after screenshots; the factory runs the live preview
// (tryout.go) and sends one email. Land lands it, Drop drops it, and anything else is a change
// request: the card goes back to building and code review, then to a new Ready for you.

// readyPrefix starts the Needs Zach note of a card at the Ready for you stop.
const readyPrefix = "ready for you"

// readyNote is the note of the n-th Ready for you of a card. Each pass has its own note, so it gets
// its own email (labelTold holds the note already emailed).
func readyNote(n int) string {
	if n <= 1 {
		return readyPrefix
	}
	return fmt.Sprintf("%s (try %d)", readyPrefix, n)
}

func isReadyNote(note string) bool { return strings.HasPrefix(note, readyPrefix) }

// olderFinishedNotes are the notes cards that waited before Ready for you still carry: they are shown
// and previewed as Ready for you.
var olderFinishedNotes = []string{"built and reviewed: try the preview", "review passed", "review found an architecture change"}

// isFinishedNote reports whether note is the finished-work stop, Ready for you, in any form.
func isFinishedNote(note string) bool {
	if isReadyNote(note) {
		return true
	}
	for _, p := range olderFinishedNotes {
		if strings.HasPrefix(note, p) {
			return true
		}
	}
	return false
}

// readyStop reports whether a waits on Zach at the Ready for you stop.
func readyStop(a Agent) bool { return a.L(labelStage) == "zach" && isFinishedNote(a.L(labelNote)) }

// readyMsg asks the builder of a card that passed code review, and comes to Zach for reason, for what he
// reads at Ready for you.
func readyMsg(reason string) string {
	return "Code review passed. This change now goes to Zach once, finished, as \"Ready for you\", before it lands (" +
		reason + "). Write it for reading on a phone, in plain words. Start with three lines:\n" +
		"DECISION: <land this change?, in one line>\n" +
		"OVERVIEW: <what changed and why, at most 3 short sentences; it is built and reviewed, nothing is live yet>\n" +
		"NEEDS FROM ZACH: <try it, then land it, ask for a change or drop it>\n" +
		"Then exactly 3 lines, each starting `CHANGED: ` with one plain sentence on what changed for him (no file names, " +
		"ids or jargon). If it changes something he sees (a page, an email, a message), take before (origin/main) and after " +
		"(your branch) screenshots at desktop and phone widths, on test or fake data, and name them with the evidence lines " +
		"the overview rule gives. Do not start a server, leave anything running or upload anything: the factory starts the " +
		"live preview this repo declares and links it for him. Then at most 3 bullets: what could go wrong. Then any " +
		"question he must answer, each as a QUESTION: block. Do not land and do not change the code; stop after this."
}

// readyTitle is the board title of a Ready for you ask.
const readyTitle = "Ready to try"

// readyAsk is the Ready for you ask; why (labelAskWhy) says why it comes to Zach, when known.
func readyAsk(why string) *Ask {
	w := "Built, tested and passed by code review; nothing is live yet."
	if why != "" {
		w += " It comes to you because " + why + "."
	}
	return &Ask{Title: readyTitle, From: "builder",
		Why:     w + " Try the preview, then land it, ask for a change, or drop it.",
		Actions: []Action{primary("approve", "Land"), reply("Change", "What should change?"), act("close", "Drop")},
		Ready:   &Ready{}}
}

// Ready is what a Ready for you ask shows besides the agent's reply: what changed and the preview.
type Ready struct {
	Changed []string `json:"changed,omitempty"` // the builder's CHANGED: lines, at most 3
	Preview string   `json:"preview,omitempty"` // the live preview link, once it loads
	State   string   `json:"state,omitempty"`   // preview state (tryout.go): starting, ready, failed, queued, none
	Log     string   `json:"log,omitempty"`     // the last lines of a failed preview's output
	Sample  string   `json:"sample,omitempty"`  // a no-UI repo's sample output
}

var changedLine = regexp.MustCompile(`(?m)^\s*(?:[-*]\s+)?\**CHANGED:\**\s*(.+?)\s*$`)

// ParseChanged returns the CHANGED: lines of an agent's reply, at most 3.
func ParseChanged(text string) []string {
	var out []string
	for _, m := range changedLine.FindAllStringSubmatch(text, -1) {
		if len(out) == 3 {
			break
		}
		out = append(out, m[1])
	}
	return out
}

// fillReady completes a Ready for you ask of card id from the agent's reply text and its preview.
func (f *Factory) fillReady(ask *Ask, id, text string) {
	if ask == nil || ask.Ready == nil {
		return
	}
	ask.Ready.Changed = ParseChanged(text)
	if p, ok := f.previewOf(id); ok {
		ask.Ready.State, ask.Ready.Log, ask.Ready.Sample = p.State, p.Log, p.Sample
		if p.State == previewReady {
			ask.Ready.Preview = p.URL
		}
	}
}

// landWords land a card at Ready for you; dropWords drop it; any other reply is a change request.
var landWords = []string{"land", "land it"}

// readyAnswer applies the Ready for you reply rule. Letters answer the builder's questions (qs) and go to
// it like any change request.
func readyAnswer(text string, qs []Question) answer {
	clean := cleanReply(text)
	if len(qs) == 1 && loneLetter.MatchString(clean) {
		clean = "1" + clean // one question: its letter alone answers it
	}
	switch {
	case clean == "":
		return answer{}
	case slices.Contains(landWords, clean):
		return answer{Action: "approve"}
	case slices.Contains(dropWords, clean):
		return answer{Action: "close"}
	case letterReply.MatchString(clean):
		if expanded, ok := expandLetters(clean, qs); ok {
			return answer{Action: "send", Message: "Zach's answers:\n" + expanded}
		}
	}
	return answer{Action: "send", Message: replyText(text)}
}

// previewWaitText is what the email and board say when there is no preview link.
func previewWaitText(r *Ready) string {
	switch r.State {
	case previewFailed:
		return "The preview didn't start."
	case previewQueued:
		return "Preview not started: too many running. It starts when one stops."
	case previewStarting:
		return "The preview is still starting."
	}
	return ""
}

// readyEmail is the Ready for you email: what changed, screenshots, the Preview button, how to answer.
func (f *Factory) readyEmail(a Agent, c Card, ask *Ask) email {
	repos := []string{c.Repo, a.Repo}
	title := c.Title
	if title == "" {
		title = a.Title
	}
	title = plainText(title, repos...)
	cwd := c.Cwd
	if cwd == "" {
		cwd = a.Cwd
	}
	r := ask.Ready
	link := f.cardLink(a.ID)
	slots := loadEvidenceSlots(cwd, ask.Evidence)
	var atts []mailAttachment
	for _, s := range slots {
		atts = append(atts, s.att)
	}
	// agent text passes through plainText, as in every email: no hashes, ids, repo slugs or file names
	var changed []string
	for _, l := range r.Changed {
		changed = append(changed, plainText(l, repos...))
	}
	if len(changed) == 0 && ask.Overview != nil && ask.Overview.Overview != "" {
		changed = []string{plainText(ask.Overview.Overview, repos...)}
	}
	answerLine := "Reply \"land\" to ship it, \"drop\" to drop it, or say what to change."
	if len(ask.Questions) > 0 { // shorter, to stay under the email word cap with the questions
		answerLine = "Reply \"land\", \"drop\", or what to change. Letters like 1A answer the questions."
	}
	var qs []string                   // the builder's questions, numbered, with lettered options
	for i, q := range ask.Questions { // cut like a decision email's (mail.go): the board has them whole
		qs = append(qs, fmt.Sprintf("%d. %s", i+1, firstWords(plainText(q.Text, repos...), questionCap)))
		for j, o := range q.Options {
			if j == optionsEmailed {
				break
			}
			qs = append(qs, fmt.Sprintf("   %s. %s", letter(j), firstWords(plainText(o, repos...), optionCap)))
		}
	}

	var t strings.Builder
	fmt.Fprintf(&t, "Ready to try: %s\n", title)
	if len(changed) > 0 {
		t.WriteString("\nWhat changed:\n")
	}
	for _, l := range changed {
		fmt.Fprintf(&t, "- %s\n", l)
	}
	for _, s := range slots {
		fmt.Fprintf(&t, "\n%s screenshot: %s (attached)\n", s.label, s.name)
	}
	if r.Preview != "" {
		fmt.Fprintf(&t, "\nTry it: %s\n", r.Preview)
	} else if w := previewWaitText(r); w != "" {
		fmt.Fprintf(&t, "\n%s\n", w)
	}
	if r.Sample != "" {
		fmt.Fprintf(&t, "\nSample output:\n%s\n", r.Sample)
	}
	if len(qs) > 0 {
		fmt.Fprintf(&t, "\n%s\n", strings.Join(qs, "\n"))
	}
	fmt.Fprintf(&t, "\n%s\n", answerLine)
	if link != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", link)
	}

	e := html.EscapeString
	button := func(href, label, bg string) string {
		return fmt.Sprintf(`<a href="%s" style="display:inline-block;background:%s;color:#ffffff;text-decoration:none;`+
			`font-weight:600;padding:10px 18px;border-radius:6px;margin-right:8px">%s</a>`, e(href), bg, e(label))
	}
	buildHTML := func(includeImages bool) string {
		var h strings.Builder
		h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;max-width:640px">`)
		fmt.Fprintf(&h, `<p style="font-size:17px"><b>Ready to try: %s</b></p><ul>`, e(title))
		for _, l := range changed {
			fmt.Fprintf(&h, "<li>%s</li>", e(l))
		}
		h.WriteString("</ul>")
		if includeImages {
			for _, s := range slots {
				fmt.Fprintf(&h, `<div style="margin:12px 0"><p style="margin:0 0 4px;font-weight:600">%s</p>`+
					`<img src="cid:%s" alt="%s" style="max-width:100%%;border:1px solid #d1d9e0;border-radius:6px"></div>`,
					e(s.label), e(s.cid), e(s.label))
			}
		}
		if r.Preview != "" {
			fmt.Fprintf(&h, `<p>%s</p>`, button(r.Preview, "Preview", "#0969da"))
		} else if w := previewWaitText(r); w != "" {
			fmt.Fprintf(&h, `<p style="color:#59636e">%s</p>`, e(w))
		}
		if r.Sample != "" {
			fmt.Fprintf(&h, `<p style="margin-bottom:4px"><b>Sample output</b></p><pre style="white-space:pre-wrap;`+
				`background:#f6f8fa;padding:8px 12px;font-size:13px">%s</pre>`, e(r.Sample))
		}
		if len(qs) > 0 {
			fmt.Fprintf(&h, `<pre style="white-space:pre-wrap;font-family:inherit">%s</pre>`, e(strings.Join(qs, "\n")))
		}
		fmt.Fprintf(&h, "<p>%s</p>", e(answerLine))
		if link != "" {
			fmt.Fprintf(&h, `<p>%s</p>`, button(link, "Open board", "#59636e"))
		}
		h.WriteString("</div>")
		return h.String()
	}
	return email{
		Subject:     "Ready to try: " + truncWords(title, 80),
		Text:        t.String(),
		HTML:        buildHTML(true),
		HTMLNoImage: buildHTML(false),
		Attachments: atts,
	}
}

// readyRound is the number of the next Ready for you of card a.
func readyRound(a Agent) int {
	n, _ := strconv.Atoi(a.L(labelReady))
	return n + 1
}
