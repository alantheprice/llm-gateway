/* LLM Gateway — chat with named browser-local sessions.
   Storage: llmgw_sessions = [{id,title,created,updated,model,messages:[{role,content,thinking,stats}]}]
            llmgw_active   = session id or ''
   Server never sees chat history.
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
  const newBtn = document.getElementById('newchat');

  function loadStore() {
    try { sessions = JSON.parse(localStorage.getItem('llmgw_sessions') || '[]'); } catch (e) { sessions = []; }
    activeId = localStorage.getItem('llmgw_active') || '';
    if (!sessions.find(s => s.id === activeId)) activeId = '';
  }
  function saveStore() {
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
          const label = t.name === 'web_search' ? '🌐 ' : '📄 ';
          const arg = (t.args || '').replace(/^"|"$/g, '').slice(0, 70);
          chip.textContent = label + (t.name === 'web_search' ? 'searching: ' : 'reading: ') + arg;
          if (t.status !== 'running') chip.textContent += ' ✓';
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
    if (!s) { renderLanding(); return; }
    if (!s.messages.length) { renderLanding(); return; }
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

  function renderLanding() {
    const list = [...sessions].sort((a, b) => b.updated - a.updated);
    landing.innerHTML =
      '<div class="card" style="margin:40px auto;max-width:640px">' +
      '<h2>Conversations</h2>' +
      (list.length ? '<ul class="session-list">' + list.map(s => {
        const st = s.messages.filter(m => m.stats && m.stats.tg);
        const avgTg = st.length ? Math.round(st.reduce((a, m) => a + m.stats.tg, 0) / st.length) : 0;
        return '<li data-id="' + esc(s.id) + '">' +
        '<span class="s-title">' + esc(s.title) + '</span>' +
        '<span class="s-meta">' + s.messages.length + ' msg' +
        (avgTg ? ' · ⚡' + avgTg + ' tok/s avg' : '') +
        ' · ' + fmtTime(s.updated) + '</span>' +
        '<span class="s-actions">' +
        '<button class="btn sm" data-act="rename" data-id="' + esc(s.id) + '" title="Rename">✎</button>' +
        '<button class="btn sm danger" data-act="del" data-id="' + esc(s.id) + '" title="Delete">🗑</button>' +
        '</span></li>';
      }).join('') + '</ul>'
        : '<p class="muted">No conversations yet. Start one below.</p>') +
      '<div class="row" style="margin-top:14px"><button class="btn primary" id="landing-new">+ Start a new chat</button></div></div>';
    landing.style.display = 'block';
    landing.querySelectorAll('li').forEach(li => {
      li.addEventListener('click', ev => {
        if (ev.target.closest('button')) return;
        activeId = li.dataset.id; saveStore(); render();
      });
    });
    landing.querySelectorAll('[data-act]').forEach(b => b.addEventListener('click', ev => {
      ev.stopPropagation();
      const s = sessions.find(x => x.id === b.dataset.id);
      if (!s) return;
      if (b.dataset.act === 'del') {
        if (!confirm('Delete "' + s.title + '"? This only affects this browser.')) return;
        sessions = sessions.filter(x => x.id !== s.id);
        if (activeId === s.id) activeId = '';
        saveStore(); renderLanding();
      } else if (b.dataset.act === 'rename') {
        const t = prompt('Rename conversation:', s.title);
        if (t && t.trim()) { s.title = t.trim().slice(0, 80); saveStore(); renderLanding(); }
      }
    }));
    const ln = document.getElementById('landing-new');
    if (ln) ln.addEventListener('click', () => { newSession(); render(); promptEl.focus(); });
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
    const headers = { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + cfg.api_key };
    if (useTools) headers['X-Session-Id'] = s.id;
    try {
      const resp = await fetch(url, {
        method: 'POST',
        signal: abortCtrl.signal,
        headers,
        body: JSON.stringify({
          model: modelEl.value, stream: !useTools,
          messages: s.messages.filter(m => !m.pending && m.content)
            .map(m => ({ role: m.role, content: m.content }))
        })
      });
      if (!resp.ok) {
        const err = await resp.text();
        a.content = '⚠ ' + resp.status + ': ' + err.slice(0, 300);
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
            } else if (ev === 'content') {
              a.content += d.content || '';
            } else if (ev === 'error') {
              a.content += '⚠ ' + (d.error || 'agent error');
            } else if (ev === 'done') {
              if (d.content && !a.content) a.content = d.content;
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

  function send() {
    if (busy) { if (abortCtrl) abortCtrl.abort(); return; }
    const text = promptEl.value.trim();
    if (!text || !cfg) return;
    promptEl.value = '';
    promptEl.style.height = 'auto';
    const s = ensureActive();
    if (s.messages.length === 0) s.title = text.slice(0, 60);
    s.model = modelEl.value;
    s.messages.push({ role: 'user', content: text });
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
    for (const mid of cfg.models) {
      const o = document.createElement('option');
      o.value = mid; o.textContent = mid;
      if (mid === 'qwen3.8-27b') o.selected = true;
      modelEl.appendChild(o);
    }
    loadStore();
    render();
  }

  promptEl.addEventListener('input', () => {
    promptEl.style.height = 'auto';
    promptEl.style.height = Math.min(promptEl.scrollHeight, 180) + 'px';
  });
  const toolsToggle = document.getElementById('toolsToggle');
  const toolsLabel = document.getElementById('toolsLabel');
  if (toolsToggle && toolsLabel) {
    toolsToggle.addEventListener('change', () => {
      toolsLabel.classList.toggle('on', toolsToggle.checked);
    });
  }
  sendBtn.addEventListener('click', () => { if (busy) { if (abortCtrl) abortCtrl.abort(); } else send(); });
  promptEl.addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); if (!busy) send(); }
  });
  newBtn.addEventListener('click', () => { newSession(); render(); promptEl.focus(); });
  init();
})();
