package factory

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Model hand-off. A card agent whose same-model recovery is used up (recover.go), or whose
// provider limit resets more than handoffResetGap away, is handed once to another model in the same
// worktree. The replacement is started with factory.replaces=<old> and the carried labels
// (replace.go); the old agent is archived only after the replacement is confirmed. The steps are
// stored in factory.handoff on the old agent, so a restart finishes them. Nothing here mails Zach.
const (
	handoffResetGap     = 2 * time.Hour
	handoffStartGrace   = 2 * time.Minute // "starting" with no replacement: start once more after this
	handoffConfirmGrace = 5 * time.Minute // "started" never confirmed: give up after this
	handoffTries        = 2
	handoffTextCap      = 12000
)

// handoffState is factory.handoff: "<phase> <new id or -> <choice id> <unix seconds> <tries> [kind]".
// Phases: starting, started, confirmed, failed. Kind is "" for a model hand-off and "stage" for a
// fresh agent at a stage change (stagehandoff.go).
type handoffState struct {
	Phase  string
	New    string
	Choice string
	At     time.Time
	Tries  int
	Kind   string
}

func (s handoffState) String() string {
	n := s.New
	if n == "" {
		n = "-"
	}
	out := fmt.Sprintf("%s %s %s %d %d", s.Phase, n, s.Choice, s.At.Unix(), s.Tries)
	if s.Kind != "" {
		out += " " + s.Kind
	}
	return out
}

func parseHandoff(v string) handoffState {
	p := strings.Fields(v)
	if len(p) != 5 && len(p) != 6 {
		return handoffState{}
	}
	sec, err1 := strconv.ParseInt(p[3], 10, 64)
	tries, err2 := strconv.Atoi(p[4])
	if err1 != nil || err2 != nil {
		return handoffState{}
	}
	s := handoffState{Phase: p[0], New: p[1], Choice: p[2], At: time.Unix(sec, 0).UTC(), Tries: tries}
	if s.New == "-" {
		s.New = ""
	}
	if len(p) == 6 {
		s.Kind = p[5]
	}
	return s
}

// handoffOf is agent a's hand-off state: this process's last write wins over a poll-old snapshot.
func (f *Factory) handoffOf(a Agent) handoffState {
	f.handoffMu.Lock()
	s, ok := f.handoffs[a.ID]
	f.handoffMu.Unlock()
	if ok {
		return s
	}
	return parseHandoff(a.L(labelHandoff))
}

func (f *Factory) saveHandoff(id string, s handoffState, labels map[string]string) error {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelHandoff] = s.String()
	if err := f.setLabels(id, labels); err != nil {
		return err
	}
	f.handoffMu.Lock()
	if f.handoffs == nil {
		f.handoffs = map[string]handoffState{}
	}
	f.handoffs[id] = s
	f.handoffMu.Unlock()
	return nil
}

// handoffBusy: a hand-off of a is under way, so tickHandoffs owns it and no other rule moves it.
func (f *Factory) handoffBusy(a Agent) bool {
	ph := f.handoffOf(a).Phase
	return ph == "starting" || ph == "started"
}

// handoffReleased: a replaced predecessor may be archived. Until its hand-off is confirmed it stays,
// so the card never loses its last confirmed agent; a failed one goes once the card itself is
// finished (its newest agent landed or archived).
func handoffReleased(byID map[string]Agent, a Agent) bool {
	switch parseHandoff(a.L(labelHandoff)).Phase {
	case "", "confirmed":
		return true
	case "failed":
		cur, ok := byID[resolveCardID(byID, a.ID)]
		return ok && cur.ID != a.ID && (cur.archived() || cur.L(labelStage) == "landed")
	}
	return false
}

// pickHandoff is the model a failing card moves to: Opus 5.5 unless Opus's provider is the one
// failing, else the best-ranked Frontier model (rankAgentChoices). It skips the failing provider
// entirely (its other models share the quota), unavailable providers and exhausted plans.
func pickHandoff(failing string, statuses map[string]string, usages map[string]AgentUsage, now time.Time) (AgentChoice, bool) {
	ordered, _ := rankAgentChoices(statuses, usages, now)
	ok := func(c AgentChoice) bool {
		p := c.providerID()
		return c.Frontier && p != failing && statuses[p] == "available" && usageScore(usages[p], now).tier > 0
	}
	if opus, found := agentByID(defaultAgent); found && ok(opus) {
		return opus, true
	}
	for _, c := range ordered {
		if ok(c) {
			return c, true
		}
	}
	return AgentChoice{}, false
}

