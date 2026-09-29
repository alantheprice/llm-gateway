/* LLM Gateway — shared helpers */
function esc(s) {
  const d = document.createElement('div');
  d.textContent = s == null ? '' : String(s);
  return d.innerHTML;
}

// escAttr: escape for embedding inside a double-quoted HTML attribute
// (value="..."). Escapes quotes and angle brackets.
function escAttr(s) {
  return String(s == null ? '' : s)
    .replaceAll('&', '&amp;').replaceAll('"', '&quot;')
    .replaceAll('<', '&lt;').replaceAll('>', '&gt;');
}

function flash(msg, isErr, id = 'flash') {
  const f = document.getElementById(id);
  if (!f) return;
  f.textContent = msg;
  f.className = 'flash ' + (isErr ? 'err' : 'okm');
  f.style.display = 'block';
  if (!isErr) setTimeout(() => { f.style.display = 'none'; }, 6000);
}

async function api(path, opts = {}) {
  const r = await fetch(path, {headers: {Accept: 'application/json'}, ...opts});
  const ct = r.headers.get('content-type') || '';
  if (!ct.includes('application/json')) {
    // non-JSON means a redirect to the login page — session expired
    return {ok: false, status: 401, data: {error: 'session expired — reloading…'}};
  }
  let d = {};
  try { d = await r.json(); } catch (e) { /* non-JSON */ }
  return {ok: r.ok, status: r.status, data: d};
}

function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
  if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
  return d.toLocaleDateString();
}

function fmtNum(n) { return (n || 0).toLocaleString(); }

/* --- Timezone helpers ------------------------------------------------
   The backend buckets usage & cost by UTC day and reports instants in UTC
   (e.g. a 429 "resets at UTC midnight"). These convert to the viewer's
   local time so the UI reads correctly; the UTC convention is surfaced,
   not hidden. A UTC day is a 24h window that straddles two local calendar
   days, so bucket labels keep the UTC day name and a window note explains
   the local span. All helpers no-op/annotate away in a UTC zone. */
const TZ = (() => {
  const offsetMin = () => -new Date().getTimezoneOffset();
  function tzName() {
    try {
      const s = new Date().toLocaleTimeString(undefined, { timeZoneName: 'short' });
      const i = s.lastIndexOf(' ');
      return i > -1 ? s.slice(i + 1) : '';
    } catch (e) { return ''; }
  }
  function offsetLabel() {
    const m = offsetMin();
    if (!m) return 'UTC';
    const h = Math.floor(Math.abs(m) / 60), mm = Math.abs(m) % 60;
    return 'UTC' + (m > 0 ? '+' : '−') + h + (mm ? ':' + String(mm).padStart(2, '0') : '');
  }
  const parts = ymd => String(ymd).split('-').map(Number);
  // "YYYY-MM-DD" UTC day key -> "Sep 25" (the day the key names).
  function dayLabel(ymd) {
    const [y, mo, d] = parts(ymd);
    return new Date(Date.UTC(y, mo - 1, d)).toLocaleDateString(undefined,
      { month: 'short', day: 'numeric', timeZone: 'UTC' });
  }
  // "YYYY-MM-DD" UTC bucket -> its 24h window in the viewer's local time,
  // e.g. "Sep 25, 7 PM – Sep 26, 6:59 PM (CDT, UTC-5)".
  function dayWindow(ymd) {
    const [y, mo, d] = parts(ymd);
    const f = dt => dt.toLocaleString(undefined,
      { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
    const suffix = ` (${tzName() ? tzName() + ', ' + offsetLabel() : offsetLabel()})`;
    return f(new Date(Date.UTC(y, mo - 1, d, 0, 0, 0))) + ' - ' +
           f(new Date(Date.UTC(y, mo - 1, d, 23, 59, 59))) + suffix;
  }
  // One-line note for a chart of UTC buckets; '' in a UTC zone.
  function bucketNote(ymd) {
    if (!offsetMin()) return '';
    return `Buckets are UTC days - "${dayLabel(ymd)}" covers ${dayWindow(ymd)}.`;
  }
  // Note for "today" stats: the current UTC bucket and its local span.
  function todayBucketNote() {
    if (!offsetMin()) return '';
    const n = new Date();
    const ymd = n.toISOString().slice(0, 10);
    return `"Today" = the current UTC day (${dayLabel(ymd)}), covering ${dayWindow(ymd)}.`;
  }
  // The next 00:00 UTC instant, as ISO (the quota window reset).
  function nextUtcMidnight() {
    const n = new Date();
    return new Date(Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), n.getUTCDate() + 1)).toISOString();
  }
  // ISO instant -> viewer-local string, e.g. "9/25, 11:00 PM (CDT)".
  function instantLocal(iso, opts) {
    if (!iso) return '';
    const d = new Date(iso);
    if (isNaN(d)) return '';
    const o = Object.assign({ hour: 'numeric', minute: '2-digit' }, opts || {});
    return d.toLocaleString(undefined, o) + ` (${tzName() || offsetLabel()})`;
  }
  return { offsetMin, offsetLabel, tzName, dayLabel, dayWindow,
           bucketNote, todayBucketNote, nextUtcMidnight, instantLocal };
})();

