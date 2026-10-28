package factory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const tokenFileLimit int64 = 32 << 20

// TokenCounts normalizes input to include cache reads/writes. Cache and reasoning
// are subsets: Total is Input + Output, never the sum of every displayed column.
type TokenCounts struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

func (c *TokenCounts) add(d TokenCounts) {
	c.Input += d.Input
	c.Output += d.Output
	c.CacheRead += d.CacheRead
	c.CacheWrite += d.CacheWrite
	c.Reasoning += d.Reasoning
	c.Total += d.Total
}

type tokenEvent struct {
	At, From time.Time // From is unknown for an initial cumulative snapshot or counter reset.
	Model    string
	Counts   TokenCounts
}

type tokenRead struct {
	events []tokenEvent
	status string
}

type nativeTokens struct {
	Input         *int64 `json:"input_tokens"`
	Output        int64  `json:"output_tokens"`
	Cached        int64  `json:"cached_input_tokens"`
	CacheRead     int64  `json:"cache_read_input_tokens"`
	CacheWrite    int64  `json:"cache_creation_input_tokens"`
	Write         int64  `json:"cache_write_input_tokens"`
	MuseCached    int64  `json:"cached_tokens"`
	MuseRead      int64  `json:"cache_read_tokens"`
	MuseWrite     int64  `json:"cache_write_tokens"`
	Reasoning     int64  `json:"reasoning_output_tokens"`
	MuseReasoning int64  `json:"reasoning_tokens"`
	Details       struct {
		Thinking int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (n nativeTokens) counts(provider string) (TokenCounts, bool) {
	if n.Input == nil || *n.Input < 0 {
		return TokenCounts{}, false
	}
	c := TokenCounts{Input: *n.Input, Output: n.Output, CacheRead: n.Cached, CacheWrite: n.Write, Reasoning: n.Reasoning}
	switch provider {
	case "claude":
		c.CacheRead, c.CacheWrite, c.Reasoning = n.CacheRead, n.CacheWrite, n.Details.Thinking
		c.Input += c.CacheRead + c.CacheWrite
	case "muse":
		c.CacheRead, c.CacheWrite, c.Reasoning = max(n.MuseCached, n.MuseRead), n.MuseWrite, n.MuseReasoning
	}
	c.Total = c.Input + c.Output
	return c, c.Total >= 0 && c.Input >= 0 && c.Output >= 0 && c.CacheRead >= 0 && c.CacheWrite >= 0 && c.Reasoning >= 0 && c.CacheRead <= c.Input && c.CacheWrite <= c.Input && c.Reasoning <= c.Output
}

func tokenPath(home string, a Agent) string {
	id := nativeSessionID(a.SessionID)
	if id == "" {
		return ""
	}
	switch a.Provider {
	case "claude":
		return claudePath(home, a.Cwd, id)
	case "codex":
		return firstFile(filepath.Join(home, ".codex/sessions/*/*/*", "rollout-*-"+id+".jsonl"), filepath.Join(home, ".codex/archived_sessions", "rollout-*-"+id+".jsonl"), filepath.Join(home, ".codex/archived_sessions/*/*/*", "rollout-*-"+id+".jsonl"))
	case "muse":
		return firstFile(filepath.Join(home, ".local/share/muse/sessions/*/*/*", id, "session.jsonl"))
	}
	return ""
}

// tokenParser is one session file's decoding state between reads: counters, message ids, models
// and times only. Record text is never retained.
type tokenParser struct {
	provider   string
	status     string // unsupported until a usage record; error after a bad one
	events     []tokenEvent
	seen       map[string]int // Claude message id or Muse time+model -> index in events
	cumulative map[TokenCounts]bool
	prev       TokenCounts // last Codex cumulative reading
	previous   time.Time
	model      string
}

func newTokenParser(provider string) *tokenParser {
	return &tokenParser{provider: provider, status: "unsupported", seen: map[string]int{}, cumulative: map[TokenCounts]bool{}}
}

// line decodes one complete record. Blank lines are skipped: a record committed without its
// newline is followed by one.
func (p *tokenParser) line(b []byte) {
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	var d struct {
		Type, Timestamp string
		Recorded        int64  `json:"recorded_at"`
		PayloadType     string `json:"payload_type"`
		Message         struct {
			ID, Model string
			Usage     nativeTokens
		}
		Payload struct {
			Type, Model string
			Info        *struct {
				Total nativeTokens `json:"total_token_usage"`
			}
			Event struct {
				Kind, Model string
				Usage       nativeTokens
			}
		}
	}
	if json.Unmarshal(b, &d) != nil {
		p.status = "error"
		return
	}
	var c TokenCounts
	var ok bool
	at := parseTime(d.Timestamp)
	from := at
	key := ""
	switch p.provider {
	case "claude":
		if d.Type != "assistant" {
			return
		}
		c, ok = d.Message.Usage.counts(p.provider)
		p.model = d.Message.Model
		key = d.Message.ID
		if key == "" {
			ok = false
		}
	case "codex":
		if d.Type == "turn_context" {
			p.model = d.Payload.Model
			return
		}
		if d.Type != "event_msg" || d.Payload.Type != "token_count" || d.Payload.Info == nil {
			return
		}
		c, ok = d.Payload.Info.Total.counts(p.provider)
		if !ok {
			break
		}
		if p.status == "unsupported" {
			p.status = "readable"
		}
		if p.cumulative[c] {
			return
		}
		p.cumulative[c] = true
		from = p.previous
		if c.Input < p.prev.Input || c.Output < p.prev.Output {
			p.prev = TokenCounts{}
			from = time.Time{}
		}
		total := c
		prev := p.prev
		if c.CacheRead < prev.CacheRead || c.CacheWrite < prev.CacheWrite || c.Reasoning < prev.Reasoning {
			p.status = "error"
		}
		c = TokenCounts{Input: c.Input - prev.Input, Output: c.Output - prev.Output, CacheRead: c.CacheRead - prev.CacheRead, CacheWrite: c.CacheWrite - prev.CacheWrite, Reasoning: c.Reasoning - prev.Reasoning, Total: c.Total - prev.Total}
		c.CacheRead, c.CacheWrite, c.Reasoning = max(0, c.CacheRead), max(0, c.CacheWrite), max(0, c.Reasoning)
		p.prev, p.previous = total, at
		if c == (TokenCounts{}) {
			return
		}
	case "muse":
		if d.PayloadType != "runtime.session" || d.Payload.Event.Kind != "model_completed" {
			return
		}
		c, ok = d.Payload.Event.Usage.counts(p.provider)
		p.model = d.Payload.Event.Model
		if d.Recorded > 0 {
			at = time.UnixMicro(d.Recorded).UTC()
		}
		from = at
		if !at.IsZero() {
			key = at.Format(time.RFC3339Nano) + p.model
		}
	}
	if !ok {
		p.status = "error"
		return
	}
	e := tokenEvent{At: at, From: from, Model: p.model, Counts: c}
	if i, exists := p.seen[key]; key != "" && exists {
		// Claude repeats a message while its output grows. Keep one final reading.
		if e.At.Before(p.events[i].At) {
			return
		}
		e.From = p.events[i].From
		p.events[i] = e
	} else {
		if key != "" {
			p.seen[key] = len(p.events)
		}
		p.events = append(p.events, e)
	}
}

// result is the parse so far. Its events are shared: callers only read them.
func (p *tokenParser) result() tokenRead {
	r := tokenRead{status: p.status, events: p.events}
	if r.status == "unsupported" && len(r.events) > 0 {
		r.status = "readable"
	}
	return r
}

// readTokenFile reads one whole session file up to tokenFileLimit, with no cache.
func readTokenFile(ctx context.Context, path, provider string) tokenRead {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return tokenRead{status: "missing"}
		}
		return tokenRead{status: "error"}
	}
	if st.Size() > tokenFileLimit {
		return tokenRead{status: "limited"}
	}
	var tf tokenFile
	r, _ := tf.read(ctx, path, provider, tokenFileLimit, tokenFileLimit)
	return r
}

