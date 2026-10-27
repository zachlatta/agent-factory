package factory

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Ask says, for a card in Needs Zach, what Zach is being asked, why, what to read (Detail, from the
// builder's or the reviewer's last message), and which buttons answer it.
type Ask struct {
	// Report retains the full parsed reply for board details; Detail keeps the email preview.
	Report   string       `json:"report,omitempty"`
	Request  *CardRequest `json:"request,omitempty"`
	Evidence Evidence     `json:"evidence"`
	Title    string       `json:"title"`
	Why      string       `json:"why"`
	From     string       `json:"from"` // builder or reviewer: whose last message is Detail
	Detail   string       `json:"detail"`
	Actions  []Action     `json:"actions"`
	// Questions are the QUESTION: blocks in the agent's last reply (ParseQuestions), shown as choices.
	Questions []Question `json:"questions,omitempty"`
	// Overview is the agent's DECISION / OVERVIEW / NEEDS FROM ZACH block (ParseOverview).
	Overview *Overview `json:"overview,omitempty"`
	// Yes/No retain their action meanings for API compatibility. Open asks with both set to
	// "say" get explicit Yes/No reply choices alongside Other on the board.
	Yes string `json:"yes,omitempty"`
	No  string `json:"no,omitempty"`
}

// Question is one multiple-choice question an agent asks Zach. He can also write his own answer.
type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

// Action is one button. ID is the POST /api/<id> action ("reply" posts to send with the typed text).
type Action struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Primary bool   `json:"primary,omitempty"`
	Hint    string `json:"hint,omitempty"` // placeholder for the reply box
	Repo    string `json:"repo,omitempty"` // approve on a repo picker: start the card in this repo
}

func act(id, label string) Action { return Action{ID: id, Label: label} }
func primary(id, label string) Action {
	return Action{ID: id, Label: label, Primary: true}
}
func reply(label, hint string) Action { return Action{ID: "reply", Label: label, Hint: hint} }

// askFor builds the Ask for a Needs Zach card, with its Yes/No meaning.
func askFor(c Card, a Agent) *Ask {
	ask := baseAsk(c, a)
	if ask == nil {
		return nil
	}
	ask.Yes, ask.No = yesNo(ask)
	return ask
}

// yesNo maps the board's Yes/No buttons onto the ask, by its buttons. A brief (rework cap or escalation)
// gets Yes = go with the builder's recommendation; a yes/no question (primary approve or close) gets
// that action; both send No as a change request. An open-ended ask sends Yes or No as a reply. Asks
// whose answer is Stop, Re-run or a repo, and finished cards, get none.
func yesNo(ask *Ask) (string, string) {
	has := func(id string) bool {
		for _, x := range ask.Actions {
			if x.ID == id {
				return true
			}
		}
		return false
	}
	switch {
	case has("stop") || has("resume") || has("rerun") || has("pickrepo") || ask.Title == finishedTitle:
		return "", ""
	case has("recommend"):
		return "recommend", "changes"
	}
	for _, x := range ask.Actions {
		if x.Primary && (x.ID == "approve" || x.ID == "close") {
			return x.ID, "changes"
		}
	}
	return "say", "say"
}

const finishedTitle = "Finished: read the result"
const handTitle = "Moved here by hand"
const repoTitle = "Which repo should this start in?"

