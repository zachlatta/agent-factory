(() => {
  'use strict';
  const el = id => document.getElementById(id);
  const preview = el('hostPreview');
  const status = el('hostStatus');
  const history = [];
  let latest = null, failed = false;
  const percent = v => Number.isFinite(v) ? v.toFixed(1) + '%' : '—';
  const ratio = v => v && v.total > 0 ? 100 * v.used / v.total : null;
  const bytes = v => {
    if (!Number.isFinite(v)) return '—';
    const units = ['B','KiB','MiB','GiB','TiB'];
    let i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return v.toFixed(i ? 1 : 0) + ' ' + units[i];
  };
  function freshness() {
    const age = latest ? Math.max(0, Math.floor((Date.now() - Date.parse(latest.sampledAt)) / 1000)) : null;
    const stale = failed || age === null || age > 6;
    const note = age === null ? 'No sample yet' : 'Sample ' + age + 's ago';
    if (status) {
      status.textContent = (stale ? 'Updates unavailable · ' : 'Live · ') + note;
      status.dataset.failed = String(stale);
    }
    if (preview) {
      preview.dataset.stale = String(stale);
      if (!latest) { preview.textContent = failed ? 'Host metrics unavailable · View host load →' : 'Host metrics loading…'; return; }
      const m = latest;
      preview.textContent = (stale ? 'Host · Updates unavailable · ' : 'Host · ') +
        'CPU ' + percent(m.cpuPercent) + ' · Load ' + (m.load ? m.load[0].toFixed(2) : '—') +
        ' · RAM ' + percent(ratio(m.memory)) + ' · Disk ' + percent(ratio(m.disk)) + ' · ' + note + ' →';
    }
  }
  function record(sample) {
    history.push(sample);
    const cutoff = Date.now() - 5 * 60 * 1000;
    while (history.length && (history[0].at < cutoff || history.length > 150)) history.shift();
  }
  function chart(name, values, max) {
    const line = el(name + 'Trend');
    if (!line) return;
    // Separate polylines prevent missing samples from looking like continuous data.
    const svg = line.parentElement;
    svg.querySelectorAll('.segment').forEach(n => n.remove());
    let points = [];
    const flush = () => {
      if (points.length) {
        const segment = document.createElementNS('http://www.w3.org/2000/svg','polyline');
        segment.setAttribute('class','segment'); segment.setAttribute('points',points.join(' '));
        svg.appendChild(segment); points = [];
      }
    };
    values.forEach((v,i) => {
      if (!Number.isFinite(v)) { flush(); return; }
      points.push(((history[i].at - history[0].at) * 300 / Math.max(1,history[history.length-1].at - history[0].at)).toFixed(1) + ',' + (78 - Math.min(1,v/max) * 76).toFixed(1));
    });
    flush();
  }
  function render(m) {
    if (!status) return;
    const text = (id,v) => { el(id).textContent = v; };
    const errors = m.errors || {};
    text('cpuValue',percent(m.cpuPercent));
    text('cpuDetail',errors.cpu || (m.cpuPercent === null ? 'Warming up · two samples needed' : 'Busy across ' + m.cpuCount + ' logical CPUs'));
    text('loadValue',m.load ? m.load[0].toFixed(2) : '—');
    text('loadDetail',m.load ? m.load.map(v => v.toFixed(2)).join(' / ') + ' over 1 / 5 / 15 minutes · ' + m.cpuCount + ' CPUs' : errors.load || 'Unavailable');
    ['memory','disk'].forEach(name => {
      const v = m[name];
      text(name+'Value',percent(ratio(v)));
      text(name+'Detail',v ? bytes(v.used) + ' used / ' + bytes(v.total) + ' total · ' + bytes(v.available) + ' available' + (name === 'disk' ? ' · Work filesystem' : '') : errors[name] || 'Unavailable');
    });
    chart('cpu',history.map(v => v.cpu),100);
    chart('memory',history.map(v => v.memory),100);
    chart('disk',history.map(v => v.disk),100);
    const maxLoad = Math.max(1,m.cpuCount,...history.map(v => v.load || 0));
    chart('load',history.map(v => v.load),maxLoad);
    text('loadScale','0–'+maxLoad.toFixed(1)+' tasks');
  }
  async function poll() {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(),5000);
    try {
      const response = await fetch('/api/host',{cache:'no-store',signal:controller.signal});
      if (!response.ok) throw new Error('Host metrics unavailable');
      const m = await response.json();
      if (!Number.isFinite(Date.parse(m.sampledAt))) throw new Error('Invalid sample');
      failed = false;
      if (!latest || latest.sampledAt !== m.sampledAt) {
        if (history.length && Date.parse(m.sampledAt) - history[history.length-1].at > 6000) {
          record({at:Date.parse(m.sampledAt)-1,cpu:null,load:null,memory:null,disk:null});
        }
        record({at:Date.parse(m.sampledAt),cpu:m.cpuPercent,load:m.load ? m.load[0] : null,memory:ratio(m.memory),disk:ratio(m.disk)});
      }
      latest = m;
      render(m);
    } catch (_) {
      failed = true;
      record({at:Date.now(),cpu:null,load:null,memory:null,disk:null});
    } finally {
      clearTimeout(timeout);
      freshness();
      setTimeout(poll,2000);
    }
  }
  setInterval(freshness,1000);
  poll();
})();
