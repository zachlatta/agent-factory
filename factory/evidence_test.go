package factory

import (
	"bytes"
	stdimage "image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEvidence(t *testing.T) {
	rest, ev := parseEvidence("Report.\n\nBEFORE: ../secret.png\nBEFORE: before.png\nBEFORE: ignored.png\nAFTER: after.jpg\n\nTIER: low")
	if ev.Before != "before.png" || ev.After != "after.jpg" || !strings.Contains(rest, "Report.") || strings.Contains(rest, "BEFORE:") {
		t.Fatalf("%q %+v", rest, ev)
	}
	for _, name := range []string{"../x.png", "/x.png", "a/b.png", `a\b.png`, "x.svg", ".", "x.png?token=1"} {
		if validEvidenceName(name) {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestEvidenceServing(t *testing.T) {
	dir := t.TempDir()
	evidence := filepath.Join(dir, ".factory-evidence")
	if err := os.Mkdir(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2)))
	os.WriteFile(filepath.Join(evidence, "before.png"), b.Bytes(), 0600)
	os.WriteFile(filepath.Join(evidence, "unlisted.png"), b.Bytes(), 0600)
	os.WriteFile(filepath.Join(evidence, "bad.png"), []byte("not an image"), 0600)
	os.WriteFile(filepath.Join(evidence, "large.png"), make([]byte, (5<<20)+1), 0600)
	outside := filepath.Join(dir, "outside.png")
	os.WriteFile(outside, b.Bytes(), 0600)
	os.Symlink(outside, filepath.Join(evidence, "link.png"))
	a := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan"})
	a.Cwd = dir
	f, p := newTestFactory(t)
	for _, tc := range []struct {
		name   string
		status int
	}{{"before.png", 200}, {"bad.png", 404}, {"missing.png", 404}, {"link.png", 404}, {"large.png", 404}, {"../outside.png", 404}} {
		p.logs["c1"] = "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\nBEFORE: " + tc.name
		a.LastActivityAt = a.LastActivityAt.Add(time.Second)
		f.SetAgents([]Agent{a})
		r := httptest.NewRecorder()
		f.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/api/evidence/c1/before", nil))
		if r.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.name, r.Code, r.Body.String())
		}
		if r.Code == 200 && (r.Header().Get("Content-Type") != "image/png" || r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("X-Content-Type-Options") != "nosniff") {
			t.Fatal(r.Header())
		}
	}
	for _, path := range []string{"/api/evidence/c1/unlisted.png", "/api/evidence/unknown/before", "/api/evidence/c1/after"} {
		r := httptest.NewRecorder()
		f.Handler().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 404 {
			t.Errorf("%s: %d", path, r.Code)
		}
	}
}

func TestAskRequestAndEvidenceCache(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan"})
	f, p := newTestFactory(t)
	p.logs["c1"] = "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\nBEFORE: before.png\nAFTER: after.png\n\nDetails."
	f.SetAgents([]Agent{a})
	for i := 0; i < 2; i++ {
		c, _ := f.card("c1")
		if c.Ask.Evidence.Before != "before.png" || c.Ask.Evidence.After != "after.png" || strings.Contains(c.Ask.Detail, "BEFORE:") {
			t.Fatalf("%+v", c.Ask)
		}
		f.SetAgents([]Agent{a})
	}
}

func TestRecommendCarriesAnswers(t *testing.T) {
	for _, plan := range []bool{false, true} {
		a := card("c1", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"})
		if plan {
			a.Labels[labelPrefix+labelAwaiting] = "plan"
		}
		f, p := newTestFactory(t, a)
		if _, err := f.Act("recommend", Body{ID: "c1", Message: "Keep the cache"}); err != nil {
			t.Fatal(err)
		}
		msgs := p.sent("c1")
		if len(msgs) != 1 || !strings.Contains(msgs[0], "Keep the cache") {
			t.Fatal(msgs)
		}
	}
}

func TestAskOriginalRequest(t *testing.T) {
	a := reqCard("c1", "board")
	a.Labels[labelPrefix+labelStage] = "zach"
	a.Labels[labelPrefix+labelAwaiting] = "plan"
	f, _ := newTestFactory(t)
	writeSession(t, f.cfg.Home, a.Cwd, a.SessionID, sessionLine(fakeEmailPrompt, false))
	f.SetAgents([]Agent{a})
	c, _ := f.card(a.ID)
	if c.Ask.Request == nil || c.Ask.Request.Base != "Paint the shed blue,\nand the door too." {
		t.Fatalf("%+v", c.Ask.Request)
	}
}

func TestAskFullReport(t *testing.T) {
	a := card("c1", map[string]string{labelStage: "zach", labelAwaiting: "plan"})
	f, p := newTestFactory(t)
	report := strings.Repeat("Plan detail. ", 700) + "End of plan."
	p.logs[a.ID] = "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\n" + report
	f.SetAgents([]Agent{a})
	c, _ := f.card(a.ID)
	if c.Ask.Report != report || len(c.Ask.Detail) > 6100 {
		t.Fatal("full report missing or email detail changed")
	}
}

func TestEvidencePrompts(t *testing.T) {
	for name, prompt := range map[string]string{"rule": overviewRule, "reminder": overviewReminder, "summary": summaryMsg, "brief": briefMsg("code", true, "notes", briefAsk(card("c", map[string]string{labelNote: "reviewer escalated"})))} {
		for _, want := range []string{"BEFORE:", "AFTER:", ".factory-evidence/", "never commit"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s lacks %s", name, want)
			}
		}
	}
}

func TestEvidenceRejectsDirectorySymlinkAndPixels(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, ".factory-evidence")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEvidence(root, "before.png"); err == nil {
		t.Fatal("accepted directory symlink")
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".factory-evidence"), 0700)
	var b bytes.Buffer
	if err := png.Encode(&b, stdimage.NewGray(stdimage.Rect(0, 0, 5001, 4000))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".factory-evidence", "before.png"), b.Bytes(), 0600)
	if _, _, err := readEvidence(dir, "before.png"); err == nil {
		t.Fatal("accepted over 20 megapixels")
	}
}