// failingProvider is the Paseo provider id agent a runs on.
func failingProvider(a Agent) string {
	if a.Provider != "" {
		return a.Provider
	}
	if c, ok := agentByID(a.L(labelAgent)); ok {
		return c.providerID()
	}
	return ""
}

func modelLabel(a Agent) string {
	if c, ok := agentByID(a.L(labelAgent)); ok {
		return c.Label
	}
	if a.Model != "" {
		return a.Model
	}
	return a.Provider
}

func (f *Factory) chooseHandoff(a Agent) (AgentChoice, bool) {
	usages := f.providerUsages()
	statuses := map[string]string{}
	for _, c := range AgentChoices {
		p := c.providerID()
		if _, done := statuses[p]; !done {
			st, err := f.waitProviderStatus(p)
			if err != nil {
				st = "unknown"
			}
			statuses[p] = st
		}
	}
	return pickHandoff(failingProvider(a), statuses, usages, f.Now())
}

// handoffAllowed: card agents only, one model hand-off per card. A model replacement in the lineage
// (factory.model-handoff) or a model hand-off recorded on any of its agents means the card had it;
// fresh agents at stage changes (stagehandoff.go) do not count.
func (f *Factory) handoffAllowed(a Agent) bool {
	if a.L(labelRole) != "" || a.archived() {
		return false
	}
	if s := f.handoffOf(a); s.Phase != "" && (s.Kind == "" || s.Phase == "starting" || s.Phase == "started") {
		return false
	}
	byID := f.byID()
	if lineageLabel(byID, a, labelModelHandoff) != "" {
		return false
	}
	for _, x := range cardLineage(byID, a.ID) {
		if s := parseHandoff(x.L(labelHandoff)); s.Phase != "" && s.Kind == "" {
			return false
		}
	}
	return true
}

// tryHandoff starts the hand-off of errored card agent a, if it may have one. It reports whether
// the hand-off owns the card now; false leaves a to the same-model recovery (wait or stuck).
func (f *Factory) tryHandoff(a Agent, why string) (bool, error) {
	if !f.handoffAllowed(a) {
		return false, nil
	}
	choice, ok := f.chooseHandoff(a)
	if !ok {
		f.logf("%s: no other model to hand off to", short(a.ID))
		return false, nil
	}
	prompt, err := f.handoffPrompt(a, choice, why)
	if err != nil {
		// without the card's own instructions the new agent would not know its gates
		f.logf("%s: no hand-off: %v", short(a.ID), err)
		return false, nil
	}
	if live, err := f.stillStopped(a); !live {
		return true, err // running or archived since the poll: look again next pass
	}
	s := handoffState{Phase: "starting", Choice: choice.ID, At: f.Now(), Tries: 1}
	// a crash after this write is finished by tickHandoffs: it adopts a replacement or starts one
	if err := f.saveHandoff(a.ID, s, nil); err != nil {
		return true, err
	}
	return true, f.startHandoff(a, s, choice, prompt)
}

