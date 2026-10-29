import { test } from 'node:test';
import assert from 'node:assert/strict';
import { access, mkdtemp, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { capture, withPreview } from './board-screenshot.mjs';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { chromium } from 'playwright';

// Run against the real embedded page and real API, with the preview's fake inputs.
test('For you and card links render at exact widths and preserve navigation', async () => {
  const out = await mkdtemp('.factory-scratch/capture-test-');
  try {
    await withPreview(async url => {
      const results = await capture(url, { card: 'preview-zach', out });
      for (const [i, name] of ['desktop', 'phone'].entries()) {
        const png = await readFile(join(out, `${name}.png`));
        assert.equal(png.readUInt32BE(16), [1280, 390][i]);
        assert.equal(png.readUInt32BE(20), [800, 844][i]);
      }
      assert.equal(results.length, 3);
      assert.deepEqual(results.map(r => r.view), ['inbox', 'inbox', 'inbox']);
      const questions=await readFile(join(out,'phone-questions.png'));
      assert.equal(questions.readUInt32BE(16),390);
      assert.equal(questions.readUInt32BE(20),844);
      assert.match(await readFile(join(out,'phone-questions.txt'),'utf8'),/one extra tap is enough/);
      assert.match(await readFile(join(out,'phone.txt'),'utf8'),/FOR YOU/i);
      // the desktop kanban is a view; the phone stays on For you
      const columns = await capture(url, { view: 'columns', out });
      assert.deepEqual(columns.slice(0, 2).map(r => r.view), ['columns', 'inbox']);
      await assert.rejects(capture(url, { view: 'missing', out }), /Unknown view/);
      await assert.rejects(capture(url, { card: 'missing', out }), /Unknown card/);
      const browser = await chromium.launch({ headless: true });
      try {
        const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
        await page.goto(url);
        await page.locator('#inbox .sec').first().waitFor();
        assert.equal(await page.locator('.tabs').count(), 0);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
        // the decision is the one asked before building: allow a side effect, with its plain reason in the card
        assert.equal(await page.locator('.fy[data-id="preview-zach"] .fk').innerText(), 'Allow the build');
        assert.match(await page.locator('.fy[data-id="preview-zach"] a.fq').innerText(), /restart the test board service/);
        // tapping a For you card's title opens it; a working card's line opens it too
        await page.locator('.fy[data-id="preview-zach"] a.ft').click();
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pAsk').innerText(), /card list load faster[\s\S]*down for about ten seconds/);
        await page.locator('#panel > .x').click();
        await page.locator('#panel.open').waitFor({ state: 'hidden' });
        await page.locator('.line[data-id="preview-building"]').click();
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Building:/);
        await page.locator('#panel > .x').click();
        await page.locator('#panel.open').waitFor({ state: 'hidden' });
        // the same line is a link a keyboard reaches and opens with Enter
        const line = page.locator('a.line[data-id="preview-building"]');
        assert.equal(await line.getAttribute('href'), '?card=preview-building');
        await line.focus();
        await page.keyboard.press('Enter');
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Building:/);
        await page.locator('#panel > .x').click();
        await page.locator('#panel.open').waitFor({ state: 'hidden' });
        // No on a decision answers in one tap, with no box and no text, as an emailed "no" does
        const no = page.waitForRequest(r => r.url().endsWith('/api/choose'));
        await page.locator('.fy[data-id="preview-zach"] button[data-choice="no"]').click();
        assert.deepEqual(JSON.parse((await no).postData()), { id: 'preview-zach', choice: 'no' });
        assert.equal(await page.locator('.fy[data-id="preview-zach"] textarea').count(), 0);
        // and the answered decision leaves For you for Working on it, as the real send moves it
        await page.locator('.fy[data-id="preview-zach"]').waitFor({ state: 'detached' });
        await page.locator('#inbox a.line[data-id="preview-zach"]').waitFor();
        await page.evaluate(() => load());
        assert.equal(await page.locator('.fy[data-id="preview-zach"]').count(), 0);
        // a request with no answer keeps Dismiss beside its status on the phone, and Dismiss removes it
        // from No answer yet; a reload (the next board poll) does not bring it back
        const request = page.locator('#inbox .line.stuck', { hasText: 'Order more desk lamps' });
        const dismiss = request.locator('.ls button.link');
        assert.equal(await dismiss.innerText(), 'Dismiss');
        const [rowBox, buttonBox] = [await request.boundingBox(), await dismiss.boundingBox()];
        assert.ok(buttonBox.x + buttonBox.width <= rowBox.x + rowBox.width && buttonBox.y >= rowBox.y);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
        const dismissed = page.waitForRequest(r => r.url().endsWith('/api/dismiss-request'));
        await dismiss.click();
        assert.deepEqual(JSON.parse((await dismissed).postData()), { id: 'sample-request-1' });
        await request.waitFor({ state: 'detached' });
        await page.evaluate(() => load());
        assert.equal(await page.locator('#inbox .line', { hasText: 'Order more desk lamps' }).count(), 0);
        assert.equal(await page.locator('#inbox .line.stuck', { hasText: 'Move the team call' }).count(), 1);
        // the sample is marked as one on the page itself, not only in the tab title
        assert.match(await page.locator('#previewNote').innerText(), /sample cards/i);
        assert.equal(await page.locator('#previewNote').isVisible(), true);
        // each For you card names the kind of input it wants: finished work to try, a build to allow
        assert.equal(await page.locator('.fy[data-id="preview-ready"] .fk').innerText(), 'Ready: try it, then land');
        // work that landed with one thing to try: its label, the thing to do, the test buttons, and no "None"
        const tryCard = page.locator('.fy[data-id="preview-try"]');
        assert.equal(await tryCard.locator('.fk').innerText(), 'Work landed. Try it:');
        assert.equal(await tryCard.locator('a.fq').innerText(), 'Try it, then say if it works');
        assert.match(await tryCard.locator('.todo').innerText(), /usage page on your phone/);
        // the thing to try is in a green box, as its label is
        assert.equal(await tryCard.locator('.todo').evaluate(e => getComputedStyle(e).backgroundColor), 'rgb(218, 251, 225)');
        assert.deepEqual(await tryCard.locator('.choices button').allInnerTexts(), ['It works, mark done', "It's broken", 'Skip testing, mark done']);
        assert.doesNotMatch(await tryCard.innerText(), /\bNone\b/);
        // a button's words fit inside it at phone width: none is wider than its button
        assert.equal(await tryCard.locator('.choices button').evaluateAll(bs => bs.every(b => b.scrollWidth <= b.clientWidth)), true);
        // finished work starts with no option picked: Land sends no answers; its options say they are optional
        assert.equal(await page.locator('.fy[data-id="preview-ready"] .opt.on').count(), 0);
        assert.match(await page.locator('.fy[data-id="preview-ready"] .opthint').innerText(), /Optional.*Change.*Land/);
        // its panel starts with Zach's own ask and the finished work's changes
        await page.locator('.fy[data-id="preview-ready"] a.ft').click();
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pAsk').innerText(), /answer buttons clearer on a phone/);
        assert.match(await page.locator('#pAsk').innerText(), /Land the clearer board buttons\?/);
        assert.doesNotMatch(await page.locator('#pAsk').innerText(), /Not available/);
        await page.locator('#panel > .x').click();
        await page.locator('#panel.open').waitFor({ state: 'hidden' });
        // A periodic refresh must not reopen a query-linked card after it is closed.
        await page.goto(`${url}/?card=preview-zach`);
        await page.locator('#panel.open').waitFor();
        await page.locator('#panel > .x').click();
        await page.evaluate(() => load());
        assert.equal(await page.locator('#panel.open').count(), 0);
        await page.goto(url);
        await page.goto(`${url}/?card=preview-zach#preview-plan`);
        // Preserve main's hash precedence; a later hash still opens its card.
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Plan:/);
        await page.locator('#panel > .x').click();
        // Changing just the fragment must keep main's hashchange navigation working.
        const hashLink = new URL(page.url());
        hashLink.hash = 'preview-building';
        await page.goto(hashLink.href);
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Building:/);
        // The live board resolves links to replaced predecessors and short IDs.
        await page.route('**/api/board', async route => {
          const response = await route.fetch();
          const board = await response.json();
          board.cards.find(c => c.id === 'preview-building').replaces = ['prior-preview-building'];
          await route.fulfill({ response, json: board });
        });
        await page.goto(`${url}/?card=prior-preview-b`);
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Building:/);
        await page.locator('#panel > .x').click();
        await page.evaluate(() => load());
        assert.equal(await page.locator('#panel.open').count(), 0);
        await page.goto(`${url}/?view=invalid&card=unknown`);
        await page.locator('#inbox .sec').first().waitFor();
        assert.equal(await page.locator('#panel.open').count(), 0);
      } finally { await browser.close(); }
    });
  } finally { await rm(out, { recursive: true, force: true }); }
});

