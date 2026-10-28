package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Landing reviews: a separate agent reviews the diff and the tests before anything lands on main in a
// git repo, especially a public one. Factory cards get one
// through their code review. Any other Paseo agent (a scheduled job, say) asks for one with
// POST /api/land-review {"by": "<its PASEO_AGENT_ID>", "why": "..."} once its work is committed, waits
// on GET /api/land-review/<by>, and lands only on approve or nits for the same HEAD. The `land-review`
// command (this binary under that name) does both; bin/land and the pre-push hook run it for every
// agent push, after the rebase, so what is published is what was approved.
//
// A factory card asks too, and only while it is authorized to land (cardLandBlock, on the board's
// snapshot and on the card's current agent file and Paseo state). When its HEAD is the commit a
// completed code review passed (factory.code-approved, which every new review clears), that approval
// is the answer and no reviewer runs. Any other HEAD (a rebase, a conflict fix, a card approved before
// the marker existed) gets a fresh landing review of the actual candidate; its approval, if the card
// is still authorized when it finishes, becomes the card's reviewed and approved commit. A card's
// earlier landing-review record is never proof. An outside agent's finished approval of the same HEAD
// in the same checkout is handed out again. Every answer is given only after the checkout is checked.

// LandReview is one landing review, stored by the requesting agent's id.
type LandReview struct {
	By         string    `json:"by"`
	Review     string    `json:"review"` // the reviewer agent
	Head       string    `json:"head"`   // the commit under review
	Cwd        string    `json:"cwd"`
	Repo       string    `json:"repo"`
	Visibility string    `json:"visibility"`
	Why        string    `json:"why"`
	Status     string    `json:"status"` // running, done
	Verdict    string    `json:"verdict,omitempty"`
	Findings   string    `json:"findings,omitempty"`
	Card       bool      `json:"card,omitempty"` // asked by a factory card: its approval must still be authorized
	StartedAt  time.Time `json:"startedAt"`
	DoneAt     time.Time `json:"doneAt,omitzero"`
}

// landReviewKeep is how long finished reviews stay in the store.
const landReviewKeep = 7 * 24 * time.Hour

// LandReviewStore keeps landing reviews in one JSON file.
type LandReviewStore struct {
	path string
	mu   sync.Mutex
}

func (s *LandReviewStore) Get(by string) (LandReview, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.load()[by]
	return r, ok
}

func (s *LandReviewStore) load() map[string]LandReview {
	m := map[string]LandReview{}
	readJSON(s.path, &m)
	return m
}

// put stores r under r.By and drops finished reviews older than landReviewKeep.
func (s *LandReviewStore) put(r LandReview, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	for k, v := range m {
		if v.Status == "done" && now.Sub(v.DoneAt) > landReviewKeep {
			delete(m, k)
		}
	}
	m[r.By] = r
	os.MkdirAll(filepath.Dir(s.path), 0o755)
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// inspected is what the factory needs from `paseo inspect <id> --json`.
type inspected struct {
	Id       string
	Status   string
	Archived bool
	Cwd      string
}

func (f *Factory) inspect(id string) (inspected, error) {
	var s inspected
	out, err := f.paseo("inspect", id, "--json")
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return s, fmt.Errorf("paseo inspect %s: %v", short(id), err)
	}
	return s, nil
}

// landReviewRoots are where a landing review may run, under the factory user's home.
var landReviewRoots = []string{"dev", ".paseo/worktrees"}

