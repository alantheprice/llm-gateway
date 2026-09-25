/* LLM Gateway service worker: app-shell caching + update flow.
   Strategy: network-first for pages/API (fresh always), cache-first for
   static assets (immutable-ish: versioned URLs). Never caches /v1/*. */
const CACHE = 'llmgw-v2';
const SHELL = [
  '/static/app.css', '/static/app.js', '/static/icons.js',
  '/static/favicon.svg', '/static/manifest.json'
];

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET') return;                    // POSTs pass through
  if (url.pathname.startsWith('/v1/')) return;               // never cache inference
  if (url.pathname.startsWith('/api/')) return;              // never cache API

  // Static assets: network-first with cache fallback (v= params bust on
  // deploy, but don't trust them — always try network first so CSS/JS
  // updates land immediately; cache is the offline fallback only).
  if (url.pathname.startsWith('/static/')) {
    e.respondWith(
      fetch(e.request).then(resp => {
        if (resp.ok) {
          const copy = resp.clone();
          caches.open(CACHE).then(c => c.put(e.request, copy));
        }
        return resp;
      }).catch(() =>
        caches.match(e.request).then(hit => hit || Response.error())
      )
    );
    return;
  }

  // Pages: network-first, cache fallback (offline shell)
  e.respondWith(
    fetch(e.request).then(resp => {
      if (resp.ok) {
        const copy = resp.clone();
        caches.open(CACHE).then(c => c.put(e.request, copy));
      }
      return resp;
    }).catch(() =>
      caches.match(e.request).then(hit => hit || caches.match('/chat'))
    )
  );
});
