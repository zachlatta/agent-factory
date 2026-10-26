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
askPicks.c1={};askActions.c1='other';nodes['r-c1'].value='Revise first';await askSubmit('c1');assert.equal(sent[1][0],'send');assert(sent[1][1].message.includes('Revise first'));
askPicks.c1={};askActions.c1=2;await askSubmit('c1');assert.equal(confirmations,1);assert.equal(sent.length,2);
acceptDrop=true;await askSubmit('c1');assert.equal(sent.at(-1)[0],'close');
for(const action of ['recommend','rerun','stop','done']) {
 c.ask.actions=[{id:action,label:action}];c.ask.questions=[];delete askActions.c1;
 const before=sent.length;await askSubmit('c1');assert.equal(sent.length,before,'must choose '+action);
 assert(askHtml(c).includes('data-action="0"'));nodes['r-c1'].value='';askActions.c1=0;
 await askSubmit('c1');assert.equal(sent.at(-1)[0],action);
}
// Message-ignoring actions must never silently swallow complete or partial answers.
for(const action of ['done','rerun','stop','close','pickrepo']) {
 c.ask.actions=[{id:action,label:action}];c.ask.questions=[{text:'Store?',options:['Postgres','SQLite']},{text:'Color?',options:['Blue','Green']}];
 askPicks.c1={0:0};askActions.c1=0;
 const before=sent.length;await askSubmit('c1');assert.equal(sent.length,before,'lost answers on '+action);
 askPicks.c1={0:0,1:1};await askSubmit('c1');assert.equal(sent.length,before,'lost complete answers on '+action);
 askPicks.c1={};nodes['r-c1'].value='Keep this instruction';await askSubmit('c1');assert.equal(sent.length,before,'lost draft on '+action);nodes['r-c1'].value='';
}
c.ask.actions=[{id:'done',label:'Mark done'},{id:'reply',label:'Send it on',primary:true,hint:'What remains?'}];
askActions.c1=1;askPicks.c1={0:0};nodes['r-c1'].value='Finish the rest';
await askSubmit('c1');assert.equal(sent.at(-1)[0],'send');assert(sent.at(-1)[1].message.includes('Postgres'));assert(sent.at(-1)[1].message.includes('Finish the rest'));
askActions.c1=1;html=askHtml(c);assert(html.includes('Send it on (recommended)'));assert(html.includes('What remains?'));
// Answer-only reply sends the completed choices without requiring extra prose.
c.ask.questions=[{text:'Store?',options:['Postgres','SQLite']}];askPicks.c1={0:0};askActions.c1=1;nodes['r-c1'].value='';
await askSubmit('c1');assert.equal(sent.at(-1)[0],'send');assert(sent.at(-1)[1].message.includes('Postgres'));
// A card with only Done still has a way to send answers separately.
c.ask.actions=[{id:'done',label:'Mark done'}];assert(decisionChoices(c.ask).some(([x])=>x.id==='reply'));
c.ask.questions=[];nodes['r-c1'].value='';

c.routing=true;c.ask.actions=[{id:'approve',label:'Repo',repo:'example/repo'},{id:'pickrepo',label:'Other repo'}];askActions.c1=0;await askSubmit('c1');assert.equal(sent.at(-1)[1].repo,'example/repo');askActions.c1=1;nodes['p-c1']={value:'example/repo'};await askSubmit('c1');assert.equal(sent.at(-1)[0],'approve');assert.equal(sent.at(-1)[1].repo,'example/repo');
assert(allowedEvidenceSrc('/api/evidence/c1/before','c1'));for(const src of ['https://example.org/x.png','data:image/png;base64,x','file:///x.png','/api/evidence/other/before','/api/evidence/c1/before?x'])assert(!allowedEvidenceSrc(src,'c1'));
c.ask.request=null;assert(askHtml(c).includes('Original task unavailable'));
c.routing=false;c.ask.actions=[{id:'reply',label:'Reply'},{id:'done',label:'Mark done'}];c.ask.yes='say';c.ask.no='say';
html=askHtml(c);assert(html.includes('>Yes</button>'));assert(html.includes('>No</button>'));assert(html.includes("askChoose('c1','yes')"));
askActions.c1='yes';await askSubmit('c1');assert.equal(sent.at(-1)[0],'send');assert.equal(sent.at(-1)[1].message,'Yes.');
askActions.c1='no';await askSubmit('c1');assert.equal(sent.at(-1)[1].message,'No.');
// A brief's as-is approval deliberately ignores the open questions.
const brief={id:'brief',ask:{actions:[{id:'approve',label:'Land as it is'},{id:'recommend',label:'Apply recommendations'},{id:'reply',label:'Reply'}],questions:[{text:'Store?',options:['Postgres','SQLite']},{text:'Color?',options:['Blue','Green']}]}};
B.cards=[brief];nodes['r-brief']={value:''};askActions.brief=0;
await askSubmit('brief');assert.equal(sent.at(-1)[0],'approve');assert.deepEqual(sent.at(-1)[1],{id:'brief'});
for(const picks of [{0:0},{0:0,1:1}]) {
 askActions.brief=0;askPicks.brief=picks;const before=sent.length;
 await askSubmit('brief');assert.equal(sent.length,before,'as-is must not swallow answers');
}
askPicks.brief={};nodes['r-brief'].value='Preserve these instructions';
const beforeDraft=sent.length;await askSubmit('brief');assert.equal(sent.length,beforeDraft);assert(notices.at(-1).includes('reply option'));
nodes['r-brief'].value='';askActions.brief=1;askPicks.brief={0:0,1:1};
await askSubmit('brief');assert.equal(sent.at(-1)[0],'recommend');assert(sent.at(-1)[1].message.includes('Postgres'));
html=askHtml({id:'c2',ask:{request:{Base:'Unique task text'},overview:{overview:'Overview text'},actions:[],questions:[]}});assert.equal(html.split('Unique task text').length-1,1);assert(html.includes('<p class="gist">Overview text</p>'));

})().catch(e=>{console.error(e);process.exit(1)});`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
