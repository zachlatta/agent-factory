package factory

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDerive(t *testing.T) {
	arch := time.Now()
	cases := []struct {
		name string
		a    Agent
		want string
	}{
		{"archived", Agent{ArchivedAt: arch, Labels: map[string]string{labelPrefix + labelStage: "building"}}, "landed"},
		{"label wins", Agent{Status: "running", Labels: map[string]string{labelPrefix + labelStage: "review"}}, "review"},
		{"running", Agent{Status: "running"}, "building"},
		{"unread finish", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished"}, "zach"},
		{"seen and idle", Agent{Status: "idle"}, "landed"},
		{"permission wait", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "permission"}, "zach"},
		{"app error without attention flag", Agent{Status: "error"}, "zach"},
		{"closed session", Agent{Status: "closed", RequiresAttention: true, AttentionReason: "finished"}, "landed"},
		{"email agent answered by email", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished",
			Labels: map[string]string{"source": "mail-bot"}}, "landed"},
		{"scheduled run finished", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "finished",
			Labels: map[string]string{"paseo.schedule-id": "x"}}, "landed"},
		{"scheduled run blocked", Agent{Status: "idle", RequiresAttention: true, AttentionReason: "permission",
			Labels: map[string]string{"paseo.schedule-id": "x"}}, "zach"},
	}
	for _, c := range cases {
		if got := Derive(c.a); got != c.want {
			t.Errorf("%s: Derive = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildBoardFoldsChildren(t *testing.T) {
	now := t0
	parent := Agent{ID: "p", Title: "parent", Status: "running", LastActivityAt: now, Labels: map[string]string{}}
	sub := Agent{ID: "s", Title: "sub", Status: "idle", CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
		Labels: map[string]string{"paseo.parent-agent-id": "p"}}
	rev := Agent{ID: "r", Title: "rev", Status: "running", Labels: map[string]string{labelPrefix + labelRole: "reviewer", labelPrefix + labelReviewOf: "p", labelPrefix + labelReviewKind: "code"}}
	anchor := Agent{ID: "a", Labels: map[string]string{"role": "mail-anchor"}}
	sched := Agent{ID: "sc", Title: "Nightly check", Status: "running", LastActivityAt: now, Labels: map[string]string{"paseo.schedule-id": "x"}}
	cards := BuildBoard([]Agent{parent, sub, rev, anchor, sched}, nil, now)
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2 (parent, schedule)", len(cards))
	}
	p := cards[0]
	if p.ID != "p" || len(p.Children) != 2 || !p.Reviewing {
		t.Errorf("parent card = %+v", p)
	}
	// the board's sub-agent rows show when each started and last moved
	i := slices.IndexFunc(p.Children, func(k Child) bool { return k.ID == "s" })
	if i < 0 {
		t.Fatalf("no sub-agent child: %+v", p.Children)
	}
	if k := p.Children[i]; k.CreatedAt != iso(now.Add(-time.Hour)) || k.LastActivityAt != iso(now.Add(-time.Minute)) {
		t.Errorf("sub-agent child = %+v", k)
	}
	if cards[1].Source != "schedule" || cards[1].Tier != "routine" {
		t.Errorf("schedule card = %+v", cards[1])
	}
}

func TestBuildBoardFlags(t *testing.T) {
	now := t0
	stalled := Agent{ID: "x", Status: "running", LastActivityAt: now.Add(-30 * time.Minute), Labels: map[string]string{}}
	done := Agent{ID: "y", Status: "idle", LastActivityAt: now, Labels: map[string]string{}}
	cards := BuildBoard([]Agent{stalled, done}, nil, now)
	if len(cards[0].Flags) == 0 || cards[0].Flags[0] != "stalled 30 min" {
		t.Errorf("stalled flags = %v", cards[0].Flags)
	}
	if len(cards[1].Flags) == 0 || cards[1].Flags[0] != "not archived" {
		t.Errorf("done flags = %v", cards[1].Flags)
	}
}

func TestBuildBoardIncludesWaitingIdeas(t *testing.T) {
	ideas := []Idea{{ID: "idea-1", Title: "faster tests", Status: "waiting", Repo: "example/desk", By: "scout", CreatedAt: t0},
		{ID: "idea-2", Title: "done", Status: "started"}}
	cards := BuildBoard(nil, ideas, t0)
	if len(cards) != 1 || cards[0].Stage != "idea" || cards[0].ID != "idea-1" {
		t.Errorf("cards = %+v", cards)
	}
}

func TestBuildBoardIdeaViews(t *testing.T) {
	ideas := []Idea{
		{ID: "idea-new", Title: "second", Summary: "S2", Status: "waiting", CreatedAt: t0.Add(time.Minute)},
		{ID: "idea-old", Title: "first", Summary: "S1", Why: "W1", DoneWhen: "D1", Request: "long brief",
			By: "scout", Status: "waiting", CreatedAt: t0},
		{ID: "idea-run", Title: "running", Summary: "S3", Status: "started", Card: "c1", CreatedAt: t0},
		{ID: "idea-gone", Title: "gone", Status: "dismissed", Card: "", CreatedAt: t0},
	}
	agents := []Agent{
		{ID: "c1", Title: "running", Status: "running", LastActivityAt: t0, Labels: map[string]string{labelPrefix + labelOrigin: "idea"}},
		{ID: "c2", Title: "other", Status: "running", LastActivityAt: t0, Labels: map[string]string{}},
	}
	byID := map[string]Card{}
	for _, c := range BuildBoard(agents, ideas, t0) {
		byID[c.ID] = c
	}
	old := byID["idea-old"].Idea
	if old == nil || old.Summary != "S1" || old.Why != "W1" || old.DoneWhen != "D1" || old.By != "scout" || old.Queue != 1 {
		t.Errorf("idea-old view = %+v", old)
	}
	if v := byID["idea-new"].Idea; v == nil || v.Queue != 2 {
		t.Errorf("idea-new view = %+v", v)
	}
	if v := byID["c1"].Idea; v == nil || v.ID != "idea-run" || v.Summary != "S3" || v.Status != "started" || v.Queue != 0 {
		t.Errorf("started card view = %+v", v)
	}
	if byID["c2"].Idea != nil {
		t.Errorf("card not from an idea got %+v", byID["c2"].Idea)
	}
}

func TestStalledFactoryCardShowsInNeedsZach(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "building"})
	a.Status, a.LastActivityAt = "running", t0.Add(-30*time.Minute)
	c := BuildBoard([]Agent{a}, nil, t0)[0]
	if c.Stage != "zach" || c.Ask == nil || actionIDs(c.Ask) != "stop,reply" {
		t.Errorf("card = %+v ask = %+v", c, c.Ask)
	}
	// an unlabeled stalled agent stays in Building with its flag
	u := Agent{ID: "u", Status: "running", LastActivityAt: t0.Add(-30 * time.Minute), Labels: map[string]string{}}
	if c := BuildBoard([]Agent{u}, nil, t0)[0]; c.Stage != "building" || c.Ask != nil {
		t.Errorf("unlabeled card = %+v", c)
	}
	// a running factory card with recent activity stays put
	a.LastActivityAt = t0.Add(-time.Minute)
	if c := BuildBoard([]Agent{a}, nil, t0)[0]; c.Stage != "building" {
		t.Errorf("active card stage = %q", c.Stage)
	}
}