// gpuName: the one way every page shows a GPU engine — its identity label
// from the server (host from the cost config + engine port), a "link" badge
// when it is reached over a link agent, and the raw backend URL on hover.
function gpuName(o, url) {
  o = o || {};
  const raw = url || o.backend || o.url || '';
  const label = o.gpu_label || raw.replace(/^https?:\/\//, '');
  const via = o.via === 'link' ? ' <span class="badge ok" title="served over a link agent">link</span>' : '';
  return '<span title="' + escAttr(raw) + '">' + esc(label) + '</span>' + via;
}

// cardLine: one line of "what is actually serving" from a model-card summary
// (server-side cardSummary): weights · KV · speculative decoding · GPU · context.
function cardLine(sum) {
  if (!sum) return '';
  const parts = [];
  if (sum.weights) parts.push(sum.weights + ' weights');
  if (sum.kv_cache) parts.push('KV ' + sum.kv_cache);
  if (sum.speculative) parts.push(sum.speculative + (sum.draft_tokens ? ' ×' + sum.draft_tokens : ''));
  if (sum.gpu) parts.push(String(sum.gpu).replace(/^NVIDIA (GeForce )?/, ''));
  if (sum.context) parts.push(Math.round(sum.context / 1000) + 'K ctx');
  return parts.length
    ? '<br><span class="muted" style="font-size:12px" title="' + escAttr(sum.name || '') + '">' + esc(parts.join(' · ')) + '</span>'
    : '';
}

// capBadges: a model's capabilities as small badges (Chat, Vision, Tools…).
// The title says where they came from: the engine, an admin, or a guess.
function capBadges(c) {
  if (!c) return '';
  const out = [];
  const add = (label, cls) => out.push('<span class="capb' + (cls ? ' ' + cls : '') + '">' + label + '</span>');
  const has = (l, v) => (l || []).includes(v);
  if (has(c.endpoints, 'chat')) add('Chat');
  if (has(c.endpoints, 'completions') && !has(c.endpoints, 'chat')) add('Completions');
  if (has(c.endpoints, 'embeddings')) add('Embeddings');
  if (has(c.endpoints, 'images')) add('Image gen', 'strong');
  if (has(c.input_modalities, 'image')) add('Vision', 'strong');
  if (has(c.input_modalities, 'audio')) add('Audio in', 'strong');
  if (has(c.input_modalities, 'video')) add('Video in', 'strong');
  if (has(c.features, 'tools')) add('Tools');
  if (has(c.features, 'thinking')) add('Thinking');
  const src = { engine: 'Reported by the engine', config: 'Set by an admin', name: 'Guessed from the model name' }[c.source] || '';
  return out.length ? '<span class="capbs" title="' + src + '">' + out.join('') + (c.source === 'name' ? '<span class="capb guess">guessed</span>' : '') + '</span>' : '';
}

// ---------- shared view controls ----------

function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }

