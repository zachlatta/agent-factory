package factory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	textCap  = 60000     // chars per tool input/output; the rest is cut with a note
	imageCap = 3_000_000 // base64 chars per inline image
)

type TranscriptSource struct{ Provider, SessionID, Cwd string }

type Image struct {
	Kind    string `json:"kind"`
	Media   string `json:"media,omitempty"`
	Data    string `json:"data,omitempty"`
	Missing bool   `json:"missing,omitempty"`
	Ref     string `json:"ref,omitempty"` // line.k: the k-th image of line `line`, served by /api/transcript/{id}/image/{ref}
	From    string `json:"from,omitempty"`
	Ts      string `json:"ts,omitempty"`
	Line    int    `json:"line,omitempty"`
}

type Event struct {
	Kind   string         `json:"kind"` // user, assistant, system, thinking, tool, result, image
	Text   string         `json:"text,omitempty"`
	ID     string         `json:"id,omitempty"`
	Name   string         `json:"name,omitempty"`
	Native string         `json:"native,omitempty"` // the provider's own tool name, when Name is mapped
	Input  map[string]any `json:"input,omitempty"`
	Images []Image        `json:"images,omitempty"`
	Error  bool           `json:"error,omitempty"`
	Media  string         `json:"media,omitempty"`
	Data   string         `json:"data,omitempty"`
	Miss   bool           `json:"missing,omitempty"`
	Ref    string         `json:"ref,omitempty"`
	Prev   int            `json:"prev,omitempty"` // from the agent this many back in the card's lineage (a replaced agent)
	From   string         `json:"from,omitempty"`
	Ts     string         `json:"ts,omitempty"`
	Line   int            `json:"line,omitempty"`
}

type Transcript struct {
	Events   []Event `json:"events"`
	Next     int     `json:"next"`
	Before   int     `json:"before,omitempty"` // a newest-first page starts at this line; lines before it are older pages
	Older    bool    `json:"older,omitempty"`  // there are lines before Before
	More     bool    `json:"more,omitempty"`   // a forward read stopped at its budget: read again from Next
	Fallback bool    `json:"fallback,omitempty"`
	Replace  bool    `json:"replace,omitempty"` // Events is the whole transcript, not lines after `after`
	Provider string  `json:"provider,omitempty"`
	Path     string  `json:"path,omitempty"`
}

var systemText = regexp.MustCompile(`^\s*<(command-|local-command|system-reminder|task-notification|bash-|user-memory)`)

func capText(s string) string {
	if len(s) > textCap {
		return s[:textCap] + fmt.Sprintf("\n\n[... %d more characters not shown]", len(s)-textCap)
	}
	return s
}

// ReadTranscript reads an agent's own session file, which has every message, thinking block, full
// tool input and output, and image; `paseo logs` truncates those and keeps nothing once an agent is
// archived. Each provider has its reader (transcript_providers.go for all but Claude). after is a
// line offset for incremental polling; a reader whose file changes in place sets Replace and sends
// everything each time. With no file, or nothing readable in it yet, it returns Fallback.
func ReadTranscript(home string, src TranscriptSource, after int) (Transcript, error) {
	return ReadTranscriptPage(home, src, Page{After: after})
}

// TranscriptBudget is about how many bytes of events one transcript page sends: the page shows the
// newest text first and loads older pages, and images, only when asked.
const TranscriptBudget = 300_000

const pageEvents = 400 // most events in one page

// Page is the part of a transcript to read. The zero Page is every event; Tail and Before read
// newest first, back from the end or from line Before, until Budget; After reads forward.
type Page struct {
	After  int  // forward: lines after this one
	Before int  // newest first: lines before this one
	Tail   bool // newest first from the last line
	From   int  // a reader that sends everything each time (Replace) keeps events from this line on
	Budget int  // bytes of encoded events per page, at whole lines, at least one; 0 is no limit
	Refs   bool // images carry a Ref to fetch them by instead of their data
}

func (p Page) back() bool { return p.Tail || p.Before > 0 }

// lineParser turns line n of a session file into its events.
type lineParser func(n int, line []byte) []Event

// sessionFile is the session file of src and its line parser. Antigravity rewrites its file in
// place and pairs steps across lines, so it has no line parser and is read whole.
func sessionFile(home string, src TranscriptSource) (string, lineParser) {
	id := nativeSessionID(src.SessionID)
	switch {
	case id == "":
	case src.Provider == "claude":
		return claudePath(home, src.Cwd, id), claudeLineEvents
	case src.Provider == "codex":
		return codexPath(home, id), codexLineEvents
	case src.Provider == "muse":
		return musePath(home, id), museLineEvents
	}
	return "", nil
}

