package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fresh agents per stage. Every move of a card to new work starts a new agent on the same model in the
// same worktree instead of resuming the card's session, which by then re-reads hundreds of thousands
// of tokens on every call: a passed plan (plan reviewer, arbiter or Zach) going to build, plan-review
// fixes, code-review or arbiter fixes, the land prompt (sendLanding), and a failed landing, a dropped
// dependency or work left after a landing going back to building. The new agent gets a compact handoff (stagePrompt) and the ids of
// the earlier agents, whose full transcripts it can read in PDW or the transcript API. The swap is a model hand-off
// of kind "stage" (handoff.go): same steps, the card's labels carried (reviewed commit, code approval,
// held approval, review generation, recovery counts, email thread), the old agent archived once the
// new one is confirmed. A lander is checked from its own labels by the landing review like any card
// agent (landreview.go). Anything that keeps the swap from starting sends the stage message to the old
// session, as before; so does a parked (archived) card, which a send wakes (reap.go). Messages within a
// stage stay in the session: Zach's replies, steers, nudges, and summary, brief or preview requests.

// stageHandoffMarker opens every stage handoff prompt; a later swap reads its sections back.
const stageHandoffMarker = "Factory stage handoff."

const (
	stageGitCap  = 4000 // characters of the commit list and of the diffstat
	stageFollows = 8    // newest follow-ups from Zach in a handoff
)

// stageHandoff is the prompt of a swap under way and the stage message to send the old session if
// it never starts, kept in the state dir so a restart retries the same prompt.
type stageHandoff struct {
	Prompt string `json:"prompt"`
	Msg    string `json:"msg"`
}

func (f *Factory) stageHandoffPath(id string) string {
	return filepath.Join(f.cfg.StateDir, "handoffs", filepath.Base(id)+".json")
}

