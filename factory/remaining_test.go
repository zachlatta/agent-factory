package factory

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRemaining(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		items      []string
		found      bool
	}{
		{"none", "Landed abc1234.\nREMAINING: none", nil, true},
		{"none with a full stop", "Landed.\nRemaining: None.", nil, true},
		{"bold", "Landed.\n**REMAINING:** none", nil, true},
		{"one item on the line", "Landed push 1.\nREMAINING: push 2 is not landed", []string{"push 2 is not landed"}, true},
		{"list", "Landed.\nREMAINING:\n- push 2 is not landed\n* check production\n\nThanks.",
			[]string{"push 2 is not landed", "check production"}, true},
		{"numbered", "REMAINING:\n1. set the env var\n2) restart the unit", []string{"set the env var", "restart the unit"}, true},
		{"line and list", "REMAINING: two things\n- a\n- b", []string{"two things", "a", "b"}, true},
		{"last line wins", "REMAINING: x\nlater\nREMAINING: none", nil, true},
		{"empty list", "REMAINING:\n\nDone.", nil, false},
		{"missing", "Landed it, all good.", nil, false},
		{"mid-sentence is not the line", "End with a line REMAINING: none when done.", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, found := ParseRemaining(tc.text)
			if found != tc.found || !reflect.DeepEqual(items, tc.items) {
				t.Errorf("ParseRemaining = %q, %v; want %q, %v", items, found, tc.items, tc.found)
			}
		})
	}
}

// Every landing message ends with the rule, so the agent's last line says what is left.
func TestLandingMessagesAskWhatRemains(t *testing.T) {
	f := &Factory{cfg: Config{Guarded: []string{"example/public"}}}
	pub := card("p", map[string]string{})
	pub.Repo = "example/public"
	mail := inboxCard("m", map[string]string{gmailThreadLabel: "t1"})
	for name, s := range map[string]string{
		"landMsgFor":         f.landMsgFor(card("c", map[string]string{})),
		"landMsgFor guarded": f.landMsgFor(pub),
		"landMsgFor email":   f.landMsgFor(mail),
		"rerun":              rerunLandMsg(false),
		"rerun guarded":      rerunLandMsg(true),
	} {
		if !strings.HasSuffix(s, remainingRule) {
			t.Errorf("%s does not end with the REMAINING rule: %q", name, s)
		}
		if !strings.Contains(s, "this card's own request") || !strings.Contains(s, "another card") {
			t.Errorf("%s does not limit REMAINING to this card's own work: %q", name, s)
		}
		if strings.Contains(s, "\n") {
			t.Errorf("%s spans lines: the log fallback could read the rule as the agent's answer", name)
		}
	}
}

func TestLandingReadsWhatRemains(t *testing.T) {
	for _, tc := range []struct {
		name, reply, stage, note string
	}{
		{"none", "Landed abc1234.\nREMAINING: none", "landed", "landed"},
		{"items", "Landed push 1.\nREMAINING:\n- push 2, not landed\n- check production",
			"zach", "landed; still to do: push 2 not landed; check production"},
		{"no line", "Landed abc1234.", "landing", "asking what remains"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute)})
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			p.logs["c1"] = tc.reply
			f.Tick()
			if l := p.labels("c1"); l[labelStage] != tc.stage || l[labelNote] != tc.note {
				t.Errorf("labels = %v", l)
			}
			if l := p.labels("c1"); l[labelLandedVerified] != "1" {
				t.Errorf("verified landing not recorded: %v", l)
			}
			if p.called("archive", "c1") {
				t.Error("card archived")
			}
		})
	}
}

// When the transcript cannot be read, the log fallback reads past the landing message: the rule's
// own "REMAINING: none" is never taken for the agent's answer.
func TestLandingRuleIsNotTheAnswer(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute)})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	p.logs["c1"] = fmt.Sprintf("[User] Code review passed (low tier). %s\n[Shell] bin/land\nLanded it.", f.landMsgFor(c))
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelNote] != "asking what remains" {
		t.Errorf("labels = %v", l)
	}
}

func TestLandedNext(t *testing.T) {
	const nothing = "DECISION: None. It is landed and live.\nOVERVIEW: The fix is on main.\nNEEDS FROM ZACH: Nothing.\n\n"
	for _, tc := range []struct {
		name, reply, stage, note string
		followUp                 []string
	}{
		{"none", nothing + "REMAINING: none", "landed", "landed", nil},
		{"no line", nothing + "Landed.", "zach", "landed; did not say what remains", nil},
		{"nothing needed, agent work left", nothing + "REMAINING:\n- agent: the phone-width check, was not run",
			"landed", "landed; left for agents: the phone-width check was not run", []string{"the phone-width check, was not run"}},
		{"nothing now", "DECISION: Stopped.\nNEEDS FROM ZACH: Nothing now.\n\nREMAINING:\n- retry the drill",
			"landed", "landed; left for agents: retry the drill", []string{"retry the drill"}},
		{"nothing for this fix", "DECISION: Landed.\nNEEDS FROM ZACH: nothing for this fix.\n\nREMAINING: confirm the land prompt",
			"landed", "landed; left for agents: confirm the land prompt", []string{"confirm the land prompt"}},
		{"none in bold", "**DECISION:** Landed.\n**NEEDS FROM ZACH:** **None.**\n\nREMAINING:\n- Agent: rerun the check",
			"landed", "landed; left for agents: rerun the check", []string{"rerun the check"}},
		{"a real ask", "DECISION: Save the admin login.\nNEEDS FROM ZACH: Save the admin login in the vault.\n\nREMAINING:\n- rerun the videos",
			"zach", "landed; still to do: rerun the videos", nil},
		{"nothing now but a login", "DECISION: Landed.\nNEEDS FROM ZACH: Nothing now, but please log in to the admin page.\n\nREMAINING:\n- rerun the videos",
			"zach", "landed; still to do: rerun the videos", nil},
		{"a Zach item", nothing + "REMAINING:\n- agent: rerun the check\n- Zach: set the billing limit",
			"zach", "landed; still to do: agent: rerun the check; Zach: set the billing limit", nil},
		{"a question", nothing + "QUESTION: Rerun it now?\n- Yes (recommended)\n- No\n\nREMAINING:\n- rerun it",
			"zach", "landed; still to do: rerun it", nil},
		{"no overview", "Landed.\nREMAINING:\n- rerun the check", "zach", "landed; still to do: rerun the check", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, note, followUp := landedNext(tc.reply)
			if stage != tc.stage || note != tc.note || !reflect.DeepEqual(followUp, tc.followUp) {
				t.Errorf("landedNext = %q, %q, %q; want %q, %q, %q", stage, note, followUp, tc.stage, tc.note, tc.followUp)
			}
		})
	}
}

