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
