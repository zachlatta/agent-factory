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
		{"no line", "Landed abc1234.", "zach", "landed; did not say what remains"},
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
	if l := p.labels("c1"); l[labelStage] != "zach" || l[labelNote] != "landed; did not say what remains" {
		t.Errorf("labels = %v", l)
	}
}
