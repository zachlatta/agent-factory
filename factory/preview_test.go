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
	for _, route := range []string{"/api/approve", "/api/new", "/api/host", "/api/peers"} {
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