// startLandReview checks the request and starts a reviewer for agent b.By's checkout. The checkout is
// read from Paseo, never from the request: a caller on the tailnet can only get a read-only reviewer for
// an agent that already exists, as /api/review allows.
func (f *Factory) startLandReview(b Body) (LandReview, error) {
	if strings.TrimSpace(b.By) == "" {
		return LandReview{}, errors.New("by: your PASEO_AGENT_ID is required")
	}
	ag, err := f.inspect(b.By)
	if err != nil {
		return LandReview{}, fmt.Errorf("unknown agent %s: %v", short(b.By), err)
	}
	if ag.Id == "" || ag.Archived {
		return LandReview{}, fmt.Errorf("agent %s is archived or unknown", short(b.By))
	}
	by := ag.Id
	a, known := f.agentOK(by)
	if known && a.L(labelRole) != "" {
		return LandReview{}, fmt.Errorf("agent %s is a factory %s: it does not land", short(by), a.L(labelRole))
	}
	isCard := known && a.L(labelStage) != ""
	if isCard {
		cur, why := f.currentCardLandBlock(by)
		if why != "" {
			return LandReview{}, fmt.Errorf("card %s is not authorized to land: %s", short(by), why)
		}
		a = cur
	}
	cwd, err := f.landReviewCwd(ag.Cwd)
	if err != nil {
		return LandReview{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.GitOut(ctx, cwd, "remote", "get-url", "origin"); err != nil {
		return LandReview{}, fmt.Errorf("%s has no origin remote: %v", cwd, err)
	}
	dirty, err := f.dirtyStatus(ctx, cwd)
	if err != nil {
		return LandReview{}, err
	}
	if dirty != "" {
		return LandReview{}, fmt.Errorf("worktree not clean: commit your work (do not land) and delete or gitignore "+
			"scratch files, then ask again. git status:\n%s", trunc(dirty, 1500))
	}
	head, err := f.GitOut(ctx, cwd, "rev-parse", "HEAD")
	if err != nil {
		return LandReview{}, err
	}
	head = strings.TrimSpace(head)
	prev, had := f.landReviews.Get(by)
	same := had && prev.Head == head && prev.Cwd == cwd && prev.Card == isCard
	switch {
	case same && prev.Status == "running":
		return prev, nil
	case isCard && head == strings.TrimSpace(a.L(labelReviewed)) && head == strings.TrimSpace(a.L(labelCodeOK)):
		r := LandReview{By: by, Head: head, Cwd: cwd, Repo: a.Repo, Visibility: f.visibility(cwd), Card: true,
			Why: trunc(strings.TrimSpace(b.Why), 500), Status: "done", Verdict: "approve", StartedAt: f.Now(), DoneAt: f.Now(),
			Findings: "HEAD is the commit the card's code review approved for landing: no new review."}
		if err := f.landReviews.put(r, f.Now()); err != nil {
			return LandReview{}, err
		}
		f.logf("landing review for %s at %s: its code approval stands", short(by), short(head))
		return r, nil
	case !isCard && same && prev.Status == "done" && (prev.Verdict == "approve" || prev.Verdict == "nits"):
		return prev, nil
	}
	c := Card{ID: by, Host: f.cfg.Host, Title: a.Title, Cwd: cwd, Repo: a.Repo, Branch: a.Branch, WorkspaceID: a.WorkspaceID}
	if c.Title == "" {
		c.Title = "agent " + short(by)
	}
	why := trunc(strings.TrimSpace(b.Why), 500)
	vis := f.visibility(cwd)
	rid, err := f.runReviewer(c, "land", reviewSpec{}, vis, "Landing review: "+trunc(c.Title, 50), why, head)
	if err != nil {
		return LandReview{}, err
	}
	r := LandReview{By: by, Review: rid, Head: head, Cwd: cwd, Repo: c.Repo, Visibility: vis, Why: why,
		Card: isCard, Status: "running", StartedAt: f.Now()}
	if err := f.landReviews.put(r, f.Now()); err != nil {
		return r, err
	}
	f.logf("started landing review %s for %s at %s (%s)", short(rid), short(by), short(head), vis)
	return r, nil
}

// landReviewView is agent by's latest landing review as GET /api/land-review reports it: a card's
// approval reads as an escalation once the card may no longer land (current state, as at the start
// and finish). The stored record is left as it was.
func (f *Factory) landReviewView(by string) (LandReview, bool) {
	r, ok := f.landReviews.Get(by)
	if !ok || !r.Card || r.Status != "done" || (r.Verdict != "approve" && r.Verdict != "nits") {
		return r, ok
	}
	if _, why := f.currentCardLandBlock(r.By); why != "" {
		r.Verdict, r.Findings = "escalate", "The card is no longer authorized to land ("+why+"): do not land.\n\n"+r.Findings
	}
	return r, true
}

// landReviewCwd resolves cwd (symlinks too) and checks it is a git checkout under landReviewRoots.
func (f *Factory) landReviewCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", errors.New("agent has no working directory")
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("working directory: %v", err)
	}
	inside := false
	for _, root := range landReviewRoots {
		r, err := filepath.EvalSymlinks(filepath.Join(f.cfg.Home, root))
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(r, real); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			inside = true
		}
	}
	if !inside {
		return "", fmt.Errorf("%s is outside ~/dev and ~/.paseo/worktrees", real)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.GitOut(ctx, real, "rev-parse", "--show-toplevel"); err != nil {
		return "", fmt.Errorf("%s is not a git checkout", real)
	}
	return real, nil
}

// cardLandBlock is why factory card a may not land now ("" when it may): it must be in Landing,
// not archived or parked, not held on a dependency, not replaced, with a reviewed commit recorded, no
// review queued and no rejecting verdict.
func cardLandBlock(a Agent) string {
	v := a.L(labelVerdict)
	switch {
	case a.ID == "":
		return "not on the board"
	case a.archived():
		return "archived or parked"
	case a.L(labelReplacedBy) != "":
		return "replaced by " + short(a.L(labelReplacedBy))
	case a.L(labelStage) != "landing":
		return "stage is " + a.L(labelStage) + ", not landing"
	case a.L(labelHeld) != "":
		return "held until " + a.L(labelHeld) + " lands"
	case a.L(labelAutoReview) != "":
		return "a " + a.L(labelAutoReview) + " review is queued"
	case v == "close" || strings.HasSuffix(v, " changes") || strings.HasSuffix(v, " none"):
		return "last verdict is " + v
	case strings.TrimSpace(a.L(labelReviewed)) == "":
		return "no reviewed commit recorded"
	}
	return ""
}

