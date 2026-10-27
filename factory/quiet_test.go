package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func quietAnchorFixture(f *Factory) Agent {
	a := card("quiet-anchor", map[string]string{labelRole: quietAnchorRole})
	a.Cwd = filepath.Join(f.cfg.StateDir, "quiet-anchor")
	a.WorkspaceID = "anchor-workspace"
	a.Labels[parentAgentLabel] = quietBootstrapParent
	return a
}

func TestQuietLaunchUsesLiveAnchorForEveryProvider(t *testing.T) {
	for _, choice := range AgentChoices {
		t.Run(choice.ID, func(t *testing.T) {
			f, p := newTestFactory(t)
			f.SetAgents([]Agent{quietAnchorFixture(f)})
			f.quiet = quietState{}
			for _, role := range []string{"card", "reviewer", "scout", "picker", "replacement"} {
				args := append(choice.runArgs(), "--cwd", "/work/example", "--label", "factory.role="+role, "prompt")
				if _, err := f.paseo(args...); err != nil {
					t.Fatal(err)
				}
			}
			for _, call := range runArgs(p) {
				labels := quietRunLabels(call)
				if labels[parentAgentLabel] != "quiet-anchor" || labels[labelPrefix+labelQuiet] != "quiet-anchor" {
					t.Fatalf("launch is not quiet: %v", call)
				}
			}
		})
	}
}

func TestQuietReconcilePreservesParentsAndPersonalAgents(t *testing.T) {
	f, p := newTestFactory(t)
	root := card("root", map[string]string{labelStage: "building"})
	root.Status = "running"
	child := Agent{ID: "child", Status: "running", Labels: map[string]string{parentAgentLabel: "root"}}
	grandchild := Agent{ID: "grandchild", Status: "idle", Labels: map[string]string{parentAgentLabel: "child"}}
	personal := Agent{ID: "personal", Status: "idle", Labels: map[string]string{}}
	anchor := quietAnchorFixture(f)
	f.SetAgents([]Agent{grandchild, personal, child, root, anchor})
	if err := f.reconcileQuiet(); err != nil {
		t.Fatal(err)
	}
	if p.labels("root")[parentAgentLabel] != anchor.ID {
		t.Fatal("root was not quieted")
	}
	for _, a := range []Agent{child, grandchild} {
		if got := p.labels(a.ID)[parentAgentLabel]; got != a.Labels[parentAgentLabel] {
			t.Fatalf("parent changed for %s: %q", a.ID, got)
		}
	}
	if len(p.verbs("personal")) != 0 {
		t.Fatal("personal agent changed")
	}
	before := p.countCalls("agent", "update")
	if err := f.reconcileQuiet(); err != nil {
		t.Fatal(err)
	}
	if p.countCalls("agent", "update") != before {
		t.Fatal("reconciliation is not idempotent")
	}
}

