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
