package factory

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDecisionSubmission(t *testing.T) {
	page := boardPage(t)
	i, j := strings.Index(page, "// decision: begin"), strings.Index(page, "// decision: end")
	if i < 0 || j < i {
		t.Fatal("missing decision implementation")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const assert=require('assert');
let B={cards:[],repos:['example/repo']}, sent=[], notices=[], confirmations=0, acceptDrop=false;
const nodes={}, $=id=>nodes[id], esc=s=>String(s??'').replaceAll('<','&lt;'), md=s=>s;
const document={querySelector:()=>null}, post=async(a,b)=>sent.push([a,b]);
const toast=m=>notices.push(m), refresh=()=>{}, releaseKeyboard=()=>{}, confirm=()=>{confirmations++;return acceptDrop};
nodes.grid={dataset:{}};
` + page[i:j] + `
(async()=>{
const c={id:'c1',ask:{request:{Base:'Original task'},title:'Approve?',why:'Gate',actions:[{id:'approve',label:'Approve plan'},{id:'reply',label:'Changes'},{id:'close',label:'Drop it'}],questions:[{text:'Store?',options:['Cache','Disk']},{text:'Color?',options:['Blue','Green']} ]}};
B.cards=[c]; nodes['r-c1']={value:''};
let html=askHtml(c); assert(html.includes('Original task'));assert(html.includes('Needs your input'));assert(!html.includes('askYesNo'));assert(!html.includes('open>'));
await askSubmit('c1');assert.equal(sent.length,0);
askPicks.c1={0:0,1:'o'};nodes['o-c1-1']={value:''};askActions.c1=0;
await askSubmit('c1');assert.equal(sent.length,0);
nodes['o-c1-1'].value='Purple';await askSubmit('c1');assert.equal(sent[0][0],'approve');assert(sent[0][1].message.includes('Purple'));
askPicks.c1={0:1,1:0};askActions.c1='other';nodes['r-c1'].value='Revise first';await askSubmit('c1');assert.equal(sent[1][0],'send');assert(sent[1][1].message.includes('Revise first'));
askPicks.c1={0:1,1:0};askActions.c1=2;await askSubmit('c1');assert.equal(confirmations,1);assert.equal(sent.length,2);
acceptDrop=true;await askSubmit('c1');assert.equal(sent.at(-1)[0],'close');
for(const action of ['recommend','rerun','stop','done']) {c.ask.actions=[{id:action,label:action}];c.ask.questions=[];delete askActions.c1;await askSubmit('c1');assert.equal(sent.at(-1)[0],action)}
c.routing=true;c.ask.actions=[{id:'approve',label:'Repo',repo:'example/repo'},{id:'pickrepo',label:'Other repo'}];askActions.c1=0;await askSubmit('c1');assert.equal(sent.at(-1)[1].repo,'example/repo');askActions.c1=1;nodes['p-c1']={value:'example/repo'};await askSubmit('c1');assert.equal(sent.at(-1)[0],'approve');assert.equal(sent.at(-1)[1].repo,'example/repo');
assert(allowedEvidenceSrc('/api/evidence/c1/before','c1'));for(const src of ['https://example.org/x.png','data:image/png;base64,x','file:///x.png','/api/evidence/other/before','/api/evidence/c1/before?x'])assert(!allowedEvidenceSrc(src,'c1'));
c.ask.request=null;assert(askHtml(c).includes('Original task unavailable'));
})().catch(e=>{console.error(e);process.exit(1)});`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