// Session files are read progressively: each refresh reads only bytes it has not decoded, within a
// shared byte budget and a per-file chunk, and keeps the decoder state for the next refresh.
const (
	tokenLineLimit   = 4 << 20 // a longer record is skipped as an error
	tokenCheckWindow = 4 << 10 // bytes hashed at the start and before the read offset
)

// tokenFile is the cache entry of one session file: its identity at the last read, how far it is
// decoded (always the end of a complete record) and the decoder state. It keeps hashes of two
// short windows of the decoded bytes, never the bytes.
type tokenFile struct {
	provider string
	info     os.FileInfo // the stat behind offset: inode, size and modification time
	offset   int64
	skipping bool // inside an over-long record that ends past offset
	headLen  int64
	headSum  [sha256.Size]byte
	tailLen  int64 // the window ends at offset
	tailSum  [sha256.Size]byte
	parser   *tokenParser
}

func (tf *tokenFile) reset(provider string) {
	*tf = tokenFile{provider: provider, parser: newTokenParser(provider)}
}

// read brings tf up to date with path within budget bytes (chunk at most from this file). It returns
// the parse with status "limited" while records are left unread (budget, chunk, a cancelled
// context, or a record still being written), never as complete. Bytes read come back too.
func (tf *tokenFile) read(ctx context.Context, path, provider string, budget, chunk int64) (tokenRead, int64) {
	if tf.parser == nil || tf.provider != provider {
		tf.reset(provider)
	}
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			tf.reset(provider)
			return tokenRead{status: "missing"}, 0
		}
		return tokenRead{status: "error"}, 0
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return tokenRead{status: "error"}, 0
	}
	var used int64
	if tf.info != nil && !(os.SameFile(tf.info, st) && st.Size() == tf.info.Size() && st.ModTime().Equal(tf.info.ModTime())) {
		switch {
		case !os.SameFile(tf.info, st) || st.Size() < tf.offset:
			tf.reset(provider) // replaced or truncated
		case tf.headLen+tf.tailLen > budget:
			return tf.partial(), 0 // cannot check it this refresh: earlier counts, flagged limited
		default:
			used = tf.headLen + tf.tailLen
			if !windowMatches(fh, 0, tf.headLen, tf.headSum) || !windowMatches(fh, tf.offset-tf.tailLen, tf.tailLen, tf.tailSum) {
				tf.reset(provider) // rewritten in place
			}
		}
	}
	tf.info = st
	size := st.Size()
	n := min(size-tf.offset, budget-used, chunk)
	// A read that may end inside a record decodes nothing of it and is repeated next time, so a
	// short budget waits for the next refresh instead of spending itself on that.
	if n < min(size-tf.offset, tokenLineLimit+1, chunk) {
		n = 0
	}
	if n <= 0 {
		if tf.offset == size && !tf.skipping {
			return tf.parser.result(), used
		}
		return tf.partial(), used
	}
	sec := io.NewSectionReader(fh, tf.offset, n)
	br := bufio.NewReaderSize(sec, 64<<10)
	toEnd := tf.offset+n == size
	var pos int64 // bytes of sec consumed
	var line []byte
	var tail, headBuf []byte
	head := tf.headLen == 0 && tf.offset == 0
	// feed adds decoded bytes, in file order, to the check windows
	feed := func(b []byte) {
		if head && len(headBuf) < tokenCheckWindow {
			headBuf = append(headBuf, b[:min(len(b), tokenCheckWindow-len(headBuf))]...)
		}
		tail = append(tail, b...)
		if len(tail) > tokenCheckWindow {
			tail = append(tail[:0], tail[len(tail)-tokenCheckWindow:]...)
		}
	}
	start := tf.offset
	for ctx.Err() == nil {
		part, err := br.ReadSlice('\n')
		pos += int64(len(part))
		switch {
		case tf.skipping:
			feed(part) // skipped through to its end, so decoded
		case len(line)+len(part) > tokenLineLimit:
			tf.skipping = true
			tf.parser.status = "error"
			feed(line)
			feed(part)
			line = line[:0]
		default:
			line = append(line, part...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil { // a complete record
			if !tf.skipping {
				tf.parser.line(bytes.TrimRight(line, "\r\n"))
				feed(line)
			}
			tf.skipping = false
			tf.offset = start + pos
			line = line[:0]
			continue
		}
		// end of the section: an over-long record is skipped through it; a finished last
		// record without a newline counts; anything else waits for the next read
		switch {
		case tf.skipping:
			tf.offset = start + pos
		case toEnd && err == io.EOF && (len(bytes.TrimSpace(line)) == 0 || json.Valid(bytes.TrimSpace(line))):
			tf.parser.line(line)
			feed(line)
			tf.offset = start + pos
		}
		break
	}
	used += pos
	if head && len(headBuf) > 0 {
		tf.headLen, tf.headSum = int64(len(headBuf)), sha256.Sum256(headBuf)
	}
	if len(tail) > 0 {
		tf.tailLen, tf.tailSum = int64(len(tail)), sha256.Sum256(tail)
	}
	if tf.offset == size && !tf.skipping && ctx.Err() == nil {
		return tf.parser.result(), used
	}
	return tf.partial(), used
}

// partial is the parse so far of a file with records left unread.
func (tf *tokenFile) partial() tokenRead {
	r := tf.parser.result()
	r.status = "limited"
	return r
}

// pending is how many bytes of the file are not decoded yet.
func (tf *tokenFile) pending() int64 {
	if tf.info == nil {
		return 0
	}
	return max(0, tf.info.Size()-tf.offset)
}

func windowMatches(fh *os.File, at, n int64, sum [sha256.Size]byte) bool {
	if n == 0 {
		return true
	}
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, at); err != nil {
		return false
	}
	return sha256.Sum256(b) == sum
}

