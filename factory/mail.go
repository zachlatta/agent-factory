package factory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Needs Zach email. Every card that waits on Zach gets one email per wait reason (factory.told), sent
// by the factory itself through the zrl-bot mailbox, carrying what the board's Ask shows: the title,
// why, the card, the full last message of the builder or reviewer, its questions, the reply words and
// a board link. Inbox cards answer in their own thread (gmail-thread). Other cards get one new thread
// to Config.ZachEmail on their first wait (factory.zach-thread); later waits reply in it. His email
// replies reach the factory through the dispatcher's POST /api/answer (answer.go).

// Mailer calls one zrl_bot_email_control tool with args and returns its JSON result.
type Mailer interface {
	Call(ctx context.Context, tool string, args any) (json.RawMessage, error)
}

// ErrMailMaybeSent: a call timed out, so the server may have done it. A send is never retried after it.
var ErrMailMaybeSent = errors.New("timed out; it may have gone through")

const (
	mailTimeout    = 5 * time.Minute  // one email: thread read plus send
	mailRetryAfter = 5 * time.Minute  // a failed send waits this long before the next try
	mailProbeEvery = 10 * time.Minute // health probe of the mailbox
)

// PDWMailer runs `pdw call zrl_bot_email_control__<tool> --output structured`, the JSON args on stdin.
type PDWMailer struct{ Bin string }

func (m PDWMailer) Call(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	bin := m.Bin
	if bin == "" {
		bin = "pdw"
	}
	data, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "call", "zrl_bot_email_control__"+tool, "--output", "structured")
	cmd.Stdin = bytes.NewReader(data)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", tool, ErrMailMaybeSent)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v: %s", tool, err, trunc(strings.TrimSpace(stderr.String()+" "+out.String()), 300))
	}
	// a tool error prints the MCP envelope with isError
	var env struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(out.Bytes(), &env) == nil && env.IsError {
		return nil, fmt.Errorf("%s: tool error: %s", tool, trunc(out.String(), 300))
	}
	return out.Bytes(), nil
}

type mailAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
	DataBase64  string `json:"data_base64"`
}

// email is one message to Zach.
type email struct {
	Subject     string
	Text        string
	HTML        string
	HTMLNoImage string
	Attachments []mailAttachment
}

func (m email) withoutAttachments() email {
	h := m.HTMLNoImage
	if h == "" {
		h = m.HTML
	}
	return email{Subject: m.Subject, Text: m.Text, HTML: h}
}

// mailable reports whether Zach is emailed about this ask: only decisions, never board-only waits.
func mailable(ask *Ask) bool {
	return ask != nil && !ask.BoardOnly
}

// mailZach emails Zach why card a waits on him, once per wait note.
func (f *Factory) mailZach(a Agent, c Card) error {
	byID := f.byID()
	if replacedPredecessor(byID, a) {
		return nil
	}
	ask := askFor(c, a)
	if !mailable(ask) {
		return nil
	}
	note := waitNote(a)
	if lineageTold(byID, a) == note {
		return nil
	}
	if w := parseTime(a.L(labelWriting)); !w.IsZero() && !writtenFor(a, w, f.Now()) {
		return nil // the builder is still to write the decision or preview the email carries
	}
	f.mu.Lock()
	retry := f.mailRetry[a.ID]
	f.mu.Unlock()
	if f.Now().Before(retry) {
		return nil
	}
	if lineageEmailThread(byID, a) == "" && f.cfg.ZachEmail == "" {
		if f.noteOnce("no-zach-email", "") {
			f.logf("no FACTORY_ZACH_EMAIL: board cards waiting on Zach are not emailed")
		}
		return nil
	}
	_, qs, ov := f.askDetail(c, ask)
	ask.Questions, ask.Overview = qs, ov
	m := f.composeEmail(a, c, ask)
	// told first: one email per wait even if the factory stops mid-send
	if err := f.setLabels(a.ID, map[string]string{labelTold: note}); err != nil {
		return err
	}
	thread, err := f.deliver(a, m)
	switch {
	case errors.Is(err, ErrMailMaybeSent):
		f.logf("%s email to Zach (%s) possibly sent: %v", short(a.ID), note, err)
		return nil
	case err != nil:
		f.mu.Lock()
		f.mailRetry[a.ID] = f.Now().Add(mailRetryAfter)
		f.mu.Unlock()
		return errors.Join(fmt.Errorf("emailing Zach: %w", err), f.setLabels(a.ID, map[string]string{labelTold: ""}))
	}
	f.logf("%s waits on Zach (%s): emailed him", short(a.ID), note)
	if thread != "" {
		return f.setLabels(a.ID, map[string]string{labelZachThread: thread})
	}
	return nil
}

