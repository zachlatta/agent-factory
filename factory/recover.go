package factory

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Automatic recovery. Errors, usage limits, stalls, failed reviewers, dropped dependencies and failed
// landings are retried by the factory or handed back to the card, never to Needs Zach or email.
// Every kind has a hard cap; when it runs out the card keeps its column marked stuck
// (factory.stuck, markStuck): a board flag and the daily digest, never an email.
//
// One label, factory.recovery, stores the state as base64 JSON (Paseo splits label values on
// commas). Revision protects a send from a stale snapshot within this process.

const (
	limitGrace           = 2 * time.Minute
	limitResumeMessage   = "The provider limit reset. Check the current worktree and continue the interrupted step."
	errorRetryMessage    = "The factory is retrying after a provider error. Check the current worktree and continue the interrupted step."
	reviewerRetryCap     = 2 // replacement reviewers per review round
	landReturnCap        = 2 // returns to building after a failed landing
	directReturnCap      = 1 // retries of a direct card's landing
	stuckAgentError      = "agent error"
	stuckReview          = "review could not finish"
	stuckStopped         = "stopped with nothing queued"
	stuckLanding         = "landing failed"
	landDiffNotePrefix   = "landed diff differs from the reviewed one"
	landDiffScanMark     = "; secret scan "
	landFailMax          = 1500
	stuckFlagPrefix      = "stuck: "
	stuckTitlePrefix     = "Stuck: "
	resumingNote         = "resuming interrupted step"
	rebuildingNotePrefix = "rebuilding: "
)

// errorBackoff: an error with no known reset, or a repeat after the reset resume, retries on the
// same model after each of these in turn.
var errorBackoff = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

type recovery struct {
	Revision int
	// the current error episode
	Stage     string
	Error     string    // fingerprint of the error being recovered
	Reset     time.Time // when the next automatic send is due
	Attempt   time.Time
	UserAt    time.Time
	Phase     string // waiting, attempted, exhausted, complete
	FromReset bool   // the pending send is the resume after a known reset
	ResetUsed bool   // that resume was spent in this episode
	Timed     int    // backoff retries sent in this episode
	Manual    bool   // the pending attempt is Zach's Resume or Reply: its failure does not rearm retries
	// a card stopped with nothing queued: 1 = nudged, 2 = its step re-ran
	Nudges     int
	NudgeStage string
	// failed landings sent back to building (or a direct card told to land again)
	Lands      int
	LandedDiff bool // the last return was a landed diff that differs: its HEAD may already be on main
}

func readRecovery(a Agent) (r recovery) {
	v := a.L(labelRecovery)
	if v == "" {
		v = a.L(legacyLimitLabel) // cards in flight before factory.recovery
	}
	if b, err := base64.RawURLEncoding.DecodeString(v); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	if r.Phase == "blocked" {
		r.Phase = "exhausted"
	}
	return r
}

func errorFingerprint(a Agent) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(a.LastError))) }

// Callers hold recoveryMu through persistence and send, serializing ticks and Resume.
func (f *Factory) recoveryState(a Agent) recovery {
	r := readRecovery(a)
	if cached := f.recoveries[a.ID]; cached.Revision > r.Revision {
		r = cached
	}
	return r
}

func (f *Factory) saveRecovery(a Agent, r recovery, labels map[string]string) error {
	r.Revision++
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelRecovery] = base64.RawURLEncoding.EncodeToString(b)
	if a.L(legacyLimitLabel) != "" {
		labels[legacyLimitLabel] = ""
	}
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	if f.recoveries == nil {
		f.recoveries = map[string]recovery{}
	}
	f.recoveries[a.ID] = r
	return nil
}

// updateRecovery changes a's recovery state and writes it with labels, under recoveryMu.
func (f *Factory) updateRecovery(a Agent, change func(*recovery), labels map[string]string) error {
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	r := f.recoveryState(a)
	change(&r)
	return f.saveRecovery(a, r, labels)
}

