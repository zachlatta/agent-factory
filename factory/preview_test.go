package factory

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewIsolatedRealHandler(t *testing.T) {
	// A poisoned host environment must never be used to construct preview state.
	host := t.TempDir()
	t.Setenv("HOME", host)
	t.Setenv("PATH", "") // any attempt to start a real CLI fails
	t.Setenv("FACTORY_DEFAULT_REPO", "private/sentinel")
	if err := os.WriteFile(filepath.Join(host, "sentinel"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h, err := PreviewHandler(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/", "/api/board", "/api/agents", "/api/lanes", "/api/deploy", "/api/hosts", "/host-metrics.js", "/api/logs/preview-zach", "/api/logs/idea-preview", "/api/transcript/preview-zach"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", route, nil))
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", route, r.Code, r.Body.String())
		}
		if strings.Contains(r.Body.String(), "private/sentinel") {
			t.Fatalf("host data leaked: %s", route)
		}
		if route == "/api/hosts" && !strings.Contains(r.Body.String(), `"cpuCount":4`) {
			t.Fatal("host metrics did not use fake reader")
		}
		if route == "/api/transcript/preview-zach" && (strings.Contains(r.Body.String(), `"fallback":true`) || !strings.Contains(r.Body.String(), "Approve the board layout plan?")) {
			t.Fatal("real transcript reader did not load fixture session")
		}
		if route == "/api/board" {
			var b struct {
				Cards []Card
				Repos []string
			}
			if err := json.Unmarshal(r.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if len(b.Cards) < len(Stages) || len(b.Repos) == 0 {
				t.Fatalf("missing fixtures: %+v", b)
			}
			var found bool
			for _, c := range b.Cards {
				if c.ID == "preview-zach" {
					found = c.Stage == "zach" && c.Ask != nil && c.Ask.Detail != ""
				}
			}
			if !found {
				t.Fatal("real board did not build approval fixture")
			}
		}
	}
	for _, route := range []string{"/api/new", "/api/host", "/api/peers"} {
		method := "GET"
		if route == "/api/approve" || route == "/api/new" {
			method = "POST"
		}
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, route, strings.NewReader(`{"id":"preview-zach"}`))
		req.Header.Set("X-Factory", "1")
		h.ServeHTTP(r, req)
		if r.Code != 403 {
			t.Errorf("unsafe route %s: %d", route, r.Code)
		}
	}
	entries, err := os.ReadDir(host)
	if err != nil || len(entries) != 1 {
		t.Fatalf("preview wrote real home: %v %v", entries, err)
	}
}

func TestPreviewRequiresEmptyRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := PreviewHandler(root); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewHandler(root); err == nil {
		t.Fatal("preview reused existing state")
	}
}

func TestPreviewSimulatesOnlyFabricatedCardActions(t *testing.T) {
	h, err := PreviewHandler(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := func(action, body string, header bool) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/"+action, strings.NewReader(body))
		if header {
			req.Header.Set("X-Factory", "1")
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("approve", `{"id":"preview-zach"}`, false); got != 403 {
		t.Fatalf("no header: %d", got)
	}
	if got := request("approve", `{"id":"not-a-fixture"}`, true); got != 404 {
		t.Fatalf("unknown card: %d", got)
	}
	for _, tc := range []struct{ action, body string }{
		{"approve", `{"id":"preview-zach"}`}, {"send", `{"id":"preview-building","message":"Sample answer"}`},
		{"stage", `{"id":"preview-plan","stage":"review"}`}, {"close", `{"id":"preview-review"}`},
	} {
		if got := request(tc.action, tc.body, true); got != 200 {
			t.Fatalf("%s: %d", tc.action, got)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/board", nil))
	var board struct{ Cards []Card }
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	stages := map[string]string{}
	for _, c := range board.Cards {
		stages[c.ID] = c.Stage
	}
	if stages["preview-zach"] != "building" || stages["preview-plan"] != "review" || stages["preview-review"] != "landed" {
		t.Fatalf("fake changes not reflected: %v", stages)
	}
	for _, a := range []string{"new", "scout", "review", "answer", "land-review", "recommend"} {
		if got := request(a, `{"id":"preview-plan"}`, true); got != 403 {
			t.Errorf("unsafe %s: %d", a, got)
		}
	}
}

func TestRepoPreviewDeclarationUsesSafeCLI(t *testing.T) {
	raw, err := os.ReadFile("../.factory.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Preview *PreviewSpec }
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Preview == nil {
		t.Fatal("repo has no fake-data preview declaration")
	}
	if !strings.Contains(cfg.Preview.Run, `-listen "${HOST:?}:${PORT:?}"`) || strings.Contains(cfg.Preview.Run, "-addr") {
		t.Fatal("preview must use the actual CLI and require its host and port")
	}
	if cfg.Preview.Path != "/?stage=zach" {
		t.Fatalf("preview should open question choices: %q", cfg.Preview.Path)
	}
}

func TestRepoPreviewBootstrapThenReadyLifecycle(t *testing.T) {
	raw, err := os.ReadFile("../.factory.json")
	if err != nil {
		t.Fatal(err)
	}
	f, _, runner := previewFactory(t, `{}`, readyCard("bootstrap"))
	f.tickPreviews(t0)
	waitPreview(t, f, "bootstrap", previewNone)
	if n, _ := runner.count(); n != 0 {
		t.Fatal("branch declaration ran before origin/main declared it")
	}
	// A subsequent Ready stop reads the now-landed declaration from origin/main.
	f.GitOut = gitStub(map[string]string{"show origin/main:.factory.json": string(raw)})
	f.SetAgents([]Agent{readyCard("subsequent")})
	f.tickPreviews(t0)
	p := waitPreview(t, f, "subsequent", previewReady)
	if p.URL != "https://board.example:7700/?stage=zach" {
		t.Fatalf("live preview link: %+v", p)
	}
	c := mustCard(t, f, "subsequent")
	f.fillReady(c.Ask, c.ID, "CHANGED: Sample layout")
	if c.Ask.Ready.Preview != p.URL {
		t.Fatal("Ready card did not expose live Preview")
	}
	f.SetAgents(nil)
	f.tickPreviews(t0)
	if _, stopped := runner.count(); stopped != 1 {
		t.Fatalf("preview cleanup: %d", stopped)
	}
}
