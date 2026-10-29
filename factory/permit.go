package factory

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// permitTimeout: a factory agent still on a permission prompt this long after its last activity,
// though denyPrompts refused it, is stuck; Zach gets one email about it.
const permitTimeout = 20 * time.Minute

// mailStuckPrompts emails Zach once per move of a card (factory.permit-told) when one of the factory's
// agents still waits on a permission prompt permitTimeout after its last activity: the refusal failed
// or did not end the prompt. It never answers the prompt; Zach approves or denies it in Paseo, or stops
// the agent. Dry and read-only runs send nothing.
func (f *Factory) mailStuckPrompts(agents []Agent, now time.Time) {
	if f.passive() || f.Mail == nil {
		return
	}
	managed, byID, owners, told := quietManaged(agents), f.byID(), cardOwners(agents, nil), map[string]bool{}
	for _, a := range agents {
		if !managed[a.ID] || a.archived() || a.Status != "running" || a.Pending == "" ||
			a.LastActivityAt.IsZero() || now.Sub(a.LastActivityAt) < permitTimeout {
			continue
		}
		o, ok := promptOwner(byID, owners, a)
		if !ok || told[o.ID] {
			continue
		}
		mark := permitMark(o)
		if o.L(labelPermitTold) == mark || f.mailWaits(o.ID) {
			continue
		}
		if lineageEmailThread(byID, o) == "" && lineageLabel(byID, o, labelZachThread) == "" && f.cfg.ZachEmail == "" {
			continue // mailZach logs the missing address
		}
		told[o.ID] = true
		sent, err := f.mailOnce(o, labelPermitTold, mark, f.stuckPromptEmail(o, a))
		if err != nil {
			f.logf("%s: emailing Zach about %s's stuck permission prompt: %v", short(o.ID), short(a.ID), err)
			continue
		}
		if sent {
			f.logf("%s: %s is still on a permission prompt (%s): emailed Zach", short(o.ID), short(a.ID), trunc(a.Pending, 120))
		}
	}
}

// permitMark names the owner's current move (its stage and when it entered it), so a later move that
// gets stuck again is emailed again.
func permitMark(o Agent) string {
	if st := o.L(labelStage); st != "" {
		return st + "@" + o.L(labelSince)
	}
	return o.L(labelRole) + "@" + iso(o.CreatedAt)
}

// promptOwner is the factory agent whose thread hears about a's stuck prompt: the card the board files a
// under (cardOwners: the card a reviewer reviews, or the top of a sub-agent's parents), else a itself
// when the factory started it. Agents the factory did not start have no owner.
func promptOwner(byID map[string]Agent, owners map[string]string, a Agent) (Agent, bool) {
	if id, ok := owners[a.ID]; ok {
		if o, ok := byID[id]; ok && factoryAgent(o) {
			return o, true
		}
	}
	if factoryAgent(a) {
		return a, true
	}
	return Agent{}, false
}

// stuckPromptEmail tells Zach that agent a, working for owner o, is still on a permission prompt.
func (f *Factory) stuckPromptEmail(o, a Agent) email {
	title := o.Title
	if c, ok := f.card(o.ID); ok && c.Title != "" {
		title = c.Title
	}
	who := "The agent"
	if a.ID != o.ID {
		who = "A helper agent"
	}
	lines := []string{
		who + " working on \"" + title + "\" is still waiting on a permission prompt, " +
			fmt.Sprintf("%d minutes after the factory refused it.", int(permitTimeout/time.Minute)),
		"The prompt: " + trunc(a.Pending, 300),
		"The factory never approves a prompt. Approve or deny it in Paseo, " +
			"or open the board to stop the agent or send it a message.",
	}
	var t, h strings.Builder
	h.WriteString(`<div style="font-family:-apple-system,system-ui,sans-serif;font-size:16px;line-height:1.5;color:#1f2328;max-width:640px">`)
	for _, l := range lines {
		t.WriteString(l + "\n")
		fmt.Fprintf(&h, "<p>%s</p>", html.EscapeString(l))
	}
	if link := f.cardLink(o.ID); link != "" {
		fmt.Fprintf(&t, "\nOpen the board: %s\n", link)
		fmt.Fprintf(&h, `<p><a href="%s">Open the board</a></p>`, html.EscapeString(link))
	}
	h.WriteString("</div>")
	return email{Subject: "An agent is still stuck on a permission prompt", Text: t.String(), HTML: h.String()}
}
