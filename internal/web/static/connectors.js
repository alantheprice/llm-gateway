/* Connectors: the user's remote MCP servers — the dialog shared by Chat and
   Settings. Markup: {{template "connectors" .}} in _layout.html.
   Connectors.open() shows it; 'connectors:change' (detail: servers) fires
   whenever the list loads or changes. */
const Connectors = (() => {

  const cx = document.getElementById('connectors');
  let cxServers = [];
  // Pages react to the list (the chat's Web label, Settings' summary).
  function cxLabel() {
    document.dispatchEvent(new CustomEvent('connectors:change', { detail: cxServers.slice() }));
  }
  function cxToolList(res) {
    if (!res) return '';
    if (!res.ok) return '<div class="cx-err">✗ ' + esc(res.error || 'failed') + '</div>' +
      (/credentials|401|403/.test(res.error || '') ? '<div class="muted cx-hint">If the service offers it, choose <strong>Sign in with the service</strong> instead.</div>' : '');
    const tools = res.tools || [];
    return '<div class="cx-ok">✓ Connected' + (res.legacy_sse ? ' (older SSE transport)' : '') + ' · ' + tools.length + ' tool' + (tools.length === 1 ? '' : 's') + '</div>' +
      '<ul class="cx-tools">' + tools.map(t => '<li><code>' + esc(t.name) + '</code> <span class="muted">' + esc(t.description || '') + '</span></li>').join('') + '</ul>';
  }
  function cxRender() {
    const list = document.getElementById('cxList');
    list.innerHTML = cxServers.length ? cxServers.map(v =>
      '<div class="cx-item" data-id="' + escAttr(v.id) + '">' +
        '<div class="cx-line"><label class="cx-on"><input type="checkbox" data-cxon' + (v.enabled ? ' checked' : '') + '> <strong>' + esc(v.name) + '</strong></label>' +
        '<span class="cx-url">' + esc(v.url) + '</span></div>' +
        '<div class="cx-line muted">' + (v.auth === 'oauth'
          ? (v.signed_in ? '<span class="cx-ok">✓ Signed in</span>' : '<span class="cx-err">Not signed in</span>')
          : (Object.keys(v.headers || {}).map(k => esc(k) + ': ' + esc(v.headers[k])).join(' · ') || 'no auth header')) + '</div>' +
        '<div class="cx-actions">' +
          (v.auth === 'oauth' ? (v.signed_in ? '<button class="btn sm" data-cxsignout>Sign out</button>' : '<button class="btn sm primary" data-cxsignin>Sign in</button>') : '') +
          '<button class="btn sm" data-cxtest>Test</button><button class="btn sm danger" data-cxdel>Remove</button></div>' +
        '<div class="cx-out"></div></div>').join('')
      : '<p class="muted">No connectors yet.</p>';
    list.querySelectorAll('.cx-item').forEach(el => {
      const id = el.dataset.id;
      el.querySelector('[data-cxon]').onchange = e => cxPost({ action: 'update', id, enabled: e.target.checked });
      el.querySelector('[data-cxdel]').onclick = () => {
        const v = cxServers.find(x => x.id === id);
        if (confirm('Remove ' + (v ? v.name : 'this connector') + '? Its saved auth header is deleted too.')) cxPost({ action: 'delete', id });
      };
      const si = el.querySelector('[data-cxsignin]');
      if (si) si.onclick = () => cxSignIn(id);
      const so = el.querySelector('[data-cxsignout]');
      if (so) so.onclick = () => cxPost({ action: 'sign_out', id });
      el.querySelector('[data-cxtest]').onclick = async e => {
        const out = el.querySelector('.cx-out');
        e.target.disabled = true; out.innerHTML = '<span class="muted">Connecting…</span>';
        const r = await api('/api/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'test', id }) });
        e.target.disabled = false;
        out.innerHTML = r.ok ? cxToolList(r.data.test) : '<div class="cx-err">✗ ' + esc(r.data.error || 'failed') + '</div>';
      };
    });
    cxLabel();
    cxQuickRender();
  }
  async function cxLoad() {
    const r = await api('/api/mcp');
    if (r.ok) { cxServers = r.data.servers || []; cxRender(); }
  }
  async function cxPost(body) {
    const r = await api('/api/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    if (!r.ok) { flash(r.data.error || 'failed', true, 'cxFlash'); await cxLoad(); return null; }
    cxServers = r.data.servers || [];
    cxRender();
    return r.data;
  }
  function cxFormBody(action) {
    const b = { action, name: document.getElementById('cxName').value.trim(), url: document.getElementById('cxURL').value.trim() };
    const mode = document.getElementById('cxAuth').value;
    const hn = document.getElementById('cxHName').value.trim(), hv = document.getElementById('cxHVal').value.trim();
    b.headers = mode === 'header' && hn && hv ? { [hn]: hv } : {};
    if (mode === 'oauth') b.auth = 'oauth';
    return b;
  }
  // OAuth: the service's sign-in page in a popup; it reports back by message.
  function cxSignIn(id) {
    const w = window.open('/api/mcp/oauth/start?id=' + encodeURIComponent(id), 'mcp-oauth', 'width=540,height=720');
    if (!w) { flash('Allow pop-ups for this site to sign in.', true, 'cxFlash'); return; }
    const timer = setInterval(() => { if (w.closed) { clearInterval(timer); cxLoad(); } }, 800);
  }
  window.addEventListener('message', e => {
    if (e.origin !== location.origin || !e.data || e.data.type !== 'mcp-oauth') return;
    flash(e.data.message, e.data.status !== 'ok', 'cxFlash');
    cxLoad();
  });
  // Well-known remote servers: one click fills the form.
  const CX_QUICK = [
    ['deepwiki', 'https://mcp.deepwiki.com/mcp', 'none', 'DeepWiki', 'Docs for any GitHub repo'],
    ['context7', 'https://mcp.context7.com/mcp', 'none', 'Context7', 'Up-to-date library docs'],
    ['huggingface', 'https://huggingface.co/mcp', 'none', 'Hugging Face', 'Models, datasets, Spaces'],
    ['cloudflare-docs', 'https://docs.mcp.cloudflare.com/mcp', 'none', 'Cloudflare Docs', 'Cloudflare documentation'],
    ['notion', 'https://mcp.notion.com/mcp', 'oauth', 'Notion', 'Sign in'],
    ['linear', 'https://mcp.linear.app/mcp', 'oauth', 'Linear', 'Sign in'],
    ['sentry', 'https://mcp.sentry.dev/mcp', 'oauth', 'Sentry', 'Sign in'],
    ['atlassian', 'https://mcp.atlassian.com/v1/sse', 'oauth', 'Atlassian', 'Jira & Confluence; sign in'],
    ['github', 'https://api.githubcopilot.com/mcp/', 'header', 'GitHub', 'Personal access token'],
    ['stripe', 'https://mcp.stripe.com', 'header', 'Stripe', 'Secret or restricted key'],
  ];
  function cxQuickRender() {
    const box = document.getElementById('cxQuick');
    if (!box) return;
    const have = new Set(cxServers.map(v => v.url));
    box.innerHTML = '<span class="muted">Quick add:</span>' + CX_QUICK.filter(q => !have.has(q[1])).map((q, i) =>
      '<button type="button" class="cx-chip" data-q="' + CX_QUICK.indexOf(q) + '" title="' + escAttr(q[4] + ' · ' + q[1]) + '">' + esc(q[3]) + '</button>').join('');
    box.querySelectorAll('[data-q]').forEach(b => b.onclick = () => {
      const [name, url, auth] = CX_QUICK[+b.dataset.q];
      document.getElementById('cxName').value = name;
      document.getElementById('cxURL').value = url;
      const sel = document.getElementById('cxAuth');
      sel.value = auth; sel.onchange();
      document.getElementById('cxHVal').value = '';
      (auth === 'header' ? document.getElementById('cxHVal') : document.querySelector('#cxForm button[type=submit]')).focus();
    });
  }
  if (cx) {
    document.getElementById('cxClose').onclick = () => cx.close();
    cx.addEventListener('click', e => { if (e.target === cx) cx.close(); });
    const authSel = document.getElementById('cxAuth');
    authSel.onchange = () => {
      document.getElementById('cxHeaderRow').hidden = authSel.value !== 'header';
      document.getElementById('cxOAuthHint').hidden = authSel.value !== 'oauth';
    };
    document.getElementById('cxForm').onsubmit = async e => {
      e.preventDefault();
      const b = cxFormBody('add');
      if (!b.name) b.name = (b.url.replace(/^https?:\/\//, '').split(/[./:]/)[0] || 'mcp').toLowerCase();
      const d = await cxPost(b);
      if (d) {
        e.target.reset(); document.getElementById('cxHName').value = 'Authorization'; authSel.onchange();
        document.getElementById('cxTestOut').innerHTML = '';
        if (b.auth === 'oauth') {
          const added = (d.servers || []).find(x => x.name === b.name);
          if (added) cxSignIn(added.id);
        } else flash('Added ' + b.name + '. Turn on 🌐 Web to use it.', false, 'cxFlash');
      }
    };
    document.getElementById('cxTest').onclick = async e => {
      const out = document.getElementById('cxTestOut'), b = cxFormBody('test');
      if (!b.url) { out.innerHTML = '<div class="cx-err">Enter the server URL.</div>'; return; }
      b.name = b.name || 'test';
      e.target.disabled = true; out.innerHTML = '<span class="muted">Connecting…</span>';
      const r = await api('/api/mcp', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b) });
      e.target.disabled = false;
      out.innerHTML = r.ok ? cxToolList(r.data.test) : '<div class="cx-err">✗ ' + esc(r.data.error || 'failed') + '</div>';
    };
    cxLoad();
  }


  return { open: () => { if (cx) { cxLoad(); cx.showModal(); } }, reload: cxLoad };
})();
