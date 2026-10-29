package factory

import (
	"path"
	"strings"
)

// uiReviewRule is added to code and landing reviews of a change that touches a user interface: the
// reviewer runs it and clicks through it with computer use, and reports UI feedback in a
// block the factory forwards to the builder (uiFeedback).
const uiReviewRule = "\n\n" + computerUseRule + `
UI check: this change touches a user interface. Test it with computer use:
- Run the builder's version from a scratch copy in your worktree's .factory-scratch/ (listed in .git/info/exclude),
  using the safe test setup above. Stop every server you start: a factory -dry-run deletes its copied state when it stops.
- Click through every flow the change touches, at desktop width and at phone width (about 390 px). Read the page
  text and take screenshots; check it does what the request asked.
- Blocking: a broken flow, a layout broken at either width, or console errors the change causes.
- Never click anything that sends, pays, deletes or changes real data, except inside such a dry run, where clicks
  are recorded and not run.
- If it cannot run safely here, or the computer-use gateway is full, say why and skip the check; a skip alone is not
  a blocking finding.
- Put usability, copy and layout feedback (not blocking) in your final message, before the TIER line, as:
UI feedback:
- <one item per line>`

// boardLayoutRule applies only to this repo; other repos retain uiReviewRule.
const boardLayoutRule = "\n\n" + computerUseRule + `
UI check: this change touches the agent-factory board.
- Follow README's Board layout checks. Run npm run board:screenshot -- --stage=zach
  and add --card=preview-zach to inspect the panel. Inspect both 1280x800 and 390x844
  PNGs and saved page text in .factory-evidence; use other fixture stages/cards as needed.
- Use computer use for every changed interactive flow on fake data. Read the current
  openai-computer-use skill first. Start cmd/board-preview bound to this host's tailnet IP
  from ` + "`tailscale ip -4`" + ` so the remote browser can reach it. The layout preview
  only simulates sample-card actions; use the README's documented dry run to verify real action flows.
- Bind direct remote checks to the tailnet IP, never 0.0.0.0.
- Phone layout is checked with the helper's exact viewport; computer use checks interaction.
  Screenshots alone do not verify an approval, Drop confirmation, or other interactive flow.
- Blocking: a broken flow, broken layout at either width, or console errors caused by this change.
- Report failed or skipped checks and the exact reason; never call an incomplete check a pass.
- Close your tabs and stop your servers. Keep captures in excluded .factory-evidence.
- Put non-blocking usability, copy and layout feedback before TIER as:
UI feedback:
- <one item per line>`

// Included in the shared Fellow template as a conditional instruction, without
// teaching agents working in other repos to run this repo's helper.
const boardBuilderRule = `For UI changes in zachlatta/agent-factory, follow README's Board layout checks:
run npm run board:screenshot -- --stage=zach (add --card=preview-zach for the panel),
inspect desktop and phone captures and page text, and use computer use for changed
interactive flows on fake data or a documented dry run. The helper checks layout;
it does not prove mutations work. In other repos follow their UI checking instructions.`

// uiExt and uiDir name the files that make a change a frontend change.
var (
	uiExt = map[string]bool{".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true, ".less": true,
		".js": true, ".jsx": true, ".mjs": true, ".ts": true, ".tsx": true, ".vue": true, ".svelte": true,
		".astro": true, ".gohtml": true, ".tmpl": true}
	uiDir = map[string]bool{"web": true, "frontend": true, "ui": true, "static": true, "templates": true, "public": true}
)

// touchesUI reports whether any of the changed files is frontend code.
func touchesUI(files []string) bool {
	for _, f := range files {
		if uiExt[strings.ToLower(path.Ext(f))] {
			return true
		}
		dirs := strings.Split(f, "/")
		for _, d := range dirs[:len(dirs)-1] {
			if uiDir[d] {
				return true
			}
		}
	}
	return false
}

// uiChange reports whether c's worktree changed frontend files. When git cannot say, the UI rule is
// included: the reviewer skips the check if nothing visible changed.
func (f *Factory) uiChange(c Card) bool {
	pf := f.peerFiles(c.Cwd, f.Now())
	if pf.err != "" {
		f.logf("%s: changed files unknown (%s), UI rule included", short(c.ID), pf.err)
		return true
	}
	return touchesUI(pf.files)
}

const uiFeedbackCap = 1500

// uiFeedback returns the last "UI feedback:" block of a reviewer's notes, normalized to "- " bullets, or
// "" when there is none or it has no bullets.
func uiFeedback(notes string) string {
	lines := strings.Split(notes, "\n")
	start := -1
	for i, l := range lines {
		h := strings.ToLower(strings.Trim(strings.TrimSpace(l), "#* "))
		if h == "ui feedback" || h == "ui feedback:" {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	var items []string
	for _, l := range lines[start+1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			if len(items) > 0 {
				break
			}
			continue
		}
		item, ok := strings.CutPrefix(l, "- ")
		if !ok {
			item, ok = strings.CutPrefix(l, "* ")
		}
		if !ok {
			break
		}
		items = append(items, "- "+strings.TrimSpace(item))
	}
	if len(items) == 0 {
		return ""
	}
	return trunc("UI feedback:\n"+strings.Join(items, "\n"), uiFeedbackCap)
}

// uiFeedbackLandMsg goes after the landing message when the reviewer left UI feedback.
func uiFeedbackLandMsg(fb string) string {
	return "\n\nThe code reviewer tested the UI and left this non-blocking feedback. Do not change code for it. " +
		"After you land, end your summary with it under 'UI feedback:', and include it in your email to Zach if " +
		"there is one.\n\n" + fb
}

// uiFeedbackHoldMsg tells a builder whose card waits for Zach about the reviewer's UI feedback.
// uiFeedbackPreviewNote carries a UI reviewer's feedback in the message that asks the builder for Zach's Ready for you.
const uiFeedbackPreviewNote = "\n\nThe code reviewer tested the UI and left this non-blocking feedback. Do not change anything " +
	"for it now: show it to Zach in your message, and when you land later, end your summary with it under 'UI feedback:'.\n\n"

func uiFeedbackHoldMsg(fb string) string {
	return "Code review passed and the card now waits for Zach. The reviewer tested the UI and left this " +
		"non-blocking feedback. Do not change anything now. Zach reads your reply on his card: reply with the " +
		"DECISION:, OVERVIEW: and NEEDS FROM ZACH: lines for him (approve landing of the reviewed work), then stop. When you " +
		"land later, end your summary with this feedback under 'UI feedback:'.\n\n" + fb
}