// writeWait: how long the email waits for a builder asked to write Zach a summary before it goes
// out with whatever the builder last said.
const writeWait = 10 * time.Minute

// writtenFor reports whether a, asked at w to write for Zach, has replied since (the 45 s grace covers
// the gap between the message and the agent starting), or writeWait has passed.
func writtenFor(a Agent, w, now time.Time) bool {
	return (a.LastActivityAt.After(w) && now.Sub(w) > 45*time.Second) || now.Sub(w) > writeWait
}

// noteOnce reports true the first time key is seen with value v.
func (f *Factory) noteOnce(key, v string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.once[key]; ok && old == v {
		return false
	}
	f.once[key] = v
	return true
}

// deliver sends m in the card's thread, or starts one. It returns a new thread's id, or "".
// If sending with inline image attachments fails for a reason other than timeout, it retries once without them.
func (f *Factory) deliver(a Agent, m email) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
	defer cancel()
	th, err := f.deliverOnce(ctx, a, m)
	if err != nil && len(m.Attachments) > 0 && !errors.Is(err, ErrMailMaybeSent) {
		f.logf("%s: sending with attachments failed (%v); retrying without images", short(a.ID), err)
		return f.deliverOnce(ctx, a, m.withoutAttachments())
	}
	return th, err
}

