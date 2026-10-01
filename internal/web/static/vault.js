/* Vault: per-user at-rest encryption for chats + documents.
 *
 * The gateway stores each user's data sealed with a random DEK that lives only
 * in gateway memory. To (re)unlock it, the browser derives a 32-byte "wrap
 * key" from the user's login password via PBKDF2-SHA256(password, salt) and
 * hands it to the gateway, which unwraps the DEK. The password itself never
 * leaves the browser, and the gateway only stores a one-way hash of it — so a
 * stolen data.db + users.json + password-hash set cannot be opened.
 *
 * This file is dependency-free and runs on plain-HTTP (no secure context):
 * crypto.subtle isn't guaranteed on a LAN gateway, so we ship a pure-JS
 * PBKDF2-SHA256 and verify it against a reference in tests.
 */
(function () {
  'use strict';

  // ---- SHA-256 (pure JS, returns a 32-byte Array) ----
  // K is derived, not transcribed: the first word of the fractional part of
  // the cube root of the i-th prime (FIPS 180-4 §5.3.3). Deriving it at load
  // time removes the entire "typed the constant wrong" failure mode.
  function isPrime(n) {
    if (n < 2) return false;
    if (n < 4) return true;
    if (n % 2 === 0 || n % 3 === 0) return false;
    for (let f = 5; f * f <= n; f += 6) if (n % f === 0 || n % (f + 2) === 0) return false;
    return true;
  }
  const K = (() => {
    const out = [];
    let p = 2;
    while (out.length < 64) {
      if (isPrime(p)) out.push(Math.floor((Math.cbrt(p) % 1) * 0x100000000) >>> 0);
      p++;
    }
    return out;
  })();
  function rotr(x, n) { return (x >>> n) | (x << (32 - n)); }
  // sha256(bytes: Uint8Array) -> 32-byte Uint8Array
  function sha256(bytes) {
    const H = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
    const len = bytes.length;
    // Pad: append 0x80, then zeros, then an 8-byte big-endian bit-length.
    // Total size = smallest multiple of 64 that is >= len + 9.
    const total = (len + 9 + 63) & ~63;
    const m = new Uint8Array(total);
    m.set(bytes);
    m[len] = 0x80;
    const dv = new DataView(m.buffer);
    dv.setUint32(total - 8, Math.floor(len / 0x20000000), false); // bit-length hi
    dv.setUint32(total - 4, (len * 8) >>> 0, false);             // bit-length lo
    const w = new Array(64);
    for (let off = 0; off < total; off += 64) {
      for (let t = 0; t < 16; t++) w[t] = dv.getUint32(off + t * 4, false);
      for (let t = 16; t < 64; t++) {
        const s0 = rotr(w[t - 15], 7) ^ rotr(w[t - 15], 18) ^ (w[t - 15] >>> 3);
        const s1 = rotr(w[t - 2], 17) ^ rotr(w[t - 2], 19) ^ (w[t - 2] >>> 10);
        w[t] = (w[t - 16] + s0 + w[t - 7] + s1) | 0;
      }
      let [a, b, c, d, e, f, g, h] = H;
      for (let t = 0; t < 64; t++) {
        const S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
        const ch = (e & f) ^ (~e & g);
        const t1 = (h + S1 + ch + K[t] + w[t]) | 0;
        const S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
        const maj = (a & b) ^ (a & c) ^ (b & c);
        const t2 = (S0 + maj) | 0;
        h = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
      }
      H[0] = (H[0] + a) | 0; H[1] = (H[1] + b) | 0; H[2] = (H[2] + c) | 0; H[3] = (H[3] + d) | 0;
      H[4] = (H[4] + e) | 0; H[5] = (H[5] + f) | 0; H[6] = (H[6] + g) | 0; H[7] = (H[7] + h) | 0;
    }
    const out = new Uint8Array(32);
    const ov = new DataView(out.buffer);
    for (let i = 0; i < 8; i++) ov.setUint32(i * 4, H[i], false);
    return out;
  }

  // ---- HMAC-SHA256(key, msg) -> 32-byte Uint8Array ----
  function hmacSha256(key, msg) {
    let k = new Uint8Array(64);
    if (key.length > 64) k = sha256(key);
    k.set(key);
    const inner = new Uint8Array(64 + msg.length);
    for (let i = 0; i < 64; i++) inner[i] = k[i] ^ 0x36;
    inner.set(msg, 64);
    const ih = sha256(inner);
    const outer = new Uint8Array(64 + 32);
    for (let i = 0; i < 64; i++) outer[i] = k[i] ^ 0x5c;
    outer.set(ih, 64);
    return sha256(outer);
  }

  // ---- PBKDF2-HMAC-SHA256(password, salt, iters, 32) -> Uint8Array ----
  // Single block (dkLen=32), so no block chaining. RFC 2898: U1 =
  // HMAC(P, S || INT32-BE(1)); U_{i+1} = HMAC(P, U_i); DK = XOR of all U.
  function pbkdf2Sha256(password, salt, iterations) {
    const pw = new TextEncoder().encode(password);
    const salt1 = new Uint8Array(salt.length + 4);
    salt1.set(salt);
    salt1.set([0, 0, 0, 1], salt.length); // block index 1
    let U = hmacSha256(pw, salt1);
    const out = new Uint8Array(32);
    for (let i = 0; i < 32; i++) out[i] = U[i];
    for (let n = 1; n < iterations; n++) {
      U = hmacSha256(pw, U);
      for (let i = 0; i < 32; i++) out[i] ^= U[i];
    }
    return out;
  }

  // ---- hex <-> bytes ----
  function bytesToHex(b) { let s = ''; for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, '0'); return s; }
  function hexToBytes(h) {
    h = (h || '').toLowerCase();
    if (h.length % 2) h = '0' + h;
    const out = new Uint8Array(h.length / 2);
    for (let i = 0; i < out.length; i++) out[i] = parseInt(h.substr(i * 2, 2), 16);
    return out;
  }

  // Default KDF params (mirror internal/vault: PBKDF2-SHA256, 100k, 32B key).
  const DEFAULTS = { iterations: 100000, hash: 'SHA-256' };

  // deriveWrapKey(password, saltHex, iters?) -> 64-hex-char wrap key.
  function deriveWrapKey(password, saltHex, iterations) {
    const it = iterations || DEFAULTS.iterations;
    const salt = saltHex ? hexToBytes(saltHex) : new Uint8Array(32);
    return bytesToHex(pbkdf2Sha256(password || '', salt, it));
  }

  // ---- API client (session-cookie based) ----
  async function get(path, opts) {
    const r = await fetch(path, Object.assign({ headers: { Accept: 'application/json' } }, opts));
    const ct = r.headers.get('content-type') || '';
    if (!ct.includes('application/json')) return { ok: r.ok, status: r.status, data: {} };
    let d = {}; try { d = await r.json(); } catch (e) {}
    return { ok: r.ok, status: r.status, data: d };
  }
  function post(path, body) {
    return get(path, { method: 'POST', body: JSON.stringify(body) });
  }

  // randomSalt -> 64-hex-char 32-byte salt (the KDF input; public by design).
  function randomSalt() {
    const b = new Uint8Array(32);
    if (window.crypto && crypto.getRandomValues) crypto.getRandomValues(b);
    else for (let i = 0; i < 32; i++) b[i] = (Math.random() * 256) & 255;
    return bytesToHex(b);
  }

  const Vault = {
    // Status + KDF params (session). {enabled, unlocked, salt, kdf:{iterations,hash}, migration}
    status: () => get('/api/vault'),
    // Pre-login salt lookup (no session): the login page derives the wrap key
    // from it. Returns {ok, data:{salt, enabled, iterations, hash}}.
    wrapInfo: (username) => get('/api/vault/wrapinfo?user=' + encodeURIComponent(username)),
    enable: (wrapKey, saltHex) => post('/api/vault/enable', { wrap_key: wrapKey, salt: saltHex || null }),
    unlock: (wrapKey) => post('/api/vault/unlock', { wrap_key: wrapKey }),
    lock: () => post('/api/vault/lock'),
    rekey: (oldKey, newKey) => post('/api/vault/rekey', { old_key: oldKey, new_key: newKey }),
    derive: deriveWrapKey,
    randomSalt,
    // Create the vault on first use: fresh salt, wrap key from the password.
    async enableWithPassword(password) {
      const s = await this.status();
      if (s.ok && s.data && s.data.enabled) return { ok: true }; // already enabled
      const salt = randomSalt();
      const key = deriveWrapKey(password, salt);
      const e = await this.enable(key, salt);
      return e.ok ? { ok: true } : { ok: false, error: (e.data && e.data.error) || 'could not enable' };
    },
    DEFAULTS,
    // Re-unlock with a password: fetch salt, derive, POST unlock.
    async unlockWithPassword(password) {
      const s = await this.status();
      if (!s.ok || !s.data.salt) return { ok: false, error: 'no vault salt' };
      const key = deriveWrapKey(password, s.data.salt, s.data.kdf && s.data.kdf.iterations);
      const u = await this.unlock(key);
      return u.ok ? { ok: true } : { ok: false, error: (u.data && u.data.error) || 'wrong password' };
    },
  };

  // ---- Global "vault locked" interceptor ----
  // Wraps window.fetch: a 503 with code "vault_locked" opens a one-shot modal,
  // asks for the password, re-unlocks, and retries the original request.
  // Vault endpoints are never intercepted (they are the unlock path itself).
  (function installInterceptor() {
    if (window.__vaultInterceptorInstalled) return;
    window.__vaultInterceptorInstalled = true;
    const isVaultURL = (u) => String(u).indexOf('/api/vault') !== -1;
    let modalPromise = null;
    function openModal() {
      if (modalPromise) return modalPromise;
      modalPromise = new Promise((resolve) => {
        const overlay = document.createElement('div');
        overlay.className = 'vlt-overlay';
        overlay.innerHTML =
          '<div class="vlt-card">' +
          '  <h3>Unlock your data</h3>' +
          '  <p class="vlt-sub">Your chats and documents are encrypted and private to you. ' +
          'Enter your password to unlock them for this session.</p>' +
          '  <input type="password" autocomplete="current-password" placeholder="Password">' +
          '  <div class="vlt-actions">' +
          '    <button class="vlt-cancel">Cancel</button>' +
          '    <button class="btn primary vlt-go">Unlock</button>' +
          '  </div><div class="vlt-err" style="display:none"></div>' +
          '</div>';
        document.body.appendChild(overlay);
        const input = overlay.querySelector('input');
        const errEl = overlay.querySelector('.vlt-err');
        const finish = (ok) => {
          if (document.body.contains(overlay)) overlay.remove();
          modalPromise = null;
          resolve(ok);
        };
        overlay.querySelector('.vlt-cancel').onclick = () => finish(false);
        overlay.querySelector('.vlt-go').onclick = () => {
          Vault.unlockWithPassword(input.value).then((res) => {
            if (res.ok) finish(true);
            else { errEl.textContent = res.error || 'Wrong password.'; errEl.style.display = 'block'; input.focus(); }
          });
        };
        input.onkeydown = (e) => { if (e.key === 'Enter') overlay.querySelector('.vlt-go').click(); };
        input.focus();
      });
      return modalPromise;
    }

    const origFetch = window.fetch.bind(window);
    window.fetch = async function (input, init) {
      const url = typeof input === 'string' ? input : (input && input.url) || '';
      const res = await origFetch(input, init);
      if (res.status === 503 && !isVaultURL(url)) {
        // Peek the body to check the code, then retry if it was a lock.
        const clone = res.clone();
        let code = null;
        try { const j = await clone.json(); code = j && j.code; } catch (e) {}
        if (code === 'vault_locked') {
          const ok = await openModal();
          if (ok) {
            const retry = await origFetch(input, init);
            return retry;
          }
          return res; // user cancelled → surface the original 503
        }
      }
      return res;
    };
  })();

  window.Vault = Vault;
  if (typeof module !== 'undefined' && module.exports) module.exports = { deriveWrapKey, pbkdf2Sha256, sha256, hmacSha256, bytesToHex, hexToBytes };
})();
