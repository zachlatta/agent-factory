package factory

import (
	"path"
	"strings"
)

// uiReviewRule is added to code and landing reviews of a change that touches a user interface: the
// reviewer runs it and clicks through it with computer use, and reports UI feedback in a
// block the factory forwards to the builder (uiFeedback).
const uiReviewRule = `

UI check: this change touches a user interface. Test it with computer use:
- Read the openai-computer-use skill first, and reach the computer-use browser as it describes.
- Run the builder's version from a scratch copy (/tmp), on test or fake data only: never the live service or
  production data. Serve it on this host's tailnet name (` + "`hostname`" + `), not 127.0.0.1, so the computer-use browser can reach it.
- Click through every flow the change touches, at desktop width and at phone width (about 390 px). Read the page
  text and take screenshots; check it does what the request asked.
- Blocking: a broken flow, a layout broken at either width, or console errors the change causes.
- Never click anything that sends, pays, deletes or changes real data.
- If it cannot run safely here, or the computer-use gateway is full, say why and skip the check; a skip alone is not
  a blocking finding.
- Close every tab you opened and stop every server you started before your final message.
- Put usability, copy and layout feedback (not blocking) in your final message, before the TIER line, as:
UI feedback:
- <one item per line>`

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
func uiFeedbackHoldMsg(fb string) string {
	return "Code review passed and the card now waits for Zach. The reviewer tested the UI and left this " +
		"non-blocking feedback. Do not change anything now. Zach reads your reply on his card: reply with the " +
		"DECISION:, OVERVIEW: and NEEDS FROM ZACH: lines for him (approve landing of the reviewed work), then stop. When you " +
		"land later, end your summary with this feedback under 'UI feedback:'.\n\n" + fb
}