func TestBuildBoardShowsScoutsInIdeas(t *testing.T) {
	running := Agent{ID: "s1", Title: "Scout: ideas for example/desk", Status: "running", LastActivityAt: t0,
		Labels: map[string]string{labelPrefix + labelRole: "scout"}}
	finished := Agent{ID: "s2", Status: "closed", ArchivedAt: t0, LastActivityAt: t0,
		Labels: map[string]string{labelPrefix + labelRole: "scout"}}
	cards := BuildBoard([]Agent{running, finished}, nil, t0)
	if c := cards[0]; c.Source != "scout" || c.Stage != "idea" || c.Note != "looking for ideas" {
		t.Errorf("running scout = %+v, want it in Ideas", c)
	}
	if c := cards[1]; c.Stage != "landed" {
		t.Errorf("archived scout stage = %q, want landed", c.Stage)
	}
}

// Unread: the agent finished a turn Zach has not opened. Paseo never clears its finished
// attention, so factory.seen (unix seconds) marks the last look, compared with the agent's last
// incoming message: label writes move lastActivityAt, never lastUserMessageAt.
func TestBuildBoardUnread(t *testing.T) {
	msg := t0.Add(-10 * time.Minute)
	finished := func(status string, labels map[string]string) Agent {
		a := card("c", labels)
		a.Status, a.RequiresAttention, a.AttentionReason, a.LastUserMessageAt = status, true, "finished", msg
		return a
	}
	unix := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	noMsg := finished("idle", map[string]string{labelSeen: unix(t0.Add(-2 * time.Hour))}) // created at t0-1h
	noMsg.LastUserMessageAt = time.Time{}
	cases := []struct {
		name string
		a    Agent
		want bool
	}{
		{"finished, never seen", finished("idle", nil), true},
		{"seen after the last message", finished("idle", map[string]string{labelSeen: unix(msg.Add(time.Minute))}), false},
		{"seen in the second of the last message", finished("idle", map[string]string{labelSeen: unix(msg)}), false},
		{"messaged again after seen and finished", finished("idle", map[string]string{labelSeen: unix(msg.Add(-time.Minute))}), true},
		{"running: nothing new to read yet", finished("running", nil), false},
		{"bad seen value counts as unseen", finished("idle", map[string]string{labelSeen: "soon"}), true},
		{"no message: created after seen", noMsg, true},
		{"not finished", card("c", nil), false},
	}
	for _, c := range cases {
		if got := BuildBoard([]Agent{c.a}, nil, t0)[0].Unread; got != c.want {
			t.Errorf("%s: unread = %v, want %v", c.name, got, c.want)
		}
	}
	// other attention reasons keep their flag, seen or not
	perm := card("c", map[string]string{labelSeen: unix(t0)})
	perm.RequiresAttention, perm.AttentionReason = true, "permission"
	if c := BuildBoard([]Agent{perm}, nil, t0)[0]; c.Unread || !slices.Contains(c.Flags, "needs attention: permission") {
		t.Errorf("permission card = %+v", c)
	}
}

