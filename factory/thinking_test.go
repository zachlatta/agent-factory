package factory

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
)

func thinkingBody(t *testing.T, raw string) Body {
	t.Helper()
	var b Body
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestThinkingLaunchAndValidation(t *testing.T) {
	for _, tc := range []struct {
		agent, level, want string
		invalid            bool
	}{
		{"opus", "", "medium", false},
		{"sonnet", "", "medium", false},
		{"codex-sol", "", "medium", false},
		{"codex-sol", "high", "high", false},
		{"opus", "low", "low", false},
		{"sonnet", "xhigh", "xhigh", false},
		{"codex-astra", "max", "max", false},
		{"codex-sol", "ultra", "ultra", false},
		{"codex-sol", "off", "", true},
		{"opus", "ultra", "", true},
		{"gemini-argon", "medium", "", true},
		{"muse-wonder", "low", "", true},
	} {
		t.Run(tc.agent+"/"+tc.level, func(t *testing.T) {
			f, p := newTestFactory(t)
			b := thinkingBody(t, `{"request":"test","agent":"`+tc.agent+`","thinking":"`+tc.level+`"}`)
			_, err := f.Act("new", b)
			runs := p.runsWithLabel(labelArg(labelStage, "plan"))
			if tc.invalid {
				if err == nil || len(runs) != 0 {
					t.Fatalf("invalid choice launched: %v %v", err, runs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || flag(runs[0], "--thinking") != tc.want {
				t.Fatalf("runs = %v, want thinking %s", runs, tc.want)
			}
		})
	}
}

func TestThinkingSurvivesRepoAuto(t *testing.T) {
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk")
	res, err := f.Act("new", thinkingBody(t, `{"request":"test","repo":"auto","agent":"codex-sol","thinking":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(map[string]string)["id"]
	if err := f.startRouted(picker(id, nil), "example/desk"); err != nil {
		t.Fatal(err)
	}
	runs := p.runsWithLabel(labelArg(labelAgent, "codex-sol"))
	if len(runs) != 1 || flag(runs[0], "--thinking") != "high" {
		t.Fatalf("routed runs = %v", runs)
	}
}

func TestIdeaThinkingAndOverride(t *testing.T) {
	for _, tc := range []struct{ agent, level, want string }{
		{"", "", "high"}, {"codex-sol", "low", "low"}, {"sonnet", "", "medium"},
	} {
		t.Run(tc.agent+tc.level, func(t *testing.T) {
			f, p := newTestFactory(t)
			i, err := f.addIdea(thinkingBody(t, `{"request":"test","title":"test","agent":"codex-sol","thinking":"high"}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Act("approve", thinkingBody(t, `{"id":"`+i.ID+`","agent":"`+tc.agent+`","thinking":"`+tc.level+`"}`)); err != nil {
				t.Fatal(err)
			}
			runs := p.runsWithLabel(labelArg(labelOrigin, "idea"))
			if len(runs) != 1 || flag(runs[0], "--thinking") != tc.want {
				t.Fatalf("idea runs = %v", runs)
			}
		})
	}
	f, p := newTestFactory(t)
	i, err := f.addIdea(thinkingBody(t, `{"request":"test","agent":"codex-sol","thinking":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Act("approve", thinkingBody(t, `{"id":"`+i.ID+`","thinking":"invalid"}`)); err == nil {
		t.Fatal("invalid idea start accepted")
	}
	if len(runArgs(p)) != 0 {
		t.Fatal("invalid start launched an agent")
	}
}

func TestThinkingMetadataAndPreferredModels(t *testing.T) {
	f, _ := newTestFactory(t)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/agents", nil))
	var data struct {
		Agents []struct {
			ID, Thinking    string
			ThinkingOptions []string `json:"thinkingOptions"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Agents) != 6 {
		t.Fatalf("agents=%+v", data.Agents)
	}
	for _, a := range data.Agents {
		if a.Thinking != "" && !slices.Contains(a.ThinkingOptions, a.Thinking) {
			t.Errorf("%s default missing from levels: %+v", a.ID, a)
		}
	}
	for provider, want := range map[string]string{"claude": "opus", "codex": "codex-sol"} {
		ranked, suggested := rankAgentChoices(map[string]string{provider: "available"}, nil, f.Now())
		if suggested != want || ranked[0].ID != want {
			t.Errorf("%s suggestion=%s, first=%s", provider, suggested, ranked[0].ID)
		}
	}
}

func TestIdeaRejectsUnsupportedThinkingBeforeSaving(t *testing.T) {
	for _, raw := range []string{
		`{"request":"test","agent":"codex-sol","thinking":"off"}`,
		`{"request":"test","thinking":"high"}`,
		`{"request":"test","agent":"muse-wonder","thinking":"medium"}`,
	} {
		f, _ := newTestFactory(t)
		if _, err := f.addIdea(thinkingBody(t, raw)); err == nil {
			t.Fatal("unsupported idea thinking accepted")
		}
		if got := f.ideas.List(); len(got) != 0 {
			t.Fatal("invalid idea saved")
		}
	}
}
