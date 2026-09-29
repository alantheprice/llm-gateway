/* LLM Gateway — chat with named sessions.
   Storage: llmgw_sessions = [{id,title,created,updated,model,messages:[{role,content,thinking,stats}]}]
            llmgw_active   = session id or ''
            llmgw_synced   = {id: updated} last saved to / loaded from the server
   Conversations are saved per user on the server (/api/chats) so they
   follow the user between devices; localStorage is the fast local copy.
   A session listed by the server but not yet loaded here has stale:true
   and empty messages until it's opened.
   stats: {tg, pp, cache_pct, draft_pct, out_tokens} from engine timings chunk. */
(function () {
  const CAP_MSGS = 200;
  let sessions = [];
  let activeId = '';
  let cfg = null;
  let busy = false;
  let abortCtrl = null;

  const msgs = document.getElementById('msgs');
  const landing = document.getElementById('landing');
  const promptEl = document.getElementById('prompt');
  const sendBtn = document.getElementById('send');
  const modelEl = document.getElementById('model');
  // Header ⓘ: the selected model's card on the Models page.
  const infoEl = document.getElementById('modelinfo');
  if (infoEl) infoEl.addEventListener('click', () => {
    if (modelEl.value) infoEl.href = '/models#' + encodeURIComponent(modelEl.value);
  });
  const newBtn = document.getElementById('newchat');

  function loadStore() {
    try { sessions = JSON.parse(localStorage.getItem('llmgw_sessions') || '[]'); } catch (e) { sessions = []; }
    activeId = localStorage.getItem('llmgw_active') || '';
    if (!sessions.find(s => s.id === activeId)) activeId = '';
  }
  function saveStore(push = true) {
    if (push) schedulePush();
    try {
      localStorage.setItem('llmgw_sessions', JSON.stringify(sessions));
      localStorage.setItem('llmgw_active', activeId);
    } catch (e) {
      while (sessions.length > 1) {
        sessions.sort((a, b) => a.updated - b.updated);
        sessions.shift();
        try { localStorage.setItem('llmgw_sessions', JSON.stringify(sessions)); return; } catch (e2) {}
      }
    }
  }
  const active = () => sessions.find(s => s.id === activeId);

  /* ---------- server sync ---------- */
  let synced = {};
  let serverSync = true;
  let pushTimer = null;
  function loadSynced() {
    try { synced = JSON.parse(localStorage.getItem('llmgw_synced') || '{}'); } catch (e) { synced = {}; }
  }
  function saveSynced() {
    try { localStorage.setItem('llmgw_synced', JSON.stringify(synced)); } catch (e) {}
  }
  const forServer = s => ({ title: s.title, model: s.model, created: s.created, updated: s.updated,
    messages: s.messages.filter(m => !m.pending).map(m => { const c = Object.assign({}, m); delete c.pending; return c; }) });
  function schedulePush() {
    clearTimeout(pushTimer);
    pushTimer = setTimeout(pushChanges, 1500);
  }
  // Save every session changed since it was last synced (not mid-reply).
  async function pushChanges() {
    if (!serverSync) return;
    for (const s of [...sessions]) {
      if (s.stale || !s.messages.length || (synced[s.id] || 0) >= s.updated) continue;
      if (busy && s.id === activeId) { schedulePush(); continue; }
      let r;
      try {
        r = await fetch('/api/chats/' + encodeURIComponent(s.id), { method: 'PUT',
          headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(forServer(s)) });
      } catch (e) { schedulePush(); return; }   // offline: retry later
      if (r.ok) synced[s.id] = s.updated;
      else if (r.status === 409) await loadFull(s);           // newer copy elsewhere wins
      else if (r.status === 503) { serverSync = false; break; } // no history store: local only
      else if (r.status === 413) { synced[s.id] = s.updated; console.warn('chat not saved:', (await r.json()).error); }
      else break;
    }
    saveSynced();
  }
  // Fetch one conversation's messages from the server.
  async function loadFull(s) {
    let r;
    try { r = await fetch('/api/chats/' + encodeURIComponent(s.id)); } catch (e) { return false; }
    if (r.status === 404) {
      sessions = sessions.filter(x => x.id !== s.id);
      if (activeId === s.id) activeId = '';
      delete synced[s.id];
    } else if (r.ok) {
      const d = await r.json();
      Object.assign(s, { title: d.title, model: d.model, created: d.created, updated: d.updated,
        messages: d.messages || [], stale: false });
      delete s.msgCount;
      synced[s.id] = d.updated;
    } else return false;
    saveStore(false); saveSynced();
    return true;
  }
  // Merge the server's list: new and newer conversations, deletions.
  async function pullChanges() {
    if (!serverSync) return;
    let r;
    try { r = await fetch('/api/chats'); } catch (e) { return; }
    if (r.status === 503) { serverSync = false; return; }
    if (!r.ok) return;
    const d = await r.json();
    let changed = false;
    for (const m of d.chats || []) {
      const s = sessions.find(x => x.id === m.id);
      const inUse = busy && m.id === activeId;
      if (m.deleted) {
        if (s && !inUse && s.updated <= m.updated) {
          sessions = sessions.filter(x => x.id !== m.id);
          if (activeId === m.id) activeId = '';
          changed = true;
        }
        delete synced[m.id];
        continue;
      }
      if (!s) {
        sessions.push({ id: m.id, title: m.title, model: m.model, created: m.created, updated: m.updated,
          messages: [], msgCount: m.msg_count, stale: true });
        synced[m.id] = m.updated;
        changed = true;
      } else if (m.updated > s.updated && !inUse) {
        Object.assign(s, { title: m.title, model: m.model, updated: m.updated, msgCount: m.msg_count, stale: true });
        synced[m.id] = m.updated;
        if (s.id === activeId) await loadFull(s);
        changed = true;
      }
    }
    saveSynced();
    if (changed) { saveStore(false); if (!busy) render(); }
    schedulePush(); // anything only this browser has (first sync uploads it)
  }
  // Open a session, fetching its messages first if needed.
  async function openSession(id) {
    activeId = id; saveStore(false);
    const s = active();
    if (s && s.stale) {
      msgs.innerHTML = '<p class="muted" style="text-align:center;margin-top:40px">Opening conversation…</p>';
      landing.style.display = 'none';
      if (!await loadFull(s)) { msgs.innerHTML = '<p class="muted" style="text-align:center;margin-top:40px">Couldn’t load this conversation. Check your connection.</p>'; return; }
    }
    render();
  }

  function newSession() {
    const s = { id: 's' + Date.now(), title: 'New chat', created: Date.now(), updated: Date.now(), model: modelEl.value || '', messages: [] };
    sessions.unshift(s);
    activeId = s.id;
    saveStore();
    return s;
  }

  function ensureActive() {
    if (!active()) newSession();
    return active();
  }

  /* ---------- markdown-lite renderer (airgap-safe, no deps) ----------
     Handles: fenced code blocks, inline code, bold, italic, headers,
     bullet/numbered lists, links. Escapes everything by construction:
     text nodes are created via textContent; only structure is markup. */
  function renderMarkdown(text) {
    const container = document.createElement('div');
    container.className = 'md';
    const parts = String(text).split(/```/);
    parts.forEach((part, i) => {
      if (i % 2 === 1) {  // fenced code block; first line may be a language tag
        const nl = part.indexOf('\n');
        const lang = nl > -1 && nl < 20 ? part.slice(0, nl).trim() : '';
        const code = nl > -1 ? part.slice(nl + 1) : part;
        const wrap = document.createElement('div');
        wrap.className = 'codeblock';
        const bar = document.createElement('div');
        bar.className = 'codebar';
        bar.innerHTML = '<span>' + esc(lang || 'code') + '</span>';
        const cp = document.createElement('button');
        cp.className = 'btn sm copycode'; cp.textContent = 'Copy';
        cp.addEventListener('click', () => {
          navigator.clipboard.writeText(code).then(() => {
            cp.textContent = 'Copied'; setTimeout(() => cp.textContent = 'Copy', 1500);
          });
        });
        bar.appendChild(cp);
        const pre = document.createElement('pre');
        pre.appendChild(document.createTextNode(code.replace(/\n$/, '')));
        wrap.appendChild(bar); wrap.appendChild(pre);
        container.appendChild(wrap);
      } else {
        renderInline(container, part);
      }
    });
    return container;
  }

  function renderInline(root, text) {
    const lines = text.split('\n');
    let list = null;
    for (const line of lines) {
      const h = line.match(/^(#{1,4})\s+(.*)/);
      const ul = line.match(/^\s*[-*]\s+(.*)/);
      const ol = line.match(/^\s*\d+\.\s+(.*)/);
      if (h) { list = null; const el = document.createElement('h' + (h[1].length + 2)); inlineSpan(el, h[2]); root.appendChild(el); }
      else if (ul || ol) {
        const tag = ul ? 'ul' : 'ol';
        if (!list || list.tagName.toLowerCase() !== tag) { list = document.createElement(tag); root.appendChild(list); }
        const li = document.createElement('li'); inlineSpan(li, (ul || ol)[1]); list.appendChild(li);
      } else if (line.trim() === '') { list = null; if (root.lastChild && root.lastChild.tagName !== 'P') root.appendChild(document.createElement('br')); }
      else {
        list = null;
        const p = document.createElement('p');
        p.style.margin = '0 0 6px';
        inlineSpan(p, line);
        root.appendChild(p);
      }
    }
  }

  function inlineSpan(el, text) {
    // tokenize **bold**, *italic*, `code`, [text](url)
    const re = /(\*\*[^*]+\*\*|\*[^*]+\*|`[^`]+`|\[[^\]]+\]\([^)]+\))/g;
    let last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) el.appendChild(document.createTextNode(text.slice(last, m.index)));
      const tok = m[0];
      if (tok.startsWith('**')) { const b = document.createElement('strong'); b.textContent = tok.slice(2, -2); el.appendChild(b); }
      else if (tok.startsWith('*')) { const em = document.createElement('em'); em.textContent = tok.slice(1, -1); el.appendChild(em); }
      else if (tok.startsWith('`')) { const c = document.createElement('code'); c.textContent = tok.slice(1, -1); el.appendChild(c); }
      else {
        const lm = tok.match(/\[([^\]]+)\]\(([^)]+)\)/);
        const a = document.createElement('a');
        a.href = /^https?:/.test(lm[2]) ? lm[2] : '#';
        a.target = '_blank'; a.rel = 'noopener';
        a.textContent = lm[1]; el.appendChild(a);
      }
      last = m.index + tok.length;
    }
    if (last < text.length) el.appendChild(document.createTextNode(text.slice(last)));
  }

  /* ---------- stats footer ---------- */
  function statsLine(st) {
    if (!st || !st.tg) return '';
    let s = '⚡ ' + Math.round(st.tg) + ' tok/s';
    if (st.pp) s += ' · pp ' + Math.round(st.pp);
    if (st.cache_pct != null) {
      s += ' · cache ' + Math.round(st.cache_pct) + '%';
      if (st.computed_n) s += ' (' + fmtNum(st.computed_n) + ' computed)';
    }
    if (st.draft_pct != null) s += ' · draft ' + Math.round(st.draft_pct) + '%';
    if (st.out_tokens) s += ' · ' + st.out_tokens + ' out';
    return s;
  }

  /* ---------- stats for nerds ---------- */
  // Model cards per pool/model name, from GET /v1/models/<name> (the gateway
  // lists each pool member's card summary, labelled by GPU).
  const cardCache = {};
  function loadCard(model) {
    if (!model || cardCache[model] !== undefined) return;
    cardCache[model] = null; // in flight / unavailable
    fetch('/v1/models/' + encodeURIComponent(model), {
      headers: { 'Authorization': 'Bearer ' + (cfg && cfg.api_key || '') }
    }).then(r => r.ok ? r.json() : null).then(j => {
      if (j) { cardCache[model] = j; render(); }
    }).catch(() => {});
  }
  function cardFor(nerd) {
    const info = nerd && cardCache[nerd.model];
    if (!info) return null;
    if (!info.pool) return info.model_card ? { summary: info.card_summary || null, card: info.model_card } : null;
    const members = info.members || [];
    const label = nerd.gateway && nerd.gateway.gpu_label;
    const m = members.find(x => x.gpu_label === label) || (members.length === 1 ? members[0] : null);
    return m ? { summary: m.card_summary, card: m.model_card } : null;
  }
  function nerdPanel(m) {
    const n = m.nerd || {};
    const st = n.stats || {};
    const gw = n.gateway || {};
    const rows = [];
    const add = (k, v) => { if (v !== undefined && v !== null && v !== '') rows.push([k, String(v)]); };
    const ms = v => (v == null ? null : (v >= 1000 ? (v / 1000).toFixed(2) + ' s' : Math.round(v) + ' ms'));
    add('Served by', gw.gpu_label ? gw.gpu_label + (gw.via === 'link' ? ' (via link)' : '') : null);
    add('Engine model', gw.served_model);
    const c = cardFor(n);
    const sum = c && c.summary;
    if (sum) {
      add('Model', sum.source_model);
      add('Weights', sum.weights);
      add('KV cache', sum.kv_cache);
      add('Speculative', sum.speculative ? sum.speculative + (sum.draft_tokens ? ' ×' + sum.draft_tokens : '') : null);
      add('GPU', sum.gpu ? sum.gpu + (sum.vram_mib ? ' (' + Math.round(sum.vram_mib / 1024) + ' GB)' : '') : null);
      add('Context', sum.context ? fmtNum(sum.context) + ' tokens' : null);
      add('Engine', sum.engine);
    }
    add('Time to first token', ms(st.ttft_ms));
    add('Queue wait', ms(st.queue_wait_ms));
    add('Prefill', ms(st.prefill_ms));
    add('Decode', ms(st.decode_ms));
    add('Decode speed', st.tokens_per_second ? Math.round(st.tokens_per_second) + ' tok/s' : null);
    if (st.prompt_tokens != null) {
      add('Prompt', fmtNum(st.prompt_tokens) + ' tokens' +
        (st.cache_hit_tokens != null ? ' · ' + fmtNum(st.cache_hit_tokens) + ' cached (' + Math.round(st.cache_hit_pct || 0) + '%)' : ''));
    }
    add('Cache reuse path', st.prefix_reuse_path);
    add('Output', st.completion_tokens != null ? fmtNum(st.completion_tokens) + ' tokens' +
      (st.reasoning_tokens ? ' (' + fmtNum(st.reasoning_tokens) + ' thinking)' : '') : null);
    const sp = st.speculative;
    if (sp) add('Draft acceptance', sp.backend + ' ×' + sp.draft_window + ': ' + sp.accepted_tokens + '/' + sp.draft_tokens +
      ' (' + Math.round(sp.acceptance_pct || 0) + '%)');
    if (!rows.length) return null;
    const det = document.createElement('details');
    det.className = 'nerd';
    det.open = !!m.nerdOpen;
    det.addEventListener('toggle', () => { m.nerdOpen = det.open; });
    const summ = document.createElement('summary');
    summ.textContent = 'Stats for nerds';
    det.appendChild(summ);
    const tbl = document.createElement('table');
    for (const [k, v] of rows) {
      const tr = document.createElement('tr');
      const th = document.createElement('th'); th.textContent = k;
      const td = document.createElement('td'); td.textContent = v;
      tr.appendChild(th); tr.appendChild(td); tbl.appendChild(tr);
    }
    det.appendChild(tbl);
    if (n.model) {
      const more = document.createElement('a');
      more.href = '/models#' + encodeURIComponent(n.model);
      more.textContent = 'Full model card →';
      more.className = 'nerd-more';
      det.appendChild(more);
    }
    return det;
  }

  /* ---------- rendering ---------- */
  function msgDiv(m, idx) {
    const row = document.createElement('div');
    row.className = 'msg-row ' + m.role;
    const av = document.createElement('div');
    av.className = 'avatar';
    av.textContent = m.role === 'user' ? (cfg && cfg.username ? cfg.username[0].toUpperCase() : 'U') : '\u26a1';
    row.appendChild(av);
    const div = document.createElement('div');
    div.className = 'msg ' + m.role;
    row.appendChild(div);
    if (m.role === 'assistant') {
      if (m.thinking) {
        const wrap = document.createElement('div');
        const label = document.createElement('span');
        label.className = 'thinking-label';
        label.textContent = '\u2727 Thinking';
        const th = document.createElement('div');
        th.className = 'thinking-body collapsed';
        th.textContent = m.thinking;
        label.addEventListener('click', () => th.classList.toggle('collapsed'));
        wrap.appendChild(label); wrap.appendChild(th);
        div.appendChild(wrap);
      }
      if (m.tools && m.tools.length) {
        for (const t of m.tools) {
          const chip = document.createElement('div');
          chip.className = 'toolchip';
          const arg = (t.args || '').replace(/^"|"$/g, '').slice(0, 70);
          if (t.name === 'notice') {
            chip.classList.add('warn');
            chip.textContent = '⚠ ' + (t.summary || '');
          } else if (t.name.includes('__')) {
            // A connector's tool: <server>__<tool>.
            const i = t.name.indexOf('__');
            chip.textContent = '🔌 ' + t.name.slice(0, i) + ' · ' + t.name.slice(i + 2) + (arg && arg !== '{}' ? ': ' + arg : '');
            if (t.status !== 'running') chip.textContent += t.status === 'error' ? ' ✗' : ' ✓';
            if (t.summary) chip.title = t.summary;
          } else {
            chip.textContent = (t.name === 'web_search' ? '🌐 searching: ' : '📄 reading: ') + arg;
            if (t.status !== 'running') chip.textContent += t.status === 'error' ? ' ✗' : ' ✓';
            if (t.status === 'error' && t.summary) chip.title = t.summary;
          }
          div.appendChild(chip);
        }
      }
      if (m.content) div.appendChild(renderMarkdown(m.content));
      if (m.stats && (m.stats.tg || m.stats.out_tokens)) {
        const f = document.createElement('div');
        f.className = 'statsline';
        f.textContent = statsLine(m.stats);
        div.appendChild(f);
      }
      if (m.nerd) {
        loadCard(m.nerd.model);
        const panel = nerdPanel(m);
        if (panel) div.appendChild(panel);
      }
      // actions on completed assistant messages
      if (idx != null && !m.pending) {
        const acts = document.createElement('div');
        acts.className = 'msg-actions';
        const cp = document.createElement('button');
        cp.className = 'btn sm'; cp.textContent = 'Copy';
        cp.addEventListener('click', () => {
          navigator.clipboard.writeText(m.content).then(() => {
            cp.textContent = 'Copied!'; setTimeout(() => cp.textContent = 'Copy', 1200);
          });
        });
        acts.appendChild(cp);
        if (idx === s_messagesLastAssistantIdx()) {
          const rg = document.createElement('button');
          rg.className = 'btn sm'; rg.textContent = 'Regenerate';
          rg.addEventListener('click', regenerate);
          acts.appendChild(rg);
        }
        div.appendChild(acts);
      }
    } else {
      div.appendChild(document.createTextNode(m.content || ''));
    }
    return row;
  }

  function s_messagesLastAssistantIdx() {
    const s = active();
    if (!s) return -1;
    for (let i = s.messages.length - 1; i >= 0; i--)
      if (s.messages[i].role === 'assistant') return i;
    return -1;
  }

  function render() {
    const s = active();
    landing.style.display = 'none';
    msgs.innerHTML = '';
    msgs.appendChild(landing); // the welcome screen lives inside #msgs
    if (!busy) renderConvos();
    if (!s || !s.messages.length) { renderWelcome(); return; }
    s.messages.forEach((m, i) => msgs.appendChild(msgDiv(m, i)));
    // typing indicator while the assistant message is pending with no text
    const last = s.messages[s.messages.length - 1];
    if (busy && last && last.role === 'assistant' && !last.content && !last.thinking && !(last.tools && last.tools.some(t => t.status === 'running'))) {
      const row = document.createElement('div');
      row.className = 'typing';
      row.innerHTML = '<i></i><i></i><i></i>';
      msgs.appendChild(row);
    }
    msgs.scrollTop = msgs.scrollHeight;
  }

  /* ---------- conversation list (side panel) ---------- */
  const convosEl = document.getElementById('convos');
  const convoList = document.getElementById('convoList');
  const convoSearch = document.getElementById('convoSearch');
  const convosBackdrop = document.getElementById('convosBackdrop');
  const msgCount = s => s.stale ? (s.msgCount || 0) : s.messages.length;
  function dayStart(offset) { const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() - offset); return d.getTime(); }
  function groupOf(t) {
    if (t >= dayStart(0)) return 'Today';
    if (t >= dayStart(1)) return 'Yesterday';
    if (t >= dayStart(7)) return 'Previous 7 days';
    if (t >= dayStart(30)) return 'Previous 30 days';
    return 'Older';
  }
  function whenLabel(t) {
    const d = new Date(t);
    if (t >= dayStart(0)) return d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
    if (t >= dayStart(7)) return d.toLocaleDateString(undefined, { weekday: 'short' });
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }
  function matches(s, q) {
    if (!q) return true;
    if ((s.title || '').toLowerCase().includes(q)) return true;
    return s.messages.some(m => (m.content || '').toLowerCase().includes(q));
  }
  function renderConvos() {
    const q = (convoSearch.value || '').trim().toLowerCase();
    // Empty chats (nothing sent yet) aren't listed; "New chat" is that state.
    const list = sessions.filter(s => msgCount(s) > 0 && matches(s, q)).sort((a, b) => b.updated - a.updated);
    let h = '', group = '';
    for (const s of list) {
      const g = groupOf(s.updated);
      if (g !== group) { h += '<div class="convo-group">' + g + '</div>'; group = g; }
      const n = msgCount(s);
      h += '<div class="convo' + (s.id === activeId ? ' active' : '') + '" role="link" tabindex="0" data-id="' + escAttr(s.id) + '"' +
        (s.id === activeId ? ' aria-current="page"' : '') + ' title="' + escAttr(s.title) + '">' +
        '<span class="c-text"><span class="c-title">' + esc(s.title) + '</span>' +
        '<span class="c-meta">' + esc(whenLabel(s.updated)) + ' · ' + n + ' message' + (n === 1 ? '' : 's') + '</span></span>' +
        '<span class="c-actions"><button type="button" data-act="rename" title="Rename" aria-label="Rename">✎</button>' +
        '<button type="button" class="del" data-act="del" title="Delete" aria-label="Delete">🗑</button></span></div>';
    }
    convoList.innerHTML = h || '<p class="convo-empty muted">' + (q ? 'No chats match “' + esc(q) + '”.' : 'No chats yet. Your conversations will appear here.') + '</p>';
    const title = document.getElementById('chatTitle');
    const a = active();
    if (title) title.textContent = a && msgCount(a) ? a.title : 'New chat';
  }
  convoList.addEventListener('click', ev => {
    const row = ev.target.closest('.convo');
    if (!row) return;
    const s = sessions.find(x => x.id === row.dataset.id);
    if (!s) return;
    const act = ev.target.closest('[data-act]');
    if (!act) { openSession(s.id); closeConvos(); return; }
    ev.stopPropagation();
    if (act.dataset.act === 'del') {
      if (!confirm('Delete "' + s.title + '"? It\'s removed from all your devices.')) return;
      sessions = sessions.filter(x => x.id !== s.id);
      if (activeId === s.id) activeId = '';
      delete synced[s.id]; saveSynced();
      if (serverSync) fetch('/api/chats/' + encodeURIComponent(s.id) + '?at=' + Date.now(), { method: 'DELETE' }).catch(() => {});
      saveStore(false); render();
    } else if (act.dataset.act === 'rename') {
      const t = prompt('Rename chat:', s.title);
      if (t && t.trim()) { s.title = t.trim().slice(0, 80); s.updated = Date.now(); saveStore(); renderConvos(); }
    }
  });
  convoList.addEventListener('keydown', ev => {
    if ((ev.key === 'Enter' || ev.key === ' ') && ev.target.classList.contains('convo')) { ev.preventDefault(); ev.target.click(); }
  });
  convoSearch.addEventListener('input', renderConvos);
  function openConvos() { convosEl.classList.add('open'); convosBackdrop.classList.add('show'); }
  function closeConvos() { convosEl.classList.remove('open'); convosBackdrop.classList.remove('show'); }
  document.getElementById('convosBtn').addEventListener('click', openConvos);
  document.getElementById('convosClose').addEventListener('click', closeConvos);
  convosBackdrop.addEventListener('click', closeConvos);
  // New chat: reuse an empty one instead of piling up blanks.
  function newChat() {
    const a = active();
    if (!a || msgCount(a) > 0 || busy) newSession();
    closeConvos(); render(); promptEl.focus();
  }
  document.getElementById('convosNew').addEventListener('click', newChat);

  // New-chat screen: the model in use and a few ways to start.
  const STARTERS = [
    'Explain a concept to me simply',
    'Help me write or edit something',
    'Review this code and suggest fixes',
    'Plan a project step by step',
  ];
  function renderWelcome() {
    landing.innerHTML = '<div class="welcome"><h2>What can I help with?</h2>' +
      '<p class="w-sub">Chatting with <strong>' + esc(modelEl.value || 'a model') + '</strong>. Turn on 🌐 Web to let it search the web and use your connectors.</p>' +
      '<div class="w-starters">' + STARTERS.map(t => '<button type="button">' + esc(t) + '</button>').join('') + '</div></div>';
    landing.style.display = 'block';
    landing.querySelectorAll('.w-starters button').forEach(b => b.onclick = () => {
      promptEl.value = b.textContent + ': ';
      promptEl.focus();
      promptEl.dispatchEvent(new Event('input'));
    });
  }

  /* ---------- chat ---------- */
  function setBusy(b) {
    busy = b;
    sendBtn.disabled = b;
    sendBtn.innerHTML = b ? 'Stop' : (ICONS['send'] + ' Send');
  }

  async function generate() {
    const s = ensureActive();
    const a = { role: 'assistant', content: '', thinking: '', pending: true, tools: [] };
    s.messages.push(a);
    s.updated = Date.now();
    busy = true; setBusy(true); render();
    abortCtrl = new AbortController();

    let tok0 = null, tokEnd = null;
    const useTools = document.getElementById('toolsToggle') &&
                     document.getElementById('toolsToggle').checked;
    // Agent mode goes through the gateway proxy (same-origin, auth-gated);
    // the gateway forwards to the seed-agent sidecar.
    const url = useTools ? '/v1/agent/chat' : '/v1/chat/completions';

    // Send one generation request. On 401 the stored UI key is stale (another
    // surface minted a newer one, or the gateway restarted): refetch
    // /chat/config — the session cookie is still valid, so this mints/
    // returns a fresh key — and retry ONCE before surfacing the error.
    async function sendOnce() {
      const headers = { 'Content-Type': 'application/json',
                        'Authorization': 'Bearer ' + cfg.api_key };
      if (useTools) headers['X-Session-Id'] = s.id;
      return fetch(url, {
        method: 'POST',
        signal: abortCtrl.signal,
        headers,
        body: JSON.stringify({
          model: modelEl.value, stream: !useTools,
          // Per-response engine stats (NInfer serving standard) for the
          // "Stats for nerds" panel; the gateway adds which GPU served it.
          return_stats: !useTools,
          messages: s.messages.filter(m => !m.pending && m.content)
            .map(m => ({ role: m.role, content: m.content }))
        })
      });
    }
    let resp = await sendOnce();
    if (resp.status === 401) {
      const cr = await fetch('/chat/config');
      if (cr.ok) {
        const c2 = await cr.json();
        if (c2.api_key && c2.api_key !== cfg.api_key) {
          cfg.api_key = c2.api_key;   // fresh key, same session
          resp = await sendOnce();
        }
      }
    }
    try {
      if (!resp.ok) {
        const body = await resp.text();
        if (resp.status === 429) {
          // 429 has two producers on this route: the daily-quota gate
          // (type "rate_limit_error", carries resets_at — a UTC instant to
          // localize) and the global per-IP throttle (type "rate_limited",
          // no reset). Branch on type so we don't mislabel a throttle 429
          // as a quota reset.
          try {
            const e = JSON.parse(body.slice(0, 2000)).error || {};
            if (e.type === 'rate_limit_error' && e.resets_at && typeof TZ !== 'undefined') {
              const used = (e.used != null && e.limit != null)
                ? ` (${e.used} of ${e.limit} tokens)` : '';
              a.content = '⚠ 429: Daily token limit reached' + used +
                ' — resets ' + TZ.instantLocal(e.resets_at, {month:'short', day:'numeric', hour:'numeric', minute:'2-digit'}) + '.';
            } else if (e.type === 'rate_limited') {
              a.content = '⚠ 429: Too many requests from your IP — slow down and retry.';
            } else {
              a.content = '⚠ 429: ' + (e.message || body.slice(0, 300));
            }
          } catch (e) {
            a.content = '⚠ 429: ' + body.slice(0, 300);
          }
        } else {
          a.content = '⚠ ' + resp.status + ': ' + body.slice(0, 300);
        }
      } else if (useTools) {
        // seed-agent SSE: start / tool_start / tool_end / content / done
        const reader = resp.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const lines = buf.split('\n');
          buf = lines.pop();
          let ev = '';
          for (const line of lines) {
            if (line.startsWith('event: ')) { ev = line.slice(7).trim(); continue; }
            if (!line.startsWith('data: ') || !ev) continue;
            let d = {};
            try { d = JSON.parse(line.slice(6)); } catch (e) { continue; }
            if (ev === 'tool_start') {
              a.tools.push({ name: d.tool || 'tool', args: d.args || '', status: 'running' });
              if (s.title === 'New chat') s.title = '🔎 ' + (d.args || 'search').slice(0, 58);
            } else if (ev === 'tool_end') {
              const t = [...a.tools].reverse().find(t => t.status === 'running');
              if (t) { t.status = d.status || 'done'; t.summary = d.summary || ''; }
            } else if (ev === 'notice') {
              a.tools.push({ name: 'notice', status: 'done', summary: d.message || '' });
            } else if (ev === 'content') {
              a.content += d.content || '';
            } else if (ev === 'reasoning') {
              a.thinking += d.content || '';
            } else if (ev === 'error') {
              a.content += '⚠ ' + (d.error || 'agent error');
            } else if (ev === 'done') {
              // The final answer; replaces text streamed on the way (e.g.
              // "let me search…" before a tool call).
              if (d.content) a.content = d.content;
            }
            s.updated = Date.now();
            render();
          }
        }
      } else {
        const reader = resp.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const lines = buf.split('\n');
          buf = lines.pop();
          for (const line of lines) {
            if (!line.startsWith('data: ')) continue;
            const data = line.slice(6).trim();
            if (data === '[DONE]') continue;
            try {
              const j = JSON.parse(data);
              if (j.timings) {
                const t = j.timings;
                // cache_n = tokens served from cache; prompt_n = tokens
                // computed this turn. Hit rate is cache / (cache+computed).
                const denom = (t.cache_n || 0) + (t.prompt_n || 0);
                a.stats = {
                  tg: t.predicted_per_second,
                  pp: t.prompt_per_second,
                  cache_pct: denom ? ((t.cache_n || 0) / denom) * 100 : null,
                  cached_n: t.cache_n || 0,
                  computed_n: t.prompt_n || 0,
                  draft_pct: t.draft_n ? (t.draft_n_accepted / t.draft_n) * 100 : null,
                  out_tokens: t.predicted_n,
                };
              }
              if (j.stats || j.gateway) {
                a.nerd = Object.assign(a.nerd || {}, { model: modelEl.value },
                  j.stats ? { stats: j.stats } : {}, j.gateway ? { gateway: j.gateway } : {});
                loadCard(modelEl.value);
              }
              const d = j.choices && j.choices[0] && j.choices[0].delta || {};
              if (d.reasoning_content) { a.thinking += d.reasoning_content; if (!tokEnd) tok0 = tok0 || performance.now(); }
              if (d.content) { a.content += d.content; tokEnd = performance.now(); }
            } catch (e) {}
          }
          render();
        }
      }
    } catch (e) {
      if (e.name === 'AbortError') { a.content += a.content ? '' : ' ⚠ stopped'; }
      else a.content += (a.content ? '\n' : '') + '⚠ connection error: ' + e;
    }
    delete a.pending;
    if (a.stats && tok0 && tokEnd && tokEnd > tok0) {
      a.stats.wall_tps = Math.round((a.stats.out_tokens || 0) / ((tokEnd - tok0) / 1000));
    }
    s.updated = Date.now();
    saveStore(); busy = false; abortCtrl = null; setBusy(false); render();
  }

  async function send() {
    if (busy) { if (abortCtrl) abortCtrl.abort(); return; }
    const text = promptEl.value.trim();
    if (!text || !cfg) return;
    if (active() && active().stale && !await loadFull(active())) return;
    promptEl.value = '';
    promptEl.style.height = 'auto';
    const s = ensureActive();
    if (s.messages.length === 0) s.title = text.slice(0, 60);
    s.model = modelEl.value;
    s.messages.push({ role: 'user', content: text });
    s.updated = Date.now();
    renderConvos();
    generate();
  }

  function regenerate() {
    if (busy) return;
    const s = active();
    if (!s) return;
    // drop trailing assistant message(s) and re-run from the last user turn
    while (s.messages.length && s.messages[s.messages.length - 1].role === 'assistant') s.messages.pop();
    if (!s.messages.length || s.messages[s.messages.length - 1].role !== 'user') { render(); return; }
    s.updated = Date.now();
    saveStore();
    generate();
  }

  /* ---------- init ---------- */
  async function init() {
    const r = await fetch('/chat/config');
    if (!r.ok) { location = '/'; return; }
    cfg = await r.json();
    // Chat-capable models only (no embedding / code-completion models),
    // grouped: shared models, your & shared GPUs, admin-only single GPUs.
    const groups = cfg.model_groups || [{ label: '', models: cfg.models || [] }];
    for (const g of groups) {
      let parent = modelEl;
      if (g.label && groups.length > 1) {
        parent = document.createElement('optgroup');
        parent.label = g.label;
        modelEl.appendChild(parent);
      }
      for (const mid of g.models) {
        const o = document.createElement('option');
        o.value = mid; o.textContent = mid;
        if (mid === 'qwen3.8-27b') o.selected = true; // default until you pick one
        parent.appendChild(o);
      }
    }
    // Remember the last model picked (when it's still available).
    let lastModel = '';
    try { lastModel = localStorage.getItem('chat.model') || ''; } catch (e) {}
    if (lastModel && [...modelEl.options].some(o => o.value === lastModel)) modelEl.value = lastModel;
    modelEl.addEventListener('change', () => { try { localStorage.setItem('chat.model', modelEl.value); } catch (e) {} });
    loadStore();
    loadSynced();
    const cur = active();
    if (cur && cur.stale) openSession(cur.id); else render();
    pullChanges();
    window.addEventListener('focus', pullChanges);
    setInterval(() => { if (!document.hidden) pullChanges(); }, 60000);
  }

  promptEl.addEventListener('input', () => {
    promptEl.style.height = 'auto';
    promptEl.style.height = Math.min(promptEl.scrollHeight, 180) + 'px';
  });
  const toolsToggle = document.getElementById('toolsToggle');
  const toolsLabel = document.getElementById('toolsLabel');
  if (toolsToggle && toolsLabel) {
    // Web mode stays as you left it.
    try { toolsToggle.checked = localStorage.getItem('chat.web') === '1'; } catch (e) {}
    toolsLabel.classList.toggle('on', toolsToggle.checked);
    toolsToggle.addEventListener('change', () => {
      toolsLabel.classList.toggle('on', toolsToggle.checked);
      try { localStorage.setItem('chat.web', toolsToggle.checked ? '1' : '0'); } catch (e) {}
    });
  }
  /* ---------- connectors (remote MCP servers) ---------- */
  const cx = document.getElementById('connectors');
  let cxServers = [];
  function cxLabel() {
    const on = cxServers.filter(x => x.enabled).length;
    const t = document.getElementById('toolsText');
    if (t) t.textContent = on ? 'Web + ' + on + ' connector' + (on > 1 ? 's' : '') : 'Web';
    if (toolsLabel) toolsLabel.title = on ? 'Let the model search the web and use your connectors' : 'Let the model search the web';
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
    document.getElementById('connectorsBtn').onclick = () => { cxLoad(); cx.showModal(); };
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

  sendBtn.addEventListener('click', () => { if (busy) { if (abortCtrl) abortCtrl.abort(); } else send(); });
  promptEl.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); if (!busy) send(); }
  });
  newBtn.addEventListener('click', newChat);
  modelEl.addEventListener('change', () => { const a = active(); if (!a || !a.messages.length) render(); });
  init();
})();