test('preview is stopped even when capture fails', async () => {
  let address;
  await assert.rejects(withPreview(async url => { address = url; throw Error('capture failed'); }), /capture failed/);
  await assert.rejects(fetch(address));
});

test('missing shared browser reports setup without downloading', async () => {
  const proc = spawn(process.execPath, ['scripts/board-screenshot.mjs'], {
    env: { ...process.env, PLAYWRIGHT_BROWSERS_PATH: '.factory-scratch/no-browser-cache' },
  });
  let stderr = '';
  proc.stderr.on('data', chunk => { stderr += chunk; });
  const [code] = await once(proc, 'exit');
  assert.equal(code, 1);
  assert.match(stderr, /Browser missing\. Setup: npx playwright install chromium/);
  await assert.rejects(access('.factory-scratch/no-browser-cache'), { code: 'ENOENT' });
});

test('declared live preview uses actual CLI and only simulates sample-card actions', async () => {
  const {execFileSync}=await import('node:child_process');
  const {createInterface}=await import('node:readline');
  const {resolve}=await import('node:path');
  const spec=JSON.parse(await readFile('.factory.json','utf8')).preview;
  const home=await mkdtemp(resolve('.factory-scratch/declared-preview-'));
  const goEnv=JSON.parse(execFileSync('go',['env','-json','GOPATH','GOMODCACHE','GOCACHE'],{encoding:'utf8'}));
  const proc=spawn('sh',['-c',spec.run],{
    detached:true,stdio:['ignore','pipe','pipe'],
    env:{PATH:process.env.PATH,HOME:home,HOST:'127.0.0.1',PORT:'0',...goEnv},
  });
  const exited=once(proc,'exit');
  let stderr='';proc.stderr.on('data',b=>{stderr+=b});
  const lines=createInterface({input:proc.stdout});
  let address;
  try{
    address=await new Promise((accept,reject)=>{
      const timer=setTimeout(()=>reject(Error('Declared preview startup timed out: '+stderr)),30000);
      lines.on('line',line=>{if(/^http:\/\/127\.0\.0\.1:\d+$/.test(line)){clearTimeout(timer);accept(line)}});
      exited.then(()=>{clearTimeout(timer);reject(Error('Declared preview exited: '+stderr))},reject);
    });
    assert.equal((await fetch(address+spec.path)).status,200);
    const before=await (await fetch(address+'/api/board')).json();
    assert.equal(before.cards.find(c=>c.id==='preview-zach').stage,'zach');
    const post=(action,id)=>fetch(`${address}/api/${action}`,{method:'POST',headers:{'X-Factory':'1','Content-Type':'application/json'},body:JSON.stringify({id})});
    assert.equal((await post('approve','preview-zach')).status,200);
    const after=await (await fetch(address+'/api/board')).json();
    assert.equal(after.cards.find(c=>c.id==='preview-zach').stage,'building');
    // the board's shared buttons are simulated too: Land on the finished-work card starts landing
    const choose=await fetch(`${address}/api/choose`,{method:'POST',headers:{'X-Factory':'1','Content-Type':'application/json'},body:JSON.stringify({id:'preview-ready',choice:'land'})});
    assert.equal(choose.status,200);
    assert.equal((await (await fetch(address+'/api/board')).json()).cards.find(c=>c.id==='preview-ready').stage,'landing');
    assert.equal((await post('approve','real-card')).status,404);
    assert.equal((await post('new','preview-plan')).status,403);
  }finally{
    lines.close();
    try{process.kill(-proc.pid,'SIGTERM')}catch(e){if(e.code!=='ESRCH')throw e}
    const timer=setTimeout(()=>{try{process.kill(-proc.pid,'SIGKILL')}catch{}},5000);
    await exited.finally(()=>clearTimeout(timer));
    if(address){
      for(let i=0;i<30;i++){
        try{await fetch(address);await new Promise(r=>setTimeout(r,50))}catch{break}
      }
      await assert.rejects(fetch(address));
    }
    await rm(home,{recursive:true,force:true});
  }
});

