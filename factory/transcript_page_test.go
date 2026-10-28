package factory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakePNG is n deterministic bytes standing in for screenshot i.
func fakePNG(i, n int) []byte {
	b := make([]byte, n)
	for j := range b {
		b[j] = byte(i*31 + j*7)
	}
	return b
}

// largeClaudeSession writes a Claude session of 1 prompt, then rounds of a tool call and its result
// carrying a 100 KB screenshot, an agent message every 10 rounds, and a last agent message. It
// returns the source and the number of lines.
func largeClaudeSession(t *testing.T, home string, rounds int) (TranscriptSource, int) {
	t.Helper()
	cwd := "/srv/u/.paseo/worktrees/big"
	line := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	lines := []string{line(map[string]any{"type": "user", "timestamp": "t0", "message": map[string]any{"content": "Make it fast"}})}
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("u%d", i)
		lines = append(lines, line(map[string]any{"type": "assistant", "timestamp": "t1", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("shot %d", i)}}}}}))
		lines = append(lines, line(map[string]any{"type": "user", "timestamp": "t1", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("took shot %d", i)},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png",
					"data": base64.StdEncoding.EncodeToString(fakePNG(i, 100_000))}}}}}}}))
		if i%10 == 9 {
			lines = append(lines, line(map[string]any{"type": "assistant", "timestamp": "t2", "message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("progress note %d", i)}}}}))
		}
	}
	lines = append(lines, line(map[string]any{"type": "assistant", "timestamp": "t3", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "the last word"}}}}))
	writeFile(t, home+"/.claude/projects/-srv-u--paseo-worktrees-big/big.jsonl", strings.Join(lines, "\n")+"\n")
	return TranscriptSource{Provider: "claude", SessionID: "big", Cwd: cwd}, len(lines)
}

// pageBack reads src newest first, then pages back with before until nothing is older, and returns
// every event in order with the number of pages.
func pageBack(t *testing.T, home string, src TranscriptSource, budget int) ([]Event, int) {
	t.Helper()
	res, err := ReadTranscriptPage(home, src, Page{Tail: true, Budget: budget, Refs: true})
	if err != nil {
		t.Fatal(err)
	}
	all, pages := res.Events, 1
	for res.Older {
		before := res.Before
		if res, err = ReadTranscriptPage(home, src, Page{Before: before, Budget: budget, Refs: true}); err != nil {
			t.Fatal(err)
		}
		if res.Before >= before {
			t.Fatalf("before did not move back: %d then %d", before, res.Before)
		}
		all, pages = append(append([]Event{}, res.Events...), all...), pages+1
		if pages > 1000 {
			t.Fatal("paging never ends")
		}
	}
	return all, pages
}

func TestTranscriptTailIsSmallAndRecent(t *testing.T) {
	home := t.TempDir()
	src, n := largeClaudeSession(t, home, 300)
	if fi, _ := os.Stat(home + "/.claude/projects/-srv-u--paseo-worktrees-big/big.jsonl"); fi.Size() < 30_000_000 {
		t.Fatalf("fake session is only %d bytes", fi.Size())
	}
	res, err := ReadTranscriptPage(home, src, Page{Tail: true, Budget: TranscriptBudget, Refs: true})
	if err != nil || res.Fallback {
		t.Fatalf("%+v %v", res.Fallback, err)
	}
	body, _ := json.Marshal(res)
	if len(body) > 2*TranscriptBudget {
		t.Errorf("tail is %d bytes", len(body))
	}
	if bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(fakePNG(299, 100_000))[:200])) {
		t.Error("tail carries image data")
	}
	if last := res.Events[len(res.Events)-1]; last.Text != "the last word" {
		t.Errorf("last = %+v", last)
	}
	if res.Next != n || !res.Older || res.Before <= 1 || res.Before > n {
		t.Errorf("next = %d (want %d), older = %v, before = %d", res.Next, n, res.Older, res.Before)
	}
	// a result shows its image as a reference to the line it came from
	var r Event
	for _, e := range res.Events {
		if e.Kind == "result" {
			r = e
		}
	}
	if len(r.Images) != 1 || r.Images[0].Data != "" || r.Images[0].Ref != fmt.Sprintf("%d.0", r.Line) || r.Images[0].Media != "image/png" {
		t.Errorf("result = %+v", r)
	}
}

