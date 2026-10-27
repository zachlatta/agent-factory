package factory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scheduleFixture struct {
	request           scheduleRequest
	policy            schedulePolicy
	now               time.Time
	workspace, output string
}

func scheduleJSON(v any) []byte           { b, _ := json.Marshal(v); return b }
func scheduleMap(b []byte) map[string]any { var m map[string]any; json.Unmarshal(b, &m); return m }
func scheduleEdit(b []byte, edit func(map[string]any)) []byte {
	m := scheduleMap(b)
	edit(m)
	return scheduleJSON(m)
}
func newScheduleFixture(t *testing.T) scheduleFixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	state := filepath.Join(root, "state")
	output := filepath.Join(root, "result")
	for _, p := range []string{workspace, state} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(output, []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2020, 1, 2, 12, 0, 0, 0, time.UTC)
	d := map[string]any{"version": 1, "schedulerId": "scheduler", "scheduleId": "schedule", "resultKind": "durable-output", "requirements": map[string]any{"outputs": []any{map[string]any{"name": "result", "path": output}}}}
	i := map[string]any{"runId": "run", "agentId": "agent", "workspaceId": "workspace", "canonicalPath": workspace, "commonDir": nil, "branch": nil, "head": nil}
	o := map[string]any{"status": "succeeded", "startedAt": now.Add(-time.Minute).Format(time.RFC3339Nano), "endedAt": now.Add(-30 * time.Second).Format(time.RFC3339Nano), "interrupted": false, "canceled": false, "pendingPermission": false, "unresolved": false}
	manifest := map[string]any{"outputs": []any{map[string]any{"name": "result", "sha256": scheduleHash([]byte("result"))}}}
	// The fixed field shapes match the v1 producer, including null non-Git identity.
	r := map[string]any{"version": 1, "recordKind": "success", "schedulerId": "scheduler", "scheduleId": "schedule", "resultKind": "durable-output", "outcome": o, "declarationDigest": scheduleHash(scheduleCanonicalTest(t, d)), "manifestDigest": scheduleHash(scheduleCanonicalTest(t, manifest)), "verifiedAt": now.Add(-20 * time.Second).Format(time.RFC3339Nano), "retirementAuthorized": false, "evidence": map[string]any{"kind": "durable-output", "outputs": []any{map[string]any{"name": "result", "path": output, "sha256": scheduleHash([]byte("result")), "size": 6}}}}
	for k, v := range i {
		r[k] = v
	}
	receipt := scheduleJSON(r)
	release := scheduleRelease{SchedulerID: "scheduler", ScheduleID: "schedule", RunID: "run", AgentID: "agent", WorkspaceID: "workspace", CanonicalPath: workspace, Generation: "generation", ReceiptDigest: scheduleHash(receipt), DeclarationProvenance: "approved-declaration", ReleasedAt: now.Add(-10 * time.Second), ExpiresAt: now.Add(time.Minute)}
	return scheduleFixture{scheduleRequest{receipt, scheduleJSON(d), scheduleJSON(o), scheduleJSON(i), scheduleJSON(manifest), release}, schedulePolicy{state, time.Minute}, now, workspace, output}
}
func scheduleCanonicalTest(t *testing.T, v any) []byte {
	t.Helper()
	m, err := scheduleObject(scheduleJSON(v))
	if err != nil {
		t.Fatal(err)
	}
	return scheduleCanonical(m)
}
func (f scheduleFixture) validate() error {
	return validateScheduleEvidence(context.Background(), f.policy, f.request, f.now)
}