func (f *Factory) saveStageHandoff(id string, st stageHandoff) error {
	path := f.stageHandoffPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (f *Factory) loadStageHandoff(id string) (stageHandoff, error) {
	var st stageHandoff
	b, err := os.ReadFile(f.stageHandoffPath(id))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Prompt == "" {
		return st, errors.New("empty stage handoff")
	}
	return st, nil
}

func (f *Factory) dropStageHandoff(id string) {
	if err := os.Remove(f.stageHandoffPath(id)); err != nil && !os.IsNotExist(err) {
		f.logf("%s: stage handoff file: %v", short(id), err)
	}
}

// advance moves card agent a (its labels already set for the new stage) to the step msg describes:
// in a fresh agent when it can, else by resuming a's session. step names the stage in the handoff.
func (f *Factory) advance(a Agent, step, msg string) error {
	if f.cfg.NoStageHandoff || a.L(labelRole) != "" || f.handoffBusy(a) {
		return f.send(a.ID, msg)
	}
	choice, ok := sameChoice(a)
	if !ok {
		f.logf("%s: model unknown, resuming its session for %s", short(a.ID), step)
		return f.send(a.ID, msg)
	}
	prompt, err := f.stagePrompt(a, choice, step, msg)
	if err != nil {
		f.logf("%s: no stage handoff (%v), resuming its session", short(a.ID), err)
		return f.send(a.ID, msg)
	}
	if live, err := f.stillStopped(a); err != nil || !live {
		return f.send(a.ID, msg)
	}
	if err := f.saveStageHandoff(a.ID, stageHandoff{Prompt: prompt, Msg: msg}); err != nil {
		f.logf("%s: stage handoff not saved (%v), resuming its session", short(a.ID), err)
		return f.send(a.ID, msg)
	}
	s := handoffState{Phase: "starting", Choice: choice.ID, At: f.Now(), Tries: 1, Kind: "stage"}
	if err := f.saveHandoff(a.ID, s, nil); err != nil {
		f.dropStageHandoff(a.ID)
		return f.send(a.ID, msg)
	}
	if err := f.startHandoff(a, s, choice, prompt); err != nil {
		// it may have started anyway: tickHandoffs adopts it, retries, or resumes a's session
		f.logf("%s: %v", short(a.ID), err)
	}
	return nil
}

// stageGaveUp: the fresh agent never started, so the old session gets its stage message.
func (f *Factory) stageGaveUp(a Agent) error {
	st, err := f.loadStageHandoff(a.ID)
	f.dropStageHandoff(a.ID)
	if err != nil || st.Msg == "" {
		return f.markStuck(a, "the next stage's fresh agent did not start")
	}
	f.logf("%s: fresh agent did not start, resuming its session", short(a.ID))
	return f.send(a.ID, st.Msg)
}

// sameChoice is the model a's card runs on: its factory.agent choice, else the one whose provider
// and model match a's.
func sameChoice(a Agent) (AgentChoice, bool) {
	if c, ok := agentByID(a.L(labelAgent)); ok {
		return c, true
	}
	if a.Provider == "" || a.Model == "" {
		return AgentChoice{}, false
	}
	for _, c := range AgentChoices {
		if c.Provider == a.Provider+"/"+a.Model {
			return c, true
		}
	}
	return AgentChoice{}, false
}

// stagePrompt is the fresh agent's first message: what it is and where the past is, the card's
// original instructions verbatim, Zach's follow-ups, the approved plan, the branch, the last
// agent's message and the stage message. An unreadable first session is an error.
func (f *Factory) stagePrompt(a Agent, choice AgentChoice, step, msg string) (string, error) {
	byID := f.byID()
	byID[a.ID] = a
	lin := cardLineage(byID, a.ID)
	if len(lin) == 0 {
		lin = []Agent{a}
	}
	rootEvs, err := storedEvents(f.cfg.Home, lin[0])
	if err != nil {
		return "", err
	}
	first, _, _ := handoffParts(rootEvs)
	if strings.TrimSpace(first) == "" {
		return "", fmt.Errorf("%w: no first message in %s's session", ErrNoHistory, short(lin[0].ID))
	}
	plan := f.lineagePlan(lin)
	last, _ := storedReplyOne(f.cfg.Home, a)
	last = strings.TrimSpace(last)

	var b strings.Builder
	fmt.Fprintf(&b, "%s You are this factory card's fresh agent (%s) for its next step: %s. Earlier agents of the card "+
		"worked in this same worktree and branch; the factory moved its labels, email thread, review rounds and board "+
		"history to you, and starts each stage in a fresh agent so the context stays small. This message has what you "+
		"need: the card's original instructions, Zach's follow-ups, the approved plan, the branch's commits, the last "+
		"agent's message and this step's instructions. Do not start over: first check where the work stands (git status, "+
		"git log --oneline origin/main..HEAD, the files the plan names).\n\n", stageHandoffMarker, choice.Label, step)
	b.WriteString("Earlier agents of this card, oldest first:\n")
	for _, x := range lin {
		fmt.Fprintf(&b, "- Paseo agent %s (%s", x.ID, modelLabel(x))
		if x.SessionID != "" {
			fmt.Fprintf(&b, ", %s session %s", x.Provider, nativeSessionID(x.SessionID))
		}
		b.WriteString(")\n")
	}
	port := f.cfg.Port
	if port == 0 {
		port = 7478
	}
	fmt.Fprintf(&b, "Read their full transcripts only when this handoff is not enough, and only read-only: in PDW "+
		"(`pdw readme agent-sessions`, then marts_ai_conversations.sessions and .events by session_id, or this "+
		"worktree's cwd, or `pdw search`; PDW can lag by a few minutes), or from the factory's transcript API, newest "+
		"page first: `curl -s 'localhost:%d/api/transcript/<agent id>?tail=1'`, then `before=<line>` for older pages. "+
		"Never run `paseo logs` on an earlier agent: it loads that agent in Paseo and can resume an archived one.\n\n", port)
	b.WriteString("=== Original instructions ===\nThey still apply (rules, gates, the public-repo scan, the opening lines), " +
		"but this step replaces their \"Step 1\".\n\n")
	b.WriteString(strings.TrimSpace(first))
	b.WriteString("\n")
	if fus := f.cardRequest(a).FollowUps; len(fus) > 0 {
		if len(fus) > stageFollows {
			fus = fus[len(fus)-stageFollows:]
		}
		b.WriteString("\n=== Zach's follow-ups (newest last; they override the request where they conflict) ===\n")
		for _, fu := range fus {
			fmt.Fprintf(&b, "%s:\n%s\n\n", fu.label(), trunc(strings.TrimSpace(fu.Text), reviewBlockCap))
		}
	}
	if plan != "" {
		fmt.Fprintf(&b, "\n=== Approved plan ===\n%s\n", trunc(plan, handoffTextCap))
	}
	fmt.Fprintf(&b, "\n=== Branch ===\n%s\n", f.stageBranch(a.Cwd))
	if last != "" && last != plan && !strings.Contains(plan, last) {
		fmt.Fprintf(&b, "\n=== Last message of the previous agent ===\n%s\n", trunc(last, handoffTextCap))
	}
	if step == "landing" {
		b.WriteString("\nLanding: run the deploy steps of the reviewed build summary. If the messages above do not " +
			"list them, read them from the previous agent's transcript before you land.\n")
	}
	fmt.Fprintf(&b, "\n=== This step ===\n%s\n", msg)
	if fromInbox(a) {
		b.WriteString("\n" + f.delegateRule(false))
	}
	return b.String(), nil
}

// stageBranch is the branch's commits and diffstat against origin/main, capped.
func (f *Factory) stageBranch(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commits, err := f.GitOut(ctx, cwd, "log", "--oneline", "origin/main..HEAD")
	commits = strings.TrimSpace(commits)
	if err != nil || commits == "" {
		return "(no commits on the branch yet, or git could not list them: run git log yourself)"
	}
	out := "git log --oneline origin/main..HEAD:\n" + trunc(commits, stageGitCap)
	if stat, err := f.GitOut(ctx, cwd, "diff", "--stat", "origin/main...HEAD"); err == nil && strings.TrimSpace(stat) != "" {
		out += "\n\ngit diff --stat origin/main...HEAD:\n" + trunc(strings.TrimSpace(stat), stageGitCap)
	}
	return out + "\n\nRead the diff itself with git diff origin/main...HEAD."
}

// lineagePlan is the card's approved plan, newest agent first: the reply before a "Build it now"
// message in its session, the plan section of a handoff it started with, or (the planner itself, at
// the plan's approval) its newest reply with a TIER: line.
func (f *Factory) lineagePlan(lin []Agent) string {
	for i := len(lin) - 1; i >= 0; i-- {
		evs, err := storedEvents(f.cfg.Home, lin[i])
		if err != nil {
			continue
		}
		first, plan, _ := handoffParts(evs)
		if plan != "" {
			return plan
		}
		if p := handoffSection(first, "Approved plan"); p != "" {
			return p
		}
		if p := lastTierReply(evs); p != "" {
			return p
		}
	}
	return ""
}

// handoffSection is the text under "=== name ===" in a handoff prompt, up to the next section.
func handoffSection(prompt, name string) string {
	_, rest, ok := strings.Cut(prompt, "=== "+name+" ===\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n=== "); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// lastTierReply is the newest assistant reply (a run of text blocks) with a TIER: line: a plan.
func lastTierReply(evs []Event) string {
	var run []string
	best := ""
	flush := func() {
		if r := strings.TrimSpace(strings.Join(run, "\n\n")); r != "" && ParseTier(r) != "" {
			best = r
		}
		run = nil
	}
	for _, e := range evs {
		switch e.Kind {
		case "assistant":
			if strings.TrimSpace(e.Text) != "" {
				run = append(run, e.Text)
			}
		case "tool", "result":
			run = nil
		case "user":
			flush()
		}
	}
	flush()
	return best
}

// labeled is a with factory labels l (keys without the prefix) applied, as just written to Paseo.
func labeled(a Agent, l map[string]string) Agent {
	out := make(map[string]string, len(a.Labels)+len(l))
	for k, v := range a.Labels {
		out[k] = v
	}
	for k, v := range l {
		out[labelPrefix+k] = v
	}
	a.Labels = out
	return a
}
