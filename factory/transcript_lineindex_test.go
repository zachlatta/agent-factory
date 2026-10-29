package factory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// bigSession writes a Claude session of about 90 MB under home: short turns with a screenshot every
// 30 lines. It returns the source, the file's path and its line count.
func bigSession(tb testing.TB, home string) (TranscriptSource, string, int) {
	tb.Helper()
	dir := home + "/.claude/projects/-w-big"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	path := dir + "/big.jsonl"
	var sb strings.Builder
	n := 0
	for i := 0; sb.Len() < 90<<20; i++ {
		var v map[string]any
		if i%30 == 29 {
			v = map[string]any{"type": "user", "timestamp": "t", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("u%d", i), "content": []any{
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png",
						"data": base64.StdEncoding.EncodeToString(fakePNG(i, 100_000))}}}}}}}
		} else {
			v = map[string]any{"type": "assistant", "timestamp": "t", "message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("step %d: %s", i, strings.Repeat("work ", 80))}}}}
		}
		b, _ := json.Marshal(v)
		sb.Write(b)
		sb.WriteByte('\n')
		n++
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		tb.Fatal(err)
	}
	return TranscriptSource{Provider: "claude", SessionID: "big", Cwd: "/w/big"}, path, n
}

// BenchmarkTranscriptPoll is one open running transcript's requests on a 90 MB session: the newest
// page, a poll for new lines and one image.
func BenchmarkTranscriptPoll(b *testing.B) {
	home := b.TempDir()
	src, _, total := bigSession(b, home)
	for _, c := range []struct {
		name string
		read func() error
	}{
		{"tail", func() error {
			_, err := ReadTranscriptPage(home, src, Page{Tail: true, Budget: TranscriptBudget, Refs: true})
			return err
		}},
		{"poll", func() error {
			_, err := ReadTranscriptPage(home, src, Page{After: total, Budget: TranscriptBudget, Refs: true})
			return err
		}},
		{"image", func() error {
			_, _, err := TranscriptImage(home, src, "30.0")
			return err
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if err := c.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// textLines is a parser that makes each line one event with its text.
func textLines(n int, line []byte) []Event {
	return []Event{{Kind: "user", Text: string(line), Line: n}}
}

func lineTexts(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Text)
	}
	return strings.Join(s, "|")
}

// indexOf is path's line index through the cache, with the bytes the call scanned for newlines.
func indexOf(t *testing.T, path string) (lineIdx, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := indexScanned.Load()
	x, err := indexFor(f, path)
	if err != nil {
		t.Fatal(err)
	}
	return x, indexScanned.Load() - before
}

// freshIndex is path's line index built from scratch, without the cache.
func freshIndex(t *testing.T, path string) lineIdx {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	x, err := scanIndex(f, 0, fi.Size(), lineIdx{off: []int64{0}})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	res, err := readLines(path, textLines, Page{})
	if err != nil {
		t.Fatal(err)
	}
	return lineTexts(res.Events)
}

func TestLineIndexGrowthScansOnlyTheNewBytes(t *testing.T) {
	path := t.TempDir() + "/s.jsonl"
	var sb strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&sb, "{\"n\":%d}\n", i)
	}
	writeFile(t, path, sb.String())
	x, scanned := indexOf(t, path)
	if x.lines() != 1000 || scanned != int64(sb.Len()) {
		t.Fatalf("first read: %d lines, scanned %d of %d", x.lines(), scanned, sb.Len())
	}
	if _, scanned := indexOf(t, path); scanned != 0 {
		t.Errorf("an unchanged file scanned %d bytes", scanned)
	}
	more := "{\"n\":1000}\n{\"n\":1001}\n"
	appendFile(t, path, more)
	x, scanned = indexOf(t, path)
	if scanned != int64(len(more)) {
		t.Errorf("growth scanned %d bytes, want %d", scanned, len(more))
	}
	if want := freshIndex(t, path); fmt.Sprint(x) != fmt.Sprint(want) {
		t.Errorf("grown index = %v, fresh = %v", x, want)
	}
	res, err := readLines(path, textLines, Page{After: 1000})
	if err != nil || lineTexts(res.Events) != `{"n":1000}|{"n":1001}` || res.Next != 1002 {
		t.Errorf("poll = %+v, %v", res, err)
	}
}