func TestScheduleEvidenceStrictV1(t *testing.T) {
	f := newScheduleFixture(t)
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*scheduleFixture){
		"unknown receipt key": func(f *scheduleFixture) {
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["extra"] = true })
		},
		"missing false flag": func(f *scheduleFixture) {
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { delete(m, "retirementAuthorized") })
		},
		"permission": func(f *scheduleFixture) {
			f.request.Outcome = scheduleEdit(f.request.Outcome, func(m map[string]any) { m["pendingPermission"] = true })
		},
		"failed": func(f *scheduleFixture) {
			f.request.Outcome = scheduleEdit(f.request.Outcome, func(m map[string]any) { m["status"] = "failed" })
		},
		"outcome record": func(f *scheduleFixture) {
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["recordKind"] = "outcome" })
		},
		"version boolean": func(f *scheduleFixture) {
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["version"] = true })
		},
		"unknown requirements": func(f *scheduleFixture) {
			f.request.Declaration = scheduleEdit(f.request.Declaration, func(m map[string]any) { m["requirements"].(map[string]any)["quality"] = true })
		},
		"partial identity": func(f *scheduleFixture) {
			f.request.Identity = scheduleEdit(f.request.Identity, func(m map[string]any) { m["branch"] = "topic" })
		},
		"altered digest": func(f *scheduleFixture) { f.request.Release.ReceiptDigest = strings.Repeat("0", 64) },
		"altered output": func(f *scheduleFixture) { os.WriteFile(f.output, []byte("changed"), 0600) },
		"output symlink": func(f *scheduleFixture) {
			os.Rename(f.output, f.output+"-saved")
			os.Symlink(f.output+"-saved", f.output)
		},
		"unset store":              func(f *scheduleFixture) { f.policy.Store = "" },
		"unset age":                func(f *scheduleFixture) { f.policy.MaxAge = 0 },
		"expired receipt":          func(f *scheduleFixture) { f.now = f.now.Add(41 * time.Second) },
		"expired release":          func(f *scheduleFixture) { f.request.Release.ExpiresAt = f.now },
		"future release":           func(f *scheduleFixture) { f.request.Release.ReleasedAt = f.now.Add(time.Second) },
		"missing provenance":       func(f *scheduleFixture) { f.request.Release.DeclarationProvenance = "" },
		"release identity":         func(f *scheduleFixture) { f.request.Release.WorkspaceID = "other" },
		"state overlaps workspace": func(f *scheduleFixture) { f.policy.Store = f.workspace },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newScheduleFixture(t)
			edit(&f)
			if err := f.validate(); err == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"nested":{"a":1,"a":2}}`, `{} {}`, `null`, `{"version":1.0}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := scheduleObject([]byte(raw)); err == nil {
				t.Fatal("accepted ambiguous or noncanonical input")
			}
		})
	}
}

