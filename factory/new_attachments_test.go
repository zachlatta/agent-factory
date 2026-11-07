package factory

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestNewCardScreenshots(t *testing.T) {
	for _, repo := range []string{"example/ops", "auto"} {
		t.Run(repo, func(t *testing.T) {
			f, p := newTestFactory(t)
			withRepos(t, f, "example/ops")
			data := []byte("screenshot")
			_, err := f.Act("new", Body{Request: "Fix layout", Repo: repo, Attachments: []Attachment{{Name: "screen.png", Type: "image/png", DataBase64: base64.StdEncoding.EncodeToString(data)}}})
			if err != nil {
				t.Fatal(err)
			}
			runs := runArgs(p)
			prompt := runs[0][len(runs[0])-1]
			marker := "Attached files (saved on " + f.cfg.Host + "):\n- "
			i := strings.LastIndex(prompt, marker)
			if i < 0 {
				t.Fatal("screenshot path absent from request")
			}
			path := strings.SplitN(prompt[i+len(marker):], "\n", 2)[0]
			if repo == "auto" {
				if err := f.startRouted(Agent{ID: "new-agent-1"}, "example/ops"); err != nil {
					t.Fatal(err)
				}
				runs = runArgs(p)
			}
			last := runs[len(runs)-1]
			found := false
			for j := 0; j+1 < len(last); j++ {
				if last[j] == "--image" && last[j+1] == path {
					found = true
				}
			}
			if !found {
				t.Fatalf("builder missing image argument: %v", last[:len(last)-1])
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(data) {
				t.Fatalf("attachment not saved: %v", err)
			}
		})
	}
}
func TestNewCardRejectsInvalidScreenshot(t *testing.T) {
	f, p := newTestFactory(t)
	_, err := f.Act("new", Body{Request: "Fix layout", Repo: "example/ops", Attachments: []Attachment{{Name: "screen.png", Type: "image/png", DataBase64: "!"}}})
	if err == nil || len(runArgs(p)) != 0 {
		t.Fatal("invalid attachment launched a card")
	}
}

func TestAttachmentStorageSharedByReplies(t *testing.T) {
	f, _ := newTestFactory(t)
	att := Attachment{Name: "same.png", Type: "image/png", DataBase64: base64.StdEncoding.EncodeToString([]byte("image"))}
	files, err := f.chat("sample", "Look here", []Attachment{att, att})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] == files[1] {
		t.Fatal("attachments must have distinct paths")
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file permissions: %v, %v", info, err)
		}
	}
	_, _, err = f.saveAttachments("sample", []Attachment{{Name: "blank.png", Type: "image/png"}})
	if err == nil || !strings.Contains(err.Error(), "blank.png is empty") {
		t.Fatalf("empty attachment error: %v", err)
	}
	_, _, err = f.saveAttachments("sample", make([]Attachment, 11))
	if err == nil {
		t.Fatal("accepted more than ten attachments")
	}
}