// ReadTranscriptPage reads part of a transcript (see Page and ReadTranscript).
func ReadTranscriptPage(home string, src TranscriptSource, p Page) (Transcript, error) {
	var (
		res   Transcript
		err   error
		ok    bool
		empty bool // the whole transcript has no events
	)
	if path, parse := sessionFile(home, src); path != "" {
		res, err = readLines(path, parse, p)
		ok, empty = true, len(res.Events) == 0 && p.After == 0 && p.Before == 0
	} else if id := nativeSessionID(src.SessionID); id != "" && src.Provider == "antigravity" {
		if res, ok, err = readAntigravity(home, id); ok {
			empty = len(res.Events) == 0
			res = windowEvents(res, p)
		}
	}
	if err != nil {
		return res, err
	}
	if !ok || empty {
		return Transcript{Fallback: true, Replace: true, Provider: src.Provider, Events: []Event{}}, nil
	}
	res.Provider = src.Provider
	return res, nil
}

// ErrNoHistory means an agent's own session file cannot be read. Summaries, transcripts and reads
// made long after an agent stopped report it rather than asking Paseo: `paseo logs` loads the
// agent, resuming its provider session even when it is archived.
var ErrNoHistory = errors.New("history unavailable")

// transcriptReaders are the providers ReadTranscript has a session-file reader for.
var transcriptReaders = map[string]bool{"claude": true, "codex": true, "antigravity": true, "muse": true}

// storedEvents are agent a's transcript events from its own session file, never from Paseo. Every
// failure wraps ErrNoHistory and says why.
func storedEvents(home string, a Agent) ([]Event, error) {
	id := nativeSessionID(a.SessionID)
	switch {
	case a.SessionID == "":
		return nil, fmt.Errorf("%w: agent %s has no stored session", ErrNoHistory, short(a.ID))
	case !transcriptReaders[a.Provider]:
		return nil, fmt.Errorf("%w: no session-file reader for provider %q", ErrNoHistory, a.Provider)
	case id == "":
		return nil, fmt.Errorf("%w: agent %s has an unreadable session id", ErrNoHistory, short(a.ID))
	}
	res, err := ReadTranscript(home, TranscriptSource{Provider: a.Provider, SessionID: a.SessionID, Cwd: a.Cwd}, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s session %s: %v", ErrNoHistory, a.Provider, id, err)
	}
	if res.Fallback {
		return nil, fmt.Errorf("%w: no %s session file for %s, or nothing in it yet", ErrNoHistory, a.Provider, id)
	}
	return res.Events, nil
}