func TestLineIndexRechecksAHeldBackLastLine(t *testing.T) {
	home := t.TempDir()
	path := home + "/.claude/projects/-w/s.jsonl"
	writeFile(t, path, "a\n{\"x\":")
	if x, _ := indexOf(t, path); x.lines() != 1 || x.held {
		t.Fatalf("a half-written last line counts: %+v", x)
	}
	appendFile(t, path, "1}")
	if x, _ := indexOf(t, path); x.lines() != 2 || !x.held {
		t.Fatalf("a whole last line with no newline does not count: %+v", x)
	}
	if res, err := readLines(path, textLines, Page{After: 1}); err != nil || lineTexts(res.Events) != `{"x":1}` {
		t.Errorf("held line = %+v, %v", res.Events, err)
	}
	appendFile(t, path, "\n{}\n")
	x, _ := indexOf(t, path)
	if want := freshIndex(t, path); x.lines() != 3 || x.held || fmt.Sprint(x) != fmt.Sprint(want) {
		t.Errorf("after the newline: %+v, fresh %+v", x, want)
	}
	if got := readAll(t, path); got != `a|{"x":1}|{}` {
		t.Errorf("lines = %s", got)
	}

	// an image on a held last line is served
	img := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lG"}}]}]}}`
	appendFile(t, path, img)
	src := TranscriptSource{Provider: "claude", SessionID: "s", Cwd: "/w"}
	if media, data, err := TranscriptImage(home, src, "4.0"); err != nil || media != "image/gif" || string(data) != "GIF" {
		t.Errorf("image on a held line: %s %q %v", media, data, err)
	}
}

// expectRebuild checks that change makes the next read scan the whole file and read it right.
// onStat is set when the file's size, mtime or identity alone should show the change, before any
// line is read.
func expectRebuild(t *testing.T, name, path, want string, onStat bool, change func()) {
	t.Helper()
	readAll(t, path)
	change()
	size := mustStat(t, path).Size()
	if onStat {
		if _, scanned := indexOf(t, path); scanned != size {
			t.Errorf("%s: the index scanned %d of %d bytes, want a rebuild", name, scanned, size)
		}
	}
	before := indexScanned.Load()
	if got := readAll(t, path); got != want {
		t.Errorf("%s: lines = %s, want %s", name, got, want)
	}
	if scanned := indexScanned.Load() - before; !onStat && scanned < size {
		t.Errorf("%s: scanned %d of %d bytes, want a rebuild", name, scanned, size)
	}
}

func TestLineIndexRebuildsWhenTheFileIsRewritten(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/s.jsonl"
	writeFile(t, path, "a\nb\nc\n")
	expectRebuild(t, "shrink", path, "a|bb", true, func() { writeFile(t, path, "a\nbb\n") })

	writeFile(t, path, "aa\nbb\n")
	old := mustStat(t, path).ModTime()
	expectRebuild(t, "mtime rewind", path, "a|bbb", true, func() {
		writeFile(t, path, "a\nbbb\n")
		os.Chtimes(path, old.Add(-3600e9), old.Add(-3600e9))
	})

	writeFile(t, path, "aa\nbb\n")
	expectRebuild(t, "grown with an earlier mtime", path, "a|bbb|cc", true, func() {
		writeFile(t, path, "a\nbbb\ncc\n")
		os.Chtimes(path, old.Add(-7200e9), old.Add(-7200e9))
	})

	writeFile(t, path, "aa\nbb\n")
	expectRebuild(t, "same size, new mtime", path, "a|bbb", true, func() {
		writeFile(t, path, "a\nbbb\n")
		os.Chtimes(path, old.Add(3600e9), old.Add(3600e9))
	})

	writeFile(t, path, "aa\nbb\n")
	expectRebuild(t, "replaced", path, "x|yy|z", true, func() {
		writeFile(t, dir+"/new", "x\nyy\nz\n")
		if err := os.Rename(dir+"/new", path); err != nil {
			t.Fatal(err)
		}
	})

	// an edit in place that moves a line ending, then growth: the old offsets no longer fall on
	// line ends, so reading a line finds the index stale and rebuilds it
	writeFile(t, path, "aaaa\nbb\n")
	expectRebuild(t, "moved newline", path, "aa|bbbb|cc", false, func() {
		writeFile(t, path, "aa\nbbbb\n")
		appendFile(t, path, "cc\n")
		os.Chtimes(path, old.Add(7200e9), old.Add(7200e9))
	})

	// an edit in place that splits a line while every old line end stays a newline
	writeFile(t, path, "aaaa\nbbbb\n")
	expectRebuild(t, "split line", path, "aa|a|bbbb|cc", false, func() {
		writeFile(t, path, "aa\na\nbbbb\n")
		appendFile(t, path, "cc\n")
		os.Chtimes(path, old.Add(10800e9), old.Add(10800e9))
	})
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// pad is a JSON line padded with spaces to n bytes.
func pad(line string, n int) string { return line + strings.Repeat(" ", n-len(line)) }

func TestAntigravityLogEditedInPlaceAndGrownReadsLikeAFreshRead(t *testing.T) {
	home := t.TempDir()
	user := func(s string) string {
		return `{"type":"USER_INPUT","content":"<USER_REQUEST>` + s + `</USER_REQUEST>"}`
	}
	first := pad(user("first"), 160)
	antigravityFile(t, home, "conv-e", first, user("second"))
	path := home + "/.gemini/antigravity-cli/brain/conv-e/.system_generated/logs/transcript_full.jsonl"
	res, _, err := readAntigravity(home, "conv-e")
	if err != nil || lineTexts(res.Events) != "first|second" {
		t.Fatalf("first read = %+v, %v", res.Events, err)
	}
	// the first step becomes two lines of the same total length, and a new step is added
	split := user("a") + "\n" + user("b")
	antigravityFile(t, home, "conv-e", pad(split, len(first)), user("second"), user("third"))
	res, _, err = readAntigravity(home, "conv-e")
	dropIndex(path)
	fresh, _, _ := readAntigravity(home, "conv-e")
	if err != nil || lineTexts(res.Events) != "a|b|second|third" || fmt.Sprint(res.Events) != fmt.Sprint(fresh.Events) {
		t.Errorf("after the edit = %+v, %v; fresh = %+v", res.Events, err, fresh.Events)
	}
}

func TestLineIndexKeepsTheNewerOfTwoScans(t *testing.T) {
	path := t.TempDir() + "/s.jsonl"
	writeFile(t, path, "a\n")
	small := mustStat(t, path)
	appendFile(t, path, "b\n")
	big := mustStat(t, path)
	storeIndex(path, nil, big, lineIdx{off: []int64{0, 2, 4}})
	storeIndex(path, nil, small, lineIdx{off: []int64{0, 2}})
	if x, scanned := indexOf(t, path); x.lines() != 2 || scanned != 0 {
		t.Errorf("an older scan replaced a newer one: %+v, scanned %d", x, scanned)
	}
}

func TestLineIndexesKeepTheRecentlyUsedPaths(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, lineIndexCap+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("%s/%d.jsonl", dir, i)
		writeFile(t, paths[i], "a\n")
	}
	for _, p := range paths[:lineIndexCap] {
		indexOf(t, p)
	}
	indexOf(t, paths[0])
	indexOf(t, paths[lineIndexCap])
	cached := func(p string) bool {
		lineIndexes.Lock()
		defer lineIndexes.Unlock()
		return lineIndexes.m[p] != nil
	}
	if !cached(paths[0]) || cached(paths[1]) || !cached(paths[lineIndexCap]) {
		t.Errorf("kept 0: %v, 1: %v, %d: %v", cached(paths[0]), cached(paths[1]), lineIndexCap, cached(paths[lineIndexCap]))
	}
	lineIndexes.Lock()
	n := len(lineIndexes.m)
	lineIndexes.Unlock()
	if n > lineIndexCap {
		t.Errorf("%d indexes cached, cap %d", n, lineIndexCap)
	}
}

