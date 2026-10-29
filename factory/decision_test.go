package factory

import (
	"os/exec"
	"strings"
	"testing"
)

// The board answers with one tap per choice, posting the same choice an email reply makes.
func TestDecisionChoices(t *testing.T) {
	page := boardPage(t)
	i, j := strings.Index(page, "// decision: begin"), strings.Index(page, "// decision: end")
	k, l := strings.Index(page, "// inbox: begin"), strings.Index(page, "// inbox: end")
	if i < 0 || j < i || k < 0 || l < k {
		t.Fatal("missing decision or inbox implementation")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const assert=require('assert');
let B={cards:[],repos:['example/one','example/two']}, sent=[], notices=[], sel=null, renders=0, timers=[];
const nodes={grid:{dataset:{}},inbox:{dataset:{}}}, $=id=>nodes[id], esc=s=>String(s??'').replaceAll('<','&lt;'), md=s=>s;
const post=async(a,b)=>{sent.push([a,b]);return {}}, toast=m=>notices.push(m), refresh=()=>{}, releaseKeyboard=()=>{};
const render=()=>{renders++}, fillAsk=()=>{}, setTimeout=(f)=>{timers.push(f);return timers.length}, clearTimeout=()=>{};
const findCard=id=>B.cards.find(c=>c.id===id), repoTag=()=>'', clock=()=>'18:00', ago=()=>'now';
` + page[i:j] + page[k:l] + `
(async()=>{
const ready={id:'r1',title:'Phone board',ask:{kind:'ready',title:'Try it?',overview:{decision:'Ship the phone board?'},
  choices:[{id:'land',label:'Land',primary:true},{id:'change',label:'Change',text:true,hint:'What should change?'},{id:'drop',label:'Drop'}]}};
const dec={id:'d1',title:'Store',ask:{kind:'decision',title:'Approve?',questions:[{text:'Store?',options:['Postgres (recommended)','SQLite']}],
  choices:[{id:'yes',label:'Yes',primary:true},{id:'no',label:'No',text:true},{id:'drop',label:'Drop'}]}};
const two={id:'d2',title:'Two',ask:{kind:'decision',title:'Two?',questions:[{text:'A?',options:['x','y (recommended)']},{text:'B?',options:['p','q']}],
  choices:[{id:'yes',label:'Yes',primary:true},{id:'no',label:'No',text:true}]}};
const repo={id:'p1',title:'Pick',ask:{kind:'decision',title:'Which repo?',choices:[{id:'pick:example/one',label:'Start in example/one',repo:'example/one'},{id:'pick',label:'Other repo…',text:true},{id:'drop',label:'Drop'}]}};
B.cards=[ready,dec,two,repo];
// card images come only from this card's own evidence routes: never external, data, file, another card's or a query URL
assert(allowedEvidenceSrc('/api/evidence/r1/before','r1')); assert(allowedEvidenceSrc('/api/evidence/r1/after','r1'));
for(const src of ['https://example.org/x.png','data:image/png;base64,x','file:///x.png','/api/evidence/other/before','/api/evidence/r1/before?x','//example.org/api/evidence/r1/before','/api/evidence/r1/../other/before'])assert(!allowedEvidenceSrc(src,'r1'),src);
// the images the board shows for a card use only those routes
const evCard={id:'r1',ask:{evidence:{before:'before.png',after:'after.png'}}};
for(const m of evidenceHtml(evCard).matchAll(/src="([^"]*)"/g)) assert(allowedEvidenceSrc(m[1],'r1'),m[1]);
// one tap lands: no Submit
let h=choicesHtml(ready,'l'); assert(!h.includes('Submit')); assert(h.includes('>Land</button>'));
await choose('r1','land','l'); assert.deepEqual(sent.at(-1),['choose',{id:'r1',choice:'land'}]);
// Change opens a box; Send needs text
await choose('r1','change','l'); assert.equal(sent.length,1); assert(choicesHtml(ready,'l').includes('id="say-l-r1"'));
nodes['say-l-r1']={value:''}; await choose('r1','change','l',true); assert.equal(sent.length,1); assert.match(notices.at(-1),/Write what should change/);
nodes['say-l-r1']={value:'Make it blue'}; await choose('r1','change','l',true); assert.deepEqual(sent.at(-1),['choose',{id:'r1',choice:'change',message:'Make it blue'}]);
// Drop needs a second tap in place, never a browser dialog
await choose('r1','drop','l'); assert.equal(sent.length,2); assert(choicesHtml(ready,'l').includes('Tap again to drop'));
await choose('r1','drop','l'); assert.deepEqual(sent.at(-1),['choose',{id:'r1',choice:'drop'}]);
// an armed Drop disarms on its own
await choose('r1','drop','l'); timers.at(-1)(); assert(!choicesHtml(ready,'l').includes('Tap again'));
// one question: the recommended option is picked; tapping another answers Yes with it
assert(questionsHtml(dec,'l').includes('class="opt on"'));
await askPick('d1',0,1,'l'); assert.equal(sent.at(-1)[1].choice,'yes'); assert.match(sent.at(-1)[1].message,/Store\?\n   → SQLite/);
// Yes alone sends the recommended answer
await choose('d1','yes','l'); assert.match(sent.at(-1)[1].message,/→ Postgres/);
// two questions: taps pick, Yes sends both
const before=sent.length; askPick('d2',1,1,'l'); assert.equal(sent.length,before);
await choose('d2','yes','l'); assert.match(sent.at(-1)[1].message,/A\?\n   → y \(recommended\)\n2\. B\?\n   → q/);
// No on an open question may go empty; No on a plan needs words
nodes['say-l-d1']={value:'',focus(){}}; const n0=sent.length; await choose('d1','no','l'); await choose('d1','no','l',true); assert.equal(sent.length,n0); assert.match(notices.at(-1),/Write what should change/);
await choose('d2','no','l'); nodes['say-l-d2']={value:'Do q instead'}; await choose('d2','no','l',true); assert.equal(sent.at(-1)[1].choice,'no'); assert.match(sent.at(-1)[1].message,/Do q instead/);
// an open question has no Yes or No: tapping an option answers it, Answer opens a box, empty is refused
const q={id:'q1',title:'Ask',ask:{kind:'question',title:'Waiting on you: question',questions:[{text:'Which port?',options:['8080 (recommended)','9090']}],
  choices:[{id:'answer',label:'Answer',primary:true,text:true},{id:'drop',label:'Drop'}]}};
const q0={id:'q0',title:'Ask',ask:{kind:'question',title:'Waiting on you: question',choices:q.ask.choices}};
B.cards.push(q,q0); assert(!choicesHtml(q,'l').includes('>Yes<')); assert(!choicesHtml(q,'l').includes('>No<'));
await askPick('q1',0,1,'l'); assert.equal(sent.at(-1)[1].choice,'answer'); assert.match(sent.at(-1)[1].message,/Which port\?\n   → 9090/);
await choose('q0','answer','l'); assert(choicesHtml(q0,'l').includes('id="say-l-q0"'));
nodes['say-l-q0']={value:''}; const nq=sent.length; await choose('q0','answer','l',true); assert.equal(sent.length,nq); assert.match(notices.at(-1),/Write your answer/);
nodes['say-l-q0']={value:'Use the staging host'}; await choose('q0','answer','l',true); assert.deepEqual(sent.at(-1),['choose',{id:'q0',choice:'answer',message:'Use the staging host'}]);
// a repo: one tap starts it there; Other opens a picker
await choose('p1','pick:example/one','l'); assert.deepEqual(sent.at(-1),['choose',{id:'p1',choice:'pick:example/one'}]);
await choose('p1','pick','l'); assert(choicesHtml(repo,'l').includes('id="repo-l-p1"'));
nodes['repo-l-p1']={value:'example/two'}; nodes['say-l-p1']={value:''}; await choose('p1','pick','l',true);
assert.equal(sent.at(-1)[1].repo,'example/two');
// the list: For you first, then working (stuck first), shipped today, no answer; routine behind its filter
const S={forYou:[{id:'r1',title:'Phone board',state:'Ship the phone board?'}],working:[{id:'w',title:'Retries',state:'building'}],
  stuck:[{id:'s',title:'Hung',state:'no activity for 40 min'}],shipped:[{id:'x',title:'Done thing',state:'shipped',at:'2026-10-27T15:00:00Z'}],
  noAnswer:[{title:'Order batteries',state:'no answer yet'}],routine:[{id:'q',title:'Hourly feed',state:'done'}]};
const out=inboxHtml(S,''); const o=t=>out.html.indexOf(t);
// tapping a For you card's title or question opens the card
const fyHtml=forYouHtml(ready); assert.equal((fyHtml.match(/onclick="event.preventDefault\(\);openPanel\('r1'\)"/g)||[]).length,2,fyHtml);
assert(/<a class="ft"[^>]*>.*Phone board.*<\/a>/.test(fyHtml),fyHtml);
assert.equal(out.forYou,1); assert(o('For you')<o('Ship the phone board?')); assert(o('Ship the phone board?')<o('No answer yet'));
assert(o('No answer yet')<o('Working on it')); assert(o('Hung')<o('Retries')); assert(o('Retries')<o('Shipped today'));
assert(out.html.includes('Routine runs (1)')); assert(!out.html.includes('Hourly feed'));
showRoutine=true; assert(inboxHtml(S,'').html.includes('Hourly feed'));
assert(inboxHtml(S,'retries').html.includes('Retries')); assert(!inboxHtml(S,'retries').html.includes('Hung'));
console.log('choices passed');
})().catch(e=>{console.error(e);process.exitCode=1;});`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