// RangePicker: pick a time range — Today, 7 days, 30 days, 12 months,
// All time, or custom start and end dates. Ranges are UTC days (usage and
// costs are bucketed by UTC day). The choice is remembered per id.
//   const rp = RangePicker.mount(el, { id: 'sys-users', onChange: range => … });
//   range = { preset, from, to, label }  (from/to 'YYYY-MM-DD'; '' = unbounded)
const RangePicker = (() => {
  const PRESETS = [
    ['today', 'Today', 0], ['7d', '7 days', 6], ['30d', '30 days', 29], ['365d', '12 months', 364],
    ['all', 'All time'], ['custom', 'Custom'],
  ];
  const ymd = d => d.toISOString().slice(0, 10);
  const todayUTC = () => ymd(new Date());
  const minus = (day, n) => { const d = new Date(day + 'T00:00:00Z'); d.setUTCDate(d.getUTCDate() - n); return ymd(d); };
  function resolve(v) {
    const today = todayUTC();
    const p = PRESETS.find(x => x[0] === v.preset) || PRESETS[1];
    if (p[0] === 'all') return { preset: 'all', from: '', to: '', label: 'All time' };
    if (p[0] === 'custom') {
      const from = v.from || minus(today, 6), to = v.to || today;
      const [a, b] = from <= to ? [from, to] : [to, from];
      return { preset: 'custom', from: a, to: b, label: (typeof TZ !== 'undefined' ? TZ.dayLabel(a) + ' – ' + TZ.dayLabel(b) : a + ' – ' + b) };
    }
    return { preset: p[0], from: minus(today, p[2]), to: today, label: p[0] === 'today' ? 'Today (UTC)' : 'Last ' + p[1] };
  }
  function mount(el, { id, value, onChange }) {
    let saved = null;
    try { saved = JSON.parse(lsGet('range.' + id) || 'null'); } catch (e) {}
    let cur = resolve(saved || value || { preset: '7d' });
    function draw() {
      el.innerHTML = '<div class="rangepick">' +
        '<div class="seg" role="group" aria-label="Time range">' + PRESETS.map(([k, label]) =>
          '<button type="button" data-p="' + k + '" aria-pressed="' + (cur.preset === k) + '">' + label + '</button>').join('') + '</div>' +
        (cur.preset === 'custom' ? '<span class="rp-custom"><input type="date" data-f="from" value="' + cur.from + '" max="' + todayUTC() + '" aria-label="Start date">' +
          '<span class="muted">to</span><input type="date" data-f="to" value="' + cur.to + '" max="' + todayUTC() + '" aria-label="End date"></span>' : '') +
        '</div>';
      el.querySelectorAll('[data-p]').forEach(b => b.onclick = () => set({ preset: b.dataset.p, from: cur.from, to: cur.to }));
      el.querySelectorAll('[data-f]').forEach(inp => inp.onchange = () => {
        const v = { preset: 'custom', from: cur.from, to: cur.to };
        v[inp.dataset.f] = inp.value;
        if (v.from && v.to) set(v);
      });
    }
    function set(v) {
      cur = resolve(v);
      lsSet('range.' + id, JSON.stringify({ preset: cur.preset, from: cur.from, to: cur.to }));
      draw();
      if (onChange) onChange(cur);
    }
    draw();
    return { get: () => cur, set };
  }
  return { mount, resolve };
})();

// ViewToggle: switch a panel between views (default Table | Chart),
// remembered per id.
//   const vt = ViewToggle.mount(el, { id: 'sys-users', onChange: view => … });
const ViewToggle = (() => {
  function mount(el, { id, views = [['table', 'Table'], ['chart', 'Chart']], onChange }) {
    let cur = lsGet('view.' + id);
    if (!views.some(v => v[0] === cur)) cur = views[0][0];
    function draw() {
      el.innerHTML = '<div class="seg" role="group" aria-label="View">' + views.map(([k, label]) =>
        '<button type="button" data-v="' + k + '" aria-pressed="' + (cur === k) + '">' + label + '</button>').join('') + '</div>';
      el.querySelectorAll('[data-v]').forEach(b => b.onclick = () => {
        cur = b.dataset.v; lsSet('view.' + id, cur); draw();
        if (onChange) onChange(cur);
      });
    }
    draw();
    return { get: () => cur };
  }
  return { mount };
})();

// Series colors for per-user/per-model charts (the rest go to "Others").
const SERIES_COLORS = ['#58a6ff', '#3fb950', '#e3b341', '#bc8cff', '#f778ba', '#56d4dd', '#ff9f43', '#8b949e'];