test('usage answers decisions, filters dated summaries and opens archived evidence', async () => {
  await withPreview(async url => {
    const browser=await chromium.launch({headless:true});
    try {
      for(const viewport of [{width:1280,height:800},{width:390,height:844}]) {
        const page=await browser.newPage({viewport});
        const errors=[];page.on('pageerror',e=>errors.push(e.message));
        const mutations=[];page.on('request',r=>{if(r.method()==='POST')mutations.push(r.url())});
        await page.goto(`${url}/usage`);
        await page.locator('#metrics article').first().waitFor();
        assert.match(await page.locator('#recommendation').innerText(),/Suggested next/);
        assert.equal(await page.locator('.subscription-row').count(),5);
        const overview=await page.locator('#plans').innerText();
        for(const name of ['Claude Max','ChatGPT Pro','Google AI Ultra','Muse Code Power'])assert.ok(overview.includes(name));
        assert.match(overview,/60% used/);
        assert.match(overview,/Checked 9h ago/);
        assert.match(overview,/first.account@example.com/);
        assert.match(overview,/second.account@example.com/);
        assert.match(overview,/Older reading/);
        assert.equal(await page.locator('#allowanceDetails').getAttribute('open'),null);
        await page.locator('#allowanceDetails > summary').click();
        assert.match(await page.locator('#allowanceDetailsBody').innerText(),/Sample refresh failed/);
        await page.locator('#allowanceDetails > summary').click();
        if(viewport.width===390){
          const lastSubscription=await page.locator('.subscription-row').last().boundingBox();
          assert.ok(lastSubscription.y+lastSubscription.height<=viewport.height,'All subscription readings fit on the first phone screen');
        }
        assert.match(await page.locator('#confidenceSummary').innerText(),/finished with gaps/);
        assert.match(await page.locator('#metrics').innerText(),/Review share[\s\S]*Repeated build share/);
        assert.equal(await page.locator('#records').getAttribute('open'),null);
        assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
        const allSummary=await page.locator('#metrics').innerText();
        await page.locator('#topCards button').first().click();
        assert.notEqual(await page.locator('#metrics').innerText(),allSummary);
        assert.equal(await page.locator('#plans').innerText(),overview);
        assert.notEqual(await page.locator('#records').getAttribute('open'),null);
        await page.locator('#clearFilters').click();
        assert.equal(await page.locator('#metrics').innerText(),allSummary);
        await page.locator('#period').selectOption('today');
        assert.match(await page.locator('#periodNote').innerText(),/Today is still in progress/);
        assert.match(await page.locator('#metrics').innerText(),/Yesterday/);
        await page.locator('#issues .issue', {hasText:'unsupported sessions'}).locator('button').click();
        assert.equal(await page.locator('#period').inputValue(),'all');
        assert.equal(await page.locator('#filter-status').inputValue(),'unsupported');
        assert.match(await page.locator('#totals').innerText(),/1 matching rows/);
        assert.match(await page.locator('#totals').innerText(),/Counts unavailable/);
        assert.match(await page.locator('#metrics .value').first().innerText(),/Unavailable/);
        await page.locator('#clearFilters').click();
        await page.locator('#filter-agent').selectOption('preview-usage-old');
        const details=page.locator('#rows details').first();await details.locator('summary').click();
        await page.evaluate(()=>load());
        assert.notEqual(await details.getAttribute('open'),null);
        await details.locator('a').click();
        await page.waitForURL('**/usage/agent/preview-usage-old');
        await page.locator('#events article').first().waitFor();
        assert.match(await page.locator('body').innerText(),/read-only view[\s\S]*Synthetic work is complete/);
        assert.equal(await page.locator('textarea').count(),0);
        assert.equal(mutations.length,0);
        assert.deepEqual(errors,[]);
        await page.close();
      }
      const page=await browser.newPage({viewport:{width:390,height:844}});
      await page.goto(`${url}/usage`);await page.locator('#metrics article').first().waitFor();
      const before=await page.locator('#metrics').innerText();
      await page.route('**/api/usage',route=>route.fulfill({status:503,body:'unavailable'}));
      await page.evaluate(()=>{Date.now=()=>Date.parse('2026-01-01T14:00:00Z')});
      await page.evaluate(()=>load());
      assert.match(await page.locator('#loadError').innerText(),/last successful snapshot/);
      assert.equal(await page.locator('#metrics').innerText(),before);
      assert.match(await page.locator('#recommendation').innerText(),/No provider has confirmed/);
      await page.evaluate(()=>{Date.now=()=>Date.parse('2026-01-01T12:00:00Z')});
      await page.unroute('**/api/usage');
      await page.evaluate(()=>load());
      assert.equal(await page.locator('#loadError').isVisible(),false);
      // Thousands of source rows stay paginated; unknown counters never become zero-valued evidence.
      await page.route('**/api/usage',async route=>{
        const res=await route.fetch(),v=await res.json();
        const base=v.rows.find(r=>r.counts.total>0);
        v.coverage.limited=true;
        v.rows=Array.from({length:100},(_,i)=>({...base,agent:`sample-${i}`,title:'<img src=x onerror=bad>',day:'2025-12-31'}));
        await route.fulfill({response:res,json:v});
      });
      await page.evaluate(()=>load());
      await page.locator('#records > summary').click();
      assert.match(await page.locator('#dateCoverage').innerText(),/Still counting/);
      assert.match(await page.locator('#metrics').innerText(),/Comparison waits/);
      assert.equal(await page.locator('#rows tr').count(),40);
      assert.equal(await page.locator('#rows img').count(),0);
      await page.locator('#nextPage').click();
      assert.equal(await page.locator('#pageLabel').innerText(),'Page 2 of 3');
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
    } finally {await browser.close()}
  });
});
