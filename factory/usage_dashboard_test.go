package factory

import (
	"os/exec"
	"strings"
	"testing"
)

func TestUsageDashboardDecisions(t *testing.T) {
	page := usagePage(t)
	i, j := strings.Index(page, "// usage helpers: begin"), strings.Index(page, "// usage helpers: end")
	if i < 0 || j < i {
		t.Fatal("missing helpers")
	}
	script := page[i:j] + `
const assert=require('node:assert/strict');
const now=Date.parse('2026-01-09T12:00:00Z');
const row=(day,total,extra={})=>({day,agent:'sample-agent',card:'sample-card',role:'fellow',stage:'building',counts:{input:total-1,output:1,total},...extra});
const rows=[row('2026-01-08',100),row('2026-01-02',50,{role:'code reviewer',stage:'review'}),row('2026-01-01',20),row('',30,{timeReason:'initial cumulative reading',stage:'unassigned'}),row('2026-01-09',10)];
const range=periodRange('week',now);
assert.equal(selectPeriod(rows,range).length,2);
assert.equal(sumCounts(selectPeriod(rows,range)).total,150);
assert.equal(sumCounts(selectPeriod(rows,previousRange(range))).total,20);
assert.equal(selectPeriod(rows,periodRange('today',now)).length,1);
assert.equal(selectPeriod(rows,periodRange('all',now)).length,5);
assert.equal(groupCounts(rows,'card')[0].total,210);
const stats=usageStats(rows);
assert.equal(stats.total,210);assert.equal(stats.review,50);assert.equal(stats.unassigned,30);assert.equal(stats.undated,30);
assert.equal(capacity({updatedAt:new Date(now).toISOString(),windows:[{label:'7d',remainingPct:80,resetsAt:'2026-01-10T12:00:00Z'}]},now).state,'ready');
assert.equal(capacity({updatedAt:'2026-01-08T12:00:00Z',windows:[{label:'7d',remainingPct:80}]},now).state,'stale');
assert.equal(capacity({updatedAt:new Date(now).toISOString(),windows:[{label:'5h',remainingPct:80,resetsAt:'2026-01-09T11:00:00Z'}]},now).state,'expired');
assert.equal(capacity({updatedAt:new Date(now).toISOString(),windows:[{label:'5h',remainingPct:0}]},now).state,'exhausted');
assert.equal(capacity({updatedAt:new Date(now).toISOString(),windows:[{label:'5h',remainingPct:10}]},now).state,'low');
assert.equal(capacity({windows:[]},now).state,'unknown');
assert.equal(capacity({estimated:true,updatedAt:new Date(now).toISOString(),windows:[{label:'7d',remainingPct:100}]},now).state,'estimated');
assert.ok(!plansHTML([{provider:'x',plan:'<img onerror=bad>',allowance:{}}]).includes('<img'));
assert.equal(selectRows(rows,{status:'missing'}).length,0);
assert.equal(selectRows(rows,{stage:'unassigned'}).length,1);
assert.equal(selectRows(rows,{query:'sample-agent'}).length,5);
const overview=plansHTML([
 {provider:'claude',plan:'Claude Max',allowance:{accounts:[{label:'Account 1',email:'first@example.com',active:true,updatedAt:'2026-01-01T00:00:00Z',windows:[{label:'7d',remainingPct:40},{label:'5h',remainingPct:75}]}]}},
 {provider:'codex',plan:'ChatGPT Pro',allowance:{updatedAt:'2026-01-01T00:00:00Z',windows:[{label:'7d',remainingPct:80}]}},
 {provider:'antigravity',plan:'Google AI Ultra',allowance:{}},
 {provider:'muse',plan:'Muse Code Power',allowance:{refreshError:'sample failure',updatedAt:'2026-01-01T00:00:00Z',windows:[{label:'7d',remainingPct:50}]}}
]);
for(const label of ['Claude Max','ChatGPT Pro','Google AI Ultra','Muse Code Power','60% used','25% used','40% left','Account 1','first@example.com','active','Older reading','Not reported','No reading'])assert.ok(overview.replaceAll('<wbr>','').includes(label),label);
assert.ok(overview.includes('first@<wbr>example.com'));
assert.ok(overview.includes('Refresh failed'));
assert.equal((overview.match(/class="subscription-row"/g)||[]).length,4);

const accounts=plansHTML([{provider:'claude',plan:'Claude Max',allowance:{accounts:[
 {label:'Account 1',active:true,refreshError:'Account refresh failed',updatedAt:new Date(now).toISOString(),windows:[{label:'7d',remainingPct:40}]},
 {label:'Account 2',updatedAt:new Date(now).toISOString(),windows:[{label:'7d',remainingPct:70}]}
]}}]);
assert.equal((accounts.match(/Refresh failed/g)||[]).length,1);

const wholeFailure={refreshError:'Provider refresh failed',accounts:[
 {label:'Account 1',refreshError:'Earlier account failure',windows:[{label:'7d',remainingPct:40}]},
 {label:'Account 2',active:true,updatedAt:new Date(now).toISOString(),windows:[{label:'7d',remainingPct:70}]}
]};
assert.equal(accountReading(wholeFailure,wholeFailure.accounts[1]).refreshError,'Provider refresh failed');

`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard: %v\n%s", err, out)
	}
}
