package factory

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repo names and how they land are host config: the binary carries none of them.
func TestLanesComeFromHostConfig(t *testing.T) {
	f, _ := newTestFactory(t)
	if got := f.lanes(); len(got) != 0 {
		t.Fatalf("no lanes file: got %v", got)
	}
	f.cfg.LanesFile = filepath.Join(t.TempDir(), "lanes.json")
	os.WriteFile(f.cfg.LanesFile, []byte(`{"example/desk":{"name":"desk","lands":"main via bin/land"}}`), 0o600)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/lanes", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"lands":"main via bin/land"`) {
		t.Fatalf("GET /api/lanes = %d %s", rec.Code, rec.Body)
	}
}

func TestPromptsNameTheConfiguredErrandsRepo(t *testing.T) {
	f, p := newTestFactory(t)
	if _, err := f.Act("new", Body{Request: "what is on today?"}); err != nil {
		t.Fatal(err)
	}
	prompt := p.calls[len(p.calls)-1]
	last := prompt[len(prompt)-1]
	if !strings.Contains(last, "record it in\na desk work item") || strings.Contains(last, "<errands>") {
		t.Errorf("prompt does not name the errands repo:\n%s", last)
	}
}

func TestNewCardNeedsARepo(t *testing.T) {
	f, _ := newTestFactory(t)
	f.cfg.DefaultRepo = ""
	if _, err := f.Act("new", Body{Request: "x"}); err == nil || !strings.Contains(err.Error(), "FACTORY_DEFAULT_REPO") {
		t.Fatalf("err = %v", err)
	}
}
