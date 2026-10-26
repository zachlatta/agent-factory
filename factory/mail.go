package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os/exec"
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

// email is one message to Zach.
type email struct{ Subject, Text, HTML string }

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
	detail, qs := f.askDetail(c, ask)
	ask.Questions = qs
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
func (f *Factory) deliver(a Agent, m email) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
	defer cancel()
	if th := emailThread(a); th != "" {
		return "", f.replyInThread(ctx, th, m)
	}
	if th := a.L(labelZachThread); th != "" {
		return "", f.replyInThread(ctx, th, m)
	}
	out, err := f.Mail.Call(ctx, "send_email", map[string]any{"to": []string{f.cfg.ZachEmail},
		"subject": m.Subject, "text": m.Text, "html": m.HTML})
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
}

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
// the mailbox wrote every message in it.
func (f *Factory) replyInThread(ctx context.Context, thread string, m email) error {
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
	switch {
	case zach != "":
		args["message_id"] = zach
	case last != "" && f.cfg.ZachEmail != "":
		args["message_id"], args["to"] = last, []string{f.cfg.ZachEmail}
	default:
		return fmt.Errorf("thread %s has no message to answer", short(thread))
	}
	_, err = f.Mail.Call(ctx, "reply_to_message", args)
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

// askDetail is the full Detail (not cut like the board's) and its questions.
func (f *Factory) askDetail(c Card, ask *Ask) (string, []Question) {
	text, err := f.lastReply(f.agent(askSource(c, ask)))
	if err != nil || strings.TrimSpace(text) == "" {
		return "(The agent's last message could not be read. Open the board to see it.)", nil
	}
	return ParseQuestions(text)
}

// replyWord is one word Zach can answer with by email and what it does.
type replyWord struct{ Word, Does string }

// replyWords lists the email answers for ask, matching parseAnswer.
func replyWords(ask *Ask) []replyWord {
	var out []replyWord
	if yes := actionFor(ask, ask.Yes); yes != nil && (ask.Yes == "approve" || ask.Yes == "recommend") {
		out = append(out, replyWord{"approve", "same as the \"" + yes.Label + "\" button"})
	}
	if x := actionFor(ask, "close"); x != nil {
		out = append(out, replyWord{"drop", "same as \"" + x.Label + "\": the card is archived and nothing lands"})
	}
	if len(ask.Questions) > 0 {
		does := "answers the questions above"
		if ask.Yes == "approve" || ask.Yes == "recommend" {
			does += ", then the same as \"approve\""
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

// composeEmail writes the email for card a waiting on ask, with its full detail.
func (f *Factory) composeEmail(a Agent, c Card, ask *Ask, detail string) email {
	title := c.Title
	if title == "" {
		title = a.Title
	}
	repo := c.Repo
	if repo == "" {
		repo = "no repo"
	}
	link := f.cardLink(a.ID)
	words := replyWords(ask)

	var t strings.Builder
	fmt.Fprintf(&t, "%s\n%s\n\nCard: %s (%s)\n\n%s\n", ask.Title, ask.Why, title, repo, detail)
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

	e := html.EscapeString
	var h strings.Builder
	h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:15px;line-height:1.5;color:#1f2328;max-width:640px">`)
	fmt.Fprintf(&h, `<p><b>%s</b><br>%s</p><p style="color:#59636e">Card: %s (%s)</p>`, e(ask.Title), e(ask.Why), e(title), e(repo))
	fmt.Fprintf(&h, `<div style="white-space:pre-wrap;border-left:3px solid #d1d9e0;padding-left:12px">%s</div>`, e(detail))
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
	h.WriteString("</div>")
	return email{Subject: "Needs you: " + truncWords(title, 80), Text: t.String(), HTML: h.String()}
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
