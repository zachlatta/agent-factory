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

// mailable reports whether Zach is emailed about this ask: not for finished cards or hand moves.
func mailable(ask *Ask) bool {
	return ask != nil && ask.Title != finishedTitle && ask.Title != handTitle
}

// mailZach emails Zach why card a waits on him, once per wait note.
func (f *Factory) mailZach(a Agent, c Card) error {
	ask := askFor(c, a)
	if !mailable(ask) {
		return nil
	}
	note := waitNote(a)
	if w := parseTime(a.L(labelWriting)); !w.IsZero() && !writtenFor(a, w, f.Now()) {
		return nil // the builder is still to write the summary or brief the email carries
	}
	f.mu.Lock()
	retry := f.mailRetry[a.ID]
	f.mu.Unlock()
	if f.Now().Before(retry) {
		return nil
	}
	if emailThread(a) == "" && f.cfg.ZachEmail == "" {
		if f.noteOnce("no-zach-email", "") {
			f.logf("no FACTORY_ZACH_EMAIL: board cards waiting on Zach are not emailed")
		}
		return nil
	}
	detail, qs, ov := f.askDetail(c, ask)
	ask.Questions, ask.Overview = qs, ov
	if req := f.cardRequest(a); req.Base != "" || len(req.FollowUps) > 0 {
		ask.Request = &req
	}
	m := f.composeEmail(a, c, ask, detail)
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
	if th := emailThread(a); th != "" {
		return "", f.replyInThread(ctx, a.ID, th, m, false)
	}
	if th := a.L(labelZachThread); th != "" {
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
	return parseReply(text)
}

// replyWord is one word Zach can answer with by email and what it does.
type replyWord struct{ Word, Does string }

// replyWords lists the email answers for ask, matching parseAnswer.
func replyWords(ask *Ask) []replyWord {
	var out []replyWord
	if yes := actionFor(ask, ask.Yes); yes != nil && (ask.Yes == "approve" || ask.Yes == "recommend") {
		out = append(out, replyWord{"approve", "same as the \"" + yes.Label + "\" button"})
	}
	if x := actionFor(ask, "done"); x != nil {
		out = append(out, replyWord{"done", "same as \"" + x.Label + "\""})
	}
	if x := actionFor(ask, "close"); x != nil {
		out = append(out, replyWord{"drop", "same as \"" + x.Label + "\": the card is archived and nothing lands"})
	}
	if len(ask.Questions) > 0 {
		does := "answers the questions above"
		if ask.Yes == "approve" {
			does += ", then the same as \"approve\""
		} else {
			does += "; the agent gets your answers"
		}
		out = append(out, replyWord{"1A 2B", does})
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

// factoryCheckWait reports whether card a stopped for a check the Factory raised itself after
// landing, a stall, a secret scan, or an unreviewed public-repo push, where the builder's last
// reply was not written as a decision for Zach.
func factoryCheckWait(a Agent) bool {
	note := a.L(labelNote)
	return strings.HasPrefix(note, "landed; still to do:") ||
		note == "landed; did not say what remains" ||
		note == "landing not verified" ||
		strings.HasPrefix(note, "landed diff differs") ||
		note == "stuck" ||
		strings.HasPrefix(note, "secret scan") ||
		strings.HasPrefix(note, "public repo: finished without review")
}

func requestSummary(r *CardRequest) string {
	if r == nil {
		return ""
	}
	var parts []string
	if b := strings.TrimSpace(r.Base); b != "" {
		parts = append(parts, truncWords(b, 120))
	}
	if n := len(r.FollowUps); n > 0 {
		fu := r.FollowUps[n-1]
		if txt := strings.TrimSpace(fu.Text); txt != "" {
			parts = append(parts, fu.label()+": "+truncWords(txt, 80))
		}
	}
	return strings.Join(parts, "\n\n")
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

// composeEmail writes the email for card a waiting on ask, putting the decision, original task,
// inline evidence screenshots, questions, reply words and board link above the full Detail report.
func (f *Factory) composeEmail(a Agent, c Card, ask *Ask, detail string) email {
	title := c.Title
	if title == "" {
		title = a.Title
	}
	repo := c.Repo
	if repo == "" {
		repo = "no repo"
	}
	cwd := c.Cwd
	if cwd == "" {
		cwd = a.Cwd
	}
	link := f.cardLink(a.ID)
	words := replyWords(ask)
	hideDecision := factoryCheckWait(a)
	reqText := requestSummary(ask.Request)
	slots := loadEvidenceSlots(cwd, ask.Evidence)
	var atts []mailAttachment
	for _, s := range slots {
		atts = append(atts, s.att)
	}

	var t strings.Builder
	if ov := ask.Overview; ov != nil && !hideDecision {
		for _, l := range []string{ov.Decision, ov.Overview, prefixed("Needs from you: ", ov.Needs)} {
			if l != "" {
				t.WriteString(l + "\n")
			}
		}
		t.WriteString("\n")
	}
	fmt.Fprintf(&t, "%s\n%s\n\nCard: %s (%s)\n", ask.Title, ask.Why, title, repo)
	if ov := ask.Overview; ov != nil && hideDecision && ov.Overview != "" {
		fmt.Fprintf(&t, "Summary: %s\n", ov.Overview)
	}
	if reqText != "" {
		fmt.Fprintf(&t, "\nOriginal task:\n%s\n", reqText)
	}
	for _, s := range slots {
		fmt.Fprintf(&t, "\n%s screenshot: %s (attached)\n", s.label, s.name)
	}
	if len(ask.Questions) > 0 {
		t.WriteString("\nQuestions:\n")
		for i, q := range ask.Questions {
			fmt.Fprintf(&t, "%d. %s\n", i+1, q.Text)
			for j, o := range q.Options {
				fmt.Fprintf(&t, "   %s. %s\n", letter(j), o)
			}
		}
	}
	t.WriteString("\nHow to answer: reply with one of these alone.\n")
	for _, w := range words {
		fmt.Fprintf(&t, "- \"%s\": %s\n", w.Word, w.Does)
	}
	t.WriteString("- Anything else goes to the agent as your instructions.\n")
	if link != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", link)
	}
	if strings.TrimSpace(detail) != "" {
		fmt.Fprintf(&t, "\nDetails:\n%s\n", detail)
	}

	e := html.EscapeString
	buildHTML := func(includeImages bool) string {
		var h strings.Builder
		h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;max-width:640px">`)
		if ov := ask.Overview; ov != nil && !hideDecision {
			if ov.Decision != "" {
				fmt.Fprintf(&h, `<p style="font-size:17px"><b>%s</b></p>`, e(ov.Decision))
			}
			if ov.Overview != "" {
				fmt.Fprintf(&h, `<p style="white-space:pre-wrap">%s</p>`, e(ov.Overview))
			}
			if ov.Needs != "" {
				fmt.Fprintf(&h, `<div style="white-space:pre-wrap;background:#fff8c5;border-left:3px solid #d4a72c;`+
					`padding:8px 12px;margin-bottom:12px"><b>Needs from you:</b> %s</div>`, e(ov.Needs))
			}
		}
		fmt.Fprintf(&h, `<p><b>%s</b><br>%s</p><p style="color:#59636e">Card: %s (%s)</p>`, e(ask.Title), e(ask.Why), e(title), e(repo))
		if ov := ask.Overview; ov != nil && hideDecision && ov.Overview != "" {
			fmt.Fprintf(&h, `<p style="white-space:pre-wrap"><b>Summary:</b> %s</p>`, e(ov.Overview))
		}
		if reqText != "" {
			fmt.Fprintf(&h, `<div style="white-space:pre-wrap;background:#f6f8fa;border-left:3px solid #d1d9e0;`+
				`padding:8px 12px;margin:12px 0"><b>Original task:</b><br>%s</div>`, e(reqText))
		}
		if includeImages && len(slots) > 0 {
			for _, s := range slots {
				fmt.Fprintf(&h, `<div style="margin:12px 0"><p style="margin:0 0 4px;font-weight:600">%s</p>`+
					`<img src="cid:%s" alt="%s" style="max-width:100%%;border:1px solid #d1d9e0;border-radius:6px"></div>`,
					e(s.label), e(s.cid), e(s.label))
			}
		}
		if len(ask.Questions) > 0 {
			h.WriteString(`<h3 style="border-bottom:1px solid #d1d9e0">Questions</h3><ol>`)
			for _, q := range ask.Questions {
				fmt.Fprintf(&h, "<li>%s", e(q.Text))
				for j, o := range q.Options {
					fmt.Fprintf(&h, "<br>%s. %s", letter(j), e(o))
				}
				h.WriteString("</li>")
			}
			h.WriteString("</ol>")
		}
		h.WriteString(`<h3 style="border-bottom:1px solid #d1d9e0">How to answer</h3><p>Reply with one of these alone:</p><ul>`)
		for _, w := range words {
			fmt.Fprintf(&h, "<li><b>%s</b>: %s</li>", e(w.Word), e(w.Does))
		}
		h.WriteString("<li>Anything else goes to the agent as your instructions.</li></ul>")
		if link != "" {
			fmt.Fprintf(&h, `<p><a href="%s" style="display:inline-block;background:#0969da;color:#ffffff;text-decoration:none;`+
				`font-weight:600;padding:10px 18px;border-radius:6px">Open board</a></p>`, e(link))
		}
		if strings.TrimSpace(detail) != "" {
			fmt.Fprintf(&h, `<h3 style="border-bottom:1px solid #d1d9e0;margin-top:20px">Details</h3>`+
				`<div style="white-space:pre-wrap;border-left:3px solid #d1d9e0;padding-left:12px">%s</div>`, e(detail))
		}
		h.WriteString("</div>")
		return h.String()
	}

	return email{
		Subject:     "Needs you: " + truncWords(title, 80),
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

// prefixed is p+s, or "" when s is empty.
func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}
