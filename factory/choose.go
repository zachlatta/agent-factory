package factory

import (
	"errors"
	"strings"
)

// One way to answer an ask. Every ask carries Choices: Land / Change / Drop for finished work, Yes /
// No / Change / Drop for a decision (No is one tap and sends "No."; Change takes his words) (a repo choice lists its repos instead), Answer / Drop for an agent's open
// question (its options answer through Answer), and board-only problems keep their own action
// (Re-run, Resume, Let an agent decide, Mark done) with Change and Drop. The board posts the
// choice (POST /api/choose); an email reply is parsed into the same choice (answer.go). Both run
// choose, and either one moves the card out of Needs Zach, so the other surface then finds nothing
// waiting.

// Kinds of ask (Ask.Kind).
const (
	askReady    = "ready"
	askDecision = "decision"
	askQuestion = "question"
	askProblem  = "problem"
)

// Choice is one answer to an ask.
type Choice struct {
	ID      string `json:"id"` // land, change, drop, yes, no, pick:<repo>, pick, or a problem's action
	Label   string `json:"label"`
	Primary bool   `json:"primary,omitempty"`
	Text    bool   `json:"text,omitempty"` // opens a text box: what to change, or his answer
	Hint    string `json:"hint,omitempty"` // the text box's placeholder
	Repo    string `json:"repo,omitempty"` // pick:<repo>: start the card in this repo
	action  string // the Act it runs
	says    string // a send choice's fixed message (No), so it needs no text box
}

func landChoice() Choice { return Choice{ID: "land", Label: "Land", Primary: true, action: "approve"} }
func dropChoice() Choice { return Choice{ID: "drop", Label: "Drop", action: "close"} }
func changeChoice(hint string) Choice {
	if hint == "" {
		hint = "What should change?"
	}
	return Choice{ID: "change", Label: "Change", Text: true, Hint: hint, action: "send"}
}

// choicesFor is the fixed set of answers for ask, by its Kind.
func choicesFor(ask *Ask) []Choice {
	switch ask.Kind {
	case askReady:
		return []Choice{landChoice(), changeChoice(""), dropChoice()}
	case askQuestion:
		// not a yes/no question: Zach taps one of its options (sent through Answer) or writes his own
		return []Choice{{ID: "answer", Label: "Answer", Primary: true, Text: true, Hint: "Your answer", action: "send"}, dropChoice()}
	case askDecision:
		if ask.Title == repoTitle {
			var out []Choice
			for _, x := range ask.Actions {
				switch {
				case x.ID == "approve" && x.Repo != "":
					out = append(out, Choice{ID: "pick:" + x.Repo, Label: x.Label, Primary: x.Primary, Repo: x.Repo, action: "approve"})
				case x.ID == "pickrepo":
					out = append(out, Choice{ID: "pick", Label: x.Label, Primary: x.Primary, action: "pickrepo"})
				}
			}
			return append(out, changeChoice("e.g. it's about the phone app"), dropChoice())
		}
		// Yes runs the ask's primary action (allow the build, mark a check done); No tells the agent no in
		// one tap; Change sends what to change instead
		yes := Choice{ID: "yes", Label: "Yes", Primary: true}
		for _, x := range ask.Actions {
			if x.Primary {
				yes.action = x.ID
			}
		}
		if yes.action == "close" {
			yes.Label = "Close it"
		}
		out := []Choice{yes, {ID: "no", Label: "No", action: "send", says: "No."}, changeChoice("")}
		if yes.action != "close" && actionFor(ask, "close") != nil {
			out = append(out, dropChoice())
		}
		return out
	}
	var out []Choice
	for _, x := range ask.Actions {
		switch x.ID {
		case "reply":
			c := changeChoice(x.Hint)
			c.Label, c.Primary = x.Label, x.Primary
			out = append(out, c)
		case "close":
			c := dropChoice()
			c.Label, c.Primary = x.Label, x.Primary
			out = append(out, c)
		default:
			out = append(out, Choice{ID: x.ID, Label: x.Label, Primary: x.Primary, action: x.ID})
		}
	}
	return out
}

func choiceByID(ask *Ask, id string) *Choice {
	for i := range ask.Choices {
		if ask.Choices[i].ID == id {
			return &ask.Choices[i]
		}
	}
	return nil
}

// goChoice is the ask's Land, Yes, Done it or Scan again, the choice the approve reply words stand for, or nil.
func goChoice(ask *Ask) *Choice {
	for _, id := range []string{"land", "yes", "works", "scan"} {
		if c := choiceByID(ask, id); c != nil {
			return c
		}
	}
	return nil
}

// doneChoice is the choice the done reply words stand for: Mark done, or Done it on an action ask.
func doneChoice(ask *Ask) *Choice {
	if c := choiceByID(ask, "done"); c != nil {
		return c
	}
	return choiceByID(ask, "works")
}

// textChoice is the choice that sends Zach's own words (Change, or his Answer), or nil.
func textChoice(ask *Ask) *Choice {
	for i, c := range ask.Choices {
		if c.Text && c.action == "send" {
			return &ask.Choices[i]
		}
	}
	return nil
}

// dropOf is the choice that drops the card (Drop, or Yes on a suggestion to close it), or nil.
func dropOf(ask *Ask) *Choice {
	for i, c := range ask.Choices {
		if c.action == "close" {
			return &ask.Choices[i]
		}
	}
	return nil
}

var errNoChoice = errors.New("that answer is not offered: the card may have moved on, reload the board")

// choose answers the ask of card b.ID with choice b.Choice (and b.Message, b.Repo), as the board or an
// email reply (b.Via "email") does.
func (f *Factory) choose(b Body) (any, error) {
	a, ok := f.agentOK(b.ID)
	if !ok {
		return nil, errors.New("unknown card")
	}
	c, ok := f.card(a.ID)
	if !ok {
		return nil, errors.New("unknown card")
	}
	ask := f.replyAsk(c, a)
	if ask == nil {
		return nil, ErrNotWaiting
	}
	ch := choiceByID(ask, b.Choice)
	if ch == nil {
		return nil, errNoChoice
	}
	_, err := f.runChoice(a.ID, *ch, b)
	return map[string]any{"ok": err == nil, "choice": ch.ID}, err
}

// runChoice runs choice ch on card id with b's text, repo, attachments and Via.
func (f *Factory) runChoice(id string, ch Choice, b Body) (any, error) {
	text := strings.TrimSpace(b.Message)
	body := Body{ID: id, Message: text, Via: b.Via, Attachments: b.Attachments}
	switch ch.action {
	case "send":
		if ch.says != "" {
			return f.Act("send", Body{ID: id, Message: ch.says, Via: b.Via})
		}
		if body.Message == "" && len(b.Attachments) == 0 {
			return nil, errors.New("write what should change first")
		}
		return f.Act("send", body)
	case "approve", "pickrepo":
		body.Repo = ch.Repo
		if ch.action == "pickrepo" {
			if body.Repo = strings.TrimSpace(b.Repo); body.Repo == "" {
				return nil, errors.New("pick a repo first")
			}
		}
		return f.Act("approve", body)
	}
	return f.Act(ch.action, Body{ID: id, Via: b.Via})
}