// storedText is the last n user, thinking and assistant entries of agent a's session file, in the
// shape of `paseo logs --filter text` (`[User] …`, `[Thought] …`, plain replies), for the board's
// summary and for line parsers (VERDICT, TIER, DUPLICATE OF). It never asks Paseo.
func storedText(home string, a Agent, n int) (string, error) {
	evs, err := storedEvents(home, a)
	if err != nil {
		return "", err
	}
	var entries []string
	for _, e := range evs {
		switch e.Kind {
		case "user":
			entries = append(entries, "[User] "+e.Text)
		case "thinking":
			entries = append(entries, "[Thought] "+e.Text)
		case "assistant":
			entries = append(entries, e.Text)
		}
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	return strings.Join(entries, "\n"), nil
}

// lineIdx is where each complete line of a file starts, then the end of the last one: line n is
// [off[n-1], off[n]-1). A last line with no newline counts only when it is whole JSON; otherwise it
// is still being written and is left for a later read. held is set when it counted: its end is then
// stored as if a newline followed it.
type lineIdx struct {
	off  []int64
	held bool
}

func (x lineIdx) lines() int { return len(x.off) - 1 }

// errStaleIndex is a line index that no longer matches its file, which changed in place.
var errStaleIndex = errors.New("line index is stale")

// indexScanned counts the bytes scanned for newlines, so tests can see what a read scanned.
var indexScanned atomic.Int64

// scanIndex adds to x the lines of f's bytes [from, size). x holds every line ending before from
// and no held line. It then checks again whether a last line with no newline is whole.
func scanIndex(f *os.File, from, size int64, x lineIdx) (lineIdx, error) {
	buf := make([]byte, min(1<<20, max(size-from, 1)))
	for pos := from; pos < size; {
		b := buf[:min(int64(len(buf)), size-pos)]
		if _, err := f.ReadAt(b, pos); err != nil {
			if err == io.EOF { // shorter than its Stat: it changed while being read
				err = errStaleIndex
			}
			return x, err
		}
		indexScanned.Add(int64(len(b)))
		for i := 0; ; {
			j := bytes.IndexByte(b[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
			x.off = append(x.off, pos+int64(i))
		}
		pos += int64(len(b))
	}
	if last := x.off[len(x.off)-1]; size > last && size-last <= maxLine {
		b := make([]byte, size-last)
		if _, err := f.ReadAt(b, last); err == nil && json.Valid(b) {
			x.off, x.held = append(x.off, size+1), true
		}
	}
	return x, nil
}

// lineIndexes keeps the line index of the session files read last, so a read scans only the bytes
// added since the one before. An entry stays good while its file is the same file with the same
// size and mtime; a file that only grew is scanned from its old end. A shrink, an earlier or a
// changed mtime at the same size, or another file at the path rebuilds it, as does a line that
// lineAt finds no longer ends where the index says.
var lineIndexes = struct {
	sync.Mutex
	m    map[string]*indexEntry
	tick uint64
}{m: map[string]*indexEntry{}}

const lineIndexCap = 64 // files

type indexEntry struct {
	info os.FileInfo
	x    lineIdx // shared: never written once stored
	used uint64
}

// indexFor is the line index of f, open at path.
func indexFor(f *os.File, path string) (lineIdx, error) {
	info, err := f.Stat()
	if err != nil {
		return lineIdx{}, err
	}
	lineIndexes.Lock()
	prev := lineIndexes.m[path]
	if prev != nil {
		lineIndexes.tick++
		prev.used = lineIndexes.tick
	}
	lineIndexes.Unlock()
	x, from := lineIdx{off: []int64{0}}, int64(0)
	if prev != nil && os.SameFile(prev.info, info) {
		size, mt := info.Size(), info.ModTime()
		switch {
		case size == prev.info.Size() && mt.Equal(prev.info.ModTime()):
			return prev.x, nil
		case size > prev.info.Size() && !mt.Before(prev.info.ModTime()):
			x, from = prev.x, prev.info.Size()
			if x.held {
				x.off, x.held = x.off[:len(x.off)-1:len(x.off)-1], false
			}
		}
	}
	x, err = scanIndex(f, from, info.Size(), x)
	if err != nil {
		return x, err
	}
	x.off = slices.Clip(x.off) // so a later append copies rather than writes into a shared array
	storeIndex(path, prev, info, x)
	return x, nil
}

// storeIndex saves x as the index of path's file info, built from the entry prev, unless another
// read has since saved a newer index of the same file.
func storeIndex(path string, prev *indexEntry, info os.FileInfo, x lineIdx) {
	lineIndexes.Lock()
	defer lineIndexes.Unlock()
	if cur := lineIndexes.m[path]; cur != nil && cur != prev && os.SameFile(cur.info, info) &&
		cur.info.Size() >= info.Size() && !cur.info.ModTime().Before(info.ModTime()) {
		return
	}
	if _, ok := lineIndexes.m[path]; !ok && len(lineIndexes.m) >= lineIndexCap {
		var oldest string
		for p, e := range lineIndexes.m {
			if oldest == "" || e.used < lineIndexes.m[oldest].used {
				oldest = p
			}
		}
		delete(lineIndexes.m, oldest)
	}
	lineIndexes.tick++
	lineIndexes.m[path] = &indexEntry{info: info, x: x, used: lineIndexes.tick}
}

// dropIndex forgets path's line index.
func dropIndex(path string) {
	lineIndexes.Lock()
	delete(lineIndexes.m, path)
	lineIndexes.Unlock()
}

// withIndex calls fn with path open and its line index. When fn finds the index stale it calls fn
// once more with an index built from scratch.
func withIndex(path string, fn func(f *os.File, x lineIdx) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for try := 0; ; try++ {
		x, err := indexFor(f, path)
		if err == nil {
			err = fn(f, x)
		}
		if !errors.Is(err, errStaleIndex) || try == 1 {
			return err
		}
		dropIndex(path)
	}
}

const maxLine = 256 << 20 // longer lines are skipped

// lineAt is line n of f (no newline), or nil when it is too long to read. It is errStaleIndex when
// the line does not start and end at a newline as x says, or holds one: the file changed in place.
func lineAt(f *os.File, x lineIdx, n int) ([]byte, error) {
	start, end := x.off[n-1], x.off[n]-1
	if end-start > maxLine {
		return nil, nil
	}
	held := x.held && n == x.lines() // no newline after it yet
	lo, hi := start, end+1
	if n > 1 {
		lo--
	}
	if held {
		hi = end
	}
	b := make([]byte, hi-lo)
	if _, err := f.ReadAt(b, lo); err != nil {
		if err == io.EOF {
			err = errStaleIndex
		}
		return nil, err
	}
	if n > 1 {
		if b[0] != '\n' {
			return nil, errStaleIndex
		}
		b = b[1:]
	}
	if !held {
		if b[len(b)-1] != '\n' {
			return nil, errStaleIndex
		}
		b = b[:len(b)-1]
	}
	if bytes.IndexByte(b, '\n') >= 0 {
		return nil, errStaleIndex
	}
	return b, nil
}

// readLines reads the page p of the session file at path.
func readLines(path string, parse lineParser, p Page) (Transcript, error) {
	res := Transcript{Events: []Event{}, Path: path}
	err := withIndex(path, func(f *os.File, x lineIdx) (err error) {
		res, err = readPage(f, x, path, parse, p)
		return err
	})
	return res, err
}

// readPage reads the page p of the session file f at path, with its line index x.
func readPage(f *os.File, x lineIdx, path string, parse lineParser, p Page) (Transcript, error) {
	res := Transcript{Events: []Event{}, Path: path}
	total := x.lines()
	used, count := 0, 0
	// take reports whether line n's events still fit the page; a page always takes its first line
	take := func(evs []Event) bool {
		if p.Budget == 0 || len(evs) == 0 {
			return true
		}
		size := 0
		for _, e := range evs {
			b, _ := json.Marshal(e)
			size += len(b)
		}
		if used > 0 && (used+size > p.Budget || count+len(evs) > pageEvents) {
			return false
		}
		used, count = used+size, count+len(evs)
		return true
	}
	events := func(n int) ([]Event, error) {
		line, err := lineAt(f, x, n)
		if err != nil || line == nil {
			return nil, err
		}
		evs := parse(n, line)
		if p.Refs {
			refImages(evs)
		}
		return evs, nil
	}
	res.Next = total
	if p.back() {
		before := total + 1
		if !p.Tail {
			before = min(p.Before, total+1)
		}
		var chunks [][]Event
		res.Before = before
		for n := before - 1; n >= 1; n-- {
			evs, err := events(n)
			if err != nil {
				return res, err
			}
			if !take(evs) {
				res.Older = true
				break
			}
			chunks, res.Before = append(chunks, evs), n
		}
		for i := len(chunks) - 1; i >= 0; i-- {
			res.Events = append(res.Events, chunks[i]...)
		}
		return res, nil
	}
	res.Next = max(p.After, total)
	for n := p.After + 1; n <= total; n++ {
		evs, err := events(n)
		if err != nil {
			return res, err
		}
		if !take(evs) {
			res.Next, res.More = n-1, true
			break
		}
		res.Events = append(res.Events, evs...)
	}
	return res, nil
}

// windowEvents is the page p of a transcript read whole (res.Replace): newest first by line, or for
// a forward read every event from line p.From on.
func windowEvents(res Transcript, p Page) Transcript {
	all := res.Events
	if p.Refs {
		refImages(all)
	}
	res.Events = []Event{}
	if !p.back() {
		for _, e := range all {
			if e.Line >= p.From {
				res.Events = append(res.Events, e)
			}
		}
		return res
	}
	before := int(^uint(0) >> 1)
	if !p.Tail {
		before = p.Before
	}
	end := len(all) // all[start:end] is the page
	for end > 0 && all[end-1].Line >= before {
		end--
	}
	start, used := end, 0
	res.Before = before
	for start > 0 {
		line, i, size := all[start-1].Line, start, 0
		for i > 0 && all[i-1].Line == line {
			i--
			b, _ := json.Marshal(all[i])
			size += len(b)
		}
		if p.Budget > 0 && used > 0 && (used+size > p.Budget || end-i > pageEvents) {
			res.Older = true
			break
		}
		start, used, res.Before = i, used+size, line
	}
	res.Events = append(res.Events, all[start:end]...)
	return res
}

// eachImage calls fn for each image in evs, the events of one or more lines, in order, with k
// counting the images of each line from 0. im is the event's own image (kind "image") or one of its
// Images.
func eachImage(evs []Event, fn func(line, k int, im *Image)) {
	line, k := -1, 0
	for i := range evs {
		e := &evs[i]
		if e.Line != line {
			line, k = e.Line, 0
		}
		if e.Kind == "image" {
			im := Image{Kind: "image", Media: e.Media, Data: e.Data, Missing: e.Miss, Ref: e.Ref}
			fn(line, k, &im)
			e.Media, e.Data, e.Miss, e.Ref = im.Media, im.Data, im.Missing, im.Ref
			k++
		}
		for j := range e.Images {
			fn(line, k, &e.Images[j])
			k++
		}
	}
}

// imageTypes are the image types the image route serves; others (SVG can run script) show as missing.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// refImages replaces each image's data in evs with its Ref.
func refImages(evs []Event) {
	eachImage(evs, func(line, k int, im *Image) {
		if !im.Missing && !imageTypes[im.Media] {
			im.Missing = true
		}
		if !im.Missing {
			im.Ref = fmt.Sprintf("%d.%d", line, k)
		}
		im.Data = ""
	})
}

// lineageRef is an image ref of an agent a card's agent replaced: line.k.p, p (1 to 99) agents back.
var lineageRef = regexp.MustCompile(`^([0-9]+\.[0-9]+)\.([1-9][0-9]?)$`)

var imageRef = regexp.MustCompile(`^([1-9][0-9]{0,9})\.(0|[1-9][0-9]{0,4})$`)

// TranscriptImage is the image a Ref names in src's session file: its type and decoded bytes.
func TranscriptImage(home string, src TranscriptSource, ref string) (string, []byte, error) {
	m := imageRef.FindStringSubmatch(ref)
	path, parse := sessionFile(home, src)
	if m == nil || path == "" {
		return "", nil, errNoImage
	}
	n, _ := strconv.Atoi(m[1])
	k, _ := strconv.Atoi(m[2])
	var line []byte
	err := withIndex(path, func(f *os.File, x lineIdx) (err error) {
		if n > x.lines() {
			return errNoImage
		}
		line, err = lineAt(f, x, n)
		return err
	})
	if err != nil || line == nil {
		return "", nil, errNoImage
	}
	var media, data string
	eachImage(parse(n, line), func(_, i int, im *Image) {
		if i == k && !im.Missing && imageTypes[im.Media] {
			media, data = im.Media, im.Data
		}
	})
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(data, "="))
	}
	if media == "" || err != nil || len(b) == 0 {
		return "", nil, errNoImage
	}
	return media, b, nil
}

var errNoImage = errors.New("no such image")

var plainID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// nativeSessionID is the provider's own session id from Paseo's persistence.sessionId: plain for
// Claude and Codex, `plugin:{"version":1,"data":{"conversationId"|"sessionId":...}}` for plugin
// providers. It is "" unless the id is a plain token, since it becomes part of a file path.
func nativeSessionID(raw string) string {
	if js, ok := strings.CutPrefix(raw, "plugin:"); ok {
		var p struct {
			Data struct{ ConversationID, SessionID string }
		}
		if json.Unmarshal([]byte(js), &p) != nil {
			return ""
		}
		raw = p.Data.ConversationID
		if raw == "" {
			raw = p.Data.SessionID
		}
	}
	if !plainID.MatchString(raw) {
		return ""
	}
	return raw
}

// firstFile is the first existing path of patterns (globs allowed), or "".
func firstFile(patterns ...string) string {
	for _, p := range patterns {
		hits, _ := filepath.Glob(p)
		if len(hits) > 0 {
			return hits[0]
		}
	}
	return ""
}

// scanLines calls fn for each complete line of path, in order.
// Every line is read before fn sees any, so a stale index found partway never repeats a line.
func scanLines(path string, fn func(n int, line []byte)) error {
	var lines [][]byte
	err := withIndex(path, func(f *os.File, x lineIdx) error {
		lines = make([][]byte, x.lines()+1)
		for n := 1; n <= x.lines(); n++ {
			line, err := lineAt(f, x, n)
			if err != nil {
				return err
			}
			lines[n] = line
		}
		return nil
	})
	if err != nil {
		return err
	}
	for n, line := range lines {
		if line != nil {
			fn(n, line)
		}
	}
	return nil
}

// claudePath is the Claude session file ~/.claude/projects/<cwd>/<id>.jsonl, or "" when there is none.
func claudePath(home, cwd, id string) string {
	dir := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	return firstFile(filepath.Join(home, ".claude/projects", dir, id+".jsonl"),
		filepath.Join(home, ".claude/projects/*", id+".jsonl"))
}

// claudeLineEvents are the events of line n of a Claude session file.
func claudeLineEvents(n int, line []byte) []Event {
	var d struct {
		Type, Timestamp     string
		IsSidechain, IsMeta bool
		Message             struct{ Content json.RawMessage }
	}
	if json.Unmarshal(line, &d) != nil || (d.Type != "user" && d.Type != "assistant") || d.IsSidechain {
		return nil
	}
	var blocks []map[string]any
	var s string
	if json.Unmarshal(d.Message.Content, &s) == nil {
		blocks = []map[string]any{{"type": "text", "text": s}}
	} else {
		json.Unmarshal(d.Message.Content, &blocks)
	}
	var out []Event
	for _, b := range blocks {
		ev, keep := event(d.Type, d.IsMeta, b)
		if keep {
			ev.Ts, ev.Line = d.Timestamp, n
			out = append(out, ev)
		}
	}
	return out
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func image(b map[string]any) Image {
	src, _ := b["source"].(map[string]any)
	data := str(src, "data")
	if str(src, "type") != "base64" || len(data) > imageCap {
		return Image{Kind: "image", Missing: true}
	}
	media := str(src, "media_type")
	if media == "" {
		media = "image/png"
	}
	return Image{Kind: "image", Media: media, Data: data}
}

var commandPromptRe = regexp.MustCompile(`(?s)^\s*(?:<command-(?:name|message)>.*?</command-(?:name|message)>\s*)+<command-args>(.*?)</command-args>\s*$`)

// unwrapCommandPrompt extracts the user's prompt from a Claude slash-command wrapper
// (<command-name>/goal</command-name> ... <command-args>...</command-args>) when non-empty.
func unwrapCommandPrompt(t string) (string, bool) {
	m := commandPromptRe.FindStringSubmatch(t)
	if m == nil {
		return "", false
	}
	args := strings.TrimSpace(m[1])
	return args, args != ""
}

func event(typ string, meta bool, b map[string]any) (Event, bool) {
	switch str(b, "type") {
	case "text":
		t := str(b, "text")
		if strings.TrimSpace(t) == "" || (typ == "user" && meta) {
			return Event{}, false
		}
		if typ == "user" {
			if args, ok := unwrapCommandPrompt(t); ok {
				return Event{Kind: "user", Text: args}, true
			}
			if systemText.MatchString(t) {
				return Event{Kind: "system", Text: capText(t)}, true
			}
		}
		return Event{Kind: typ, Text: t}, true
	case "thinking":
		if t := str(b, "thinking"); strings.TrimSpace(t) != "" {
			return Event{Kind: "thinking", Text: t}, true
		}
	case "image":
		im := image(b)
		return Event{Kind: "image", Media: im.Media, Data: im.Data, Miss: im.Missing, From: typ}, true
	case "tool_use":
		in, _ := b["input"].(map[string]any)
		if in == nil {
			in = map[string]any{}
		}
		for k, v := range in {
			if s, isStr := v.(string); isStr && len(s) > textCap {
				in[k] = capText(s)
			}
		}
		return Event{Kind: "tool", ID: str(b, "id"), Name: str(b, "name"), Input: in}, true
	case "tool_result":
		ev := Event{Kind: "result", ID: str(b, "tool_use_id"), Images: []Image{}}
		ev.Error, _ = b["is_error"].(bool)
		switch c := b["content"].(type) {
		case string:
			ev.Text = capText(c)
		case []any:
			var texts []string
			for _, x := range c {
				xb, _ := x.(map[string]any)
				switch str(xb, "type") {
				case "text":
					texts = append(texts, str(xb, "text"))
				case "image":
					ev.Images = append(ev.Images, image(xb))
				}
			}
			ev.Text = capText(strings.Join(texts, "\n"))
		}
		return ev, true
	}
	return Event{}, false
}
