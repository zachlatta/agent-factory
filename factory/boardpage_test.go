package factory

import (
	"net/http/httptest"
	"os/exec"
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

func TestBoardPageReviewChipsCollapse(t *testing.T) {
	page := boardPage(t)
	if strings.Contains(page, "rounds p") {
		t.Error("cards still show the separate rounds chip")
	}
	if !strings.Contains(page, "latestReviews(c.children)") {
		t.Error("cardHtml does not collapse reviewers with latestReviews")
	}
	// the panel still lists every child and both round counts
	for _, want := range []string{
		"['Sub-agents', c.children.map(",
		"plan ${c.rounds.plan} · code ${c.rounds.code}",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("panel lost %q", want)
		}
	}
	const begin, end = "// latestReviews: begin", "// latestReviews: end"
	i, j := strings.Index(page, begin), strings.Index(page, end)
	if i < 0 || j < i {
		t.Fatal("no latestReviews markers in the page")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := page[i:j] + `
const rv = (kind, status, verdict, archived) => ({role:'reviewer', kind, status, verdict, archived:!!archived});
const labels = (kids, rounds) => latestReviews(kids).map(k => reviewLabel(k, rounds));
console.log(JSON.stringify([
  labels([rv('plan','closed','changes',1), rv('plan','closed','changes',1), rv('plan','closed','nits',1),
          {role:'subagent', status:'running'}, rv('code','closed','nits',1)], {plan:3, code:1}),
  labels([rv('plan','closed','changes',1), rv('plan','closed','nits',1), rv('plan','running','')], {plan:2, code:0}),
  labels([rv('','closed','')], {plan:0, code:0}),
]));`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `[["plan round 3: nits","code round 1: nits"],["plan round 3 (running)"],["review"]]`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("labels = %s, want %s", got, want)
	}
}

func TestBoardPageTimeline(t *testing.T) {
	page := boardPage(t)
	const begin, end = "// timeline: begin", "// timeline: end"
	i, j := strings.Index(page, begin), strings.Index(page, end)
	if i < 0 || j < i {
		t.Fatal("no timeline markers in the page")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const B = {stages: [['zach','Needs Zach'],['plan','Plan'],['building','Building'],['landed','Done']]};` + "\n" +
		page[i:j] + `
clock = iso => iso.slice(11, 16);
console.log(JSON.stringify([
  timeline({
    stages: [{stage:'plan', secs:120}, {stage:'zach', secs:300}, {stage:'building', secs:180}],
    zachSecs: 300,
    totalSecs: 600,
  }),
  timeline({
    stages: [{stage:'plan', secs:120}, {stage:'zach', secs:300}, {stage:'building', secs:180}, {stage:'landed', start:'2026-10-26T14:10:00Z'}],
    zachSecs: 300,
    totalSecs: 600,
  }),
  timeline({
    stages: [{stage:'plan', secs:120}, {stage:'zach', secs:180}, {stage:'building', secs:240}, {stage:'zach', secs:120}, {stage:'landed', start:'2026-10-26T14:11:00Z'}],
    zachSecs: 300,
    totalSecs: 660,
  }),
]));`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `["Plan 2m · Needs Zach 5m · Building 3m · 10m so far","Plan 2m · Needs Zach 5m · Building 3m · done 14:10 · 10m in all","Plan 2m · Needs Zach 3m · Building 4m · Needs Zach 2m · done 14:11 · 11m in all · 5m waiting on Zach"]`
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("timeline = %s, want %s", got, want)
	}
}

// Opening a card or its transcript marks it seen. The call is quiet: a read-only canary answers
// 403, and that must not toast, so it does not go through post().
func TestPagesMarkSeen(t *testing.T) {
	page := boardPage(t)
	open := regexp.MustCompile(`function openPanel\(id\)\{[^\n]*seen\(`)
	if !open.MatchString(page) {
		t.Error("openPanel does not call seen()")
	}
	f, _ := newTestFactory(t, card("c1", nil))
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/t/c1", nil))
	for name, p := range map[string]string{"board": page, "transcript": rec.Body.String()} {
		def := regexp.MustCompile(`function seen\([^)]*\)\{[^\n]*fetch\('[./]*api/seen'[^\n]*catch\(`).FindString(p)
		if def == "" {
			t.Errorf("%s: no quiet seen() posting api/seen", name)
		}
		if strings.Contains(def, "post(") || strings.Contains(def, "toast(") {
			t.Errorf("%s: seen() can toast: %s", name, def)
		}
	}
	if !regexp.MustCompile(`poll\(\)\.then\([^\n]*seen\(`).MatchString(rec.Body.String()) {
		t.Error("transcript page does not mark the card seen on load")
	}
}

func TestBoardPageAgentPickerFollowsSuggestedDefault(t *testing.T) {
	page := boardPage(t)
	for _, want := range []string{
		`delete $('nAgent').dataset.picked`,
		`$('nAgent').dataset.picked = '1'`,
		`($('nAgent').dataset.picked && $('nAgent').value) || AG.default`,
		`$('pAgent').dataset.picked = '1'`,
		`($('pAgent').dataset.picked && $('pAgent').value) || (c.idea&&c.idea.agent) || AG.default`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("board page missing %q", want)
		}
	}
}
