package factory

import (
	"context"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestDecisionPreview serves fake cards for a manual computer-use check. It never calls Paseo
// or reads live cards. Bind explicitly with FACTORY_UI_LISTEN; normal test runs skip it.
func TestDecisionPreview(t *testing.T) {
	addr := os.Getenv("FACTORY_UI_LISTEN")
	if addr == "" {
		t.Skip("manual UI fixture")
	}
	f, p := newTestFactory(t)
	withRepos(t, f, "example/desk", "example/app")
	f.routes.put("repo-card", Route{Request: "Choose a sample repo", Title: "Sample"})
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".factory-evidence"), 0700)
	for _, slot := range []string{"before", "after"} {
		img := stdimage.NewRGBA(stdimage.Rect(0, 0, 320, 180))
		for y := 0; y < 180; y++ {
			for x := 0; x < 320; x++ {
				v := uint8(210)
				if x > 25 && x < 295 && y > 25 && y < 155 {
					v = 150
				}
				if slot == "after" {
					img.Set(x, y, color.RGBA{v, 230, v, 255})
				} else {
					img.Set(x, y, color.RGBA{230, v, v, 255})
				}
			}
		}
		file, _ := os.Create(filepath.Join(dir, ".factory-evidence", slot+".png"))
		png.Encode(file, img)
		file.Close()
	}
	var agents []Agent
	add := func(id, title string, labels map[string]string, reply string) {
		a := card(id, labels)
		a.Title = title
		a.Cwd = dir
		storeLog(t, f, p, id, "[User] go\n"+reply)
		agents = append(agents, a)
	}
	add("plan-card", "Approve a visible change", map[string]string{labelStage: "zach", labelAwaiting: "plan"}, "DECISION: Approve this layout?\nOVERVIEW: Fewer controls and clearer choices.\nNEEDS FROM ZACH: Approve to build, or send instructions.\n\nBEFORE: before.png\nAFTER: after.png\n\nThe full plan appears here.\n\nBEFORE: Old wording\nAFTER: New wording\n\nQUESTION: Which spacing?\n- Comfortable (recommended)\n- Compact\n\nQUESTION: Which accent?\n- Blue (recommended)\n- Green")
	add("brief-card", "Choose a recommendation", map[string]string{labelStage: "zach", labelNote: "rework cap reached"}, "DECISION: Which fix should we apply?\nNEEDS FROM ZACH: Choose a fix; code review follows.\n\nQUESTION: Retry policy?\n- Once (recommended)\n- Twice")
	add("repo-card", "Choose a repo", map[string]string{labelStage: "zach", labelAwaiting: "repo", labelRouting: "1", labelCandidates: "example/desk example/app"}, "The request could fit either repo.")
	add("retry-card", "Retry a review", map[string]string{labelStage: "zach", labelNote: noVerdictNote, labelStuckFrom: "review"}, "Review did not finish.")
	add("missing-card", "Missing and rejected evidence", map[string]string{labelStage: "zach", labelNote: "landing not verified"}, "DECISION: Retry the landing?\nNEEDS FROM ZACH: Send instructions.\n\nBEFORE: missing.png\nAFTER: bad.png\n\n<img src=\"https://example.org/not-allowed.png\">\n![Blocked](data:image/png;base64,bad)")
	os.WriteFile(filepath.Join(dir, ".factory-evidence", "bad.png"), []byte("invalid"), 0600)
	add("stop-card", "Stop a waiting agent", map[string]string{labelStage: "zach"}, "Waiting on permission.")
	agents[len(agents)-1].Status = "running"
	agents[len(agents)-1].Pending = "permission"
	add("done-question-card", "Finished question", map[string]string{labelStage: "zach"}, "DECISION: Which database?\nNEEDS FROM ZACH: Choose a database.\n\nQUESTION: Database?\n- Postgres\n- SQLite")
	add("work-left-card", "Work remains", map[string]string{labelStage: "zach", labelNote: "landed; still to do: finish the check"}, "DECISION: Finish the remaining work?\nNEEDS FROM ZACH: Send instructions to continue.")
	add("scan-card", "Scan stopped", map[string]string{labelStage: "zach", labelNote: "secret scan stopped"}, "DECISION: Fix the scan finding?\nNEEDS FROM ZACH: Tell the agent to fix the commit.")
	add("escalated-card", "Answer an escalation", map[string]string{labelStage: "zach", labelNote: "reviewer escalated"}, "DECISION: Which store?\nNEEDS FROM ZACH: Choose or write instructions.\n\nQUESTION: Store?\n- Postgres\n- SQLite")
	f.SetAgents(agents)
	// Keep snapshots stable while fake actions run. No real runner or production data is used.
	handler := f.Handler()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/board" {
			f.SetAgents(agents)
			f.mu.Lock()
			for i := range f.cards {
				if f.cards[i].Ask == nil {
					continue
				}
				if f.cards[i].ID == "done-question-card" {
					f.cards[i].Ask.Actions = []Action{primary("done", "Got it, mark done")}
				}
				if f.cards[i].ID == "stop-card" {
					f.cards[i].Ask.Actions = []Action{act("stop", "Stop it")}
				}
				task := "Simplify the sample decision screen."
				if f.cards[i].ID == "plan-card" {
					task += " Keep the original request visible while making the action clear. Show relevant comparisons when available. Let the reader choose an option or send instructions. Keep the full report available without opening it automatically."
				}
				f.cards[i].Ask.Request = &CardRequest{Base: task, FollowUps: []FollowUp{{Text: "Keep the choice labels clear."}}}
			}
			f.mu.Unlock()
		}
		if r.Method == "POST" {
			fmt.Println("UI action:", r.URL.Path)
		}
		handler.ServeHTTP(w, r)
	})}
	go server.Serve(listener)
	fmt.Println("UI fixture:", listener.Addr())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(15 * time.Minute):
	}
	server.Close()
}
