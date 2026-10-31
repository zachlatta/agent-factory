package factory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppTimingCollectsDelayedRequest(t *testing.T) {
	a := Agent{ID: "app", CreatedAt: t0.Add(-time.Minute)}
	s := newTimingStore(filepath.Join(t.TempDir(), "timings.json"), func() time.Time { return t0 }, time.UTC)
	cards := []Card{{ID: a.ID, Stage: "plan"}}
	s.observe(cards, []Agent{a}, 1, t0, nil)
	if _, ok := s.get(a.ID); !ok {
		t.Fatal("app not timed")
	}
	if len(s.needsAsked([]Agent{a})) != 1 {
		t.Fatal("missing prompt not retried")
	}
	s.observe(cards, []Agent{a}, 2, t0, map[string]CardRequest{a.ID: {Base: "Synthetic request", Read: true}})
	ct, _ := s.get(a.ID)
	if ct.Request != "Synthetic request" || ct.Source != "app" || !ct.RequestedAt.Equal(a.CreatedAt) {
		t.Fatalf("%+v", ct)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s = newTimingStore(s.path, func() time.Time { return t0 }, time.UTC)
	ct, _ = s.get(a.ID)
	if ct.Request != "Synthetic request" {
		t.Fatal("request lost on restart")
	}
	b, _ := json.Marshal(s.export())
	if strings.Contains(string(b), "Synthetic request") {
		t.Fatal("request exported")
	}
}

func TestTimingNormalizesLocalClock(t *testing.T) {
	local := t0.In(time.FixedZone("offset", -4*60*60))
	a := fresh("c")
	f, _ := newTestFactory(t, a)
	f.Now = func() time.Time { return local.Add(time.Minute) }
	f.recordRound("c", "plan", 1, local, "approve")
	f.SetAgents([]Agent{moved(a, "zach", "")})
	f.timings.update("c", func(ct *CardTiming) { ct.ReplyRetry = local; ct.Mails = []FactoryMail{{At: local}} })
	ct := timing(t, f, "c")
	b, _ := json.Marshal(ct)
	if strings.Contains(string(b), "-04:00") {
		t.Fatalf("offset persisted: %s", b)
	}
	if ct.Rounds[0].Start.Location() != time.UTC || ct.Rounds[0].End.Location() != time.UTC {
		t.Fatal("round not UTC")
	}
}

func TestAppPartialHistoryAndCompletionReset(t *testing.T) {
	a := Agent{ID: "app", CreatedAt: t0.Add(-time.Hour)}
	s := newTimingStore("", func() time.Time { return t0 }, time.UTC)
	s.observe([]Card{{ID: "app", Stage: "landed"}}, []Agent{a}, 1, t0, nil)
	ct, ok := s.get("app")
	if !ok || !ct.Partial || len(ct.Events) != 1 || ct.Events[0].Stage != "landed" {
		t.Fatalf("%+v", ct)
	}
	s.cards["app"].LandingChecked = true
	s.cards["app"].ReplyChecked = true
	// A replacement resumes the retained timeline and must permit fresh completion checks.
	b := a
	b.ID = "next"
	s.observe([]Card{{ID: "next", Stage: "building", Replaces: []string{"app"}}}, []Agent{b}, 2, t0.Add(time.Minute), nil)
	ct, ok = s.get("next")
	if !ok || ct.LandingChecked || ct.ReplyChecked || len(ct.Events) != 2 {
		t.Fatalf("%+v", ct)
	}
}
