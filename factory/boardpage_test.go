package factory

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// boardPage fetches the board page from the server.
func boardPage(t *testing.T) string {
	t.Helper()
	f, _ := newTestFactory(t)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("board page: %d", rec.Code)
	}
	return rec.Body.String()
}

// cssRules returns the bodies of every rule whose selector is exactly sel.
func cssRules(page, sel string) []string {
	var out []string
	for rest := page; ; {
		i := strings.Index(rest, sel+"{")
		if i < 0 {
			return out
		}
		if i > 0 && !strings.ContainsRune("\n }", rune(rest[i-1])) {
			rest = rest[i+len(sel):]
			continue
		}
		body := rest[i+len(sel)+1:]
		end := strings.IndexByte(body, '}')
		out = append(out, body[:end])
		rest = body[end:]
	}
}

func TestBoardPageSafeArea(t *testing.T) {
	page := boardPage(t)
	if !strings.Contains(page, `content="width=device-width, initial-scale=1, viewport-fit=cover"`) {
		t.Error("viewport meta lacks viewport-fit=cover")
	}
	for _, c := range []struct{ sel, want string }{
		{"#activity", "safe-area-inset-bottom"},
		{"#activity.open", "safe-area-inset-bottom"},
		{"#toast", "safe-area-inset-bottom"},
		{"main", "safe-area-inset-bottom"},
		{"aside .bd", "safe-area-inset-bottom"},
		{"aside .hd", "safe-area-inset-top"},
		{"header", "safe-area-inset-top"},
		{".tabs", "safe-area-inset-top"},
	} {
		rules := cssRules(page, c.sel)
		if len(rules) == 0 {
			t.Errorf("no %s rule", c.sel)
		}
		// every rule for the selector (desktop and phone) must keep the inset
		for _, r := range rules {
			if strings.Contains(r, "padding") || strings.Contains(r, "bottom:") || strings.Contains(r, "top:") || strings.Contains(r, "max-height") {
				if !strings.Contains(r, c.want) {
					t.Errorf("%s{%s} lacks %s", c.sel, r, c.want)
				}
			}
		}
	}
}

func TestBoardPageNeedsZachTabPinned(t *testing.T) {
	page := boardPage(t)
	// a square sticky wrapper, so scrolled tabs never show around the pill's rounded ends
	rules := cssRules(page, ".tabs .pin")
	if len(rules) == 0 || !strings.Contains(rules[0], "position:sticky") || !strings.Contains(rules[0], "left:0") || !strings.Contains(rules[0], "background:#fff") {
		t.Errorf("Needs Zach tab is not pinned at the left: %q", rules)
	}
	// on the phone the Needs Zach tab is rendered first, inside the pin
	if !strings.Contains(page, "[...stages.filter(s=>s[0]==='zach'), ...stages.filter(s=>s[0]!=='zach')]") {
		t.Error("renderPhone does not put Needs Zach first")
	}
	if !strings.Contains(page, `<span class="pin">`) {
		t.Error("renderPhone does not wrap Needs Zach in the pin")
	}
}
