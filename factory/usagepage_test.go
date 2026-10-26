package factory

import (
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func usagePage(t *testing.T) string {
	t.Helper()
	f, _ := newTestFactory(t)
	rec := httptest.NewRecorder()
	f.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/usage", nil))
	if rec.Code != 200 {
		t.Fatalf("usage page: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatal("not HTML")
	}
	return rec.Body.String()
}

func TestUsagePageRouteAndNavigation(t *testing.T) {
	page := usagePage(t)
	if !strings.Contains(boardPage(t), `href="/usage"`) {
		t.Fatal("missing board link")
	}
	for _, want := range []string{`href="/"`, "Subscription allowances", "Recorded local tokens", "across devices", "bounded", "Historical account attribution is unknown", "Initial Codex", "built-in sub-agent", "unassigned", "Cache", "Reasoning", "input plus output", `id="filters"`, `role="status"`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(page, `href="/timing`) {
		t.Fatal("links to nonexistent timing page")
	}
}

func TestUsagePageBehavior(t *testing.T) {
	page := usagePage(t)
	i, j := strings.Index(page, "// usage helpers: begin"), strings.Index(page, "// usage helpers: end")
	if i < 0 || j < i {
		t.Fatal("missing usage helpers")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node required for page behavior tests")
	}
	script := page[i:j] + `
const assert = require('node:assert/strict');
const counts = {input:100,output:20,cacheRead:60,cacheWrite:10,reasoning:5,total:120};
const row = {provider:'claude',plan:'Plan A',model:'model-a',role:'fellow',card:'card-a',cardTitle:'Example card',agent:'agent-a',title:'Example agent',stage:'building',span:2,round:0,status:'readable',counts};
const rows = [row,{...row,span:4,counts:{...counts,input:10,output:2,total:12}},
 {...row,agent:'agent-b',role:'code reviewer',stage:'review',span:5,round:2,status:'error',sharedSession:true},
 {...row,agent:'old-agent',card:'',stage:'unassigned',span:0,status:'missing',counts:{}}];
const view = {rows,stages:[['building','Building','Agent is working.'],['review','Code review','Checks the work.']],coverage:{agents:4,readableSessions:2,missingSessions:1,unsupportedSessions:3,errors:4,duplicateSessions:5,timingWithoutAgent:6,disappearedSources:7,limitedSessions:8,limited:true}};
assert.equal(selectRows(rows,{role:'fellow',stage:'building'}).length,2);
for (const key of ['provider','plan','model','role','card','agent','stage']) {
 assert.ok(selectRows(rows,{[key]:'absent'}).length===0,key);
}
assert.equal(sumCounts(selectRows(rows,{stage:'building'})).total,132);
assert.equal(sumCounts([row]).total,120); // cache/reasoning are never added to total
assert.equal(sumCounts([row]).input,100);
const html=rowsHTML(rows,view.stages,new Set(['agent-a']));
for(const word of ['span 2','span 4','round 2','error','shared session','unassigned','missing','Transcript unavailable','/t/agent-a','Code review']) assert.ok(html.includes(word),word);
assert.ok(!html.includes('/t/old-agent'));
assert.ok(!rowsHTML([{...row,title:'<img src=x onerror=bad>'}],[],new Set()).includes('<img'));
const cov=coverageText(view.coverage);
for(const word of ['bounded','4 errors','1 missing','3 unsupported','5 duplicate','6 timing','7 disappeared','8 limit-skipped']) assert.ok(cov.includes(word),word);
const allowance={summary:'50% left',updatedAt:'2026-01-01T00:00:00Z',windows:[{label:'7d',remainingPct:50,resetsAt:'2026-01-07T00:00:00Z'}]};
const plans=[{provider:'claude',plan:'Plan A',allowance},{provider:'claude',plan:'Plan A',allowance}];
assert.equal((plansHTML(plans).match(/<article/g)||[]).length,1);
assert.ok(plansHTML(plans).includes('50% remaining'));
assert.ok(plansHTML(plans).includes('Resets'));
assert.ok(plansHTML([{provider:'codex',plan:'Plan B',allowance:{}}]).includes('unknown'));
const accountHTML=plansHTML([{...plans[0],allowance:{...allowance,accounts:[{label:'Account 1',active:true,...allowance}]}}]);
assert.ok(accountHTML.includes('Account 1'));
assert.equal((accountHTML.match(/50% remaining/g)||[]).length,1);
assert.ok(snapshotText({refreshing:true}).includes('Waiting'));
assert.ok(snapshotText({updatedAt:'2026-01-01T00:00:00Z',refreshing:true}).includes('Refreshing'));
assert.ok(snapshotText({updatedAt:'2026-01-01T00:00:00Z'}).includes('old'));
(async()=>{
 global.fetch=async()=>({ok:false,status:503,json:async()=>{throw Error('must not parse')}});
 await assert.rejects(()=>fetchJSON('/api/usage'),/503/);
 global.fetch=async()=>{throw Error('offline')};
 await assert.rejects(()=>fetchJSON('/api/usage'),/offline/);
 global.fetch=async()=>({ok:true,json:async()=>view});
 assert.equal((await fetchJSON('/api/usage')).coverage.errors,4);
})().catch(e=>{console.error(e);process.exitCode=1});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("page behavior: %v\n%s", err, out)
	}
}

func TestUsagePageRefreshAndFilters(t *testing.T) {
	page := usagePage(t)
	script := strings.Split(strings.Split(page, "<script>")[1], "</script>")[0]
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node required for page behavior tests")
	}
	harness := `
const assert=require('node:assert/strict');
const elements=new Map();
global.document={activeElement:null,getElementById(id){
 if(!elements.has(id))elements.set(id,{id,textContent:'',innerHTML:'',hidden:true,listeners:{},addEventListener(k,fn){this.listeners[k]=fn},focus(){}});
 return elements.get(id);
}};
let pending,failUsage=false,failBoard=false;
global.setTimeout=fn=>{pending=fn};
const fixture={updatedAt:'2026-01-01T00:00:00Z',refreshing:true,plans:[],stages:[],coverage:{errors:3,limited:true},rows:[
 {agent:'a',card:'a',plan:'Demo',role:'fellow',model:'demo',provider:'claude',stage:'building',span:1,status:'readable',counts:{input:100,output:20,total:120}},
 {agent:'b',plan:'Demo',role:'picker',model:'demo',provider:'claude',stage:'unassigned',status:'unsupported',counts:{}}]};
global.fetch=async url=>{
 if(url==='/api/usage'&&failUsage)throw Error('offline');
 if(url==='/api/board'&&failBoard)throw Error('board offline');
 return {ok:true,json:async()=>url==='/api/usage'?fixture:{cards:[{id:'a',children:[]}]}};
};
`
	checks := `
(async()=>{
 // Complete the first asynchronous load started by the page.
 for(let i=0;i<12;i++)await Promise.resolve();
 const el=id=>document.getElementById(id);
 assert.ok(el('coverage').textContent.includes('3 errors'));
 assert.equal(el('loadError').hidden,true); // collection errors are not fetch failures
 assert.ok(el('snapshot').textContent.includes('Refreshing'));
 assert.ok(el('rows').innerHTML.includes('/t/a'));
 el('filters').listeners.change({target:{dataset:{key:'role'},value:'picker'}});
 assert.ok(el('totals').textContent.startsWith('1 matching rows'));
 assert.ok(!el('rows').innerHTML.includes('/t/a'));
 el('filters').listeners.click({target:{id:'clearFilters'}});
 assert.ok(el('totals').textContent.startsWith('2 matching rows'));
 failUsage=true;failBoard=true;
 await pending();
 assert.equal(el('loadError').hidden,false);
 assert.ok(el('loadError').textContent.includes('last successful snapshot'));
 assert.ok(el('rows').innerHTML.includes('Transcript unavailable'));
 assert.ok(!el('rows').innerHTML.includes('/t/a'));
 failUsage=false;failBoard=false;fixture.refreshing=false;
 await pending();
 assert.equal(el('loadError').hidden,true);
 assert.ok(!el('snapshot').textContent.includes('Refreshing'));
 assert.ok(el('rows').innerHTML.includes('/t/a'));
 const savedAt=fixture.updatedAt;delete fixture.updatedAt;fixture.refreshing=true;fixture.rows=[];
 await pending();
 assert.ok(el('totals').textContent.includes('Waiting'));
 assert.ok(!el('totals').textContent.includes('0 recorded tokens'));
 assert.ok(el('coverage').textContent.includes('Waiting'));
 fixture.updatedAt=savedAt;
 // A failed first fetch must not claim zero recorded tokens.
 view=null;failUsage=true;
 await pending();
 assert.ok(el('loadError').textContent.includes('No snapshot available'));
})().catch(e=>{console.error(e);process.exitCode=1});
`
	out, err := exec.Command(node, "-e", harness+script+checks).CombinedOutput()
	if err != nil {
		t.Fatalf("refresh/filter UI: %v\n%s", err, out)
	}
}
