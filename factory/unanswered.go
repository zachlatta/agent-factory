package factory

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Requests with no answer. The email dispatcher (not in this repo) keeps a ledger of every request
// Zach sends it, by email or voice memo, and checks that each got an answer in its thread. It writes
// the ones that did not to a JSON file (Config.Unanswered, a host setting); the dispatcher is that
// file's only writer. The factory reads it into Sections.NoAnswer, the one list of unanswered
// requests the board and the daily digest show, and serves it at GET /api/unanswered. Zach dismisses
// one on the board; dismissals live in the factory's own store (dismissed-requests.json in the state
// directory), never in the dispatcher's file, and hold for as long as the dispatcher lists the
// request. A card's noReply timing is a metric only.

// UnansweredRequest is one request the dispatcher flagged.
type UnansweredRequest struct {
	Key     string `json:"key"`     // the dispatcher's id for the request (its message, or its voice memo)
	Source  string `json:"source"`  // email or voice
	Thread  string `json:"thread"`  // the email thread it was asked in
	Asked   string `json:"asked"`   // RFC 3339
	Subject string `json:"subject"` // what Zach asked, in a line
	Why     string `json:"why"`     // why it counts as unanswered, in plain words
}

type unansweredFile struct {
	Updated  string              `json:"updated"`
	Requests []UnansweredRequest `json:"requests"`
}

// DismissStore is the factory's list of dismissed request keys, with when each was dismissed.
type DismissStore struct {
	path string
	mu   sync.Mutex
}

func (s *DismissStore) load() map[string]string {
	m := map[string]string{}
	readJSON(s.path, &m)
	return m
}

// add records key dismissed at at. A key the dispatcher no longer lists (not in listed) is dropped a
// month after its dismissal; a listed one is kept however old, so it never comes back.
func (s *DismissStore) add(key string, at time.Time, listed map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	m[key] = iso(at)
	for k, v := range m {
		if t := parseTime(v); !listed[k] && !t.IsZero() && at.Sub(t) > 30*24*time.Hour {
			delete(m, k)
		}
	}
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

// unanswered is the dispatcher's flagged requests not dismissed here, oldest first; none when the
// file is not set, missing or unreadable.
func (f *Factory) unanswered() []UnansweredRequest {
	if f.cfg.Unanswered == "" {
		return nil
	}
	var file unansweredFile
	readJSON(f.cfg.Unanswered, &file)
	f.dismissed.mu.Lock()
	gone := f.dismissed.load()
	f.dismissed.mu.Unlock()
	out := []UnansweredRequest{}
	for _, r := range file.Requests {
		if r.Key == "" || gone[r.Key] != "" {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Asked < out[j].Asked })
	return out
}

// noAnswerLines is the NoAnswerSource for the dispatcher's requests: each line's state says why it has
// no answer, in the dispatcher's plain words when it gives them; its key stays out of the text.
func (f *Factory) noAnswerLines(time.Time) []SectionLine {
	var out []SectionLine
	for _, r := range f.unanswered() {
		state := "no answer yet"
		if r.Source == "voice" {
			state = "voice memo, no answer yet"
		}
		if why := strings.Join(strings.Fields(r.Why), " "); why != "" {
			state += ": " + why
		}
		out = append(out, SectionLine{Key: r.Key, Title: r.Subject, State: state, At: r.Asked})
	}
	return out
}

// dismissRequest hides a flagged request from the board and the digest for good.
func (f *Factory) dismissRequest(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("no request given")
	}
	if f.cfg.Unanswered == "" {
		return errors.New("unknown request")
	}
	var file unansweredFile
	readJSON(f.cfg.Unanswered, &file)
	listed := map[string]bool{}
	for _, r := range file.Requests {
		listed[r.Key] = true
	}
	for _, r := range f.unanswered() {
		if r.Key == key {
			return f.dismissed.add(key, f.Now(), listed)
		}
	}
	return errors.New("unknown request")
}

func (f *Factory) handleUnanswered(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"requests": f.unanswered()})
}
