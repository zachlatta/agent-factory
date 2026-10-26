package factory

import (
	"net/http/httptest"
	"regexp"
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

// The board must say when it can't load, at every width: the phone CSS hides the header
// status line, so the notice lives in #banner (and in #grid when nothing has loaded yet).
func TestBoardShowsLoadFailureAtPhoneWidth(t *testing.T) {
	page := boardPage(t)

	if !strings.Contains(page, `<div class="banner" id="banner" hidden></div>`) {
		t.Error("board has no #banner")
	}
	phone := regexp.MustCompile(`(?s)@media \(max-width:760px\)\{(.*?)\n\}`).FindStringSubmatch(page)
	if phone == nil {
		t.Fatal("no phone media block")
	}
	for _, rule := range regexp.MustCompile(`([^{}]+)\{[^}]*display:none`).FindAllStringSubmatch(phone[1], -1) {
		if strings.Contains(rule[1], "banner") || strings.Contains(rule[1], "#grid") {
			t.Errorf("phone CSS hides the load notice: %q", strings.TrimSpace(rule[1]))
		}
	}

	load := regexp.MustCompile(`(?s)async function load\(\)\{.*?\n\}`).FindString(page)
	if load == "" {
		t.Fatal("no load()")
	}
	if !strings.Contains(load, "r.ok") {
		t.Error("load() parses the body without checking the HTTP status")
	}
	if strings.Contains(load, "B = await r.json()") {
		t.Error("load() replaces B before the response is known good")
	}
	for _, want := range []string{"Can't reach the factory", "retrying", "body.stale"} {
		if !strings.Contains(page, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if !strings.Contains(page, `$('grid').innerHTML`) {
		t.Error("a failed first load leaves #grid blank")
	}
}