// baseAsk builds the Ask for a Needs Zach card from its labels and Paseo attention state.
func baseAsk(c Card, a Agent) *Ask {
	if c.Stage != "zach" {
		return nil
	}
	note := a.L(labelNote)
	switch {
	case a.Status == "running" && a.Pending != "":
		return &Ask{Title: "Waiting on a permission prompt", From: "builder",
			Why: "Claude Code asked for permission (" + a.Pending + "), which it does even in bypass mode, and the factory " +
				"never answers prompts. Approve or deny it in Paseo, stop the agent, or send it a message.",
			Actions: []Action{act("stop", "Stop it"), reply("Reply", "Message to the agent")}}
	case a.Status == "running" && stalledFlag(c) == "":
		// a builder writing its brief or plan summary for Zach (sent along with the move here)
		return &Ask{Title: "Writing a summary for you…", From: "builder",
			Why:     "The agent is writing what it needs from you. It shows here when it is done.",
			Actions: []Action{act("stop", "Stop it")}}
	case a.Status == "running":
		// a factory card shown here because it stalled (BuildBoard); its labels still name its column
		return &Ask{Title: "Running, but no activity for " + strings.TrimPrefix(stalledFlag(c), "stalled "), From: "builder",
			Why:     "The agent has been quiet a long time. It may be on a slow step or hung. Stop it, or send it a message.",
			Actions: []Action{act("stop", "Stop it"), reply("Reply", "Message to the agent")}}
	case note == limitNote:
		return limitAsk(a)
	case strings.HasPrefix(note, "history unavailable"):
		return &Ask{Title: "History unavailable: decision paused", From: "builder",
			Why: "The factory could not read the session history needed for this decision within two minutes of the turn stopping. " +
				"It has not applied a verdict or moved the work forward. Re-run the step or reply with instructions.",
			Actions: []Action{primary("rerun", "Re-run the step"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
	case a.L(labelAwaiting) == "repo":
		return repoAsk(note, a.L(labelCandidates))
	case note == "stuck" && (a.L(labelStuckFrom) == "review" || a.L(labelStuckFrom) == "plan-review"):
		if r, ok := blockedReviewer(c); ok {
			return blockedReviewerAsk(r)
		}
		fallthrough
	case note == "stuck":
		from := a.L(labelStuckFrom)
		return &Ask{Title: "Stuck in " + stageName(from) + ": nothing is queued", From: "builder",
			Why: stuckWhy[from] + ". The factory had no next step for it, so it moved here. The agent's last message is below.",
			Actions: []Action{primary("rerun", "Re-run the step"), reply("Reply", "Message to the agent"),
				act("done", "Mark done"), act("close", "Drop it")}}
	case a.L(labelAwaiting) == "plan":
		return &Ask{Title: "Approve this plan?", From: "builder",
			Why: "It is an architecture change and building it has side effects (or they were not ruled out), so nothing gets built until you OK the plan. A code review still runs before it lands.",
			Actions: []Action{primary("approve", "Approve plan & build"),
				reply("Send back with changes", "What should change in the plan?"), act("close", "Drop it")}}
	case strings.HasPrefix(note, "secret scan"):
		return &Ask{Title: "Secret scan stopped this landing", From: "builder",
			Why: "This repo is public. The factory's scan " + strings.TrimPrefix(note, "secret scan ") +
				". Nothing was published. Ask the agent to remove it (rewrite its own unpushed commits), then approve to scan again.",
			Actions: []Action{{ID: "reply", Label: "Tell it to fix", Primary: true, Hint: "e.g. remove it and rewrite your commits"}, act("approve", "Scan again & land"), act("close", "Drop it")}}
	case strings.HasPrefix(note, "public repo: finished without review"):
		return &Ask{Title: "Pushed to a public repo without review", From: "builder",
			Why: "The agent took the direct track in a public repo. Check what it pushed; anything private there is already public. " +
				strings.TrimPrefix(note, "public repo: finished without review"),
			Actions: []Action{reply("Reply", "Message to the agent"), act("done", "Checked, mark done")}}
	case note == previewNote:
		return &Ask{Title: "Try it, then approve landing?", From: "builder",
			Why: "It is an architecture change, built and passed by code review; nothing is live yet. The agent's before and after and its link to try it are below.",
			Actions: []Action{primary("approve", "Approve & land"),
				reply("Ask for changes", "What should it change?"), act("close", "Drop it")}}
	case strings.HasPrefix(note, "review found an architecture change"):
		return &Ask{Title: "Approve landing?", From: "reviewer",
			Why: "The code passed review, but the reviewer found it changes the architecture, which needs your OK.",
			Actions: []Action{primary("approve", "Approve & land"),
				reply("Ask for changes", "What should it change?"), act("close", "Drop it")}}
	case strings.HasPrefix(note, "review passed"):
		return &Ask{Title: "Approve landing?", From: "builder",
			Why: "Another agent reviewed the work and passed it. This card was sent to review by hand, so you decide whether it lands.",
			Actions: []Action{primary("approve", "Approve & land"),
				reply("Ask for changes", "What should it change?"), act("close", "Drop it")}}
	case note == noVerdictNote:
		return &Ask{Title: "The review didn't finish properly", From: "reviewer",
			Why: "The reviewer stopped without a verdict, so the factory can't tell whether the work passed. " +
				"Its last message is below. Re-running starts a fresh review.",
			Actions: []Action{primary("rerun", "Re-run the review"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
	case note == "rework cap reached" || note == "reviewer escalated":
		return briefAsk(a)
	case a.L(labelVerdict) == "close":
		return &Ask{Title: "Reviewer thinks this isn't needed", From: "reviewer",
			Why:     "It says the work is a duplicate, already done, or not worth doing. Its reasons are below.",
			Actions: []Action{primary("close", "Close it"), reply("Keep going", "Why it is still needed")}}
	case isDepAskNote(note):
		return depAsk(note)
	case note == "landing not verified":
		return &Ask{Title: "Landing didn't reach main", From: "builder",
			Why:     "The agent finished landing, but its commit is not on origin/main. Its last message is below.",
			Actions: []Action{reply("Tell it to retry", "e.g. rebase and land it again"), act("done", "It's fine, mark done")}}
	case strings.HasPrefix(note, "landed; still to do: "):
		return &Ask{Title: "Landed, with work left", From: "builder",
			Why: "The agent landed its change but says part of this card is not done: " +
				strings.TrimPrefix(note, "landed; still to do: ") + ". Send it on to finish, or mark it done.",
			Actions: []Action{{ID: "reply", Label: "Send it on", Primary: true, Hint: "e.g. finish the rest"}, act("done", "Mark done")}}
	case note == "landed; did not say what remains":
		return &Ask{Title: "Landed: did it finish?", From: "builder",
			Why: "The agent landed its change but did not end with a REMAINING line, so the factory can't tell " +
				"whether anything is left. Its last message is below.",
			Actions: []Action{primary("done", "Mark done"), reply("Reply", "e.g. what is left?")}}
	case strings.HasPrefix(note, "landed diff differs from the reviewed one"):
		return &Ask{Title: "Landed something other than what was reviewed", From: "builder",
			Why: "What reached main is not exactly the reviewed commits (" +
				strings.TrimPrefix(strings.TrimPrefix(note, "landed diff differs from the reviewed one"), ": ") +
				"). Check what landed. Its last message is below.",
			Actions: []Action{reply("Reply", "Message to the agent"), act("done", "Checked, mark done")}}
	case a.L(labelStage) == "zach":
		return &Ask{Title: handTitle, From: "builder",
			Why:     "This card was put in Needs Zach by hand. The agent's last message is below.",
			Actions: []Action{reply("Reply", "Message to the agent"), act("done", "Mark done")}}
	case a.RequiresAttention && a.AttentionReason != "" && a.AttentionReason != "finished":
		return &Ask{Title: "Waiting on you: " + a.AttentionReason, From: "builder",
			Why:     "The agent stopped and is waiting for you. Open the transcript to see the prompt, or reply.",
			Actions: []Action{reply("Reply", "Message to the agent"), act("done", "Mark done")}}
	}
	return &Ask{Title: finishedTitle, From: "builder",
		Why:     "The agent finished and you haven't looked at it yet. Its final answer is below.",
		Actions: []Action{primary("done", "Got it, mark done"), reply("Reply", "Follow-up for the agent")}}
}

// repoAsk is the question for a repo picker in Needs Zach: which repo the card starts in.
func repoAsk(note, candidates string) *Ask {
	ask := &Ask{Title: repoTitle, From: "builder",
		Why: "The factory could not tell which repo this belongs in. Pick one and the card starts planning there. The picker's reasoning is below."}
	switch note {
	case "routing failed":
		ask.Why = "A repo was picked, but starting the card there failed (the reason is in the factory log). Pick a repo to try again."
	case "routing interrupted":
		ask.Why = "The factory restarted while starting this card. Check the board for a card that already started; if there is none, pick a repo."
	}
	for _, r := range strings.FieldsFunc(candidates, func(c rune) bool { return c == ' ' || c == ',' || c == ';' }) {
		if r = strings.TrimSpace(r); r != "" {
			ask.Actions = append(ask.Actions, Action{ID: "approve", Label: "Start in " + r, Primary: len(ask.Actions) == 0, Repo: r})
		}
	}
	ask.Actions = append(ask.Actions, Action{ID: "pickrepo", Label: "Other repo…", Primary: len(ask.Actions) == 0},
		reply("Reply", "e.g. it's about the PDW Android app"), act("close", "Drop it"))
	return ask
}

// noVerdictNote marks a card whose reviewer ended without a VERDICT line.
const noVerdictNote = "reviewer gave no verdict"

// Button labels on brief cards. briefMsg describes each button by these exact labels.
const (
	btnRecommend    = "Go with its recommendations"
	btnRecommendOne = "Go with its recommendation"
	btnBuildAsIs    = "Build the plan as it is"
	btnLandAsIs     = "Land as it is"
	btnTell         = "Tell it what to do"
	btnAnswer       = "Answer"
	btnDrop         = "Drop it"
)

const briefWhy = "Below is the builder's summary: what's unresolved, what it recommends, and what each button does. " +
	"The reviewer's full notes are on its card."

// briefAsk is the Ask for a card that reached the rework cap or was escalated by its reviewer. The builder was
// sent briefMsg, so Detail is its brief, and the buttons are the ones the brief describes.
func briefAsk(a Agent) *Ask {
	plan := planPending(a) && strings.HasPrefix(a.L(labelVerdict), "plan")
	if a.L(labelNote) == "reviewer escalated" {
		return &Ask{Title: "A reviewer needs your decision", From: "builder", Why: briefWhy,
			Actions: []Action{{ID: "reply", Label: btnAnswer, Primary: true, Hint: "Your decision"},
				act("recommend", btnRecommendOne), act("close", btnDrop)}}
	}
	asIs, what := btnLandAsIs, "code"
	if plan {
		asIs, what = btnBuildAsIs, "plan"
	}
	return &Ask{Title: "Reviews didn't settle: your call on the " + what, From: "builder", Why: briefWhy,
		Actions: []Action{primary("recommend", btnRecommend), reply(btnTell, "e.g. ignore the second point, fix the first"),
			act("approve", asIs), act("close", btnDrop)}}
}

// blockedReviewerAsk offers a fresh review for a card whose reviewer is stuck on a permission
// prompt or stalled. It never offers to answer the prompt.
func blockedReviewerAsk(r Child) *Ask {
	acts := []Action{primary("rerun", "Re-run the review"), reply("Reply", "Message to the agent"), act("close", "Drop it")}
	if r.Status == "error" {
		return &Ask{Title: "Reviewer stopped with an error", From: "reviewer", Actions: acts,
			Why: "The reviewer failed. Re-run retires it and starts a fresh review."}
	}
	if r.Blocked == "waiting on permission" {
		why := "Claude Code asked the reviewer for permission"
		if r.Permission != "" {
			why += " (" + r.Permission + ")"
		}
		return &Ask{Title: "Reviewer stuck on a permission prompt", From: "reviewer", Actions: acts,
			Why: why + ", which it does even in bypass mode, and the factory never answers prompts. Re-run stops this " +
				"reviewer and starts a fresh one. Or approve the prompt in Paseo and the review carries on."}
	}
	return &Ask{Title: "Reviewer " + strings.Replace(r.Blocked, "stalled ", "stalled: no activity for ", 1), From: "reviewer",
		Actions: acts, Why: "The reviewer has been running a long time with no activity; it may be hung. Re-run stops it " +
			"and starts a fresh review. If it finishes first, its verdict still moves the card on."}
}

func stalledFlag(c Card) string {
	for _, fl := range c.Flags {
		if strings.HasPrefix(fl, "stalled ") {
			return fl
		}
	}
	return ""
}

func stageName(id string) string {
	for _, st := range Stages {
		if st[0] == id {
			return st[1]
		}
	}
	return id
}

var trailer = regexp.MustCompile(`(?i)^\s*\**\s*(TIER|VERDICT):`)

type detailKey struct {
	src  string // agent id the text came from
	last string // its lastActivityAt when fetched
}

// fillAsks adds Detail to each Needs Zach card's Ask from the builder's or its latest reviewer's
// last message. Fetches only when that agent has new activity and fetch is set; without fetch a card
// keeps its cached detail, even an older one, and a card with none waits for the next loop.
func (f *Factory) fillAsks(cards []Card, agents []Agent, fetch bool) {
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	for n := range cards {
		ask := cards[n].Ask
		if ask == nil {
			continue
		}
		src := askSource(cards[n], ask)
		a := byID[src]
		key := detailKey{src, iso(a.LastActivityAt)}
		f.mu.Lock()
		cached, ok := f.details[cards[n].ID]
		f.mu.Unlock()
		if fetch {
			req := f.cardRequest(byID[cards[n].ID])
			ask.Request = &req
		} else if ok {
			ask.Request = cached.request
		}
		if ok && (cached.key == key || !fetch) {
			ask.Detail, ask.Questions, ask.Overview = cached.text, cached.questions, cached.overview
			ask.Evidence = cached.evidence
			ask.Report = cached.report
			if fetch {
				cached.request = ask.Request
				f.mu.Lock()
				f.details[cards[n].ID] = cached
				f.mu.Unlock()
			}
			continue
		}
		if !fetch {
			continue // the next loop reads fresh session detail
		}
		text, err := f.askReply(a)
		if strings.TrimSpace(text) == "" {
			if err != nil && a.SessionID == "" {
				continue
			}
			ask.Detail = unreadableReply(err)
		} else {
			// parse the whole reply: questions often close a long plan, past the Detail cut
			text, ask.Evidence = parseEvidence(text)
			text, ask.Questions, ask.Overview = parseReply(text)
			ask.Report = text
			ask.Detail = trunc(text, 6000)
		}
		f.mu.Lock()
		f.details[cards[n].ID] = detailEntry{key, ask.Detail, ask.Questions, ask.Overview, ask.Evidence, ask.Report, ask.Request}
		f.mu.Unlock()
	}
}

type detailEntry struct {
	key       detailKey
	text      string
	questions []Question
	overview  *Overview
	evidence  Evidence
	report    string
	request   *CardRequest
}

// When an agent needs Zach's input it can ask multiple-choice questions, and he can always write his
// own answer instead. Agents write each question as a block:
//
//	QUESTION: Where should it live?
//	- Keep it where it is (recommended)
//	- New repo
var (
	questionLine = regexp.MustCompile(`^\s*\**QUESTION:\**\s*(.+?)\s*$`)
	optionLine   = regexp.MustCompile(`^\s*(?:[-*]|\d{1,2}[.)]|[A-Fa-f][.)])\s+(.+?)\s*$`)
)

const maxQuestions, maxOptions = 4, 6

// ParseQuestions takes the QUESTION: blocks (2 or more options) out of an agent's reply and returns
// the rest of the text and the questions, at most 4 with 6 options each. A block with fewer than 2
// options stays in the text.
func ParseQuestions(text string) (string, []Question) {
	lines := strings.Split(text, "\n")
	var out []string
	var qs []Question
	for i := 0; i < len(lines); i++ {
		m := questionLine.FindStringSubmatch(lines[i])
		if m == nil || len(qs) == maxQuestions {
			out = append(out, lines[i])
			continue
		}
		q := Question{Text: strings.Trim(m[1], "* ")}
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" && len(q.Options) == 0 {
			j++ // a blank line between the question and its options
		}
		for ; j < len(lines); j++ {
			o := optionLine.FindStringSubmatch(lines[j])
			if o == nil {
				break
			}
			if len(q.Options) < maxOptions {
				q.Options = append(q.Options, o[1])
			}
		}
		if len(q.Options) < 2 {
			out = append(out, lines[i])
			continue
		}
		qs = append(qs, q)
		i = j - 1
	}
	return strings.TrimSpace(strings.Join(out, "\n")), qs
}

// storedReply is agent a's last reply to a message, from its own session file, which marks who
// said what. It never asks Paseo; with no readable file it returns an ErrNoHistory error. For a
// replaced card whose replacement has no reply yet, it falls back through the replacement chain.
func (f *Factory) storedReply(a Agent) (string, error) {
	text, err := storedReplyOne(f.cfg.Home, a)
	if (err != nil || strings.TrimSpace(text) == "") && a.L(labelRole) == "" && a.L(labelReplaces) != "" {
		lin := cardLineage(f.byID(), a.ID)
		for i := len(lin) - 2; i >= 0; i-- {
			if prev, perr := storedReplyOne(f.cfg.Home, lin[i]); perr == nil && strings.TrimSpace(prev) != "" {
				return prev, nil
			}
		}
	}
	return text, err
}

func storedReplyOne(home string, a Agent) (string, error) {
	evs, err := storedEvents(home, a)
	if err != nil {
		return "", err
	}
	// the trailing run of text blocks: one reply can span several (a review written in parts)
	var last []string
	for _, e := range evs {
		switch e.Kind {
		case "user", "tool", "result":
			last = nil
		case "assistant":
			last = append(last, e.Text)
		}
	}
	return stripTrailer(strings.Join(last, "\n\n")), nil
}

// askReply is the text shown on a Needs Zach card and in its email, read from the session file only
// (the card may have stopped long ago: asking Paseo would load it). It starts with storedReply;
// when an agent stopped mid-turn (for example, killed during a tool call), it falls back to the
// last assistant text in the turn, or a plain mid-run note naming the last tool and capped
// reasoning from the transcript, never raw tool output.
func (f *Factory) askReply(a Agent) (string, error) {
	if a.ID == "" {
		return "", errors.New("agent not found")
	}
	text, err := f.storedReply(a)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	evs, err := storedEvents(f.cfg.Home, a)
	if err != nil {
		return "", err
	}
	var run, lastRun []string
	var lastTool, lastToolTs, lastThinking string
	for _, e := range evs {
		switch e.Kind {
		case "user":
			run, lastRun = nil, nil
			lastTool, lastToolTs, lastThinking = "", "", ""
		case "assistant":
			if strings.TrimSpace(e.Text) != "" {
				run = append(run, e.Text)
				lastRun = run
			}
		case "tool":
			run = nil
			lastTool = e.Name
			if lastTool == "" {
				lastTool = e.Native
			}
			if lastTool == "" {
				lastTool = "tool"
			}
			if e.Ts != "" {
				lastToolTs = e.Ts
			}
		case "result":
			run = nil
		case "thinking":
			if t := strings.TrimSpace(e.Text); t != "" {
				lastThinking = t
			}
		}
	}
	if len(lastRun) > 0 {
		if s := stripTrailer(strings.Join(lastRun, "\n\n")); s != "" {
			return s, nil
		}
	}
	if lastTool != "" || lastThinking != "" {
		msg := "Stopped mid-run before writing a reply."
		if lastTool != "" {
			when := ""
			if t := parseTime(lastToolTs); !t.IsZero() {
				loc := f.cfg.Location
				if loc == nil {
					loc = time.UTC
				}
				when = " at " + t.In(loc).Format("15:04:05 MST")
			}
			msg = fmt.Sprintf("Stopped mid-run before writing a reply (last tool: %s%s).", lastTool, when)
		}
		if lastThinking != "" {
			msg += "\n\nLast reasoning:\n" + truncWords(stripTrailer(lastThinking), 600)
		}
		return msg, nil
	}
	return "", errors.New("transcript has no assistant reply or activity after the prompt")
}

func unreadableReply(err error) string {
	if err != nil {
		return fmt.Sprintf("(The agent's last message could not be read from the transcript: %v. Open the board to see it.)", err)
	}
	return "(The agent's last message could not be read from the transcript: transcript had no reply. Open the board to see it.)"
}

func stripTrailer(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !trailer.MatchString(l) {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
