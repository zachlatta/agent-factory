import { test } from 'node:test';
import assert from 'node:assert/strict';
import { access, mkdtemp, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { capture, withPreview } from './board-screenshot.mjs';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { chromium } from 'playwright';

// Run against the real embedded page and real API, with the preview's fake inputs.
test('stage/card links render at exact widths and preserve navigation', async () => {
  const out = await mkdtemp('.factory-scratch/capture-test-');
  try {
    await withPreview(async url => {
      const results = await capture(url, { stage: 'zach', card: 'preview-zach', out });
      for (const [i, name] of ['desktop', 'phone'].entries()) {
        const png = await readFile(join(out, `${name}.png`));
        assert.equal(png.readUInt32BE(16), [1280, 390][i]);
        assert.equal(png.readUInt32BE(20), [800, 844][i]);
      }
      assert.equal(results.length, 3);
      const questions=await readFile(join(out,'phone-questions.png'));
      assert.equal(questions.readUInt32BE(16),390);
      assert.equal(questions.readUInt32BE(20),844);
      assert.match(await readFile(join(out,'phone-questions.txt'),'utf8'),/Keep the clear layout/);
      await assert.rejects(capture(url, { stage: 'missing', out }), /Unknown stage/);
      await assert.rejects(capture(url, { card: 'missing', out }), /Unknown card/);
      const browser = await chromium.launch({ headless: true });
      try {
        const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
        await page.goto(`${url}/?stage=plan`);
        await page.locator('#tabs button.on[data-s="plan"]').waitFor();
        await page.locator('#tabs button[data-s="building"]').click();
        await page.locator('#tabs button.on[data-s="building"]').waitFor();
        await page.locator('.card[data-id="preview-building"]').click({ position: { x: 8, y: 8 } });
        await page.locator('#panel.open').waitFor();
        await page.locator('#panel > .x').click();
        await page.locator('#panel.open').waitFor({ state: 'hidden' });
        // A periodic refresh must not reopen a query-linked card after it is closed.
        await page.goto(`${url}/?stage=zach&card=preview-zach`);
        await page.locator('#panel.open').waitFor();
        await page.locator('#panel > .x').click();
        await page.evaluate(() => load());
        assert.equal(await page.locator('#panel.open').count(), 0);
        await page.goto(url);
        await page.goto(`${url}/?stage=zach&card=preview-zach#preview-plan`);
        // Preserve main's hash precedence; a later hash still opens its card.
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Plan:/);
        await page.locator('#panel > .x').click();
        await page.goto(`${url}/?stage=zach&card=preview-zach#preview-plan`);
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
        await page.goto(`${url}/?stage=building&card=prior-preview-b`);
        await page.locator('#tabs button.on[data-s="building"]').waitFor();
        await page.locator('#panel.open').waitFor();
        assert.match(await page.locator('#pTitle').innerText(), /Building:/);
        await page.locator('#panel > .x').click();
        await page.evaluate(() => load());
        assert.equal(await page.locator('#panel.open').count(), 0);
        await page.goto(`${url}/?stage=invalid&card=unknown`);
        await page.locator('#tabs button.on').waitFor();
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