func TestScheduleCanonicalProducerEncoding(t *testing.T) {
	m, err := scheduleObject([]byte(`{"z":"<>&é😀","a":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(scheduleCanonical(m)); got != `{"a":false,"z":"<>&\u00e9\ud83d\ude00"}` {
		t.Fatal(got)
	}
}

type scheduleTestOwner struct{ revoked bool }

func (a scheduleTestOwner) withRelease(_ context.Context, _ scheduleRelease, accept func() error) error {
	if a.revoked {
		return errScheduleEvidence
	}
	return accept()
}
func TestScheduleAcknowledgmentRefusesProductionAndReplay(t *testing.T) {
	f := newScheduleFixture(t)
	if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleUnavailableOwner{}); err == nil {
		t.Fatal("production accepted")
	}
	entries, _ := os.ReadDir(f.policy.Store)
	if len(entries) != 0 {
		t.Fatal("production wrote state")
	}
	if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{true}); err == nil {
		t.Fatal("revoked accepted")
	}
	if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{}); err != nil {
		t.Fatal(err)
	}
	// Reopening policy/store after a simulated restart must still refuse replay.
	if err := acknowledgeSchedule(context.Background(), schedulePolicy{f.policy.Store, f.policy.MaxAge}, f.request, f.now, scheduleTestOwner{}); err == nil {
		t.Fatal("replay accepted")
	}
	f.request.Release.Generation = "next-generation"
	if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{}); err == nil {
		t.Fatal("generation overwrite accepted")
	}
	if b, err := os.ReadFile(f.output); err != nil || string(b) != "result" {
		t.Fatal("resources changed")
	}
}
func TestScheduleAcknowledgmentConcurrentAndOverlap(t *testing.T) {
	f := newScheduleFixture(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{}) == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("claims: %d", successes.Load())
	}
	other := newScheduleFixture(t)
	other.policy = f.policy
	other.request.Release.RunID = "other-run"
	other.request.Release.AgentID = "other-agent"
	other.request.Release.WorkspaceID = "other-workspace"
	other.request.Release.CanonicalPath = filepath.Join(f.workspace, "nested")
	// Store protection also applies to conflicts from other run/workspace identities.
	if err := scheduleClaim(other.policy, other.request.Release, other.now); err == nil {
		t.Fatal("overlapping path accepted")
	}
	// With only the overlap removed, the same otherwise-distinct claim succeeds.
	other.request.Release.CanonicalPath = other.workspace
	if err := scheduleClaim(other.policy, other.request.Release, other.now); err != nil {
		t.Fatal("disjoint claim refused:", err)
	}
	other.request.Release = f.request.Release
	other.request.Release.CanonicalPath = other.workspace
	other.request.Release.RunID = "other"
	if err := scheduleClaim(other.policy, other.request.Release, other.now); err == nil {
		t.Fatal("workspace identity reused")
	}
}
func TestScheduleStoreUnknownStateAndLostAcknowledgment(t *testing.T) {
	for _, mode := range []string{"corrupt", "symlink", "insecure", "missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newScheduleFixture(t)
			switch mode {
			case "corrupt":
				os.WriteFile(filepath.Join(f.policy.Store, "unknown"), []byte("{"), 0600)
			case "symlink":
				p := f.policy.Store + "-link"
				os.Symlink(f.policy.Store, p)
				f.policy.Store = p
			case "insecure":
				os.Chmod(f.policy.Store, 0777)
			case "missing":
				f.policy.Store = filepath.Join(f.policy.Store, "absent")
			}
			if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{}); err == nil {
				t.Fatal("unsafe store accepted")
			}
		})
	}
	f := newScheduleFixture(t)
	// A durable claim followed by a lost reply cannot authorize another acceptance.
	if err := scheduleClaim(f.policy, f.request.Release, f.now); err != nil {
		t.Fatal(err)
	}
	if err := acknowledgeSchedule(context.Background(), f.policy, f.request, f.now, scheduleTestOwner{}); err == nil {
		t.Fatal("lost reply renewed authority")
	}
}
func TestSchedulePolicyNoDefaults(t *testing.T) {
	if _, err := schedulePolicyFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("default policy enabled")
	}
	f := newScheduleFixture(t)
	p, err := schedulePolicyFromEnv(func(k string) string {
		switch k {
		case "FACTORY_SCHEDULE_RECEIPT_MAX_AGE":
			return "1m"
		case "FACTORY_SCHEDULE_ACK_STORE":
			return f.policy.Store
		}
		return ""
	})
	if err != nil || p != f.policy {
		t.Fatal(p, err)
	}
}

func TestScheduleCommitFreshFetchAndRewrittenLanding(t *testing.T) {
	f := newScheduleFixture(t)
	root := t.TempDir()
	retirementTestGit(t, root, "init", "-b", "main")
	retirementTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@users.noreply.github.com", "commit", "--allow-empty", "-m", "base")
	head := retirementTestGit(t, root, "rev-parse", "HEAD")
	common := retirementTestGit(t, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	d := scheduleMap(f.request.Declaration)
	d["resultKind"] = "commit"
	d["requirements"] = map[string]any{"remote": root, "landingRef": "refs/heads/main"}
	id := scheduleMap(f.request.Identity)
	id["canonicalPath"] = root
	id["commonDir"] = common
	id["branch"] = "main"
	id["head"] = head
	manifest := map[string]any{"head": head}
	r := scheduleMap(f.request.Receipt)
	for k, v := range id {
		r[k] = v
	}
	r["resultKind"] = "commit"
	r["declarationDigest"] = scheduleHash(scheduleCanonicalTest(t, d))
	r["manifestDigest"] = scheduleHash(scheduleCanonicalTest(t, manifest))
	r["evidence"] = map[string]any{"kind": "commit", "remote": root, "landingRef": "refs/heads/main", "fetchedHead": head}
	f.request.Declaration = scheduleJSON(d)
	f.request.Identity = scheduleJSON(id)
	f.request.Manifest = scheduleJSON(manifest)
	f.request.Receipt = scheduleJSON(r)
	f.request.Release.CanonicalPath = root
	f.request.Release.CommonDir = common
	f.request.Release.Branch = "main"
	f.request.Release.Head = head
	f.request.Release.ReceiptDigest = scheduleHash(f.request.Receipt)
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
	// The receipt stays immutable while the owner's landing branch advances.
	retirementTestGit(t, root, "branch", "run", head)
	retirementTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@users.noreply.github.com", "commit", "--allow-empty", "-m", "advance")
	retirementTestGit(t, root, "checkout", "run")
	f.request.Identity = scheduleEdit(f.request.Identity, func(m map[string]any) { m["branch"] = "run" })
	f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["branch"] = "run" })
	f.request.Release.Branch = "run"
	f.request.Release.ReceiptDigest = scheduleHash(f.request.Receipt)
	if err := f.validate(); err != nil {
		t.Fatal("ordinary landing advance:", err)
	}
	before := retirementTestGit(t, root, "show-ref")
	missing := filepath.Join(t.TempDir(), "unavailable")
	f.request.Declaration = scheduleEdit(f.request.Declaration, func(m map[string]any) { m["requirements"].(map[string]any)["remote"] = missing })
	f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) {
		m["declarationDigest"] = scheduleHash(scheduleCanonicalTest(t, scheduleMap(f.request.Declaration)))
		m["evidence"].(map[string]any)["remote"] = missing
	})
	f.request.Release.ReceiptDigest = scheduleHash(f.request.Receipt)
	if err := f.validate(); err == nil {
		t.Fatal("missing remote accepted")
	}
	f.request.Declaration = scheduleJSON(d)
	f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) {
		m["declarationDigest"] = scheduleHash(scheduleCanonicalTest(t, d))
		m["evidence"].(map[string]any)["remote"] = root
	})
	f.request.Release.ReceiptDigest = scheduleHash(f.request.Receipt)
	if before != retirementTestGit(t, root, "show-ref") {
		t.Fatal("validation changed source refs")
	}
	// Replace landing ref with unrelated history, without moving the run branch.
	retirementTestGit(t, root, "checkout", "--orphan", "replacement")
	retirementTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@users.noreply.github.com", "commit", "--allow-empty", "-m", "replacement")
	replacement := retirementTestGit(t, root, "rev-parse", "HEAD")
	retirementTestGit(t, root, "checkout", "run")
	retirementTestGit(t, root, "update-ref", "refs/heads/main", replacement)
	if err := f.validate(); err == nil {
		t.Fatal("rewritten landing accepted")
	}
	if before == "" {
		t.Fatal("missing fixture refs")
	}
	entries, _ := os.ReadDir(f.policy.Store)
	if len(entries) != 0 {
		t.Fatal("proof scratch leaked")
	}
}

func TestScheduleStoreRejectsAlteredRecord(t *testing.T) {
	f := newScheduleFixture(t)
	if err := scheduleClaim(f.policy, f.request.Release, f.now); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.policy.Store)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			p := filepath.Join(f.policy.Store, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b = scheduleEdit(b, func(m map[string]any) { m["extra"] = true })
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	other := newScheduleFixture(t)
	other.request.Release.WorkspaceID = "other"
	other.request.Release.AgentID = "other"
	other.request.Release.RunID = "other"
	if err := scheduleClaim(f.policy, other.request.Release, f.now); err == nil {
		t.Fatal("altered store allowed new claim")
	}
}

func TestScheduleIndependentEvidenceRejections(t *testing.T) {
	cases := map[string]func(*testing.T, *scheduleFixture){
		"retirement authorization": func(t *testing.T, f *scheduleFixture) {
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["retirementAuthorized"] = true })
		},
		"same size output alteration": func(t *testing.T, f *scheduleFixture) {
			if err := os.WriteFile(f.output, []byte("RESULT"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"declaration digest": func(t *testing.T, f *scheduleFixture) {
			// Keep all schema and identity bindings consistent except the original digest.
			f.request.Declaration = scheduleEdit(f.request.Declaration, func(m map[string]any) { m["scheduleId"] = "other-schedule" })
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["scheduleId"] = "other-schedule" })
			f.request.Release.ScheduleID = "other-schedule"
		},
		"future verification": func(t *testing.T, f *scheduleFixture) {
			// Release ordering also enforces verifiedAt <= releasedAt <= now.
			f.request.Receipt = scheduleEdit(f.request.Receipt, func(m map[string]any) { m["verifiedAt"] = f.now.Add(time.Second).Format(time.RFC3339Nano) })
		},
		"release before verification": func(t *testing.T, f *scheduleFixture) {
			f.request.Release.ReleasedAt = f.now.Add(-21 * time.Second)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newScheduleFixture(t)
			if err := f.validate(); err != nil {
				t.Fatal("valid control:", err)
			}
			edit(t, &f)
			// Receipt edits are correctly bound to the release, isolating the evidence check.
			f.request.Release.ReceiptDigest = scheduleHash(f.request.Receipt)
			if err := f.validate(); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}
