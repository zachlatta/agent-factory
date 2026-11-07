package factory

import (
	"bytes"
	"fmt"
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
	const overview = "DECISION: Choose.\nNEEDS FROM ZACH: Pick one.\n\n"
	for _, tc := range []struct {
		name, in, want string
		evidence       Evidence
	}{
		{"header", overview + "BEFORE: before.png\nAFTER: after.jpg\n\nReport.", overview + "Report.", Evidence{Before: "before.png", After: "after.jpg"}},
		{"text change", overview + "BEFORE: Needs review\nAFTER: Waiting on you\nend", overview + "BEFORE: Needs review\nAFTER: Waiting on you\nend", Evidence{}},
		{"later report", overview + "Report.\nBEFORE: before.png\nAFTER: after.jpg", overview + "Report.\nBEFORE: before.png\nAFTER: after.jpg", Evidence{}},
		{"copy report", "Copy change:\nBEFORE: Old wording\nAFTER: New wording\nend", "Copy change:\nBEFORE: Old wording\nAFTER: New wording\nend", Evidence{}},
		{"no overview", "BEFORE: before.png\nAFTER: after.jpg", "BEFORE: before.png\nAFTER: after.jpg", Evidence{}},
		{"invalid then valid", overview + "BEFORE: ../secret.png\nBEFORE: before.png\nAFTER: after.jpg\n\nReport.", overview + "BEFORE: ../secret.png\n\nReport.", Evidence{Before: "before.png", After: "after.jpg"}},
	} {
		rest, ev := parseEvidence(tc.in)
		if rest != tc.want || ev != tc.evidence {
			t.Errorf("%s: %q %+v", tc.name, rest, ev)
		}
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
	a := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReviewed: reviewedSHA})
	a.Cwd = dir
	f, p := newTestFactory(t)
	for _, tc := range []struct {
		name   string
		status int
	}{{"before.png", 200}, {"bad.png", 404}, {"missing.png", 404}, {"link.png", 404}, {"large.png", 404}, {"../outside.png", 404}} {
		storeLog(t, f, p, "c1", "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\nBEFORE: "+tc.name)
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
	a := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReviewed: reviewedSHA})
	f, p := newTestFactory(t)
	storeLog(t, f, p, "c1", "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\nBEFORE: before.png\nAFTER: after.png\n\nDetails.")
	f.SetAgents([]Agent{a})
	for i := 0; i < 2; i++ {
		c, _ := f.card("c1")
		if c.Ask.Evidence.Before != "before.png" || c.Ask.Evidence.After != "after.png" || strings.Contains(c.Ask.Detail, "BEFORE:") {
			t.Fatalf("%+v", c.Ask)
		}
		f.SetAgents([]Agent{a})
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
	a := card("c1", map[string]string{labelStage: "zach", labelNote: readyNote(1), labelReviewed: reviewedSHA})
	f, p := newTestFactory(t)
	report := strings.Repeat("Plan detail. ", 700) + "End of plan."
	storeLog(t, f, p, a.ID, "[User] go\nDECISION: Go?\nNEEDS FROM ZACH: Choose.\n\n"+report)
	f.SetAgents([]Agent{a})
	c, _ := f.card(a.ID)
	if c.Ask.Report != report || len(c.Ask.Detail) > 6100 {
		t.Fatal("full report missing or email detail changed")
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

func TestEvidencePrompts(t *testing.T) {
	for name, prompt := range map[string]string{
		"new card": fmt.Sprintf(newCardPrompt, "task", "example/repo", "", wholeChangeRule, questionRule),
		"review":   ReviewPrompt(Card{ID: "c", Repo: "example/repo"}, "code", 7478, "unknown"),
	} {
		for _, want := range []string{"BEFORE:", "AFTER:", ".factory-evidence/", "never commit"} {
			if strings.Count(prompt, want) != 1 {
				t.Errorf("%s must teach %s once", name, want)
			}
		}
	}
	for name, prompt := range map[string]string{"reminder": overviewReminder, "side effects": readyMsg("x")} {
		if strings.Contains(prompt, "BEFORE:") {
			t.Errorf("%s repeats attachment instructions", name)
		}
	}
}

// webpVP8X is a minimal animated-style WebP container with the given canvas.
func webpVP8X(w, h int) []byte {
	le24 := func(n int) []byte { return []byte{byte(n), byte(n >> 8), byte(n >> 16)} }
	body := append([]byte("WEBPVP8X\x0a\x00\x00\x00\x12\x00\x00\x00"), le24(w-1)...)
	body = append(body, le24(h-1)...)
	return append(append([]byte("RIFF"), byte(len(body)), 0, 0, 0), body...)
}

func TestEvidenceAcceptsAWebpVideo(t *testing.T) {
	dir := t.TempDir()
	ev := filepath.Join(dir, ".factory-evidence")
	if err := os.Mkdir(ev, 0o700); err != nil {
		t.Fatal(err)
	}
	good := webpVP8X(1280, 800)
	if err := os.WriteFile(filepath.Join(ev, "proof.webp"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	data, media, err := readEvidence(dir, "proof.webp")
	if err != nil || media != "image/webp" || len(data) != len(good) {
		t.Fatalf("media %q, %d bytes, err %v", media, len(data), err)
	}
	truncated := append([]byte{}, good...)
	truncated[4] = 0xff
	for name, bad := range map[string][]byte{
		"not webp":     []byte("<svg onload=alert(1)>"),
		"truncated":    truncated,
		"short":        []byte("RIFF"),
		"huge canvas":  webpVP8X(10000, 10000),
		"unknown kind": append(append([]byte("RIFF\x16\x00\x00\x00WEBPXXXX"), make([]byte, 10)...), 0, 0, 0, 0)[:30],
	} {
		if err := os.WriteFile(filepath.Join(ev, "bad.webp"), bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readEvidence(dir, "bad.webp"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidWebpReadsLossyAndLosslessCanvases(t *testing.T) {
	wrap := func(chunk string, payload []byte) []byte {
		body := append([]byte("WEBP"+chunk), payload...)
		return append(append([]byte("RIFF"), byte(len(body)), 0, 0, 0), body...)
	}
	lossy := func(w, h uint16) []byte {
		p := []byte{0, 0, 0, 0, 0, 0, 0, 0x9d, 0x01, 0x2a, byte(w), byte(w >> 8), byte(h), byte(h >> 8)}
		return wrap("VP8 ", append([]byte{14, 0, 0, 0}, p[4:]...))
	}
	lossless := func(w, h uint32) []byte {
		bits := (w - 1) | (h-1)<<14
		return wrap("VP8L", []byte{10, 0, 0, 0, 0x2f, byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24), 0, 0, 0, 0, 0})
	}
	if !validWebp(lossy(800, 600)) || validWebp(lossy(16000, 16000)) {
		t.Error("lossy canvas not read")
	}
	if !validWebp(lossless(800, 600)) || validWebp(lossless(16000, 16000)) {
		t.Error("lossless canvas not read")
	}
}