func (f *Factory) deliverOnce(ctx context.Context, a Agent, m email) (string, error) {
	byID := f.byID()
	if th := lineageEmailThread(byID, a); th != "" {
		return "", f.replyInThread(ctx, a.ID, th, m, false)
	}
	if th := lineageLabel(byID, a, labelZachThread); th != "" {
		return "", f.replyInThread(ctx, a.ID, th, m, true)
	}
	args := map[string]any{"to": []string{f.cfg.ZachEmail},
		"subject": m.Subject, "text": m.Text, "html": m.HTML}
	if len(m.Attachments) > 0 {
		args["attachments"] = m.Attachments
	}
	out, err := f.sendMail(ctx, a.ID, "send_email", args)
	if err != nil {
		return "", err
	}
	var res struct {
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(out, &res) != nil || res.ThreadID == "" {
		f.logf("%s: send_email gave no thread id; the next wait starts a new thread", short(a.ID))
	}
	return res.ThreadID, nil
}

type threadMsg struct {
	ID       string   `json:"id"`
	LabelIDs []string `json:"label_ids"`
	Date     string   `json:"date,omitempty"`
}

// sent: the mailbox sent this message.
func (m threadMsg) sent() bool { return slices.Contains(m.LabelIDs, "SENT") }

// latestFromZach is the newest message in a thread that the mailbox did not send, and the newest
// message overall.
func latestFromZach(msgs []threadMsg) (zach, last string) {
	for _, m := range msgs {
		last = m.ID
		sent := false
		for _, l := range m.LabelIDs {
			sent = sent || l == "SENT"
		}
		if !sent {
			zach = m.ID
		}
	}
	return zach, last
}

// replyInThread answers Zach's latest message in thread, or the latest one (addressed to Zach) when
// the mailbox wrote every message in it. When forceTo is set (a board card's labelZachThread),
// it always sets to=f.cfg.ZachEmail even if an earlier reply in the thread came from another address.
func (f *Factory) replyInThread(ctx context.Context, id, thread string, m email, forceTo bool) error {
	out, err := f.Mail.Call(ctx, "get_thread", map[string]any{"thread_id": thread, "latest": 5})
	if errors.Is(err, ErrMailMaybeSent) {
		return errors.New("get_thread timed out") // a read: safe to retry
	}
	if err != nil {
		return err
	}
	var th struct {
		Messages []threadMsg `json:"messages"`
	}
	if err := json.Unmarshal(out, &th); err != nil {
		return fmt.Errorf("get_thread: %v", err)
	}
	zach, last := latestFromZach(th.Messages)
	args := map[string]any{"text": m.Text, "html": m.HTML, "omit_quote": true}
	if len(m.Attachments) > 0 {
		args["attachments"] = m.Attachments
	}
	switch {
	case zach != "":
		args["message_id"] = zach
		if forceTo && f.cfg.ZachEmail != "" {
			args["to"] = []string{f.cfg.ZachEmail}
		}
	case last != "" && f.cfg.ZachEmail != "":
		args["message_id"], args["to"] = last, []string{f.cfg.ZachEmail}
	default:
		return fmt.Errorf("thread %s has no message to answer", short(thread))
	}
	_, err = f.sendMail(ctx, id, "reply_to_message", args)
	return err
}

// askSource is the agent whose last message is the ask's Detail: the card, or its latest reviewer.
func askSource(c Card, ask *Ask) string {
	src := c.ID
	if ask.From == "reviewer" {
		for _, k := range c.Children {
			if k.Role == "reviewer" {
				src = k.ID // children are oldest first, so this ends on the latest
			}
		}
	}
	return src
}

// askDetail is the full Detail (not cut like the board's), its questions and its overview.
// It also parses BEFORE:/AFTER: evidence filenames onto ask.Evidence.
func (f *Factory) askDetail(c Card, ask *Ask) (string, []Question, *Overview) {
	text, err := f.askReply(f.agent(askSource(c, ask)))
	if err != nil || strings.TrimSpace(text) == "" {
		return unreadableReply(err), nil, nil
	}
	var ev Evidence
	text, ev = parseEvidence(text)
	if ask != nil {
		ask.Evidence = ev
	}
	rest, qs, ov := parseReply(text)
	return rest, askQuestions(ask, qs), ov
}

// replyWord is one word Zach can answer with by email and what it does.
type replyWord struct{ Word, Does string }

// replyWords lists the one-word email answers for ask, matching parseAnswer.
func replyWords(ask *Ask) []replyWord {
	var out []replyWord
	if yes := actionFor(ask, ask.Yes); yes != nil && (ask.Yes == "approve" || ask.Yes == "arbiter") {
		does := strings.ToLower(yes.Label)
		if len(ask.Questions) > 1 && ask.Yes == "approve" {
			does = "take the recommended answers"
		}
		out = append(out, replyWord{"yes", does})
	}
	if actionFor(ask, "done") != nil {
		out = append(out, replyWord{"done", "mark it done"})
	}
	if actionFor(ask, "close") != nil {
		out = append(out, replyWord{"drop", "stop it"})
	}
	return out
}

func actionFor(ask *Ask, id string) *Action {
	for i := range ask.Actions {
		if ask.Actions[i].ID == id {
			return &ask.Actions[i]
		}
	}
	return nil
}

// cardLink is the board link that opens card id, or "".
func (f *Factory) cardLink(id string) string {
	if f.cfg.BoardURL == "" {
		return ""
	}
	return strings.TrimRight(f.cfg.BoardURL, "#") + "#" + id
}

func letter(i int) string { return string(rune('A' + i)) }

// factoryRaised reports whether card a stopped for a check the factory raised itself (a secret scan,
// an unreviewed public-repo push), where the agent's last reply was not written as a decision for Zach.
func factoryRaised(a Agent) bool {
	note := a.L(labelNote)
	return strings.HasPrefix(note, "secret scan") || strings.HasPrefix(note, "public repo: finished without review")
}

type evidenceSlot struct {
	cid, label, name string
	att              mailAttachment
}

func loadEvidenceSlots(cwd string, ev Evidence) []evidenceSlot {
	var out []evidenceSlot
	for _, s := range []struct{ cid, label, name string }{
		{"before", "Before", ev.Before},
		{"after", "After", ev.After},
	} {
		if s.name == "" {
			continue
		}
		data, media, err := readEvidence(cwd, s.name)
		if err != nil {
			continue
		}
		out = append(out, evidenceSlot{
			cid:   s.cid,
			label: s.label,
			name:  s.name,
			att: mailAttachment{
				Filename:    s.name,
				ContentType: media,
				ContentID:   s.cid,
				DataBase64:  base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	return out
}

// Decision emails. The subject is the question; the body, under mailWordCap words, leads with the
// recommendation and any choices, then what it is about, then the one-word replies and a board link. The full report
// stays on the board. Agent text passes through plainText, so no hash, id, repo slug or file name
// reaches Zach.
const (
	mailWordCap    = 59 // words in the body, not counting the board link
	subjectCap     = 90 // runes in the subject
	questionCap    = 12 // words of a question
	optionCap      = 5  // words of an option
	optionsEmailed = 4  // options listed per question
)

var (
	uuidRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hexRe      = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)
	digitRe    = regexp.MustCompile(`[0-9]`)
	letterRe   = regexp.MustCompile(`(?i)[a-f]`)
	fileNameRe = regexp.MustCompile(`(?i)[\w./-]*\w\.(go|md|py|ts|tsx|js|mjs|json|html|css|ya?ml|sh|toml|sql|txt|jsonl|kt|swift|rb)(:\d+(-\d+)?)?\b`)
	slugRe     = regexp.MustCompile(`\b[a-z0-9_.-]+/([a-z0-9_.-]*[-_][a-z0-9_.-]*)\b`)
	spaceRe    = regexp.MustCompile(`[ \t]+`)
	spacePunct = regexp.MustCompile(` +([,.;:)])`)
	emptyParen = regexp.MustCompile(`\(\s*\)`)
)

// plainText strips what Zach should never have to read in an email from agent text: backticks,
// commit hashes and agent ids, file names, and the owner part of the repos named in repos.
func plainText(s string, repos ...string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = uuidRe.ReplaceAllString(s, "")
	s = hexRe.ReplaceAllStringFunc(s, func(w string) string {
		if digitRe.MatchString(w) && letterRe.MatchString(w) {
			return ""
		}
		return w
	})
	s = fileNameRe.ReplaceAllString(s, "")
	for _, r := range repos {
		if i := strings.Index(r, "/"); i > 0 {
			s = strings.ReplaceAll(s, r, r[i+1:])
		}
	}
	s = dropOwners(s)
	s = emptyParen.ReplaceAllString(s, "")
	s = spaceRe.ReplaceAllString(s, " ")
	s = spacePunct.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// dropOwners cuts the owner from owner/repo-name slugs outside links; and/or and Wi-Fi/cellular stay.
func dropOwners(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range slugRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > 0 && strings.ContainsRune("/:.", rune(s[m[0]-1])) {
			continue // part of a link or path
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(s[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// firstWords is s cut to n words, marked with "…" when cut.
func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) <= n {
		return strings.Join(w, " ")
	}
	return strings.TrimRight(strings.Join(w[:n], " "), ",;:.-") + "…"
}

func wordCount(s string) int { return len(strings.Fields(s)) }

// emailQuestion is the email's subject: the agent's DECISION line, unless the factory raised the
// stop or the agent decided nothing, then the ask's own question.
func emailQuestion(a Agent, ask *Ask, repos []string) string {
	q := ask.Title
	if ov := ask.Overview; ov != nil && !factoryRaised(a) {
		if d := plainText(ov.Decision, repos...); d != "" && !needsNothingRe.MatchString(d) {
			q = d
		}
	}
	return truncWords(q, subjectCap)
}

// composeEmail writes the decision email for card a waiting on ask: one question, under mailWordCap
// words, answerable with one word, with any before and after screenshots inline.
func (f *Factory) composeEmail(a Agent, c Card, ask *Ask) email {
	title := c.Title
	if title == "" {
		title = a.Title
	}
	repos := []string{c.Repo, a.Repo}
	cwd := c.Cwd
	if cwd == "" {
		cwd = a.Cwd
	}
	subject := emailQuestion(a, ask, repos)
	slots := loadEvidenceSlots(cwd, ask.Evidence)
	var atts []mailAttachment
	for _, s := range slots {
		atts = append(atts, s.att)
	}

	// required lines: the recommendation, the choices and how to answer
	var lead, reply []string
	var options []string
	words := replyWords(ask)
	switch qs := ask.Questions; {
	case len(qs) == 1:
		if subject != firstWords(plainText(qs[0].Text, repos...), questionCap) {
			lead = append(lead, firstWords(plainText(qs[0].Text, repos...), questionCap))
		}
		lead = append(lead, "I recommend A.")
		for j, o := range qs[0].Options {
			if j == optionsEmailed {
				break
			}
			options = append(options, letter(j)+". "+firstWords(plainText(o, repos...), optionCap))
		}
		reply = append(reply, "Reply with a letter")
	case len(qs) > 1:
		var rec []string
		for i := range qs {
			rec = append(rec, fmt.Sprintf("%dA", i+1))
		}
		lead = append(lead, fmt.Sprintf("%d questions; I recommend %s.", len(qs), strings.Join(rec, " ")))
		reply = append(reply, "Reply with letters like "+strings.Join(rec, " "))
	case ask.Yes == "approve" || ask.Yes == "arbiter":
		lead = append(lead, "I recommend yes.")
	}
	for _, w := range words {
		reply = append(reply, fmt.Sprintf("%q to %s", w.Word, w.Does))
	}
	replyLine := "Reply " + strings.TrimPrefix(strings.Join(reply, ", "), "Reply ")
	if len(reply) == 0 {
		replyLine = "Reply with what to do"
	}
	replyLine += ", or write what to change."
	if len(ask.Questions) == 0 && len(words) == 0 {
		replyLine = "Reply with what to do."
	}

	// optional lines, as the word budget allows: what it is about, and the agent's overview
	used := wordCount(strings.Join(lead, " ")) + wordCount(strings.Join(options, " ")) + wordCount(replyLine)
	var about, overview string
	if t := plainText(title, repos...); t != "" && !strings.EqualFold(t, subject) {
		about = "About: " + firstWords(t, 14)
		if used+wordCount(about) > mailWordCap {
			about = ""
		}
		used += wordCount(about)
	}
	if ov := ask.Overview; ov != nil && !factoryRaised(a) && mailWordCap-used >= 6 {
		overview = firstWords(plainText(ov.Overview, repos...), mailWordCap-used)
	}

	link := f.cardLink(a.ID)
	var body []string
	body = append(body, lead...)
	body = append(body, options...)
	if about != "" {
		body = append(body, about)
	}
	if overview != "" {
		body = append(body, overview)
	}
	body = append(body, replyLine)
	var t strings.Builder
	for _, l := range body {
		t.WriteString(l + "\n")
	}
	for _, s := range slots {
		fmt.Fprintf(&t, "%s screenshot attached.\n", s.label)
	}
	if link != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", link)
	}

	e := html.EscapeString
	buildHTML := func(includeImages bool) string {
		var h strings.Builder
		h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:16px;line-height:1.5;color:#1f2328;max-width:640px">`)
		for i, l := range lead {
			if i == 0 {
				fmt.Fprintf(&h, `<p style="font-size:17px"><b>%s</b></p>`, e(l))
			} else {
				fmt.Fprintf(&h, "<p>%s</p>", e(l))
			}
		}
		if len(options) > 0 {
			h.WriteString("<p>")
			for i, o := range options {
				if i > 0 {
					h.WriteString("<br>")
				}
				h.WriteString(e(o))
			}
			h.WriteString("</p>")
		}
		if about != "" {
			fmt.Fprintf(&h, `<p style="color:#59636e">%s</p>`, e(about))
		}
		if overview != "" {
			fmt.Fprintf(&h, "<p>%s</p>", e(overview))
		}
		if includeImages {
			for _, s := range slots {
				fmt.Fprintf(&h, `<div style="margin:12px 0"><p style="margin:0 0 4px;font-weight:600">%s</p>`+
					`<img src="cid:%s" alt="%s" style="max-width:100%%;border:1px solid #d1d9e0;border-radius:6px"></div>`,
					e(s.label), e(s.cid), e(s.label))
			}
		}
		fmt.Fprintf(&h, "<p>%s</p>", e(replyLine))
		if link != "" {
			fmt.Fprintf(&h, `<p><a href="%s" style="display:inline-block;background:#0969da;color:#ffffff;text-decoration:none;`+
				`font-weight:600;padding:10px 18px;border-radius:6px">Open board</a></p>`, e(link))
		}
		h.WriteString("</div>")
		return h.String()
	}

	return email{
		Subject:     subject,
		Text:        t.String(),
		HTML:        buildHTML(true),
		HTMLNoImage: buildHTML(false),
		Attachments: atts,
	}
}

// probeMail checks the mailbox every mailProbeEvery, for /api/health. It never fails the health check.
func (f *Factory) probeMail() {
	if f.Mail == nil {
		return
	}
	f.mu.Lock()
	due := f.Now().Sub(f.mailProbe.at) >= mailProbeEvery
	if due {
		f.mailProbe.at = f.Now()
	}
	f.mu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	status := "ok"
	if _, err := f.Mail.Call(ctx, "get_mailbox_info", map[string]any{}); err != nil {
		status = trunc(err.Error(), 200)
	}
	f.mu.Lock()
	f.mailProbe.status = status
	f.mu.Unlock()
}

type mailProbe struct {
	at     time.Time
	status string
}