// currentCardLandBlock checks card id from current state, not only the board's snapshot (a Drop, a
// move or a new review since the last read looks the same there): the snapshot, the card's Paseo agent
// file and Paseo's archive state must each pass cardLandBlock, and no code or plan reviewer of the card
// may be pending. It returns the card with its current labels; a failed read blocks.
func (f *Factory) currentCardLandBlock(id string) (Agent, string) {
	snap, ok := f.agentOK(id)
	if !ok {
		return Agent{}, "not on the board"
	}
	if why := cardLandBlock(snap); why != "" {
		return snap, why
	}
	labels, err := f.agentFileLabels(snap.ID)
	if err != nil {
		return snap, "current state unreadable: " + trunc(err.Error(), 150)
	}
	archived, _, err := f.liveState(snap.ID)
	if err != nil {
		return snap, "current state unreadable: " + trunc(err.Error(), 150)
	}
	cur := snap
	cur.Labels = labels
	if archived && cur.ArchivedAt.IsZero() {
		cur.ArchivedAt = f.Now()
	}
	if why := cardLandBlock(cur); why != "" {
		return cur, why
	}
	byID := f.byID()
	for _, r := range f.agents() {
		if r.L(labelRole) == "reviewer" && r.L(labelReviewKind) != "land" && r.L(labelApplied) == "" &&
			sameCard(byID, r.L(labelReviewOf), snap.ID) {
			return cur, "review " + short(r.ID) + " is pending"
		}
	}
	return cur, ""
}

// finishLandReview records a landing reviewer's verdict for the agent that asked. The agent reads it
// from GET /api/land-review; nothing is sent to it and no card moves. A card's approval is usable only
// if the card is still authorized to land, from current state: then the approved HEAD becomes its
// reviewed and approved commit (before the approval is stored, so a failed label write is retried),
// else the answer is an escalation.
func (f *Factory) finishLandReview(rev Agent, verdict, notes string) error {
	by := rev.L(labelReviewOf)
	r, ok := f.landReviews.Get(by)
	if !ok || r.Review != rev.ID {
		f.logf("landing reviewer %s: no matching request from %s", short(rev.ID), short(by))
		return nil
	}
	if r.Card && (verdict == "approve" || verdict == "nits") {
		if _, why := f.currentCardLandBlock(by); why != "" {
			f.logf("landing review %s for %s: %s approval withheld, card %s", short(rev.ID), short(by), verdict, why)
			verdict, notes = "escalate", "The card is no longer authorized to land ("+why+"): do not land.\n\n"+notes
		} else if err := f.setLabels(by, map[string]string{labelReviewed: r.Head, labelCodeOK: r.Head}); err != nil {
			return err
		}
	}
	r.Status, r.Verdict, r.Findings, r.DoneAt = "done", verdict, notes, f.Now()
	if err := f.landReviews.put(r, f.Now()); err != nil {
		return err
	}
	if a, ok := f.agentOK(by); ok && !a.archived() {
		if err := f.setLabels(by, map[string]string{labelLandReview: verdict + "@" + short(r.Head)}); err != nil {
			f.logf("labelling %s: %v", short(by), err)
		}
	}
	f.logf("landing review %s for %s: %s", short(rev.ID), short(by), verdict)
	return nil
}

// Repo visibility. Public repos get a private-data check in review. The factory asks GitHub itself
// (no token: 200 is public, 404 is private or missing) and caches the answer for an hour. Anything it
// cannot tell is "unknown", which reviewers treat as public.

const visibilityTTL = time.Hour

type visEntry struct {
	v  string
	at time.Time
}

var githubRemoteRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// visibility is "public", "private" or "unknown" for cwd's origin remote.
func (f *Factory) visibility(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url, err := f.GitOut(ctx, cwd, "remote", "get-url", "origin")
	if err != nil {
		return "unknown"
	}
	m := githubRemoteRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "unknown"
	}
	repo := m[1] + "/" + m[2]
	f.mu.Lock()
	e, ok := f.vis[repo]
	f.mu.Unlock()
	if ok && f.Now().Sub(e.at) < visibilityTTL {
		return e.v
	}
	v := f.askGitHub(ctx, repo)
	if v != "unknown" {
		f.mu.Lock()
		f.vis[repo] = visEntry{v, f.Now()}
		f.mu.Unlock()
	}
	return v
}

func (f *Factory) askGitHub(ctx context.Context, repo string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", f.GitHubAPI+"/repos/"+repo, nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		var r struct {
			Private *bool `json:"private"`
		}
		if json.NewDecoder(resp.Body).Decode(&r) == nil && r.Private != nil && !*r.Private {
			return "public"
		}
	case 404:
		return "private"
	}
	return "unknown"
}

// visibilityLine is the prompt line telling a reviewer whether the repo is public.
func visibilityLine(v string) string {
	switch v {
	case "public":
		return "Repo visibility: PUBLIC (GitHub says so). Everything that lands is published."
	case "private":
		return "Repo visibility: private (GitHub returns 404 without auth)."
	}
	return "Repo visibility: unknown (could not check). Treat it as public."
}