func newAppAgent(id string) Agent {
	a := card(id, nil)
	a.RequiresAttention, a.AttentionReason = true, "finished"
	return a
}

func TestAppCardFinishedWithReportMovesToDone(t *testing.T) {
	a := newAppAgent("app-done")
	f, p := newTestFactory(t, a)
	storeLog(t, f, p, a.ID, "[User] Backfill the warehouse events table\n[Assistant] Backfilled 42 rows and verified the counts.")
	f.SetAgents([]Agent{a})

	c := f.cards[0]
	if c.Stage != "landed" || c.Note != "finished in Paseo" || c.Ask != nil || c.Unread {
		t.Fatalf("card = %+v, want landed with finished in Paseo and no ask", c)
	}
	if c.Request == nil || c.Request.Base != "Backfill the warehouse events table" {
		t.Fatalf("card.Request = %+v, want user prompt", c.Request)
	}
	if !slices.Contains(c.Flags, "not archived") || slices.ContainsFunc(c.Flags, func(s string) bool { return strings.HasPrefix(s, "needs attention") }) {
		t.Fatalf("card.Flags = %v", c.Flags)
	}
	if got := p.labels(a.ID)[labelStage]; got != "" {
		t.Fatalf("factory.stage written to Paseo = %q, want empty", got)
	}

	s := f.sections()
	if got := sectionIDs(s.Shipped); !slices.Equal(got, []string{a.ID}) || s.Shipped[0].State != "shipped" {
		t.Fatalf("shipped = %+v, want [%s] as shipped", s.Shipped, a.ID)
	}
	if len(s.ForYou) != 0 || len(s.Stuck) != 0 {
		t.Fatalf("forYou = %+v, stuck = %+v, want both empty", s.ForYou, s.Stuck)
	}
	if got := sectionIDs(Finished(f.cards)); !slices.Equal(got, []string{a.ID}) {
		t.Fatalf("Finished = %v, want [%s]", got, a.ID)
	}

	// Personal Paseo sessions are never auto-archived by the factory reaper.
	f.reap(t0.Add(10 * time.Minute))
	if p.called("archive", a.ID) {
		t.Fatal("reap archived a personal Paseo session")
	}

	// If resumed in Paseo, the card returns to Building.
	running := a
	running.Status = "running"
	f.SetAgents([]Agent{running})
	if c := f.cards[0]; c.Stage != "building" {
		t.Fatalf("resumed card stage = %q, want building", c.Stage)
	}
}

