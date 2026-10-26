(() => {
  'use strict';
  // Polls /api/hosts every 2 seconds for the board's host icon and popover and for /host.
  const el = id => document.getElementById(id);
  const btn = el('fleetBtn'), icon = el('fleetIcon'), pop = el('fleetPop');
  const page = el('hosts'), status = el('hostStatus');
  const history = new Map();
  let hosts = [], failed = false, skew = 0, timer = null;
  const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const percent = v => Number.isFinite(v) ? v.toFixed(v < 10 ? 1 : 0) + '%' : '—';
  const ratio = v => v && v.total > 0 ? 100 * v.used / v.total : null;
  const bytes = v => {
    if (!Number.isFinite(v)) return '—';
    const units = ['B','KiB','MiB','GiB','TiB'];
    let i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + units[i];
  };
  // Ages use the server's clock (skew), so a phone with a wrong clock still reads "live".
  const age = h => h.metrics ? Math.max(0, Math.floor((Date.now() + skew - Date.parse(h.metrics.sampledAt)) / 1000)) : null;
  const stale = h => failed || age(h) === null || age(h) > 6;
  const busiest = h => {
    if (!h.metrics) return null;
    const v = [h.metrics.cpuPercent, ratio(h.metrics.memory)].filter(Number.isFinite);
    return v.length ? Math.max(...v) : null;
  };
  const level = h => stale(h) ? 'stale' : busiest(h) === null ? 'stale' : busiest(h) >= 90 ? 'hot' : busiest(h) >= 70 ? 'warm' : 'ok';
  const note = h => {
    const a = age(h);
    const when = a === null ? 'no sample yet' : a <= 6 ? 'live' : 'last sample ' + a + 's ago';
    return failed ? 'Updates unavailable · ' + when : h.error ? h.error + ' · ' + when : when;
  };

  function record(h) {
    let list = history.get(h.name);
    if (!list) history.set(h.name, list = []);
    const m = h.metrics;
    if (!m || (list.length && list[list.length - 1].sampledAt === m.sampledAt)) return;
    const at = Date.parse(m.sampledAt);
    if (list.length && at - list[list.length - 1].at > 6000) list.push({at: at - 1, gap: true});
    list.push({at, sampledAt: m.sampledAt, cpu: m.cpuPercent, load: m.load ? m.load[0] : null, memory: ratio(m.memory), disk: ratio(m.disk)});
    while (list.length && (list[0].at < at - 5 * 60 * 1000 || list.length > 150)) list.shift();
  }

  // Separate polylines keep missing samples from looking like continuous data.
  function lines(list, key, max, w, hgt) {
    if (list.length < 2) return '';
    const t0 = list[0].at, span = Math.max(1, list[list.length - 1].at - t0);
    const out = [];
    let pts = [];
    const flush = () => { if (pts.length) out.push(`<polyline class="${key}" points="${pts.join(' ')}"/>`); pts = []; };
    for (const s of list) {
      const v = s[key];
      if (s.gap || !Number.isFinite(v)) { flush(); continue; }
      pts.push(((s.at - t0) * w / span).toFixed(1) + ',' + (hgt - 1 - Math.min(1, v / max) * (hgt - 2)).toFixed(1));
    }
    flush();
    return out.join('');
  }

  function renderIcon() {
    if (!icon) return;
    const n = Math.max(1, hosts.length), bw = 4, gap = 2, w = n * bw + (n - 1) * gap;
    icon.setAttribute('viewBox', `0 0 ${w} 16`);
    icon.setAttribute('width', String(Math.max(14, w + 4)));
    icon.innerHTML = hosts.map((h, i) => {
      const v = stale(h) ? null : busiest(h);
      const fill = v === null ? 16 : Math.max(2, v / 100 * 16);
      return `<rect class="track" x="${i * (bw + gap)}" y="0" width="${bw}" height="16" rx="1"/>` +
        `<rect class="bar ${level(h)}" x="${i * (bw + gap)}" y="${(16 - fill).toFixed(1)}" width="${bw}" height="${fill.toFixed(1)}" rx="1"/>`;
    }).join('');
    const live = hosts.filter(h => !stale(h));
    const top = live.reduce((a, h) => busiest(h) !== null && (!a || busiest(h) > busiest(a)) ? h : a, null);
    const label = failed ? 'Host load: updates unavailable'
      : hosts.length + (hosts.length === 1 ? ' host' : ' hosts') + (top ? ', busiest ' + top.name + ' at ' + percent(busiest(top)) : '') +
        (live.length < hosts.length ? ', ' + (hosts.length - live.length) + ' not live' : '');
    btn.title = label + ' (click for detail)';
    btn.setAttribute('aria-label', label);
  }

  function renderPop() {
    if (!pop || pop.hidden) return;
    const rows = hosts.map(h => {
      const m = h.metrics || {};
      const list = history.get(h.name) || [];
      return `<div class="frow ${stale(h) ? 'stale' : ''}">
        <div class="fhd"><span class="fdot ${level(h)}"></span><b>${esc(h.name)}</b>${h.local ? ' <span class="sub">factory</span>' : ''}<span class="sub fnote">${esc(note(h))}</span></div>
        <div class="fstats">
          <span>CPU <b>${percent(m.cpuPercent)}</b>${m.cpuCount ? ' of ' + m.cpuCount : ''}</span>
          <span>Load <b>${m.load ? m.load.map(v => v.toFixed(2)).join(' ') : '—'}</b></span>
          <span>RAM <b>${percent(ratio(m.memory))}</b>${m.memory ? ' · ' + bytes(m.memory.used) + ' / ' + bytes(m.memory.total) : ''}</span>
          <span>Disk <b>${percent(ratio(m.disk))}</b>${m.disk ? ' · ' + bytes(m.disk.available) + ' free' : ''}</span>
        </div>
        <svg class="spark" viewBox="0 0 200 24" preserveAspectRatio="none" aria-hidden="true">${lines(list, 'cpu', 100, 200, 24)}${lines(list, 'memory', 100, 200, 24)}</svg>
      </div>`;
    }).join('');
    pop.innerHTML = rows + `<div class="ffoot"><span class="sub"><i class="k cpu"></i>CPU <i class="k memory"></i>RAM · last 5 min · every 2s</span><a href="/host">Host page →</a></div>`;
  }

  function renderPage() {
    if (!page) return;
    if (status) {
      status.textContent = failed ? 'Updates unavailable' : 'Live · ' + hosts.length + (hosts.length === 1 ? ' host' : ' hosts');
      status.dataset.failed = String(failed);
    }
    page.innerHTML = hosts.map(h => {
      const m = h.metrics || {}, e = m.errors || {}, list = history.get(h.name) || [];
      const maxLoad = Math.max(1, m.cpuCount || 1, ...list.map(s => s.load || 0));
      const card = (title, key, value, detail, max, scale) => `<section class="metric"><h3>${title}</h3><div class="value">${value}</div><p class="detail">${esc(detail)}</p><svg viewBox="0 0 300 80" preserveAspectRatio="none" aria-hidden="true">${lines(list, key, max, 300, 80)}</svg><div class="key"><span>Recent samples</span><span>${scale}</span></div></section>`;
      const space = (v, name) => v ? bytes(v.used) + ' used / ' + bytes(v.total) + ' total · ' + bytes(v.available) + ' available' + (name === 'disk' ? ' · home filesystem' : '') : e[name] || 'Unavailable';
      return `<section class="host"><h2>${esc(h.name)}${h.local ? ' <span class="muted">· runs the factory</span>' : ''}</h2>
        <p class="muted hnote" data-failed="${stale(h)}">${esc(note(h))}</p>
        <div class="metrics">
        ${card('CPU utilization', 'cpu', percent(m.cpuPercent), e.cpu || (m.cpuPercent == null ? 'Warming up · two samples needed' : 'Busy across ' + m.cpuCount + ' logical CPUs'), 100, '0–100%')}
        ${card('Load average', 'load', m.load ? m.load[0].toFixed(2) : '—', m.load ? m.load.map(v => v.toFixed(2)).join(' / ') + ' over 1 / 5 / 15 minutes · ' + m.cpuCount + ' CPUs' : e.load || 'Unavailable', maxLoad, '0–' + maxLoad.toFixed(1) + ' tasks')}
        ${card('RAM', 'memory', percent(ratio(m.memory)), space(m.memory, 'memory'), 100, '0–100%')}
        ${card('Disk space', 'disk', percent(ratio(m.disk)), space(m.disk, 'disk'), 100, '0–100%')}
        </div></section>`;
    }).join('');
  }

  function render() { renderIcon(); renderPop(); renderPage(); }

  async function poll() {
    clearTimeout(timer);
    if (document.hidden) { timer = setTimeout(poll, 2000); return; }
    const controller = new AbortController();
    const abort = setTimeout(() => controller.abort(), 5000);
    try {
      const r = await fetch('/api/hosts', {cache: 'no-store', signal: controller.signal});
      if (!r.ok) throw new Error('Host metrics unavailable');
      const body = await r.json();
      if (!Array.isArray(body.hosts)) throw new Error('Invalid response');
      if (Number.isFinite(Date.parse(body.now))) skew = Date.parse(body.now) - Date.now();
      hosts = body.hosts;
      hosts.forEach(record);
      failed = false;
    } catch (_) {
      failed = true;
    } finally {
      clearTimeout(abort);
      render();
      timer = setTimeout(poll, 2000);
    }
  }

  if (btn && pop) {
    const close = () => { pop.hidden = true; btn.setAttribute('aria-expanded', 'false'); };
    btn.addEventListener('click', e => {
      e.stopPropagation();
      pop.hidden = !pop.hidden;
      btn.setAttribute('aria-expanded', String(!pop.hidden));
      renderPop();
    });
    document.addEventListener('click', e => { if (!pop.hidden && !pop.contains(e.target)) close(); });
    document.addEventListener('keydown', e => { if (e.key === 'Escape' && !pop.hidden) { close(); btn.focus(); } });
  }
  document.addEventListener('visibilitychange', () => { if (!document.hidden) poll(); });
  setInterval(render, 1000);
  poll();
})();
