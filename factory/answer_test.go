package factory

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zachlatta/agent-factory/factory/scan"
)

func TestParseAnswer(t *testing.T) {
	qs := []Question{{Text: "Which store?", Options: []string{"Postgres", "SQLite"}},
		{Text: "When?", Options: []string{"Now", "Later"}}}
	plan := &Ask{Yes: "approve", Actions: []Action{primary("approve", "Approve plan & build"),
		reply("Send back", ""), act("close", "Drop it")}, Questions: qs}
	brief := &Ask{Yes: "recommend", Actions: []Action{primary("recommend", btnRecommend), act("close", btnDrop)}}
	rerun := &Ask{Actions: []Action{primary("rerun", "Re-run"), reply("Reply", "")}}
	for _, tc := range []struct {
		ask        *Ask
		text, want string
		msg        string // substring of the message, if any
	}{
		{plan, "Approve.", "approve", ""},
		{plan, "LGTM!", "approve", ""},
		{plan, "  go   ahead ", "approve", ""},
		{brief, "yes", "recommend", ""},
		{rerun, "yes", "send", "yes"},
		{plan, "drop it", "close", ""},
		{rerun, "drop", "send", "drop"},
		{plan, "ok, but drop step 3", "send", "ok, but drop step 3"},
		{plan, "yes to 1A, no to 2", "send", "yes to 1A"},
		{plan, "Please drop the second point and build", "send", "drop the second point"},
		{plan, "1B 2A", "approve", "Which store?\n   → SQLite"},
		{plan, "1b, 2b", "approve", "When?\n   → Later"},
		{brief, "1A", "send", "1A"}, // no questions on the brief
		{plan, "3C", "send", "3C"},
		{plan, "wait\n\n> approve", "send", "wait"},
		{plan, "approve\n\nOn Sun, Oct 25, 2026 at 7:42 PM zrl bot wrote:\n> Needs you", "approve", ""},
		{plan, "approve\n-- \nZach", "approve", ""},
		{plan, "Approve\n\nSent from my iPhone", "approve", ""},
		{plan, "  \n> approve", "", ""},
	} {
		got := parseAnswer(tc.text, tc.ask)
		if got.Action != tc.want || !strings.Contains(got.Message, tc.msg) {
			t.Errorf("parseAnswer(%q) = %+v, want %s with %q", tc.text, got, tc.want, tc.msg)
		}
		if got.Action == "send" && strings.Contains(got.Message, ">") {
			t.Errorf("parseAnswer(%q) kept quoted text: %q", tc.text, got.Message)
		}
	}
}

func planWaiting(id string) Agent {
	return card(id, map[string]string{labelStage: "zach", labelAwaiting: "plan", labelNote: "architecture: approve the plan to build",
		labelZachThread: "nt1"})
}

func TestEmailApproveBuildsAndConfirms(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "Approve."})
	if err != nil || res.(map[string]string)["action"] != "approve" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	l := p.labels("c1")
	if l[labelStage] != "building" || l[labelAnswered] != "m2" || l[labelZachPlan] != "approved" {
		t.Errorf("labels = %v", l)
	}
	if msgs := p.sent("c1"); len(msgs) != 1 || !strings.Contains(msgs[0], "Zach approved your plan") {
		t.Errorf("sent = %v", msgs)
	}
	s := m.sends()
	if len(s) != 1 || s[0].Tool != "reply_to_message" || s[0].Args["message_id"] != "m2" ||
		s[0].Args["text"] != "Approved: building now." || s[0].Args["html"] == nil {
		t.Errorf("confirmation = %+v", s)
	}
}

func TestEmailAnswerRepeatedIsIgnored(t *testing.T) {
	a := withLabels(planWaiting("c1"), map[string]string{labelAnswered: "m2"})
	f, p, m := mailFactory(t, a)
	res, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "approve"})
	if err != nil || res.(map[string]string)["action"] != "duplicate" {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if len(p.sent("c1")) != 0 || len(m.sends()) != 0 {
		t.Error("a repeated answer acted again")
	}
}

func TestEmailFreeTextGoesToTheAgentAsAFollowUp(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "ok, but drop step 3\n\n> old"}); err != nil {
		t.Fatal(err)
	}
	msgs := p.sent("c1")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "wants changes") || !strings.Contains(msgs[0], followUpPrefix) ||
		!strings.Contains(msgs[0], "ok, but drop step 3\n"+emailEnd) {
		t.Fatalf("sent = %v", msgs)
	}
	if fus := followUpsOf(msgs[0]); len(fus) != 1 || fus[0].Text != "ok, but drop step 3" {
		t.Errorf("follow-ups = %+v", fus)
	}
	if l := p.labels("c1"); l[labelStage] != "plan" {
		t.Errorf("labels = %v", l)
	}
	if s := m.sends(); len(s) != 1 || s[0].Args["text"] != "Sent back: the agent is revising the plan." {
		t.Errorf("confirmation = %+v", s)
	}
}

func TestEmailDropClosesTheCard(t *testing.T) {
	f, p, m := mailFactory(t, planWaiting("c1"))
	if _, err := f.Act("answer", Body{Thread: "nt1", MessageID: "m2", Text: "Drop it"}); err != nil {
		t.Fatal(err)
	}
	if !p.called("archive", "c1") {
		t.Error("not archived")
	}
	if s := m.sends(); len(s) != 1 || !strings.HasPrefix(s[0].Args["text"].(string), "Dropped") {
		t.Errorf("confirmation = %+v", s)
	}
}

func TestEmailApproveOnPublicRepoStillScans(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelNote: scanPassedNote, labelVerdict: "code approve",
		labelPlanRounds: "1"})
	a.Labels[gmailThreadLabel] = "t9"
	a.Repo = "zachlatta/agent-factory"
	f, p, m := mailFactory(t, a)
	f.Scan = func(context.Context, string, string) ([]scan.Hit, error) {
		return []scan.Hit{{Rule: "email", File: "x.go", Line: 1}}, nil
	}
	_, err := f.Act("answer", Body{Thread: "t9", MessageID: "m2", Text: "approve"})
	if err == nil || !strings.Contains(err.Error(), "secret scan failed") {
		t.Fatalf("a scan hit must stop an emailed approve: %v", err)
	}
	if l := p.labels("c1"); l[labelStage] == "landing" {
		t.Errorf("landed past a scan hit: %v", l)
	}
	if s := m.sends(); len(s) != 1 || !strings.HasPrefix(s[0].Args["text"].(string), "That didn't go through") {
		t.Errorf("confirmation = %+v", s)
	}
}

func TestAnswerAPIStatusTellsTheDispatcherToFallBack(t *testing.T) {
	building := card("c2", map[string]string{labelStage: "building", labelZachThread: "nt2"})
	f, _, _ := mailFactory(t, planWaiting("c1"), building)
	h := f.Handler()
	for thread, want := range map[string]int{"nope": 404, "nt2": 409} {
		req := httptest.NewRequest("POST", "/api/answer", strings.NewReader(`{"thread":"`+thread+`","message_id":"m1","text":"approve"}`))
		req.Header.Set("X-Factory", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("thread %s: status %d, want %d (%s)", thread, w.Code, want, w.Body)
		}
	}
	if !errors.Is(func() error { _, err := f.Act("answer", Body{Thread: "nope", MessageID: "m"}); return err }(), ErrNoCard) {
		t.Error("unknown thread is not ErrNoCard")
	}
}