func (f *Factory) startHandoff(a Agent, s handoffState, choice AgentChoice, prompt string) error {
	labels := copyCarriedLabels(a)
	for _, k := range []string{labelAutoReview, labelVerdict} {
		if v := a.L(k); v != "" {
			labels[labelPrefix+k] = v
		}
	}
	labels[labelPrefix+labelStage] = a.L(labelStage)
	labels[labelPrefix+labelSince] = iso(f.Now())
	labels[labelPrefix+labelAgent] = choice.ID
	labels[labelPrefix+labelReplaces] = a.ID
	if s.Kind == "stage" {
		// the new stage's own labels, already on a: the swap is invisible on the board
		for _, k := range []string{labelSince, labelNote, labelZachPlan, labelAwaiting} {
			if v := a.L(k); v != "" {
				labels[labelPrefix+k] = v
			}
		}
	} else {
		note := "handed off from " + modelLabel(a) + " to " + choice.Label
		if choice.Note != "" {
			note += " (" + choice.Note + ")"
		}
		labels[labelPrefix+labelNote] = note
		labels[labelPrefix+labelModelHandoff] = "1"
	}
	args := choice.runArgs()
	if a.WorkspaceID != "" {
		args = append(args, "--workspace", a.WorkspaceID)
	} else {
		args = append(args, "--cwd", a.Cwd)
	}
	args = append(args, "--title", a.Title)
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+strings.ReplaceAll(labels[k], ",", ";"))
	}
	out, err := f.paseo(append(args, "--background", "--quiet", prompt)...)
	if err != nil {
		return fmt.Errorf("hand-off of %s to %s: %w", short(a.ID), choice.Label, err)
	}
	id := lastLine(out)
	if s.Kind == "stage" {
		f.logf("%s: fresh %s agent %s for %s", short(a.ID), choice.Label, short(id), a.L(labelStage))
	} else {
		f.logf("%s handed off to %s as %s", short(a.ID), choice.Label, short(id))
	}
	s.Phase, s.New = "started", id
	f.poke()
	return f.saveHandoff(a.ID, s, nil)
}

// tickHandoffs drives every unfinished hand-off from its stored step. It runs on its own, not from
// tickAgent, which skips replaced predecessors.
func (f *Factory) tickHandoffs(now time.Time) {
	for _, a := range f.agents() {
		if f.stopping.Load() {
			return
		}
		s := f.handoffOf(a)
		if s.Phase == "" || a.archived() {
			continue
		}
		if err := f.tickHandoff(a, s, now); err != nil {
			f.logf("hand-off of %s: %v", short(a.ID), err)
		}
	}
}

func (f *Factory) tickHandoff(a Agent, s handoffState, now time.Time) error {
	agents := f.agents()
	byID := f.byID()
	switch s.Phase {
	case "starting":
		for _, r := range agents {
			if r.L(labelReplaces) == a.ID {
				s.Phase, s.New = "started", r.ID
				return f.saveHandoff(a.ID, s, nil)
			}
		}
		if now.Sub(s.At) < handoffStartGrace {
			return nil
		}
		choice, ok := agentByID(s.Choice)
		var prompt string
		var err error
		switch {
		case ok && s.Kind == "stage":
			var st stageHandoff
			if st, err = f.loadStageHandoff(a.ID); err == nil {
				prompt = st.Prompt
			}
		case ok:
			prompt, err = f.handoffPrompt(a, choice, "an earlier hand-off that did not start")
		}
		if !ok || err != nil || s.Tries >= handoffTries {
			s.Phase = "failed"
			if err := f.saveHandoff(a.ID, s, nil); err != nil {
				return err
			}
			return f.handoffGaveUp(a, s)
		}
		s.Tries++
		s.At = now
		if err := f.saveHandoff(a.ID, s, nil); err != nil {
			return err
		}
		return f.startHandoff(a, s, choice, prompt)
	case "started":
		r, found := byID[s.New]
		if found && !r.archived() && r.L(labelReplaces) == a.ID && r.Status != "" && r.Status != "error" {
			s.Phase = "confirmed"
			if err := f.saveHandoff(a.ID, s, map[string]string{labelReplacedBy: r.ID}); err != nil {
				return err
			}
			f.dropStageHandoff(a.ID)
			return f.archiveChecked(a.ID, false)
		}
		if now.Sub(s.At) < handoffConfirmGrace {
			return nil
		}
		s.Phase = "failed"
		if err := f.saveHandoff(a.ID, s, nil); err != nil {
			return err
		}
		if !found {
			// the card is still the old agent: it takes the stuck marker, or its stage message
			return f.handoffGaveUp(a, s)
		}
		// the card resolves to the replacement, whose own recovery carries the stuck marker; the
		// old agent stays until the card is finished
		return nil
	case "confirmed":
		return f.archiveChecked(a.ID, false) // a failed archive after confirming
	case "failed":
		if handoffReleased(byID, a) {
			return f.archiveChecked(a.ID, false)
		}
	}
	return nil
}

// handoffGaveUp leaves card agent a to the same-model recovery's end state (marked stuck), or, for a
// stage swap, sends its stage message to the old session as before (stagehandoff.go).
func (f *Factory) handoffGaveUp(a Agent, s handoffState) error {
	if s.Kind == "stage" {
		return f.stageGaveUp(a)
	}
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	return f.exhaust(a, f.recoveryState(a))
}