func TestAppCardAskingQuestionStaysInForYou(t *testing.T) {
	for name, reply := range map[string]string{
		"structured question": "[User] Fix the deploy\n[Assistant] QUESTION: Which environment should I target?\n- staging\n- prod",
		"overview decision":   "[User] Fix the deploy\n[Assistant] DECISION: Pick staging or prod\nOVERVIEW:\n- Two targets exist\nNEEDS FROM ZACH: Reply with the target.",
		"trailing question":   "[User] Fix the deploy\n[Assistant] I checked both configs. Should I drop the legacy table?",
		"question with list":  "[User] Fix the deploy\n[Assistant] Which table should I backfill?\n- events_v1\n- events_v2",
		"mid-turn tool call":  "[User] Fix the deploy\n[Bash] ls",
	} {
		t.Run(name, func(t *testing.T) {
			a := newAppAgent("app-q")
			f, p := newTestFactory(t, a)
			storeLog(t, f, p, a.ID, reply)
			f.SetAgents([]Agent{a})
			c := f.cards[0]
			if c.Stage != "zach" || c.Ask == nil {
				t.Fatalf("%s: card = %+v, want stage zach with ask", name, c)
			}
		})
	}

	t.Run("overview with DECISION none moves to done", func(t *testing.T) {
		a := newAppAgent("app-none")
		f, p := newTestFactory(t, a)
		storeLog(t, f, p, a.ID, "[User] Check the service\n[Assistant] DECISION: none\nOVERVIEW:\n- Service is healthy.\nNEEDS FROM ZACH: none")
		f.SetAgents([]Agent{a})
		if c := f.cards[0]; c.Stage != "landed" {
			t.Fatalf("card stage = %q, want landed", c.Stage)
		}
	})
}

func writeToolSession(t *testing.T, home, id, toolName string, input map[string]any, resultText string, isErr bool) {
	t.Helper()
	u := sessionLine("Run the release check and email the result.", false)
	tc, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "u1", "name": toolName, "input": input},
	}}})
	tr, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "u1", "content": resultText, "is_error": isErr},
	}}})
	writeFile(t, home+"/.claude/projects/-test/"+testSession(id)+".jsonl", string(u)+string(tc)+"\n"+string(tr)+"\n")
}

