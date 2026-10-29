import { spawn } from 'node:child_process';
import { access, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { once } from 'node:events';
import { createInterface } from 'node:readline';
import { parseArgs } from 'node:util';
import { chromium } from 'playwright';

const root = fileURLToPath(new URL('../', import.meta.url));
const scratch = join(root, '.factory-scratch');
// Playwright creates browser profiles and artifacts via os.tmpdir(). Keep those
// transient files in the worktree too; the shared installed browser stays cached.
process.env.TMPDIR = scratch;
export const browserSetup = 'npx playwright install chromium';

async function run(command, args, options) {
  const proc = spawn(command, args, options);
  const [code] = await once(proc, 'exit');
  if (code !== 0) throw Error(`${command} exited with ${code}`);
}

// Build once per invocation. Own the preview process, address and temporary state;
// never accept an arbitrary live board URL as a screenshot target.
export async function withPreview(work) {
  await mkdir(scratch, { recursive: true });
  const dir = await mkdtemp(join(scratch, 'screenshot-'));
  let proc, exited, lines;
  const stop = () => proc?.kill('SIGTERM');
  try {
    const binary = join(dir, 'board-preview');
    await run('go', ['build', '-o', binary, './cmd/board-preview'], {
      cwd: root, stdio: ['ignore', 'inherit', 'inherit'], env: { ...process.env, TMPDIR: scratch },
    });
    proc = spawn(binary, ['-scratch', dir], { cwd: root, stdio: ['ignore', 'pipe', 'inherit'] });
    exited = once(proc, 'exit');
    process.once('SIGINT', stop);
    process.once('SIGTERM', stop);
    lines = createInterface({ input: proc.stdout });
    const url = await new Promise((accept, reject) => {
      const timer = setTimeout(() => reject(Error('Preview startup timed out')), 15000);
      const finish = (fn, value) => { clearTimeout(timer); fn(value); };
      lines.on('line', line => {
        if (/^http:\/\/127\.0\.0\.1:\d+$/.test(line)) finish(accept, line);
      });
      exited.then(() => finish(reject, Error('Preview exited before startup')), reject);
    });
    return await work(url);
  } finally {
    process.removeListener('SIGINT', stop);
    process.removeListener('SIGTERM', stop);
    lines?.close();
    if (proc && proc.exitCode === null) {
      proc.kill('SIGTERM');
      const timer = setTimeout(() => proc.kill('SIGKILL'), 6000);
      await exited.finally(() => clearTimeout(timer));
    }
    await rm(dir, { recursive: true, force: true });
  }
}

// The board opens on For you at every width; --view=columns shows the desktop kanban (the phone always
// shows For you). --card opens one card's detail.
export const views = ['inbox', 'columns'];

export async function capture(url, { view = 'inbox', card, out = join(root, '.factory-evidence') } = {}) {
  try { await access(chromium.executablePath()); }
  catch { throw Error(`Browser missing. Setup: ${browserSetup}`); }
  const board = await (await fetch(`${url}/api/board`)).json();
  if (!views.includes(view)) throw Error(`Unknown view: ${view} (use ${views.join(' or ')})`);
  if (card && !board.cards.some(c => c.id === card)) throw Error(`Unknown card: ${card}`);
  await mkdir(out, { recursive: true });
  const browser = await chromium.launch({ headless: true });
  const results = [];
  try {
    for (const [name, width, height] of [['desktop', 1280, 800], ['phone', 390, 844]]) {
      const page = await browser.newPage({ viewport: { width, height }, deviceScaleFactor: 1 });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('console', msg => { if (msg.type() === 'error') errors.push(msg.text()); });
      page.on('requestfailed', request => errors.push(`${request.url()}: ${request.failure()?.errorText}`));
      page.on('response', response => { if (response.status() >= 400) errors.push(`HTTP ${response.status()}: ${response.url()}`); });
      // Keep relative ages stable without changing the production page or timers.
      await page.addInitScript(now => { Date.now = () => now; }, Date.parse(board.updated));
      const query = new URLSearchParams({ view });
      if (card) query.set('card', card);
      await page.goto(`${url}/?${query}`, { waitUntil: 'networkidle', timeout: 20000 });
      await page.waitForFunction(() => B.updated && !loadErr);
      const shown = name === 'phone' ? 'inbox' : view;
      if (shown === 'inbox') await page.locator('#inbox .sec').first().waitFor();
      else await page.locator('#grid .card').first().waitFor();
      if (card) {
        await page.locator('#panel.open').waitFor();
        await page.waitForFunction(id => sel === id && document.querySelector('#pTitle').textContent === B.cards.find(c => c.id === id).title, card);
        await page.waitForFunction(() => document.querySelector('#pLogs').textContent !== 'loading…');
      }
      await page.evaluate(() => document.fonts.ready);
      if (errors.length) throw Error(`${name}: ${errors.join('; ')}`);
      const sideways = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      if (name === 'phone' && sideways) throw Error('The phone page scrolls sideways');
      const path = resolve(out, `${name}.png`);
      await page.screenshot({ path, fullPage: false });
      const text = await page.locator('body').innerText();
      await writeFile(resolve(out, `${name}.txt`), text);
      results.push({ name, width, height, view: shown, card: card || null, file: `${name}.png` });
      if (name === 'phone') {
        // a For you card whose question options must fit the phone screen
        const forYou = new Set((board.sections?.forYou || []).map(l => l.id));
        const questionCard = board.cards.find(c => forYou.has(c.id) && c.ask?.questions?.length);
        if (questionCard) {
          if (await page.locator('#panel.open').count()) await page.locator('#panel > .x').click();
          const question = page.locator(`.fy[data-id="${questionCard.id}"] .qq`).first();
          // centred, not just inside the edge, so the choices show whole above and below
          await question.evaluate(q => q.scrollIntoView({ block: 'center' }));
          const visible = await question.locator('.opt').evaluateAll(options => options.every(o => {
            const r = o.getBoundingClientRect(); return r.top >= 0 && r.bottom <= innerHeight;
          }));
          if (!visible) throw Error('Phone question choices do not fit the capture');
          await page.screenshot({ path: resolve(out, 'phone-questions.png'), fullPage: false });
          await writeFile(resolve(out, 'phone-questions.txt'), await page.locator('body').innerText());
          results.push({ name: 'phone-questions', width, height, view: 'inbox', card: questionCard.id, file: 'phone-questions.png' });
        }
      }
      await page.close();
    }
    await writeFile(resolve(out, 'capture.json'), JSON.stringify(results, null, 2) + '\n');
    return results;
  } finally { await browser.close(); }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { view: { type: 'string', default: 'inbox' }, card: { type: 'string' }, out: { type: 'string' } } });
    const results = await withPreview(url => capture(url, values));
    for (const result of results) console.log(`${result.name}: ${result.width}x${result.height} ${result.file}`);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