// tokenStage uses the exact events behind StageSpan, with half-open spans. A
// cumulative interval crossing a boundary stays unassigned instead of being prorated.
func tokenStage(e tokenEvent, t CardTiming, now time.Time) (string, int, int) {
	if e.At.IsZero() || e.From.IsZero() || e.At.After(now) || e.From.After(e.At) {
		return "unassigned", 0, 0
	}
	for i, span := range t.Events {
		end := now.Add(time.Nanosecond)
		if i+1 < len(t.Events) {
			end = t.Events[i+1].At
		}
		if span.Stage == "landed" || e.From.Before(span.At) || !e.At.Before(end) {
			continue
		}
		round := 0
		for _, r := range t.Rounds {
			if !e.From.Before(r.Start) && !r.End.IsZero() && e.At.Before(r.End) && ((span.Stage == "plan-review" && r.Kind == "plan") || (span.Stage == "review" && r.Kind == "code")) {
				round = r.N
			}
		}
		return span.Stage, i + 1, round
	}
	return "unassigned", 0, 0
}

func usageRole(a Agent) string {
	k := agentKind(a)
	if k == "card" {
		return "fellow"
	}
	if k == "reviewer" {
		if l := a.L(labelLens); l != "" {
			return strings.TrimSpace(a.L(labelReviewKind) + " " + l + " reviewer")
		}
		return strings.TrimSpace(a.L(labelReviewKind) + " reviewer")
	}
	if k == "other" && a.Labels["paseo.parent-agent-id"] != "" {
		return "sub-agent"
	}
	return k
}