func TestAppCardErroredAfterReplyEmailMovesToDone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tool     string
		input    map[string]any
		res      string
		isErr    bool
		wantDone bool
	}{
		{
			name:     "pdw send_email succeeded",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw call zrl_bot_email_control__send_email --data '{"to":"a@b","subject":"s","body":"done"}'`},
			res:      `{"id":"m1","threadId":"th1"}`,
			wantDone: true,
		},
		{
			name:     "pdw reply_to_message with env var and -d succeeded",
			tool:     "Bash",
			input:    map[string]any{"command": `FOO=1 pdw call gmail__reply_to_message -d '{"body":"done"}'`},
			res:      `{"ok":true}`,
			wantDone: true,
		},
		{
			name:     "mcp send_email tool succeeded",
			tool:     "mcp__zrl_bot_email_control__send_email",
			input:    map[string]any{"to": "a@b", "subject": "s", "body": "done"},
			res:      "sent",
			wantDone: true,
		},
		{
			name:     "grep for send_email is not a send",
			tool:     "Bash",
			input:    map[string]any{"command": `grep -rn send_email factory/`},
			res:      `factory/inbox.go:10:send_email`,
			wantDone: false,
		},
		{
			name:     "pdw tools piped to grep is not a send",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw tools | grep send_email`},
			res:      `zrl_bot_email_control__send_email`,
			wantDone: false,
		},
		{
			name:     "pdw describe is not a send",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw describe zrl_bot_email_control__send_email`},
			res:      `{"name":"send_email"}`,
			wantDone: false,
		},
		{
			name:     "pdw call --help is not a send",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw call zrl_bot_email_control__send_email --help`},
			res:      `Usage: pdw call <tool> --data <json>`,
			wantDone: false,
		},
		{
			name:     "pdw send_email with tool error stays in ForYou",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw call zrl_bot_email_control__send_email --data '{}'`},
			res:      `missing recipient`,
			isErr:    true,
			wantDone: false,
		},
		{
			name:     "pdw send_email with non-zero exit stays in ForYou",
			tool:     "Bash",
			input:    map[string]any{"command": `pdw call zrl_bot_email_control__send_email --data '{}'`},
			res:      "Error: missing recipient\nCommand exited with code 1",
			wantDone: false,
		},
	} {
		for _, attn := range []bool{true, false} {
			t.Run(tc.name+"/attn="+strconv.FormatBool(attn), func(t *testing.T) {
				a := card("app-err", nil)
				a.Status = "error"
				if attn {
					a.RequiresAttention, a.AttentionReason = true, "error"
				}
				f, _ := newTestFactory(t, a)
				writeToolSession(t, f.cfg.Home, a.ID, tc.tool, tc.input, tc.res, tc.isErr)
				f.SetAgents([]Agent{a})
				c := f.cards[0]
				s := f.sections()
				if tc.wantDone {
					if c.Stage != "landed" || !slices.Equal(sectionIDs(s.Shipped), []string{a.ID}) || len(s.ForYou) != 0 {
						t.Fatalf("card = %+v, sections = %+v, want landed and shipped", c, s)
					}
				} else {
					if c.Stage != "zach" || c.Ask == nil || c.Ask.Title != "Waiting on you: error" || !slices.Equal(sectionIDs(s.ForYou), []string{a.ID}) {
						t.Fatalf("card = %+v, ask = %+v, forYou = %+v, want zach in ForYou with Waiting on you: error", c, c.Ask, s.ForYou)
					}
				}
			})
		}
	}
}

func TestAppCardErroredAfterLandingMovesToDone(t *testing.T) {
	for _, mode := range []string{"direct land", "squash merge", "unlanded local commit", "no commit"} {
		t.Run(mode, func(t *testing.T) {
			r := newRepos(t)
			f := realGitFactory(t)
			asked := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
			if mode == "direct land" || mode == "unlanded local commit" {
				commitFile(t, r.wt, "spec.md", "release spec\n")
			} else if mode == "squash merge" {
				commitFile(t, r.wt, "spec.md", "release spec\n")
				commitFile(t, r.wt, "release.sh", "#!/bin/sh\n")
			}
			if mode == "direct land" {
				r.land(t)
			} else if mode == "squash merge" {
				gitRun(t, r.wt, "push", "-q", "origin", "HEAD:refs/heads/card")
				gitRun(t, r.other, "fetch", "-q", "origin", "card")
				gitRun(t, r.other, "merge", "-q", "--squash", "origin/card")
				gitRun(t, r.other, "commit", "-q", "-m", "squash release")
				gitRun(t, r.other, "push", "-q", "origin", "HEAD:main")
				gitRun(t, r.wt, "fetch", "-q", "origin", "main")
			}
			a := card("app-land", nil)
			a.Cwd, a.CreatedAt, a.Status = r.wt, asked, "error"
			a.RequiresAttention, a.AttentionReason = true, "error"
			storeLog(t, f, nil, a.ID, "[User] Ship the release\n[Assistant] Watching CI...")
			f.SetAgents([]Agent{a})
			c := f.cards[0]
			s := f.sections()
			wantDone := mode == "direct land" || mode == "squash merge"
			if wantDone {
				if c.Stage != "landed" || !slices.Equal(sectionIDs(s.Shipped), []string{a.ID}) || len(s.ForYou) != 0 {
					t.Fatalf("%s: card = %+v, sections = %+v, want landed and shipped", mode, c, s)
				}
			} else {
				if c.Stage != "zach" || c.Ask == nil || c.Ask.Title != "Waiting on you: error" || !slices.Equal(sectionIDs(s.ForYou), []string{a.ID}) {
					t.Fatalf("%s: card = %+v, ask = %+v, forYou = %+v, want zach in ForYou", mode, c, c.Ask, s.ForYou)
				}
			}
		})
	}
}