func TestTranscriptPagesAddUpToTheWholeTranscript(t *testing.T) {
	home := t.TempDir()
	src, _ := largeClaudeSession(t, home, 120)
	whole, err := ReadTranscriptPage(home, src, Page{Refs: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{1, 5_000, TranscriptBudget} {
		got, pages := pageBack(t, home, src, budget)
		if !reflect.DeepEqual(got, whole.Events) {
			t.Errorf("budget %d: %d events in %d pages, want %d", budget, len(got), pages, len(whole.Events))
		}
		if budget == 1 && pages < 100 {
			t.Errorf("budget 1 gave only %d pages", pages)
		}
	}
	// with no budget, refs off, the whole read keeps image data for the factory's own readers
	full, _ := ReadTranscript(home, src, 0)
	if full.Events[2].Images[0].Data == "" || full.Events[2].Images[0].Ref != "" {
		t.Errorf("full read image = %+v", full.Events[2].Images[0].Ref)
	}
}

func TestTranscriptImageServesTheReferencedBytes(t *testing.T) {
	home := t.TempDir()
	src, _ := largeClaudeSession(t, home, 12)
	res, _ := ReadTranscriptPage(home, src, Page{Tail: true, Budget: TranscriptBudget, Refs: true})
	seen := 0
	for _, e := range res.Events {
		if e.Kind != "result" {
			continue
		}
		var i int
		fmt.Sscanf(e.ID, "u%d", &i)
		media, data, err := TranscriptImage(home, src, e.Images[0].Ref)
		if err != nil || media != "image/png" || !bytes.Equal(data, fakePNG(i, 100_000)) {
			t.Fatalf("%s: %s %d bytes %v", e.Images[0].Ref, media, len(data), err)
		}
		seen++
	}
	if seen != 12 {
		t.Errorf("checked %d images", seen)
	}
	for _, ref := range []string{"", "x", "3", "3.1", "2.0", "-1.0", "99999.0", "3.0.0", "1e3.0"} {
		if _, _, err := TranscriptImage(home, src, ref); err == nil {
			t.Errorf("ref %q served an image", ref)
		}
	}
}

func TestTranscriptForwardReadsAreBoundedAndWaitForWholeLines(t *testing.T) {
	home := t.TempDir()
	src, n := largeClaudeSession(t, home, 30)
	whole, _ := ReadTranscriptPage(home, src, Page{Refs: true})
	var got []Event
	after, reads := 0, 0
	for {
		res, err := ReadTranscriptPage(home, src, Page{After: after, Budget: 2_000, Refs: true})
		if err != nil {
			t.Fatal(err)
		}
		got, after, reads = append(got, res.Events...), res.Next, reads+1
		if !res.More {
			break
		}
		if reads > 500 {
			t.Fatal("never done")
		}
	}
	if after != n || reads < 3 || !reflect.DeepEqual(got, whole.Events) {
		t.Errorf("after = %d of %d in %d reads, %d of %d events", after, n, reads, len(got), len(whole.Events))
	}
	// a line still being written is not read (or skipped past) until it ends
	path := home + "/.claude/projects/-srv-u--paseo-worktrees-big/big.jsonl"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"assistant","timestamp":"t9","message":{"content":[{"type":"text","text":"new`)
	res, _ := ReadTranscriptPage(home, src, Page{After: after, Budget: TranscriptBudget, Refs: true})
	if len(res.Events) != 0 || res.Next != n {
		t.Errorf("partial line: %+v next %d", res.Events, res.Next)
	}
	// a whole JSON line counts even before its newline is written
	f.WriteString(` text"}]}}`)
	res, _ = ReadTranscriptPage(home, src, Page{After: after, Budget: TranscriptBudget, Refs: true})
	if len(res.Events) != 1 || res.Events[0].Text != "new text" || res.Next != n+1 {
		t.Errorf("finished line: %+v next %d", res.Events, res.Next)
	}
	f.WriteString("\n")
	f.Close()
	res, _ = ReadTranscriptPage(home, src, Page{After: n + 1, Budget: TranscriptBudget, Refs: true})
	if len(res.Events) != 0 || res.Next != n+1 {
		t.Errorf("after its newline: %+v next %d", res.Events, res.Next)
	}
}

func TestTranscriptPagesForEveryProvider(t *testing.T) {
	home := t.TempDir()
	var codex, muse, agy []string
	for i := 0; i < 40; i++ {
		codex = append(codex,
			fmt.Sprintf(`{"timestamp":"t","type":"response_item","payload":{"type":"function_call","call_id":"c%d","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}`, i),
			fmt.Sprintf(`{"timestamp":"t","type":"response_item","payload":{"type":"function_call_output","call_id":"c%d","output":[{"type":"input_text","text":"out %d"},{"type":"input_image","image_url":"data:image/jpeg;base64,%s"}]}}`,
				i, i, base64.StdEncoding.EncodeToString(fakePNG(i, 3000))))
		muse = append(muse,
			fmt.Sprintf(`{"recorded_at":1,"payload_type":"runtime.session","payload":{"event":{"kind":"assistant_message_committed","text":"note %d"}}}`, i))
		agy = append(agy,
			fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","content":"step %d","tool_calls":[{"name":"run_command","args":{"CommandLine":"ls"}}]}`, 2*i, i),
			fmt.Sprintf(`{"step_index":%d,"type":"GENERIC","status":"DONE","content":"ok %d"}`, 2*i+1, i))
	}
	writeFile(t, home+"/.codex/sessions/2026/01/02/rollout-x-sess-c.jsonl", strings.Join(codex, "\n")+"\n")
	writeFile(t, home+"/.local/share/muse/sessions/2026/01/02/sess-m/session.jsonl", strings.Join(muse, "\n")+"\n")
	for _, src := range []TranscriptSource{
		{Provider: "codex", SessionID: "sess-c"},
		{Provider: "muse", SessionID: "sess-m"},
		{Provider: "antigravity", SessionID: antigravityFile(t, home, "conv-p", agy...)},
	} {
		whole, err := ReadTranscriptPage(home, src, Page{Refs: true})
		if err != nil || whole.Fallback || len(whole.Events) < 40 {
			t.Fatalf("%s: %d events %v", src.Provider, len(whole.Events), err)
		}
		got, pages := pageBack(t, home, src, 300)
		if pages < 5 || !reflect.DeepEqual(got, whole.Events) {
			t.Errorf("%s: %d events in %d pages, want %d", src.Provider, len(got), pages, len(whole.Events))
		}
	}
	// codex images are references too, and serve their bytes
	tail, _ := ReadTranscriptPage(home, TranscriptSource{Provider: "codex", SessionID: "sess-c"}, Page{Tail: true, Budget: 300, Refs: true})
	r := tail.Events[len(tail.Events)-1]
	if len(r.Images) != 1 || r.Images[0].Data != "" || r.Images[0].Ref != "80.0" {
		t.Fatalf("codex result = %+v", r)
	}
	if media, data, err := TranscriptImage(home, TranscriptSource{Provider: "codex", SessionID: "sess-c"}, "80.0"); err != nil ||
		media != "image/jpeg" || !bytes.Equal(data, fakePNG(39, 3000)) {
		t.Errorf("codex image: %s %d %v", media, len(data), err)
	}
	// antigravity rewrites its file, so a poll sends every event from the oldest line the page has
	src := TranscriptSource{Provider: "antigravity", SessionID: `plugin:{"version":1,"data":{"conversationId":"conv-p"}}`}
	tail, _ = ReadTranscriptPage(home, src, Page{Tail: true, Budget: 300, Refs: true})
	poll, _ := ReadTranscriptPage(home, src, Page{From: tail.Before, Budget: 300, Refs: true})
	if !poll.Replace || !reflect.DeepEqual(poll.Events, tail.Events) {
		t.Errorf("antigravity poll from %d: %d events, tail had %d", tail.Before, len(poll.Events), len(tail.Events))
	}
	if _, _, err := TranscriptImage(home, src, "1.0"); err == nil {
		t.Error("antigravity has no images")
	}
}

// transcriptJSON is the page's view of /api/transcript.
type transcriptJSON struct {
	Events                         []Event
	Next, Before                   int
	Older, More, Fallback, Replace bool
}

func getTranscript(t *testing.T, f *Factory, q string) (transcriptJSON, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/transcript/"+q, nil))
	var j transcriptJSON
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
	}
	return j, rec
}

func TestTranscriptEndpointSendsRecentTextThenOlderPages(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "building"})
	f, _ := newTestFactory(t)
	src, n := largeClaudeSession(t, f.cfg.Home, 200)
	c.Provider, c.SessionID, c.Cwd = src.Provider, src.SessionID, src.Cwd
	f.SetAgents([]Agent{c})

	tail, rec := getTranscript(t, f, "c1?tail=1")
	if rec.Code != 200 || rec.Body.Len() > 2*TranscriptBudget || bytes.Contains(rec.Body.Bytes(), []byte(`"data"`)) {
		t.Fatalf("tail: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if !tail.Older || tail.Next != n || tail.Events[len(tail.Events)-1].Text != "the last word" {
		t.Errorf("tail = next %d older %v", tail.Next, tail.Older)
	}
	older, _ := getTranscript(t, f, fmt.Sprintf("c1?before=%d", tail.Before))
	if len(older.Events) == 0 || older.Events[len(older.Events)-1].Line >= tail.Events[0].Line {
		t.Errorf("older page = %d events", len(older.Events))
	}
	poll, _ := getTranscript(t, f, fmt.Sprintf("c1?after=%d&from=%d", tail.Next, tail.Before))
	if len(poll.Events) != 0 || poll.Next != n || poll.More {
		t.Errorf("poll = %+v", poll)
	}

	// the image route serves the bytes a reference names, only for an agent on the board
	var ref string
	for _, e := range tail.Events {
		if e.Kind == "result" {
			ref = e.Images[0].Ref
		}
	}
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/transcript/c1/image/"+ref, nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "private") || !bytes.Equal(rec.Body.Bytes(), fakePNG(199, 100_000)) {
		t.Errorf("image: %d %v %d bytes", rec.Code, rec.Header(), rec.Body.Len())
	}
	for _, path := range []string{"/api/transcript/nobody/image/" + ref, "/api/transcript/c1/image/1.0", "/api/transcript/c1/image/..%2Fx"} {
		rec = httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestTranscriptImageRefusesUnsafeAndMissingImages(t *testing.T) {
	home := t.TempDir()
	big := strings.Repeat("A", imageCap+4)
	lines := []string{
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"image","source":{"type":"base64","media_type":"image/svg+xml","data":"PHN2Zy8+"}}]}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u2","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}},{"type":"image","source":{"type":"base64","media_type":"image/gif","data":"R0lG"}}]}]}}`,
	}
	writeFile(t, home+"/.claude/projects/-w/s.jsonl", strings.Join(lines, "\n")+"\n")
	src := TranscriptSource{Provider: "claude", SessionID: "s", Cwd: "/w"}
	res, _ := ReadTranscriptPage(home, src, Page{Tail: true, Budget: TranscriptBudget, Refs: true})
	if im := res.Events[1].Images; !im[0].Missing || im[0].Ref != "" || im[1].Ref != "2.1" {
		t.Errorf("images = %+v", im)
	}
	if _, _, err := TranscriptImage(home, src, "1.0"); err == nil {
		t.Error("served an svg")
	}
	if _, _, err := TranscriptImage(home, src, "2.0"); err == nil {
		t.Error("served a too-large image")
	}
	if media, data, err := TranscriptImage(home, src, "2.1"); err != nil || media != "image/gif" || string(data) != "GIF" {
		t.Errorf("gif: %s %q %v", media, data, err)
	}
}

func TestTranscriptPagesAddAReplacedAgentsHistoryOnceTheStartIsReached(t *testing.T) {
	f, _ := newTestFactory(t)
	home := f.cfg.Home
	writeFile(t, home+"/.claude/projects/-w-old1/old-sess.jsonl", strings.Join([]string{
		`{"type":"user","timestamp":"t0","message":{"content":"Original task"}}`,
		`{"type":"user","timestamp":"t1","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
			base64.StdEncoding.EncodeToString(fakePNG(7, 500)) + `"}}]}]}}`,
		`{"type":"assistant","timestamp":"t2","message":{"content":[{"type":"text","text":"Old assistant reply"}]}}`,
	}, "\n")+"\n")
	newSrc, n := largeClaudeSession(t, home, 300)
	old := card("old1", map[string]string{labelStage: "building"})
	old.Provider, old.SessionID, old.Cwd = "claude", "old-sess", "/w/old1"
	repl := card("new1", map[string]string{labelStage: "building", labelReplaces: "old1"})
	repl.Provider, repl.SessionID, repl.Cwd = newSrc.Provider, newSrc.SessionID, newSrc.Cwd
	f.SetAgents([]Agent{old, repl})

	texts := func(evs []Event) string {
		var s []string
		for _, e := range evs {
			s = append(s, e.Text)
		}
		return strings.Join(s, "\n")
	}
	tail, rec := getTranscript(t, f, "new1?tail=1")
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	// walk back to the start of the new agent's history; only the page that reaches it has the old one
	pages := []transcriptJSON{tail}
	for cur := tail; cur.Older; {
		cur, _ = getTranscript(t, f, fmt.Sprintf("new1?before=%d", cur.Before))
		pages = append(pages, cur)
	}
	for i, pg := range pages {
		has := strings.Contains(texts(pg.Events), "Old assistant reply")
		if has != (i == len(pages)-1) {
			t.Errorf("page %d of %d has old history = %v", i, len(pages), has)
		}
	}
	first := pages[len(pages)-1].Events
	if first[0].Text != "Original task" || first[0].Prev != 1 || !strings.Contains(texts(first), "Switched from") {
		t.Errorf("start = %+v", first[:3])
	}
	// the old agent's image is a ref into its own session file
	var ref string
	for _, e := range first {
		if e.Prev == 1 && len(e.Images) == 1 {
			ref = e.Images[0].Ref
		}
	}
	if ref != "2.0.1" {
		t.Fatalf("old image ref = %q", ref)
	}
	rec = httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/transcript/new1/image/"+ref, nil))
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), fakePNG(7, 500)) {
		t.Errorf("old image: %d %d bytes", rec.Code, rec.Body.Len())
	}
	for _, bad := range []string{"2.0.2", "2.0.0", "2.0.10"} {
		rec = httptest.NewRecorder()
		f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/transcript/new1/image/"+bad, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	// a poll sends only new lines, never the old history again
	poll, _ := getTranscript(t, f, fmt.Sprintf("new1?after=%d&from=%d", n, tail.Before))
	if len(poll.Events) != 0 {
		t.Errorf("poll = %s", texts(poll.Events))
	}
}