// markStuck leaves card a in its column marked stuck: every automatic step for it ran out. It is a
// board flag and a digest line, never Needs Zach or an email. It clears when the card runs again.
func (f *Factory) markStuck(a Agent, why string) error {
	if a.L(labelStuck) == why {
		return nil
	}
	f.logf("%s stuck in %s: %s", short(a.ID), a.L(labelStage), why)
	return f.setLabels(a.ID, map[string]string{labelStuck: why, labelAutoReview: ""})
}

// stuckReason is why card a is marked stuck, or "" when it is not. The digest lists these.
func stuckReason(a Agent) string { return a.L(labelStuck) }

// stuckAsk is the board-only ask on a card marked stuck, in whatever column it is in.
func stuckAsk(a Agent) *Ask {
	why := a.L(labelStuck)
	if why == stuckAgentError {
		text := "The agent kept failing: the factory retried it on the same model (after the provider's reset when the " +
			"error gave one, then after 2, 10 and 30 minutes). The card stays in its column. Resume makes one more " +
			"attempt at the interrupted step."
		if a.LastError != "" {
			text += "\n\nProvider error: " + a.LastError
		}
		return &Ask{Title: stuckTitlePrefix + "the agent keeps failing", From: "builder", Why: text, BoardOnly: true,
			Actions: []Action{primary("resume", "Resume"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
	}
	return &Ask{Title: stuckTitlePrefix + why, From: "builder", BoardOnly: true,
		Why: "The factory's automatic retries for this step ran out, so the card stays in its column. Re-run the " +
			"step, send the agent a message, or drop it.",
		Actions: []Action{primary("rerun", "Re-run the step"), reply("Reply", "Message to the agent"), act("close", "Drop it")}}
}

// tickError consumes errored working cards before ordinary completion rules can run. A known reset
// resumes on the same model once; other errors, and a repeat after that resume, retry after each
// errorBackoff step; then the card is marked stuck. Error text comes from the saved agent snapshot.
func (f *Factory) tickError(a Agent, now time.Time) (bool, error) {
	if a.archived() || stuckWhy[a.L(labelStage)] == "" {
		return false, nil
	}
	if _, ok := f.card(a.ID); !ok {
		return false, nil
	}
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	r := f.recoveryState(a)
	if a.Status != "error" {
		// A running retry may still fail. Only a completed new turn ends the episode.
		if r.Phase != "" && r.Phase != "complete" && a.stopped() && a.LastUserMessageAt.After(r.UserAt) {
			r.Phase, r.UserAt = "complete", a.LastUserMessageAt
			r.FromReset, r.ResetUsed, r.Timed, r.Manual = false, false, 0, false
			return false, f.saveRecovery(a, r, nil)
		}
		return false, nil
	}
	if r.Phase == "complete" && !a.LastUserMessageAt.After(r.UserAt) {
		return true, nil // an older error snapshot cannot reopen a completed episode
	}
	if r.Phase == "exhausted" {
		return true, f.exhaust(a, r)
	}
	if r.Phase == "attempted" {
		failed := now.Sub(r.Attempt) >= limitGrace || (now.Sub(r.Attempt) > 45*time.Second && a.LastUserMessageAt.After(r.UserAt))
		if !failed {
			return true, nil // allow the send to reach the provider; do not trust a stale error
		}
		if r.Manual {
			return true, f.exhaust(a, r)
		}
		r.Error = "" // the attempt failed: plan the next one
	}
	if r.Phase == "" || r.Phase == "complete" {
		r = recovery{Revision: r.Revision, Nudges: r.Nudges, NudgeStage: r.NudgeStage, Lands: r.Lands,
			LandedDiff: r.LandedDiff, Stage: a.L(labelStage), UserAt: a.LastUserMessageAt}
	}
	if r.Error != errorFingerprint(a) {
		if !planRetry(&r, a, now) {
			return true, f.exhaustError(a, r)
		}
		if r.FromReset && r.Reset.Sub(now) > handoffResetGap {
			// too long to wait: another model takes the card (handoff.go)
			if ok, err := f.tryHandoff(a, "its provider limit resets at "+iso(r.Reset)+", over 2 hours away"); ok || err != nil {
				return true, err
			}
		}
		if err := f.saveRecovery(a, r, nil); err != nil {
			return true, err
		}
		r = f.recoveryState(a)
	}
	if now.Before(r.Reset) {
		return true, nil
	}
	msg := errorRetryMessage
	if r.FromReset {
		msg = limitResumeMessage
	}
	return true, f.attemptRecovery(a, r, now, msg, nil, false)
}

// planRetry schedules r's next automatic send for a's current error, or reports false when none is left.
func planRetry(r *recovery, a Agent, now time.Time) bool {
	observed := a.LastActivityAt
	if r.Phase == "attempted" {
		observed = now // a failed (or uncertain) attempt: the next one waits its full step from here
	}
	if observed.IsZero() || observed.After(now) {
		observed = now
	}
	r.Error, r.Phase = errorFingerprint(a), "waiting"
	if reset, known := limitReset(a.LastError, observed); known && !r.ResetUsed {
		r.Reset, r.FromReset = reset.Add(limitMargin), true
		return true
	}
	if r.Timed >= len(errorBackoff) {
		return false
	}
	r.Reset, r.FromReset = observed.Add(errorBackoff[r.Timed]), false
	return true
}

// exhaustError ends the same-model retries of a: a hand-off to another model if the card may have
// one (handoff.go), else the stuck mark.
func (f *Factory) exhaustError(a Agent, r recovery) error {
	why := "the same model could not recover"
	if a.LastError != "" {
		why = "its provider error: " + trunc(a.LastError, 300)
	}
	if ok, err := f.tryHandoff(a, why); ok || err != nil {
		return err
	}
	return f.exhaust(a, r)
}

// exhaust marks an errored card stuck once its retries ran out. It keeps the card's column.
func (f *Factory) exhaust(a Agent, r recovery) error {
	if r.Phase == "exhausted" && a.L(labelStuck) == stuckAgentError {
		return nil
	}
	if live, err := f.stillStopped(a); !live {
		return err
	}
	r.Phase = "exhausted"
	labels := map[string]string{labelStuck: stuckAgentError}
	if r.Stage != "" && a.L(labelStage) != r.Stage {
		labels[labelStage] = r.Stage
	}
	f.logf("%s stuck: agent error, retries used up", short(a.ID))
	return f.saveRecovery(a, r, labels)
}

func (f *Factory) attemptRecovery(a Agent, r recovery, now time.Time, message string, attachments []Attachment, manual bool) error {
	live, err := f.stillStopped(a)
	if err != nil || !live {
		return err
	}
	if !manual {
		if r.FromReset {
			r.ResetUsed = true
		} else {
			r.Timed++
		}
	}
	r.Phase, r.Attempt, r.UserAt, r.Manual = "attempted", now, a.LastUserMessageAt, manual
	// A crash or timeout after this write cannot cause a second send before the grace check.
	if err := f.saveRecovery(a, r, map[string]string{labelStage: r.Stage, labelSince: iso(now), labelNote: resumingNote,
		labelTold: "", labelStuck: ""}); err != nil {
		return err
	}
	if _, err := f.chat(a.ID, message, attachments); err != nil {
		if manual {
			return errors.Join(err, f.exhaust(a, f.recoveryState(a)))
		}
		return err // delivery uncertain: the grace check decides, and the next try waits its turn
	}
	return nil
}

// errorStuck reports whether card a is marked stuck because its agent kept failing.
func errorStuck(a Agent) bool { return a.L(labelStuck) == stuckAgentError }

// resumeError is Zach's Resume (or Reply) on a card stuck after errors: one attempt, which does not
// rearm the automatic retries.
func (f *Factory) resumeError(a Agent, message string, attachments []Attachment) error {
	f.recoveryMu.Lock()
	defer f.recoveryMu.Unlock()
	r := f.recoveryState(a)
	if !errorStuck(a) || r.Phase != "exhausted" || stuckWhy[r.Stage] == "" || a.archived() {
		return errors.New("this card is not waiting for error recovery")
	}
	return f.attemptRecovery(a, r, f.Now(), message, attachments, true)
}

// stepStuck handles a stopped card with nothing queued in plan, building or landing: one nudge, then
// its step re-runs, then it is marked stuck.
func (f *Factory) stepStuck(a Agent, c Card, stage string) error {
	r := readRecovery(a)
	if r.NudgeStage != stage {
		r.Nudges = 0
	}
	switch r.Nudges {
	case 0:
		f.logf("%s stopped in %s with nothing queued: nudging", short(a.ID), stage)
		if err := f.updateRecovery(a, func(r *recovery) { r.Nudges, r.NudgeStage = 1, stage },
			map[string]string{labelSince: iso(f.Now())}); err != nil {
			return err
		}
		if a.L(labelRouting) != "" {
			return f.send(a.ID, pickerNudgeMsg)
		}
		return f.send(a.ID, nudgeMsg(stage))
	case 1:
		f.logf("%s stopped in %s with nothing queued again: re-running its step", short(a.ID), stage)
		if err := f.updateRecovery(a, func(r *recovery) { r.Nudges, r.NudgeStage = 2, stage }, nil); err != nil {
			return err
		}
		if a.L(labelRouting) != "" {
			return f.routeFromReply(a) // a picker's step is reading its REPO line
		}
		return f.rerunStep(c, stage)
	}
	return f.markStuck(a, stuckStopped)
}

// clearNudges forgets the stopped-card steps once the card has moved to another column.
func (f *Factory) clearNudges(a Agent) {
	r := readRecovery(a)
	if r.Nudges == 0 || r.NudgeStage == a.L(labelStage) {
		return
	}
	if err := f.updateRecovery(a, func(r *recovery) { r.Nudges, r.NudgeStage = 0, "" }, nil); err != nil {
		f.logf("%s: clearing stopped-card steps: %v", short(a.ID), err)
	}
}

const pickerNudgeMsg = "The factory has no next step for this card: your last turn ended without a REPO: line it " +
	"could read. Pick the repo and end with the REPO: line."

func nudgeMsg(stage string) string {
	const lead = "The factory has no next step for this card: "
	switch stage {
	case "plan":
		return lead + "your last turn ended without a plan or result it could read. Finish the step the way the first " +
			"prompt says (the plan ending with its TIER: line, or the direct result with TRACK: direct), then stop."
	case "landing":
		return lead + "your landing turn ended without a verified landing. Land it now the way the land prompt says, " +
			"or say plainly what blocks you." + remainingRule
	}
	return lead + "your build turn ended with no code review queued. Finish the build, commit everything on your " +
		"branch (do NOT land), and stop with a short summary. " + overviewReminder
}

// missingLensNote tells the arbiter what to do about a panel lens that never gave a verdict.
var missingLensNote = fmt.Sprintf("This reviewer could not finish after %d tries, so its lens is missing: "+
	"check that area of the change yourself before you decide.", reviewerRetryCap+1)

// recoverReview handles a review column whose reviewer is gone without a verdict or blocked (errored,
// stalled or on a permission prompt): a panel replaces that lens, a failed arbiter is replaced once,
// and a single reviewer is replaced (replaceReviewer).
func (f *Factory) recoverReview(a Agent, stage string, agents []Agent, now time.Time) error {
	kind := reviewKindOf(stage)
	r, blocked := blockedReviewerOf(agents, a.ID, now)
	why := "gone without a verdict"
	if blocked {
		why = blockedWhy(agents, r, now)
	} else if last, ok := newestReviewerOf(agents, a.ID); ok && (last.L(labelLens) == lensArbiter || last.L(labelArbiter) != "") &&
		f.lensResultOf(last).verdict == "" {
		r, blocked = last, true // the arbiter went without a ruling
	}
	switch {
	case blocked && r.L(labelArbiter) != "":
		return f.gateArbiterFailed(a, kind, why)
	case blocked && r.L(labelLens) == lensArbiter:
		return f.arbiterFailed(a, kind, r.L(labelPanel), why)
	case kind == "code" && f.openPanel(a) != "":
		return f.replaceLenses(a, f.openPanel(a), why)
	}
	return f.replaceReviewer(a, kind, why)
}

// newestReviewerOf is card id's most recently started reviewer.
func newestReviewerOf(agents []Agent, id string) (Agent, bool) {
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	var out Agent
	for _, r := range agents {
		if r.L(labelRole) == "reviewer" && sameCard(byID, r.L(labelReviewOf), id) && (out.ID == "" || r.CreatedAt.After(out.CreatedAt)) {
			out = r
		}
	}
	return out, out.ID != ""
}

// replaceReviewer retires card a's reviewers and starts a fresh one of kind, at most reviewerRetryCap
// times per review round; then reviewersExhausted takes over. why says what went wrong. Like every
// review recovery, it runs with card a's gate held (decide, checkStuck).
func (f *Factory) replaceReviewer(a Agent, kind, why string) error {
	n := atoi(a.L(labelReviewerRetries))
	if err := f.supersedeReviewers(a.ID); err != nil {
		return err
	}
	if n >= reviewerRetryCap {
		return f.reviewersExhausted(a, kind, why)
	}
	if err := f.setLabels(a.ID, map[string]string{labelReviewerRetries: fmt.Sprint(n + 1)}); err != nil {
		return err
	}
	f.logf("%s %s reviewer %s; starting replacement %d of %d", short(a.ID), kind, why, n+1, reviewerRetryCap)
	return f.rerunReviewLocked(f.gate(a.ID), a, kind)
}

// replaceLenses replaces each lens of card a's open panel that has no result (blocked, or gone), at
// most reviewerRetryCap times per lens. A lens past that is recorded without a verdict, so the
// arbiter decides without it once the rest of the panel has finished.
func (f *Factory) replaceLenses(a Agent, panel, why string) error {
	c, ok := f.card(a.ID)
	if !ok {
		return nil
	}
	lenses := f.panelLenses(a.ID, panel)
	if len(lenses) == 0 {
		return f.replaceReviewer(a, "code", why) // the whole panel is gone: a fresh review
	}
	for _, l := range []string{lensCorrectness, lensArchitecture, lensUX} {
		r, ok := lenses[l]
		if !ok || f.lensResultOf(r).done {
			continue
		}
		if f.lensRuns(a.ID, panel, l) > reviewerRetryCap {
			f.logf("%s of %s %s; no replacements left, the arbiter decides without it", lensNames[l], short(a.ID), why)
			if err := f.retireReviewer(r, ""); err != nil {
				return err
			}
			continue
		}
		if err := f.retireReviewer(r, "superseded"); err != nil {
			return err
		}
		f.logf("%s of %s %s; starting a replacement", lensNames[l], short(a.ID), why)
		if err := f.startLenses(c, []string{l}, panel, ""); err != nil {
			return err
		}
	}
	if f.panelReady(a) {
		return f.closePanel(a)
	}
	return f.setLabels(a.ID, map[string]string{labelSince: iso(f.Now())})
}

// soloPanel is the panel round of the arbiter that stands in for card a's single reviewer of kind.
func soloPanel(a Agent, kind string) string {
	return "solo-" + kind + "-" + fmt.Sprint(atoi(a.L(roundsLabel(kind)))+1)
}

// reviewersExhausted runs when card a's single reviewer could not finish after every replacement:
// the arbiter reviews the change itself, told the reviewer is missing. A failed arbiter gets one
// replacement. factory.reviewer-retries keeps counting past the reviewer replacements, so the cap
// holds even when a gone arbiter has left the agent list.
func (f *Factory) reviewersExhausted(a Agent, kind, why string) error {
	n := atoi(a.L(labelReviewerRetries))
	if n >= reviewerRetryCap+2 {
		f.logf("%s %s arbiter %s again: the review could not finish", short(a.ID), kind, why)
		return f.markStuck(a, stuckReview)
	}
	if err := f.setLabels(a.ID, map[string]string{labelReviewerRetries: fmt.Sprint(n + 1)}); err != nil {
		return err
	}
	f.logf("%s %s review could not finish (%s): the arbiter decides (try %d)", short(a.ID), kind, why, n-reviewerRetryCap+1)
	return f.startSoloArbiter(a, kind, soloPanel(a, kind), why)
}

func (f *Factory) startSoloArbiter(a Agent, kind, panel, why string) error {
	c, ok := f.card(a.ID)
	if !ok {
		return nil
	}
	extra := fmt.Sprintf("There are no reports: the single reviewer of this %s could not finish (%s) after %d tries, "+
		"so you are the reviewer that is missing. Review it yourself, through every lens, and decide.", kind, why, reviewerRetryCap+1)
	if _, err := f.runReviewer(c, kind, reviewSpec{lens: lensArbiter, panel: panel, extra: extra}, f.visibility(c.Cwd),
		"Review (arbiter): "+trunc(c.Title, 50), "", ""); err != nil {
		return err
	}
	return f.setLabels(a.ID, map[string]string{labelSince: iso(f.Now())})
}

// arbiterFailed handles an arbiter that ended without a ruling, or is blocked: it is replaced once,
// then card a is marked stuck in its review column. Only a missing ruling lands here; an arbiter's
// ruling that sends the card back, and the review round cap, follow decide as before.
func (f *Factory) arbiterFailed(a Agent, kind, panel, why string) error {
	if err := f.supersedeReviewers(a.ID); err != nil {
		return err
	}
	if strings.HasPrefix(panel, "solo-") {
		return f.reviewersExhausted(a, kind, why)
	}
	if f.lensRuns(a.ID, panel, lensArbiter) > 1 {
		f.logf("%s arbiter %s again: the review could not finish", short(a.ID), why)
		return f.markStuck(a, stuckReview)
	}
	f.logf("%s arbiter %s; starting a replacement", short(a.ID), why)
	return f.startPanelArbiter(a, panel)
}

// gateArbiterFailed handles a gate arbiter (arbiter.go: the review round cap or an escalation) that ended
// without a ruling or is blocked: it is replaced once, then card a is marked stuck in its review column.
// A ruling, and a gate that fails again after one, follow decide and applyArbiter as before.
func (f *Factory) gateArbiterFailed(a Agent, kind, why string) error {
	if err := f.supersedeReviewers(a.ID); err != nil {
		return err
	}
	if n := atoi(a.L(labelReviewerRetries)); n > 0 {
		f.logf("%s %s arbiter %s again: the review could not finish", short(a.ID), kind, why)
		return f.markStuck(a, stuckReview)
	}
	last, escalated := "", a.L(labelVerdict) == kind+" escalate"
	byID := f.byID()
	var newest Agent
	for _, r := range f.agents() {
		if r.L(labelRole) == "reviewer" && r.L(labelArbiter) == "" && r.L(labelReviewKind) == kind &&
			sameCard(byID, r.L(labelReviewOf), a.ID) && (newest.ID == "" || r.CreatedAt.After(newest.CreatedAt)) {
			newest = r
		}
	}
	if newest.ID != "" {
		if text, err := f.storedReply(newest); err == nil {
			last = text
		}
	}
	f.logf("%s %s arbiter %s; starting a replacement", short(a.ID), kind, why)
	if err := f.startArbiter(a, kind, last, escalated); err != nil {
		return err
	}
	return f.setLabels(a.ID, map[string]string{labelReviewerRetries: "1"})
}

// reviewKindOf is the review kind run in a review column.
func reviewKindOf(stage string) string {
	if stage == "plan-review" {
		return "plan"
	}
	return "code"
}

// returnToBuild sends a card whose landing failed back to building, with note on the card and msg to
// the agent: it fixes the branch, code review runs again, then the land prompt and its scan. After
// landReturnCap returns the card stays in landing marked stuck.
func (f *Factory) returnToBuild(a Agent, note, msg string, landedDiff bool) error {
	if readRecovery(a).Lands >= landReturnCap {
		return f.markStuck(a, stuckLanding)
	}
	f.logf("%s %s: back to building", short(a.ID), note)
	// saveRecovery adds the new recovery label to labels, so a fresh agent carries the raised count
	labels := map[string]string{labelStage: "building", labelAutoReview: "code", labelSince: iso(f.Now()),
		labelNote: rebuildingNotePrefix + note, labelHeld: ""}
	if err := f.updateRecovery(a, func(r *recovery) { r.Lands++; r.LandedDiff = landedDiff }, labels); err != nil {
		return err
	}
	return f.advance(labeled(a, labels), "fixing a failed landing", msg)
}

func landFailMsg(why string) string {
	return "Your landing did not reach origin/main: " + trunc(why, landFailMax) + "\n\nDo not land now. Rebase on " +
		"origin/main, resolve any conflicts, rerun go vet and the repo's tests (or its own checks), and commit on your " +
		"branch. If the land failed on a secret scan hit, rebasing does not fix it: remove the value and rewrite your " +
		"own unpushed commits (git rebase -i, git commit --amend); a later commit that deletes it is not enough, since " +
		"the push would publish the earlier commit too. Then stop with a short summary. Code review runs again, then " +
		"the factory sends the land prompt. " + overviewReminder
}

func landDiffMsg(why string) string {
	return "What reached origin/main is not exactly the commits code review approved (" + trunc(why, landFailMax) +
		"). It is already on origin/main: do not push it again. Check what landed against what was reviewed. If " +
		"something is wrong, fix it forward on your branch rebased on origin/main, rerun the tests and commit (do " +
		"NOT land). Then stop with a short summary. Code review covers what landed, and the land prompt that follows " +
		"only verifies it. " + overviewReminder
}

// landedDiffLandNote leads the land prompt after a landed-diff return.
const landedDiffLandNote = "What landed earlier differed from its review and has now been reviewed. If your HEAD is " +
	"already on origin/main, do not push again: verify that and finish. Otherwise land your new commits as below. "

// landedDiffDiffers handles a landing whose commits differ from the reviewed ones. On a public repo
// the range that landed is scanned first: a hit, or a scan that cannot run, waits for Zach and is
// emailed, since something unreviewed is already public.
func (f *Factory) landedDiffDiffers(a Agent, c Card, why string) error {
	note := landDiffNotePrefix + ": " + trunc(why, 200)
	if f.guarded(c.Repo) {
		found, err := "", error(nil)
		if base := a.L(labelBase); base == "" {
			err = errors.New("no base commit to scan from")
		} else {
			found, err = f.scanCard(c.Cwd, base+"..HEAD")
		}
		if err != nil || found != "" {
			scan := landDiffScanMark + "found: " + found
			if err != nil {
				scan = landDiffScanMark + "could not run: " + err.Error()
			}
			f.logf("%s landed diff differs on a public repo; %s", short(a.ID), scan)
			return f.setLabels(a.ID, map[string]string{labelStage: "zach", labelNote: trunc(note+scan, 300)})
		}
	}
	return f.returnToBuild(a, "landed diff differs", landDiffMsg(why), true)
}

// returnDirect tells a direct card whose work item is not on origin/main to land it again, once;
// then it is marked stuck.
func (f *Factory) returnDirect(a Agent, why string) error {
	if readRecovery(a).Lands >= directReturnCap {
		return f.markStuck(a, stuckLanding)
	}
	f.logf("%s direct card not on main: telling it to land again", short(a.ID))
	if err := f.updateRecovery(a, func(r *recovery) { r.Lands++ },
		map[string]string{labelSince: iso(f.Now()), labelNote: "landing its result again"}); err != nil {
		return err
	}
	return f.send(a.ID, "Your result is not on origin/main: "+trunc(why, landFailMax)+"\n\nRebase on origin/main, "+
		"resolve any conflicts, and land your work item again the way the repo's AGENTS.md says. Then end with "+
		"TRACK: direct as before.")
}

// rebuildWithoutDep tells a held card that the card its plan waits on was dropped or cannot be
// found. It decides itself whether to rebuild without it or land as is; either way it goes back
// through code review.
func (f *Factory) rebuildWithoutDep(a Agent, id, state string) error {
	what := "was dropped or closed without landing"
	if state == depMissing {
		what = "is not on the board (a wrong id, or it left the board before it landed)"
	}
	f.logf("%s held: %s %s; back to building", short(a.ID), id, state)
	labels := clearHeldApproval(a, map[string]string{labelStage: "building", labelAutoReview: "code",
		labelSince: iso(f.Now()), labelHeld: "", labelWaitsOn: "", labelWaitsDone: "",
		labelNote: rebuildingNotePrefix + id + " " + state})
	if err := f.setLabels(a.ID, labels); err != nil {
		return err
	}
	return f.advance(labeled(a, labels), "rebuilding without the card it waited on", "Card "+id+", which your approved plan lands after, "+what+". Do not land now. Decide "+
		"yourself: if this change needs that card's work, rebuild without it (bring in what you need, or cut the part "+
		"that depends on it); if it does not, say so and change nothing. Rebase on origin/main, rerun the tests and "+
		"commit on your branch. Then stop with a short summary that says which you chose. Code review runs again, "+
		"then the factory sends the land prompt. "+overviewReminder)
}

// Waits from before automatic recovery. A card still in Needs Zach with one of these notes takes
// the new path on its next pass (migrateWait).
const (
	legacyLimitNote     = "agent error: resume needed"
	legacyNoVerdictNote = "reviewer gave no verdict"
	legacyNotVerified   = "landing not verified"
)

// migrateWait moves a stopped card left in Needs Zach by a wait that is now recovered automatically.
// It reports whether the card was one.
func (f *Factory) migrateWait(a Agent, c Card) (bool, error) {
	note, from := a.L(labelNote), a.L(labelStuckFrom)
	var move func() error
	switch {
	case note == legacyLimitNote:
		r := readRecovery(a)
		if stuckWhy[r.Stage] == "" {
			return false, nil
		}
		move = func() error {
			return f.updateRecovery(a, func(r *recovery) { r.Phase = "exhausted" },
				map[string]string{labelStage: r.Stage, labelStuck: stuckAgentError, labelNote: ""})
		}
	case (note == "stuck" || note == legacyNoVerdictNote) && stuckWhy[from] != "":
		why := stuckStopped
		if from == "review" || from == "plan-review" {
			why = stuckReview
		}
		move = func() error {
			return f.setLabels(a.ID, map[string]string{labelStage: from, labelStuck: why, labelNote: "", labelStuckFrom: ""})
		}
	case strings.HasPrefix(note, "waits on ") && (strings.HasSuffix(note, ": it was dropped") || strings.HasSuffix(note, ": no such card found")):
		id, _, _ := strings.Cut(strings.TrimPrefix(note, "waits on "), ":")
		state := depDropped
		switch now := depState(f.agents(), a, id); now {
		case depLanded:
			// it landed after all: release the landing as the hold would have
			move = func() error { return f.tickHeld(a, c) }
		case depWaiting:
			// back in flight: hold the landing again
			move = func() error {
				return f.setLabels(a.ID, map[string]string{labelStage: "landing", labelHeld: id, labelNote: holdNote(id)})
			}
		default:
			state = now
			move = func() error { return f.rebuildWithoutDep(a, id, state) }
		}
	case note == legacyNotVerified && a.L(labelTrack) == "direct":
		move = func() error {
			return f.setLabels(a.ID, map[string]string{labelStage: "plan", labelStuck: stuckLanding, labelNote: ""})
		}
	case note == legacyNotVerified:
		move = func() error { return f.returnToBuild(a, "landing not verified", landFailMsg(note), false) }
	case strings.HasPrefix(note, landDiffNotePrefix) && !f.guarded(c.Repo):
		why := strings.TrimPrefix(strings.TrimPrefix(note, landDiffNotePrefix), ": ")
		move = func() error { return f.returnToBuild(a, "landed diff differs", landDiffMsg(why), true) }
	default:
		return false, nil
	}
	if live, err := f.stillStopped(a); !live {
		return true, err
	}
	f.logf("%s: moving old wait %q to automatic recovery", short(a.ID), note)
	return true, move()
}