// The rule asks agents to run their own checks, name who does each leftover, and keep NEEDS FROM ZACH
// in step with it.
func TestRemainingRuleNamesWhoDoesWhatIsLeft(t *testing.T) {
	for _, want := range []string{"Zach:", "agent:", "NEEDS FROM ZACH", "Run every check"} {
		if !strings.Contains(remainingRule, want) {
			t.Errorf("remainingRule lacks %q: %s", want, remainingRule)
		}
	}
}

func TestLandingNeedsNothingGoesToDone(t *testing.T) {
	const reply = "DECISION: None. It is landed and live.\nNEEDS FROM ZACH: Nothing.\n\nLanded abc1234.\nREMAINING:\n- agent: the phone-width check was not run"
	for _, tc := range []struct {
		name, title string
		ideas       int
	}{
		{"files a follow-up", "Card summaries load fast", 1},
		{"a follow-up files none", "Follow up on landed abcdef1: Card summaries load fast", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelTier: "risky"})
			c.Cwd, c.Title = t.TempDir(), tc.title
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			p.logs["c1"] = reply
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != "landed" || l[labelNote] != "landed; left for agents: the phone-width check was not run" {
				t.Errorf("labels = %v", l)
			}
			ideas := f.ideas.List()
			if len(ideas) != tc.ideas {
				t.Fatalf("ideas = %+v", ideas)
			}
			if tc.ideas == 1 {
				i := ideas[0]
				if !strings.HasPrefix(i.Title, "Follow up on landed c1: ") || i.Tier != "risky" || i.Repo != "example/desk" ||
					!strings.Contains(i.Request, "the phone-width check was not run") {
					t.Errorf("idea = %+v", i)
				}
			}
		})
	}
}

// Cards that landed before this rule and wait in Needs Zach are read once: those that need nothing
// from Zach go to Done, the rest are marked checked and not read again.
func TestLandedCardsInNeedsZachAreCheckedOnce(t *testing.T) {
	needsNothing := "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nREMAINING:\n- the phone-width check was not run"
	realAsk := "DECISION: Save the login.\nNEEDS FROM ZACH: Save the admin login in the vault.\n\nREMAINING:\n- rerun the videos"
	waiting := func(id string) Agent {
		return card(id, map[string]string{labelStage: "zach", labelLandedVerified: "1", labelSince: ago(time.Hour),
			labelNote: "landed; still to do: x"})
	}
	free, ask := waiting("c1"), waiting("c2")
	parked := waiting("c3")
	parked.ArchivedAt, parked.Labels[labelPrefix+labelParked] = t0.Add(-time.Minute), "1"
	replied := card("c4", map[string]string{labelStage: "building", labelAutoReview: "code", labelLandedVerified: "1",
		labelSince: ago(time.Minute), labelNote: "landed; still to do: x"})
	replied.Status = "running"
	checked := waiting("c5")
	checked.Labels[labelPrefix+labelRemainingChecked] = "1"
	f, p := newTestFactory(t, free, ask, parked, replied, checked)
	for id, text := range map[string]string{"c1": needsNothing, "c2": realAsk, "c3": needsNothing, "c4": needsNothing, "c5": needsNothing} {
		storeLog(t, f, p, id, text)
	}
	f.Tick()
	for _, id := range []string{"c1", "c3"} {
		if got := p.labels(id)[labelStage]; got != "landed" {
			t.Errorf("%s stage = %q, want landed (%v)", id, got, p.labels(id))
		}
	}
	for _, id := range []string{"c4", "c5"} { // replied to, or read before: untouched
		if l := p.labels(id); len(l) != 0 {
			t.Errorf("%s changed: %v", id, l)
		}
	}
	if l := p.labels("c1"); l[labelNote] != "landed; left for agents: the phone-width check was not run" {
		t.Errorf("c1 note = %q", l[labelNote])
	}
	if l := p.labels("c2"); l[labelRemainingChecked] != "1" || l[labelStage] != "" || l[labelNote] != "" {
		t.Errorf("c2 labels = %v", l)
	}
	if p.called("send", "c3") {
		t.Error("parked card woken")
	}
	if v, ok := p.labels("c3")[labelParked]; !ok || v != "" {
		t.Errorf("c3 park not dropped: %v", p.labels("c3"))
	}
	if n := len(f.ideas.List()); n != 2 {
		t.Errorf("ideas = %d, want 2 (c1 and c3)", n)
	}
}
