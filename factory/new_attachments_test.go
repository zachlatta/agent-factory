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
			marker := "Attached screenshots (read these files):\n- "
			i := strings.LastIndex(prompt, marker)
			if i < 0 {
				t.Fatal("screenshot path absent from request")
			}
			path := strings.SplitN(prompt[i+len(marker):], "\n", 2)[0]
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