// handoffPrompt is the replacement's first message: its step, then the card's own first message
// verbatim (every gate and repo rule in it still applies), the approved plan and the old agent's
// last reply. An unreadable session is an error: the new agent must get the card's instructions.
func (f *Factory) handoffPrompt(a Agent, choice AgentChoice, why string) (string, error) {
	evs, err := storedEvents(f.cfg.Home, a)
	if err != nil {
		return "", err
	}
	first, plan, notes := handoffParts(evs)
	if strings.TrimSpace(first) == "" {
		return "", fmt.Errorf("%w: no first message in %s's session", ErrNoHistory, short(a.ID))
	}
	last, _ := storedReplyOne(f.cfg.Home, a)
	step := handoffStep(a.L(labelStage))
	var b strings.Builder
	fmt.Fprintf(&b, "You are taking over this factory card from %s, which failed: %s. The factory moved the card to you, %s, "+
		"in the same worktree and branch, with its labels, thread and review rounds. This happens once per card.\n\n"+
		"First check where the work stands: git status, git log --oneline origin/main..HEAD, and the files the plan names. "+
		"Do not start over. Your step now: %s\n\n"+
		"The card's original instructions follow verbatim. They still apply (rules, gates, the public-repo scan, the "+
		"three opening lines), but the step above replaces their \"Step 1\".\n\n"+
		"=== Original instructions ===\n%s\n",
		modelLabel(a), strings.TrimSpace(why), choice.Label, step, first)
	if plan != "" {
		fmt.Fprintf(&b, "\n=== Approved plan ===\n%s\n", trunc(plan, handoffTextCap))
		if notes != "" {
			fmt.Fprintf(&b, "\n=== Reviewer notes when the plan was approved ===\n%s\n", trunc(notes, handoffTextCap))
		}
	}
	if last = strings.TrimSpace(last); last != "" && last != strings.TrimSpace(plan) {
		fmt.Fprintf(&b, "\n=== %s's last message ===\n%s\n", modelLabel(a), trunc(last, handoffTextCap))
	}
	fmt.Fprintf(&b, "\nNow: %s\n", step)
	return b.String(), nil
}

func handoffStep(stage string) string {
	switch stage {
	case "plan":
		return "planning. Write the plan as the instructions say, then stop."
	case "building":
		return "building the approved plan. Finish it, commit on the branch, leave the worktree clean, do NOT land, " +
			"and stop with a short summary (including deploy steps, if any). A code reviewer checks it next."
	case "landing":
		return "landing. The factory had told the previous agent to land. Check whether it already landed " +
			"(git fetch, then git log origin/main); if not, land it as the instructions say, then report as they say."
	}
	return "continue the interrupted step, then stop with a short summary."
}

// handoffParts reads a session's first user message, the approved plan (the reply just before the
// last "Build it now, as planned" message) and that message's reviewer notes.
func handoffParts(evs []Event) (first, plan, notes string) {
	var run []string
	lastReply := ""
	for _, e := range evs {
		switch e.Kind {
		case "user":
			if first == "" {
				first = e.Text
			}
			if len(run) > 0 {
				lastReply = strings.Join(run, "\n\n")
			}
			run = nil
			if strings.Contains(e.Text, "Build it now, as planned") && lastReply != "" {
				plan = lastReply
				notes = ""
				if _, n, ok := strings.Cut(e.Text, "Reviewer notes:"); ok {
					notes = strings.TrimSpace(n)
				}
			}
		case "assistant":
			if strings.TrimSpace(e.Text) != "" {
				run = append(run, e.Text)
			}
		case "tool", "result":
			run = nil
		}
	}
	return first, strings.TrimSpace(plan), notes
}

// dropLineage archives the not yet archived predecessors of card id (Drop and Close).
func (f *Factory) dropLineage(byID map[string]Agent, id string) {
	for _, pid := range lineagePredecessorIDs(byID, id) {
		if p, ok := byID[pid]; ok && !p.archived() {
			if err := f.archiveAgent(pid); err != nil {
				f.logf("predecessor %s of dropped %s: %v", short(pid), short(id), err)
			}
		}
	}
}