func TestQuietAnchorRecoveryAndSend(t *testing.T) {
	f, p := newTestFactory(t)
	old := quietAnchorFixture(f)
	old.ID = "old-anchor"
	old.ArchivedAt = t0
	root := card("root", map[string]string{labelStage: "zach", labelQuiet: old.ID})
	root.Labels[parentAgentLabel] = old.ID
	f.SetAgents([]Agent{old, root})
	f.quiet = quietState{}
	if err := f.send(root.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	labels := p.labels(root.ID)
	if labels[parentAgentLabel] != "new-agent-1" {
		t.Fatalf("old anchor retained: %v", labels)
	}
	runs := runArgs(p)
	if len(runs) != 1 || quietRunLabels(runs[0])[parentAgentLabel] != quietBootstrapParent || argAfter(runs[0], "--new-workspace") != "local" {
		t.Fatalf("anchor bootstrap is not isolated and quiet: %v", runs)
	}
	if argAfter(runs[0], "--provider") != ReviewerModel {
		t.Fatal("wrong anchor model")
	}
	if len(p.sent(root.ID)) != 1 || len(p.verbs(root.ID)) == 0 || p.verbs(root.ID)[0] != "update" {
		t.Fatal("sent before quiet labeling")
	}
	// Simulate Paseo detaching a cross-workspace child after the replacement anchor is archived.
	root.Labels = map[string]string{labelPrefix + labelStage: "zach", labelPrefix + labelQuiet: "new-agent-1"}
	f.SetAgents([]Agent{root})
	p.archived["new-agent-1"] = true
	f.Now = func() time.Time { return t0.Add(time.Minute) }
	if err := f.reconcileQuiet(); err != nil {
		t.Fatal(err)
	}
	if got := p.labels(root.ID)[parentAgentLabel]; got != "new-agent-2" {
		t.Fatalf("detached card not repaired: %s", got)
	}
}

func TestQuietAnchorCreationSerializedAndReusedAfterRestart(t *testing.T) {
	f, p := newTestFactory(t)
	f.quiet = quietState{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.ensureQuietAnchor(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p.countCalls("run", "") != 1 {
		t.Fatal("duplicate anchors")
	}
	// A fresh process finds the labeled anchor on disk rather than creating another one.
	a := quietAnchorFixture(f)
	writeFile(t, agentFile(f.cfg.Home, a.ID), `{"id":"quiet-anchor","lastStatus":"idle","cwd":`+func() string { b, _ := json.Marshal(a.Cwd); return string(b) }()+`,"workspaceId":"anchor-workspace","labels":{"factory.role":"quiet-anchor"}}`)
	f.quiet = quietState{}
	f.SetAgents(nil)
	id, err := f.ensureQuietAnchor()
	if err != nil || id != a.ID || p.countCalls("run", "") != 1 {
		t.Fatalf("restart reuse: %q %v", id, err)
	}
}

func TestQuietBoardHealthAndReap(t *testing.T) {
	f, p := newTestFactory(t)
	anchor := quietAnchorFixture(f)
	anchor.LastActivityAt = t0.Add(-24 * time.Hour)
	root := card("root", map[string]string{labelStage: "building"})
	root.Labels[parentAgentLabel] = anchor.ID
	root.Status = "running"
	rev := reviewer("review", "root", "code")
	rev.Labels[parentAgentLabel] = anchor.ID
	sub := Agent{ID: "sub", Status: "idle", Labels: map[string]string{parentAgentLabel: rev.ID}}
	f.SetAgents([]Agent{anchor, root, rev, sub})
	board := BuildBoard(f.agents(), nil, t0)
	if len(board) != 1 || board[0].ID != root.ID || len(board[0].Children) != 2 {
		t.Fatalf("wrong grouping: %+v", board)
	}
	n := f.agentCounts()
	if n.ByKind[quietAnchorRole] != 1 || len(n.Cards) != 1 || n.Cards[0].Live != 3 {
		t.Fatalf("wrong health counts: %+v", n)
	}
	f.reap(t0)
	if len(p.verbs(anchor.ID)) != 0 {
		t.Fatal("anchor was reaped")
	}
}

type quietFailPaseo struct {
	*fakePaseo
	fail string
}

func (p quietFailPaseo) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == p.fail {
		return "", errors.New("test failure")
	}
	return p.fakePaseo.Run(ctx, args...)
}

func TestQuietFailuresPreventLaunchAndSend(t *testing.T) {
	f, p := newTestFactory(t, card("root", map[string]string{labelStage: "building"}))
	f.quiet = quietState{}
	f.p = quietFailPaseo{p, "run"}
	if _, err := f.paseo("run", "--provider", ReviewerModel, "prompt"); err == nil {
		t.Fatal("unquiet launch accepted")
	}
	if len(runArgs(p)) != 0 {
		t.Fatal("launched despite anchor failure")
	}
	f.p = quietFailPaseo{p, "agent"}
	f.SetAgents([]Agent{card("root", map[string]string{labelStage: "building"})})
	f.quiet = quietState{id: "quiet-anchor", checked: t0}
	if err := f.send("root", "continue"); err == nil || len(p.sent("root")) != 0 {
		t.Fatal("sent despite label failure")
	}
}

func TestQuietPassiveDoesNotCreateRuntimeState(t *testing.T) {
	for _, dry := range []bool{false, true} {
		f, p := newTestFactory(t, card("root", map[string]string{labelStage: "building"}))
		f.quiet = quietState{}
		f.cfg.ReadOnly = !dry
		f.cfg.DryRun = dry
		if err := f.reconcileQuiet(); err != nil {
			t.Fatal(err)
		}
		if len(p.calls) != 0 {
			t.Fatal("passive reconciliation called Paseo")
		}
		if _, err := os.Stat(filepath.Join(f.cfg.StateDir, "quiet-anchor")); !os.IsNotExist(err) {
			t.Fatal("passive reconciliation created directory")
		}
	}
}

func TestQuietRealLaunchPaths(t *testing.T) {
	for _, kind := range []string{"card", "picker", "scout", "plan", "code", "land"} {
		t.Run(kind, func(t *testing.T) {
			f, p := newTestFactory(t)
			withRepos(t, f, "example/desk")
			var err error
			switch kind {
			case "card":
				_, err = f.newCard(Body{Repo: "example/desk", Request: "Check the build"})
			case "picker":
				_, err = f.startPicker(Route{Request: "Check the build", Title: "Build"})
			case "scout":
				_, err = f.startScout("example/desk", "Check the build")
			default:
				_, err = f.runReviewer(Card{ID: "root", Repo: "example/desk", Cwd: "/work/example", WorkspaceID: "card-workspace"}, kind, "private", "Review", "Check the build", "example-head")
			}
			if err != nil {
				t.Fatal(err)
			}
			runs := runArgs(p)
			if len(runs) != 1 || quietRunLabels(runs[0])[parentAgentLabel] != "quiet-anchor" {
				t.Fatalf("unquiet %s launch: %v", kind, runs)
			}
		})
	}
}

func TestQuietRepairSkipsStaleArchivedCards(t *testing.T) {
	f, p := newTestFactory(t)
	f.SetAgents([]Agent{card("root", map[string]string{labelStage: "building"})})
	p.archived["root"] = true
	if err := f.reconcileQuiet(); err != nil {
		t.Fatal(err)
	}
	if len(p.labels("root")) != 0 {
		t.Fatal("updated a card archived since the snapshot")
	}
}

func TestQuietPersonalSendAndRealParent(t *testing.T) {
	f, p := newTestFactory(t)
	root := card("root", map[string]string{labelStage: "building"})
	root.Labels[parentAgentLabel] = "real-parent"
	f.SetAgents([]Agent{root, Agent{ID: "personal", Labels: map[string]string{}}})
	if err := f.send("root", "continue"); err != nil {
		t.Fatal(err)
	}
	if got := p.labels("root")[parentAgentLabel]; got != "real-parent" {
		t.Fatalf("changed real parent: %q", got)
	}
	if err := f.send("personal", "continue"); err != nil {
		t.Fatal(err)
	}
	if len(p.labels("personal")) != 0 {
		t.Fatal("quieted personal agent")
	}
}

func TestQuietSendRepairsEvenWhenSnapshotStillLooksQuiet(t *testing.T) {
	f, p := newTestFactory(t, card("root", map[string]string{labelStage: "zach"}))
	if err := f.send("root", "continue"); err != nil {
		t.Fatal(err)
	}
	if p.labels("root")[parentAgentLabel] != "quiet-anchor" {
		t.Fatal("send trusted a potentially stale quiet snapshot")
	}
}

func TestQuietMissingAnchorAndTransientLookupFailure(t *testing.T) {
	for _, missing := range []bool{false, true} {
		f, p := newTestFactory(t)
		f.Now = func() time.Time { return t0.Add(time.Minute) }
		if missing {
			p.unknown["quiet-anchor"] = true
		} else {
			p.inspectErr = errors.New("daemon unavailable")
		}
		id, err := f.ensureQuietAnchor()
		if missing {
			if err != nil || id != "new-agent-1" {
				t.Fatalf("missing anchor was not replaced: %q %v", id, err)
			}
		} else if err == nil || p.countCalls("run", "") != 0 {
			t.Fatal("transient lookup failure duplicated anchor")
		}
	}
}

func TestQuietParkedCardResumesWithNewAnchor(t *testing.T) {
	f, p := newTestFactory(t)
	a := card("root", map[string]string{labelStage: "zach", labelParked: iso(t0), labelQuiet: "old-anchor"})
	a.Labels[parentAgentLabel] = "old-anchor"
	a.ArchivedAt = t0
	f.SetAgents([]Agent{a})
	p.archived[a.ID] = true
	if err := f.send(a.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	labels := p.labels(a.ID)
	if labels[parentAgentLabel] != "quiet-anchor" || labels[labelParked] != "" || p.archived[a.ID] {
		t.Fatalf("resume lost quiet labels or park state: %v", labels)
	}
}

func TestQuietAnchorCannotBeArchivedThroughFactory(t *testing.T) {
	f, p := newTestFactory(t)
	a := quietAnchorFixture(f)
	f.SetAgents([]Agent{a})
	if err := f.archiveAgent(a.ID); err == nil {
		t.Fatal("factory archived its quiet anchor")
	}
	if len(p.verbs(a.ID)) != 0 {
		t.Fatal("anchor archive reached Paseo")
	}
}

func TestQuietLaunchPreservesExplicitParentAndIgnoresPromptFlags(t *testing.T) {
	f, p := newTestFactory(t)
	_, err := f.paseo("run", "--provider", ReviewerModel, "--label", parentAgentLabel+"=real-parent", "--label", "factory.role=reviewer", "--label paseo.parent-agent-id=prompt-value")
	if err != nil {
		t.Fatal(err)
	}
	if quietRunLabels(runArgs(p)[0])[parentAgentLabel] != "real-parent" {
		t.Fatal("explicit parent lost or prompt parsed as flags")
	}
}