func TestPollsNeverResendAReplacedAgentsHistory(t *testing.T) {
	f, _ := newTestFactory(t)
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-old2/old2-sess.jsonl",
		`{"type":"assistant","timestamp":"t0","message":{"content":[{"type":"text","text":"Old agent reply"}]}}`+"\n")
	old := card("old2", map[string]string{labelStage: "building"})
	old.Provider, old.SessionID, old.Cwd = "claude", "old2-sess", "/w/old2"
	repl := card("new2", map[string]string{labelStage: "building", labelReplaces: "old2"})
	repl.Provider, repl.SessionID, repl.Cwd = "claude", "new2-sess", "/w/new2" // no session file yet
	f.SetAgents([]Agent{old, repl})
	prevs := func(evs []Event) (n int) {
		for _, e := range evs {
			if e.Prev > 0 {
				n++
			}
		}
		return n
	}
	tail, _ := getTranscript(t, f, "new2?tail=1")
	if prevs(tail.Events) != 2 || tail.Before != 0 {
		t.Fatalf("tail = before %d, %+v", tail.Before, tail.Events)
	}
	for i := 0; i < 3; i++ {
		if poll, _ := getTranscript(t, f, "new2?after=0&from=0"); prevs(poll.Events) != 0 {
			t.Fatalf("poll %d resent the old history: %+v", i, poll.Events)
		}
	}
	// once the new agent writes its file, a poll sends only its lines
	writeFile(t, f.cfg.Home+"/.claude/projects/-w-new2/new2-sess.jsonl",
		`{"type":"assistant","timestamp":"t1","message":{"content":[{"type":"text","text":"New agent reply"}]}}`+"\n")
	poll, _ := getTranscript(t, f, "new2?after=0&from=0")
	if len(poll.Events) != 1 || poll.Events[0].Text != "New agent reply" || poll.Next != 1 {
		t.Errorf("first poll with a file = %+v", poll)
	}
}
