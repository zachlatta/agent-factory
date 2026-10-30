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
	round := inboxCard("r", map[string]string{gmailThreadLabel: "t1", labelWorkLeftRounds: "1"})
	if !strings.Contains(f.landMsgFor(mail), "reply_to_message") || strings.Contains(f.landMsgFor(round), "reply_to_message") {
		t.Error("only an email card's first landing tells Zach by email")
	}
	for name, s := range map[string]string{
		"landMsgFor":         f.landMsgFor(card("c", map[string]string{})),
		"landMsgFor guarded": f.landMsgFor(pub),
		"landMsgFor email":   f.landMsgFor(mail),
		"landMsgFor round":   f.landMsgFor(round),
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
			storeLog(t, f, p, "c1", tc.reply)
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
	storeLog(t, f, p, "c1", fmt.Sprintf("[User] Code review passed (low tier). %s\n[Shell] bin/land\nLanded it.", f.landMsgFor(c)))
	f.Tick()
	if l := p.labels("c1"); l[labelStage] != "landing" || l[labelNote] != "asking what remains" {
		t.Errorf("labels = %v", l)
	}
}

func TestLandedNext(t *testing.T) {
	const nothing = "DECISION: None. It is landed and live.\nOVERVIEW: The fix is on main.\nNEEDS FROM ZACH: Nothing.\n\n"
	for _, tc := range []struct {
		name, reply string
		rounds      int
		stage, note string
		again       []string
	}{
		{"none", nothing + "REMAINING: none", 0, "landed", "landed", nil},
		{"no line", nothing + "Landed.", 0, "zach", "landed; did not say what remains", nil},
		{"nothing needed, agent work left", nothing + "REMAINING:\n- agent: the phone-width check, was not run", 0,
			"building", "finishing work left (round 1 of 2)", []string{"the phone-width check, was not run"}},
		{"second round", nothing + "REMAINING:\n- agent: rerun the check", 1,
			"building", "finishing work left (round 2 of 2)", []string{"rerun the check"}},
		{"cap reached", nothing + "REMAINING:\n- agent: rerun the check", 2,
			"zach", "landed; still to do: rerun the check", nil},
		{"nothing now", "DECISION: Stopped.\nNEEDS FROM ZACH: Nothing now.\n\nREMAINING:\n- retry the drill", 0,
			"building", "finishing work left (round 1 of 2)", []string{"retry the drill"}},
		{"nothing for this fix", "DECISION: Landed.\nNEEDS FROM ZACH: nothing for this fix.\n\nREMAINING: confirm the land prompt", 0,
			"building", "finishing work left (round 1 of 2)", []string{"confirm the land prompt"}},
		{"none in bold", "**DECISION:** Landed.\n**NEEDS FROM ZACH:** **None.**\n\nREMAINING:\n- Agent: rerun the check", 0,
			"building", "finishing work left (round 1 of 2)", []string{"rerun the check"}},
		{"a real ask", "DECISION: Save the admin login.\nNEEDS FROM ZACH: Save the admin login in the vault.\n\nREMAINING:\n- agent: rerun the videos", 0,
			"zach", "landed; still to do: agent: rerun the videos", nil},
		{"approve ask", "DECISION: Approve X.\nNEEDS FROM ZACH: approve X.\n\nREMAINING:\n- agent: rerun the videos", 0,
			"zach", "landed; still to do: agent: rerun the videos", nil},
		{"nothing now but a login", "DECISION: Landed.\nNEEDS FROM ZACH: Nothing now, but please log in to the admin page.\n\nREMAINING:\n- rerun the videos", 0,
			"zach", "landed; still to do: rerun the videos", nil},
		{"a Zach item", nothing + "REMAINING:\n- agent: rerun the check\n- Zach: set the billing limit", 0,
			"zach", "landed; still to do: agent: rerun the check; Zach: set the billing limit", nil},
		{"Zach items only", nothing + "REMAINING:\n- Zach: set the billing limit", 0,
			"zach", "landed; still to do: Zach: set the billing limit", nil},
		{"a question", nothing + "QUESTION: Rerun it now?\n- Yes (recommended)\n- No\n\nREMAINING:\n- agent: rerun it", 0,
			"zach", "landed; still to do: agent: rerun it", nil},
		{"no overview", "Landed.\nREMAINING:\n- rerun the check", 0, "zach", "landed; still to do: rerun the check", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, note, again := landedNext(tc.reply, tc.rounds)
			if stage != tc.stage || note != tc.note || !reflect.DeepEqual(again, tc.again) {
				t.Errorf("landedNext = %q, %q, %q; want %q, %q, %q", stage, note, again, tc.stage, tc.note, tc.again)
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

// A landing that needs nothing from Zach but lists agent work goes back to the same card: it finishes the
// work, gets a code review for new commits, and lands again. No idea is filed. A card Zach asked for
// stops at his finished-work stop again when the round made new commits.
func TestWorkLeftGoesBackToTheCard(t *testing.T) {
	const reply = "DECISION: None. It is landed and live.\nNEEDS FROM ZACH: Nothing.\n\nLanded abc1234.\nREMAINING:\n- agent: the phone-width check was not run"
	for _, commits := range []string{"", "def5678\n"} {
		t.Run(fmt.Sprintf("commits %q", commits), func(t *testing.T) {
			c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelTier: "architecture",
				labelPlanRounds: "1", labelCodeRounds: "1"})
			c.Cwd = t.TempDir()
			f, p := newTestFactory(t, c)
			f.Git = func(context.Context, string, ...string) error { return nil }
			storeLog(t, f, p, "c1", reply)
			f.Tick()
			l := p.labels("c1")
			if l[labelStage] != "building" || l[labelAutoReview] != "code" || l[labelWorkLeftRounds] != "1" ||
				l[labelNote] != "finishing work left (round 1 of 2)" || l[labelLandedVerified] != "1" {
				t.Fatalf("labels = %v", l)
			}
			sent := strings.Join(p.sent("c1"), "")
			if !strings.Contains(sent, "the phone-width check was not run") || !strings.Contains(sent, "round 1 of 2") ||
				!strings.HasSuffix(sent, remainingRule) {
				t.Fatalf("sent = %q", sent)
			}
			if n := len(f.ideas.List()); n != 0 {
				t.Fatalf("ideas = %d, want none", n)
			}
			// the agent finishes the round
			for k, v := range l {
				c.Labels[labelPrefix+k] = v
			}
			now := f.Now().Add(2 * time.Minute)
			c.LastActivityAt = now.Add(-time.Minute)
			f.Now = func() time.Time { return now }
			f.GitOut = func(_ context.Context, _ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "origin/main..HEAD") {
					return commits, nil
				}
				return "", nil
			}
			headIs(f, reviewedSHA, nil)
			storeLog(t, f, p, "c1", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nRan the phone-width check.\nREMAINING: none")
			f.SetAgents([]Agent{c})
			f.Tick()
			if commits == "" {
				if l := p.labels("c1"); l[labelStage] != "landed" || p.runs != 0 {
					t.Fatalf("no-commit round: labels = %v, reviews = %d", l, p.runs)
				}
				return
			}
			// an architecture card's code review is a panel of two (panel.go)
			if p.runs != 2 || len(p.runsWithLabel(labelPrefix+labelLens+"="+lensCorrectness)) != 1 {
				t.Fatalf("new commits: reviews started = %d, want a panel of 2", p.runs)
			}
			// code review passes: a card Zach asked for waits for him before it lands again
			rev := reviewer("r1", c.ID, "code")
			for k, v := range p.labels("c1") {
				c.Labels[labelPrefix+k] = v
			}
			f.SetAgents([]Agent{c, rev})
			storeLog(t, f, p, "r1", "TIER: architecture\nVERDICT: approve")
			if err := f.ApplyVerdict(f.agent("r1")); err != nil {
				t.Fatal(err)
			}
			if l := p.labels("c1"); l[labelStage] != "zach" || !isReadyNote(l[labelNote]) {
				t.Fatalf("after review: labels = %v", l)
			}
		})
	}
}

// After workLeftCap rounds, agent work left waits on the board, and the count is not reset.
func TestWorkLeftCapWaitsOnTheBoard(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelWorkLeftRounds: "2"})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	storeLog(t, f, p, "c1", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nREMAINING:\n- agent: rerun the check")
	f.Tick()
	l := p.labels("c1")
	if l[labelStage] != "zach" || l[labelNote] != "landed; still to do: rerun the check" {
		t.Fatalf("labels = %v", l)
	}
	if v, set := l[labelWorkLeftRounds]; set {
		t.Errorf("round count changed to %q", v)
	}
	if len(p.sent("c1")) != 0 {
		t.Errorf("sent = %v", p.sent("c1"))
	}
}

func TestLandedClearsStuckMark(t *testing.T) {
	c := card("c1", map[string]string{labelStage: "landing", labelSince: ago(10 * time.Minute), labelStuck: stuckLanding})
	c.Cwd = t.TempDir()
	f, p := newTestFactory(t, c)
	f.Git = func(context.Context, string, ...string) error { return nil }
	storeLog(t, f, p, "c1", "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nLanded.\nREMAINING: none")
	if err := f.landed(c, Card{ID: c.ID, Cwd: c.Cwd}, "DECISION: None.\nNEEDS FROM ZACH: Nothing.\n\nLanded.\nREMAINING: none"); err != nil {
		t.Fatal(err)
	}
	l := p.labels("c1")
	if v, ok := l[labelStuck]; !ok || v != "" || l[labelStage] != "landed" {
		t.Fatalf("labels = %v", l)
	}
}