func TestLineIndexUnderConcurrentReadsOfAGrowingFile(t *testing.T) {
	home := t.TempDir()
	path := home + "/.claude/projects/-w/s.jsonl"
	img := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lG"}}]}]}}`
	writeFile(t, path, img+"\n")
	src := TranscriptSource{Provider: "claude", SessionID: "s", Cwd: "/w"}
	done := make(chan struct{})
	errs := make(chan error, 100)
	go func() {
		defer close(done)
		for i := range 200 {
			appendFile(t, path, fmt.Sprintf("{\"n\":%d}\n", i))
		}
	}()
	for g := range 8 {
		go func() {
			for {
				select {
				case <-done:
					errs <- nil
					return
				default:
				}
				if g%2 == 0 {
					if _, err := readLines(path, textLines, Page{Tail: true, Budget: 200}); err != nil {
						errs <- err
						return
					}
				} else if _, data, err := TranscriptImage(home, src, "1.0"); err != nil || string(data) != "GIF" {
					errs <- fmt.Errorf("image: %q %v", data, err)
					return
				}
			}
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if got := readAll(t, path); strings.Count(got, "|") != 200 {
		t.Errorf("final read has %d lines", strings.Count(got, "|")+1)
	}
	if x := freshIndex(t, path); fmt.Sprint(x) != fmt.Sprint(func() lineIdx { y, _ := indexOf(t, path); return y }()) {
		t.Error("cached index differs from a fresh one")
	}
}

func TestLineAtFindsALineThatNoLongerStartsOrEndsAtANewline(t *testing.T) {
	path := t.TempDir() + "/s.jsonl"
	at := func(body string, x lineIdx, n int) error {
		writeFile(t, path, body)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		_, err = lineAt(f, x, n)
		return err
	}
	// "a\nbbbb\ncc\n" became "a\nbbbbbb\nc\n": line 2 runs on past its old end
	if err := at("a\nbbbbbb\nc\n", lineIdx{off: []int64{0, 2, 7, 10}}, 2); err != errStaleIndex {
		t.Errorf("moved end: %v", err)
	}
	// "aaaa\nb\n" became "aaaaaa\nb\n": line 2 starts inside the old first line
	if err := at("aaaaaa\nb\n", lineIdx{off: []int64{0, 5, 7}}, 2); err != errStaleIndex {
		t.Errorf("moved start: %v", err)
	}
	if err := at("aaaa\nb\n", lineIdx{off: []int64{0, 5, 7}}, 2); err != nil {
		t.Errorf("unchanged: %v", err)
	}
}
