package factory

import (
	"os/exec"
	"strings"
	"testing"
)

func TestThinkingPickerResetsAndPreserves(t *testing.T) {
	page := boardPage(t)
	i, j := strings.Index(page, "// thinkingPicker: begin"), strings.Index(page, "// thinkingPicker: end")
	if i < 0 || j < i {
		t.Fatal("missing thinking picker")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const assert=require('node:assert/strict');
 const AG={agents:[{id:'opus',thinking:'medium',thinkingOptions:['low','medium','high']},{id:'codex-sol',thinking:'medium',thinkingOptions:['low','medium','high','ultra']},{id:'gemini-argon'}]};
 const els={};const $=id=>els[id]||(els[id]={value:'',dataset:{},hidden:false,innerHTML:''});const esc=s=>s;
 ` + page[i:j] + `
 fillThinking('nThinking','opus');assert.equal($('nThinking').value,'medium');
 $('nThinking').value='high';fillThinking('nThinking','opus');assert.equal($('nThinking').value,'high');
 fillThinking('nThinking','codex-sol');assert.equal($('nThinking').value,'medium');
 fillThinking('nThinking','gemini-argon');assert.equal($('nThinking').value,'');assert.equal($('nThinkingRow').hidden,true);
 fillThinking('nThinking','opus');assert.equal($('nThinkingRow').hidden,false);assert.equal($('nThinking').value,'medium');
 fillThinking('pThinking','codex-sol','ultra');assert.equal($('pThinking').value,'ultra');
 fillThinking('pThinking','opus','ultra');assert.equal($('pThinking').value,'medium');
 `
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("picker: %v\n%s", err, out)
	}
}
