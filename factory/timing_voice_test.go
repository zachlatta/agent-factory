package factory

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestVoiceReplySelection(t *testing.T) {
	filing := msg("filing", t0.Add(-119*time.Minute), true)
	early := msg("early", t0.Add(-50*time.Minute), true)
	late := msg("late", t0.Add(-20*time.Minute), true)
	for _, tc := range []struct {
		name     string
		messages []threadMsg
		want     time.Time
	}{
		{"filing only", []threadMsg{filing}, time.Time{}},
		{"first answer", []threadMsg{filing, early, late}, t0.Add(-50 * time.Minute)},
		{"unordered answers", []threadMsg{filing, late, early}, t0.Add(-50 * time.Minute)},
		{"cutoff accepted", []threadMsg{filing, msg("boundary", t0.Add(-5*time.Minute), true)}, t0.Add(-5 * time.Minute)},
		{"past cutoff", []threadMsg{filing, msg("outside", t0.Add(-5*time.Minute+time.Second), true)}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := replyCard(t, tc.messages)
			f.timings.cards["e1"].Source = "voice"
			f.checkReplies(t0)
			got := timing(t, f, "e1")
			if tc.want.IsZero() && metrics(f.timings.all(), t0, 7).Excluded.NoReply != 1 {
				t.Fatal("unanswered voice included in metrics")
			}
			view := got.view(t0)
			exported := f.timings.export().Cards[0]
			if view.NoReply != got.NoReply || exported.NoReply != got.NoReply || !exported.RepliedAt.Equal(got.RepliedAt) {
				t.Fatal("inconsistent presentation")
			}
			end := got.LandedAt
			if !tc.want.IsZero() {
				end = tc.want
			}
			if view.TotalSecs != secs(end.Sub(got.RequestedAt)) {
				t.Fatal("incorrect duration")
			}
			if !got.RepliedAt.Equal(tc.want) || got.NoReply != tc.want.IsZero() {
				t.Fatalf("reply %v noReply %v", got.RepliedAt, got.NoReply)
			}
		})
	}
}

func TestVoiceReplyRecheck(t *testing.T) {
	f, m := replyCard(t, []threadMsg{msg("filing", t0.Add(-119*time.Minute), true)})
	old := f.timings.cards["e1"]
	old.Source, old.ReplyChecked, old.ReplyTries = "voice", true, replyTries
	old.RepliedAt = t0.Add(-119 * time.Minute)
	f.reconcileTimings()
	got := timing(t, f, "e1")
	if got.ReplyChecked || !got.RepliedAt.IsZero() || got.ReplyTries != 0 {
		t.Fatalf("stale reply retained: %+v", got)
	}
	m.errs["get_thread"] = errors.New("unavailable")
	f.checkReplies(t0)
	f.reconcileTimings()
	if timing(t, f, "e1").ReplyTries != 1 {
		t.Fatal("invalidation repeated")
	}
	delete(m.errs, "get_thread")
	f.checkReplies(t0.Add(replyWait))
	got = timing(t, f, "e1")
	if !got.NoReply || !got.RepliedAt.IsZero() {
		t.Fatalf("filing retained: %+v", got)
	}
	restarted := newTimingStore(f.timings.path, f.Now, time.UTC)
	saved, _ := restarted.get("e1")
	if !saved.ReplyChecked || !saved.NoReply {
		t.Fatal("correction not persisted")
	}
}

func TestReplyRecipients(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, cc string
		self               bool
	}{
		{"self", "Bot <bot@example.com>", "bot@example.com", "", true},
		{"mixed", "bot@example.com", "bot@example.com", "reader@example.com", false},
		{"missing", "", "", "", false},
		{"malformed", "bot@example.com", "not an address", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := msg("answer", t0.Add(-30*time.Minute), true)
			m.From, m.To, m.Cc = tc.from, tc.to, tc.cc
			if m.onlyToSelf() != tc.self {
				t.Fatal("recipient classification")
			}
			for _, source := range []string{"email", "voice"} {
				f, _ := replyCard(t, []threadMsg{msg("filing", t0.Add(-119*time.Minute), false), m})
				f.timings.cards["e1"].Source = source
				f.checkReplies(t0)
				if timing(t, f, "e1").RepliedAt.IsZero() != tc.self {
					t.Fatal("reply selection")
				}
			}
		})
	}
}

func TestVoiceRecheckReadOnly(t *testing.T) {
	f, m := replyCard(t, nil)
	f.timings.cards["e1"].Source = "voice"
	f.timings.cards["e1"].ReplyChecked = true
	f.cfg.DryRun = true
	f.reconcileTimings()
	f.checkReplies(t0)
	if !timing(t, f, "e1").ReplyChecked || gets(m) != 0 {
		t.Fatal("read-only correction")
	}
}

func TestReplyRawHeaders(t *testing.T) {
	for _, raw := range []string{
		`{"from":"bot@example.com","to":"bot@example.com"}`,
		`{"From":"Bot <bot@example.com>","To":"bot@example.com"}`,
		`[{"name":"From","value":"bot@example.com"},{"name":"To","value":"bot@example.com"}]`,
	} {
		var m threadMsg
		if err := json.Unmarshal([]byte(`{"headers":`+raw+`}`), &m); err != nil {
			t.Fatal(err)
		}
		if !m.onlyToSelf() {
			t.Fatal("raw self recipients missed")
		}
	}
}

func TestVoiceFactoryNoticeAndRecheckRestart(t *testing.T) {
	filing := msg("filing", t0.Add(-119*time.Minute), true)
	notice := msg("notice", t0.Add(-30*time.Minute), true)
	f, m := replyCard(t, []threadMsg{filing, notice})
	ct := f.timings.cards["e1"]
	ct.Source, ct.ReplyChecked, ct.RepliedAt = "voice", true, t0.Add(-119*time.Minute)
	ct.Version, ct.LandingChecked = timingVersion, true
	f.timings.cards["other"] = &CardTiming{Thread: "th1", Mails: []FactoryMail{{ID: "notice", At: t0.Add(-30 * time.Minute)}}}
	f.reconcileTimings()
	f.timings = newTimingStore(f.timings.path, f.Now, time.UTC)
	f.reconcileTimings()
	f.checkReplies(t0)
	got := timing(t, f, "e1")
	if !got.NoReply || !got.RepliedAt.IsZero() || !got.LandedAt.Equal(ct.LandedAt) {
		t.Fatal("incorrect correction")
	}
	for _, c := range m.calls {
		if c.Tool == "get_thread" {
			b, _ := json.Marshal(c.Args)
			var args map[string]any
			json.Unmarshal(b, &args)
			if args["include_headers"] != true {
				t.Fatal("headers not requested")
			}
		}
	}
	f.timings = newTimingStore(f.timings.path, f.Now, time.UTC)
	f.reconcileTimings()
	f.checkReplies(t0.Add(time.Hour))
	if gets(m) != 1 {
		t.Fatal("completed recheck repeated")
	}
}
