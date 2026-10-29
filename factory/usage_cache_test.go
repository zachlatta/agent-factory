package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All records are synthetic. Filler lines carry no usage and only pad files to known sizes.

func claudeUsageLine(id string, in, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-10-25T19:45:00Z","message":{"id":%q,"model":"model-a","usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n", id, in, out)
}

func fillerLine(n int) string {
	return `{"type":"user","message":{"content":"` + strings.Repeat("f", n) + `"}}` + "\n"
}

func codexCountLine(at string, in, out int64) string {
	return fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d}}}}`+"\n", at, in, out)
}

func usageSession(t *testing.T, f *Factory, id, session string, created time.Time, body string) string {
	t.Helper()
	usageAgentFileAt(t, f, id, "claude", session, created)
	path := filepath.Join(f.cfg.Home, ".claude/projects/demo", session+".jsonl")
	writeFile(t, path, body)
	return path
}

func usageAgentFileAt(t *testing.T, f *Factory, id, provider, session string, created time.Time) {
	t.Helper()
	writeFile(t, agentFile(f.cfg.Home, id), fmt.Sprintf(`{"id":%q,"title":"Synthetic agent","provider":%q,"cwd":"/work/demo","createdAt":%q,"config":{"model":"fallback"},"persistence":{"sessionId":%q}}`,
		id, provider, created.Format(time.RFC3339Nano), session))
}

// A forced budget smaller than all sessions: each refresh reads new bytes only, moves on to older
// sessions, reports itself limited until done, then converges on the full totals and rereads nothing.
func TestUsageBudgetConvergesWithoutRereading(t *testing.T) {
	f, _ := newTestFactory(t)
	var want, size int64
	for i := 0; i < 6; i++ {
		body := fillerLine(900) + claudeUsageLine(fmt.Sprintf("m-%d", i), int64(10*(i+1)), 1)
		usageSession(t, f, fmt.Sprintf("agent-%d", i), fmt.Sprintf("sess-%d", i), t0.Add(-time.Duration(i)*time.Hour), body)
		want += int64(10*(i+1)) + 1
		size += int64(len(body))
	}
	f.tokenCache.budget = 2500 // about two and a half sessions
	var r UsageView
	refreshes := 0
	for ; refreshes < 10; refreshes++ {
		r, _ = f.collectTokenUsage(context.Background())
		if r.Coverage.BytesRead > f.tokenCache.budget {
			t.Fatalf("refresh %d read %d bytes over budget %d", refreshes, r.Coverage.BytesRead, f.tokenCache.budget)
		}
		if !r.Coverage.Limited {
			break
		}
		if r.Coverage.BytesRead == 0 {
			t.Fatalf("limited refresh %d made no progress: %+v", refreshes, r.Coverage)
		}
		if r.Totals.Total >= want || r.Coverage.Skipped == 0 || r.Coverage.PendingBytes == 0 {
			t.Fatalf("incomplete refresh %d looked complete: %+v %+v", refreshes, r.Totals, r.Coverage)
		}
	}
	if r.Coverage.Limited || r.Totals.Total != want || r.Coverage.Readable != 6 || r.Coverage.Skipped != 0 || r.Coverage.PendingBytes != 0 {
		t.Fatalf("after %d refreshes: totals %+v coverage %+v, want total %d", refreshes, r.Totals, r.Coverage, want)
	}
	if refreshes > 3 {
		t.Fatalf("took %d refreshes for %d bytes at budget %d", refreshes+1, size, f.tokenCache.budget)
	}
	again, _ := f.collectTokenUsage(context.Background())
	if again.Coverage.BytesRead != 0 || again.Totals != r.Totals || again.Coverage.Limited {
		t.Fatalf("unchanged sessions reread: %+v", again.Coverage)
	}
}

// An appended session reads only the new bytes (plus a small identity check), and a repeated
// Claude message and a cumulative Codex counter are not counted twice across the boundary.
func TestUsageAppendReadsOnlyNewBytes(t *testing.T) {
	f, _ := newTestFactory(t)
	path := usageSession(t, f, "a", "sess-a", t0, fillerLine(20000)+claudeUsageLine("m-1", 10, 2))
	usageAgentFileAt(t, f, "c", "codex", "sess-c", t0.Add(-time.Hour))
	cpath := filepath.Join(f.cfg.Home, ".codex/sessions/2026/01/01/rollout-demo-sess-c.jsonl")
	writeFile(t, cpath, fillerLine(20000)+codexCountLine("2026-10-25T19:00:00Z", 100, 10))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 122 || r.Coverage.Limited {
		t.Fatalf("first = %+v %+v", r.Totals, r.Coverage)
	}
	appendFile(t, path, claudeUsageLine("m-1", 10, 5)+claudeUsageLine("m-2", 20, 1))
	appendFile(t, cpath, codexCountLine("2026-10-25T19:01:00Z", 100, 10)+codexCountLine("2026-10-25T19:02:00Z", 150, 20))
	r, _ := f.collectTokenUsage(context.Background())
	// claude: m-1 final 10+5, m-2 21; codex: cumulative 150+20
	if r.Totals.Total != 15+21+170 || r.Coverage.Limited || r.Coverage.Readable != 2 {
		t.Fatalf("append = %+v %+v", r.Totals, r.Coverage)
	}
	if r.Coverage.BytesRead >= 20000 {
		t.Fatalf("append reread %d bytes", r.Coverage.BytesRead)
	}
}

// Truncation, in-place rewrites and replacement restart a session from zero; nothing from the
// old contents survives. A removed session is missing, not zero, and a new one at its path is fresh.
func TestUsageTruncateRewriteReplaceRemove(t *testing.T) {
	f, _ := newTestFactory(t)
	path := usageSession(t, f, "a", "sess-a", t0, fillerLine(5000)+claudeUsageLine("m-1", 100, 10)+claudeUsageLine("m-2", 100, 10))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 220 {
		t.Fatal(r.Totals)
	}
	// truncated and rewritten shorter
	writeFile(t, path, claudeUsageLine("m-9", 1, 1))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 2 || r.Coverage.Readable != 1 {
		t.Fatalf("truncate = %+v %+v", r.Totals, r.Coverage)
	}
	// rewritten in place, longer, with a different first line: not an append
	writeFile(t, path, claudeUsageLine("m-8", 3, 3)+claudeUsageLine("m-7", 4, 4))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 14 {
		t.Fatalf("rewrite = %+v", r.Totals)
	}
	// same size, same prefix, different content after it
	writeFile(t, path, claudeUsageLine("m-8", 3, 3)+claudeUsageLine("m-6", 5, 3))
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 14 {
		t.Fatalf("same-size rewrite = %+v", r.Totals)
	}
	writeFile(t, path, claudeUsageLine("m-8", 3, 3)+claudeUsageLine("m-6", 6, 3))
	os.Chtimes(path, future.Add(time.Minute), future.Add(time.Minute))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 15 {
		t.Fatalf("same-size edit = %+v", r.Totals)
	}
	// replaced by another file of the same size
	tmp := path + ".new"
	writeFile(t, tmp, claudeUsageLine("m-8", 3, 3)+claudeUsageLine("m-5", 7, 3))
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 16 {
		t.Fatalf("replace = %+v", r.Totals)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 0 || r.Coverage.Missing != 1 || r.Coverage.Readable != 0 {
		t.Fatalf("remove = %+v %+v", r.Totals, r.Coverage)
	}
	writeFile(t, path, claudeUsageLine("m-1", 1, 0))
	if r, _ := f.collectTokenUsage(context.Background()); r.Totals.Total != 1 {
		t.Fatalf("recreate = %+v", r.Totals)
	}
}

// A line still being written is not counted and keeps the session incomplete; once finished it
// counts once.
func TestUsagePartialLastLine(t *testing.T) {
	f, _ := newTestFactory(t)
	line := claudeUsageLine("m-2", 20, 2)
	path := usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 10, 1)+line[:len(line)/2])
	r, _ := f.collectTokenUsage(context.Background())
	if r.Totals.Total != 11 || !r.Coverage.Limited || r.Coverage.Skipped != 1 || r.Coverage.Errors != 0 {
		t.Fatalf("partial = %+v %+v", r.Totals, r.Coverage)
	}
	appendFile(t, path, line[len(line)/2:])
	r, _ = f.collectTokenUsage(context.Background())
	if r.Totals.Total != 33 || r.Coverage.Limited || r.Coverage.Readable != 1 {
		t.Fatalf("finished = %+v %+v", r.Totals, r.Coverage)
	}
}

// A finished writer cannot complete a damaged tail. Keep its good counters, report an error,
// and still accept a later completion if that session is resumed.
func TestUsageFinishedPartialLastLine(t *testing.T) {
	for _, tc := range []struct {
		name, ending string
		finished     bool
	}{
		{"archived", `"archivedAt":"2026-10-25T20:00:00Z","lastStatus":"idle"`, true},
		{"closed", `"lastStatus":"closed"`, true},
		{"running despite archive", `"archivedAt":"2026-10-25T20:00:00Z","lastStatus":"running"`, false},
		{"unknown", `"lastStatus":""`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			line := claudeUsageLine("m-2", 20, 2)
			path := usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 10, 1)+line[:len(line)/2])
			writeFile(t, agentFile(f.cfg.Home, "a"), `{"id":"a","provider":"claude",`+tc.ending+`,"persistence":{"sessionId":"sess-a"}}`)
			wantErrors, wantPending := 0, int64(len(line)/2)
			if tc.finished {
				wantErrors, wantPending = 1, 0
			}
			for i := 0; i < 2; i++ {
				r, _ := f.collectTokenUsage(context.Background())
				if r.Totals.Total != 11 || r.Coverage.Limited == tc.finished || r.Coverage.PendingBytes != wantPending || r.Coverage.Errors != wantErrors {
					t.Fatalf("finished partial pass %d = %+v %+v", i, r.Totals, r.Coverage)
				}
			}
			appendFile(t, path, line[len(line)/2:])
			r, _ := f.collectTokenUsage(context.Background())
			if r.Totals.Total != 33 || r.Coverage.Limited || r.Coverage.Errors != 0 || r.Coverage.Readable != 1 {
				t.Fatalf("completed later = %+v %+v", r.Totals, r.Coverage)
			}
			r, _ = f.collectTokenUsage(context.Background())
			if r.Totals.Total != 33 || r.Coverage.BytesRead != 0 {
				t.Fatalf("completed twice = %+v %+v", r.Totals, r.Coverage)
			}
		})
	}
}

// Shared sessions stay open if any owner can still write. A metadata slice cannot establish
// that every owner finished, even if the first owner it read is archived.
func TestUsageSharedPartialTailNeedsEveryOwnerFinished(t *testing.T) {
	for _, tc := range []struct {
		name            string
		metaLimit       int
		invalidMetadata bool
	}{
		{name: "active owner"},
		{name: "metadata slice", metaLimit: 1},
		{name: "unreadable owner", invalidMetadata: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newTestFactory(t)
			line := claudeUsageLine("m-2", 20, 2)
			usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 10, 1)+line[:len(line)/2])
			writeFile(t, agentFile(f.cfg.Home, "a"), `{"id":"a","provider":"claude","createdAt":"2026-10-25T20:00:00Z","archivedAt":"2026-10-25T21:00:00Z","lastStatus":"idle","persistence":{"sessionId":"sess-a"}}`)
			usageAgentFileAt(t, f, "z", "claude", "sess-a", t0.Add(-time.Hour))
			wantErrors := 0
			if tc.invalidMetadata {
				writeFile(t, agentFile(f.cfg.Home, "z"), `{"id":"z"`)
				wantErrors = 1
			}
			f.tokenCache.metaLimit = tc.metaLimit
			for i := 0; i < 2; i++ {
				r, _ := f.collectTokenUsage(context.Background())
				if r.Totals.Total != 11 || !r.Coverage.Limited || r.Coverage.Errors != wantErrors {
					t.Fatalf("open owner pass %d = %+v %+v", i, r.Totals, r.Coverage)
				}
			}
			writeFile(t, agentFile(f.cfg.Home, "z"), `{"id":"z","provider":"claude","lastStatus":"closed","persistence":{"sessionId":"sess-a"}}`)
			r, _ := f.collectTokenUsage(context.Background())
			if r.Totals.Total != 11 || r.Coverage.Limited || r.Coverage.PendingBytes != 0 || r.Coverage.Errors != 1 {
				t.Fatalf("all owners finished = %+v %+v", r.Totals, r.Coverage)
			}
		})
	}
}

// A session larger than the per-refresh chunk is read across refreshes, limited (not zero, not
// complete) until its end, and never skipped outright.
func TestUsageOversizedSessionProgresses(t *testing.T) {
	f, _ := newTestFactory(t)
	var body string
	for i := 0; i < 8; i++ {
		body += fillerLine(1000) + claudeUsageLine(fmt.Sprintf("m-%d", i), 10, 0)
	}
	usageSession(t, f, "a", "sess-a", t0, body)
	f.tokenCache.chunk = 3000
	var prev int64
	for i := 0; ; i++ {
		r, _ := f.collectTokenUsage(context.Background())
		if r.Coverage.BytesRead > 3000 {
			t.Fatalf("chunk exceeded: %d", r.Coverage.BytesRead)
		}
		if !r.Coverage.Limited {
			if r.Totals.Total != 80 || r.Coverage.Readable != 1 {
				t.Fatalf("done = %+v %+v", r.Totals, r.Coverage)
			}
			break
		}
		if r.Totals.Total <= prev && i > 0 || r.Coverage.Skipped != 1 || r.Coverage.PendingBytes == 0 {
			t.Fatalf("refresh %d = %+v %+v", i, r.Totals, r.Coverage)
		}
		prev = r.Totals.Total
		if i > 6 {
			t.Fatal("oversized session never finished")
		}
	}
}

// The cache keeps counters and file identity, never record text.
func TestUsageCacheKeepsNoContent(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "a", "sess-a", t0, `{"type":"user","message":{"content":"private-prompt-marker"}}`+"\n"+claudeUsageLine("m-1", 1, 1))
	f.collectTokenUsage(context.Background())
	if len(f.tokenCache.files) == 0 {
		t.Fatal("nothing cached")
	}
	for _, tf := range f.tokenCache.files {
		if s := fmt.Sprintf("%+v %+v", *tf, *tf.parser); strings.Contains(s, "private-prompt-marker") || strings.Contains(s, "content") {
			t.Fatalf("cache kept record text: %s", s)
		}
	}
}

// Agent files past the per-refresh parse limit are parsed by later refreshes, not dropped forever.
func TestUsageAgentMetadataProgresses(t *testing.T) {
	f, _ := newTestFactory(t)
	for i := 0; i < 5; i++ {
		usageSession(t, f, fmt.Sprintf("agent-%d", i), fmt.Sprintf("sess-%d", i), t0, claudeUsageLine("m", 1, 1))
	}
	f.tokenCache.metaLimit = 2
	var r UsageView
	for i := 0; i < 3; i++ {
		r, _ = f.collectTokenUsage(context.Background())
		if want := min(2*(i+1), 5); r.Coverage.Agents != want || r.Coverage.Limited != (want < 5) {
			t.Fatalf("refresh %d = %+v", i, r.Coverage)
		}
	}
	if r.Totals.Total != 10 {
		t.Fatal(r.Totals)
	}
	writeFile(t, agentFile(f.cfg.Home, "agent-0"), `{"id":"agent-0","title":"Renamed","provider":"claude","persistence":{"sessionId":"sess-0"}}`)
	r, _ = f.collectTokenUsage(context.Background())
	for _, row := range r.Rows {
		if row.Agent == "agent-0" && row.Title != "Renamed" {
			t.Fatal("changed agent file not reread")
		}
	}
}

// A record longer than tokenLineLimit is an error that is skipped, including across chunks; the
// records after it still count and the file finishes.
func TestUsageOverlongRecordSkipped(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 1, 1)+fillerLine(2*tokenLineLimit)+claudeUsageLine("m-2", 2, 2))
	f.tokenCache.chunk = tokenLineLimit + 1
	var r UsageView
	for i := 0; i < 5; i++ {
		if r, _ = f.collectTokenUsage(context.Background()); !r.Coverage.Limited {
			break
		}
	}
	if r.Coverage.Limited || r.Totals.Total != 6 || r.Coverage.Errors != 1 {
		t.Fatalf("overlong = %+v %+v", r.Totals, r.Coverage)
	}
}

// A refresh cut short keeps the sessions it did not reach; the next one goes on from them.
func TestUsageCancelledRefreshKeepsCache(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 1, 1))
	f.collectTokenUsage(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, _ := f.collectTokenUsage(ctx); !r.Coverage.Limited {
		t.Fatal(r.Coverage)
	}
	if r, _ := f.collectTokenUsage(context.Background()); r.Coverage.BytesRead != 0 || r.Totals.Total != 2 {
		t.Fatalf("after cancel = %+v %+v", r.Totals, r.Coverage)
	}
}

// getUsage requests /api/usage and waits for any refresh it started.
func getUsage(t *testing.T, f *Factory) UsageView {
	t.Helper()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUsage(t, f)
	var v UsageView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitUsage(t *testing.T, f *Factory) {
	t.Helper()
	f.tokenMu.Lock()
	done := f.tokenCache.done
	f.tokenMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh stalled")
		}
	}
}

func usageScans(f *Factory) int {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return f.tokenCache.scans
}

// While coverage is limited, a request after the short cooldown starts the next bounded slice; a
// request inside it does not. Once complete, the five-minute period applies again. Nothing scans
// without a request.
func TestUsageIncompleteSlicesAfterCooldown(t *testing.T) {
	f, _ := newTestFactory(t)
	now := t0
	f.Now = func() time.Time { return now }
	var want int64
	for i := 0; i < 6; i++ {
		usageSession(t, f, fmt.Sprintf("agent-%d", i), fmt.Sprintf("sess-%d", i), t0.Add(-time.Duration(i)*time.Hour), fillerLine(900)+claudeUsageLine(fmt.Sprintf("m-%d", i), 10, 1))
		want += 11
	}
	f.tokenCache.budget = 2500
	getUsage(t, f) // starts the first slice
	v := getUsage(t, f)
	if !v.Coverage.Limited || v.Coverage.PendingBytes == 0 || v.Totals.Total >= want || usageScans(f) != 1 {
		t.Fatalf("first slice = %+v %+v scans %d", v.Totals, v.Coverage, usageScans(f))
	}
	now = now.Add(usageSliceCooldown - time.Second)
	getUsage(t, f)
	if usageScans(f) != 1 {
		t.Fatal("sliced inside the cooldown")
	}
	for i := 0; i < 5 && v.Coverage.Limited; i++ {
		now = now.Add(usageSliceCooldown)
		getUsage(t, f)
		v = getUsage(t, f)
		if v.Coverage.Limited && v.Totals.Total >= want {
			t.Fatalf("limited snapshot with complete totals: %+v", v.Coverage)
		}
	}
	if v.Coverage.Limited || v.Totals.Total != want || v.Coverage.PendingBytes != 0 {
		t.Fatalf("not complete: %+v %+v", v.Totals, v.Coverage)
	}
	scans := usageScans(f)
	if scans > 4 {
		t.Fatalf("%d scans for three slices of work", scans)
	}
	now = now.Add(usageTTL - time.Second)
	getUsage(t, f)
	if usageScans(f) != scans {
		t.Fatal("complete snapshot rescanned before the TTL")
	}
	now = now.Add(time.Second)
	getUsage(t, f)
	if usageScans(f) != scans+1 {
		t.Fatal("complete snapshot not refreshed at the TTL")
	}
	now = now.Add(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if usageScans(f) != scans+1 {
		t.Fatal("scanned with no request")
	}
}

// Concurrent requests share one scan.
func TestUsageConcurrentRequestsScanOnce(t *testing.T) {
	f, _ := newTestFactory(t)
	usageSession(t, f, "a", "sess-a", t0, claudeUsageLine("m-1", 1, 1))
	f.tokenCache.scan.Lock() // hold the scan so every request overlaps it
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/usage", nil))
		}()
	}
	wg.Wait()
	f.tokenCache.scan.Unlock()
	waitUsage(t, f)
	if n := usageScans(f); n != 1 {
		t.Fatalf("%d scans", n)
	}
	if v := getUsage(t, f); v.Totals.Total != 2 || v.Coverage.Limited {
		t.Fatal(v)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
