// auth.js — OAuth Authorization Code + PKCE flow against identity.
//
// State management:
//   - access_token, refresh_token, token_expires_at  in localStorage
//   - pkce_verifier, oauth_state                      in sessionStorage
//     (cleared on tab close so a half-finished login doesn't leak across
//      browser sessions)
//
// Threat model: an XSS on this origin can read tokens from localStorage.
// We mitigate by serving a strict CSP from the server (script-src 'self'
// only) and by not embedding any third-party scripts. For this
// admin UI threat model this is acceptable; a SaaS would prefer
// HttpOnly cookies + a server-side session.

window.Auth = (function () {
  'use strict';

  let CONFIG = null;          // populated in bootstrap()
  let refreshInFlight = null; // Promise; coalesces concurrent refreshes

  // ── PKCE helpers ───────────────────────────────────────────────────
  function randomString(bytes) {
    const buf = new Uint8Array(bytes);
    crypto.getRandomValues(buf);
    return base64url(buf);
  }
  function base64url(buf) {
    return btoa(String.fromCharCode(...buf))
      .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  async function pkceChallenge(verifier) {
    const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
    return base64url(new Uint8Array(hash));
  }

  // ── Token storage ──────────────────────────────────────────────────
  function getAccessToken()  { return localStorage.getItem('access_token'); }
  function getRefreshToken() { return localStorage.getItem('refresh_token'); }
  function getExpiresAt()    { return parseInt(localStorage.getItem('token_expires_at') || '0', 10); }
  function saveTokens(t) {
    // Validate the response shape before persisting. A malformed identity
    // response (or a misconfigured proxy stripping fields) would otherwise
    // leave us with `localStorage.setItem('refresh_token', undefined)` —
    // which writes the literal string "undefined" — and wedge the SPA
    // permanently with no path back to a clean re-login.
    if (!t || typeof t !== 'object') {
      throw new Error('token response is not an object');
    }
    if (typeof t.access_token !== 'string' || !t.access_token) {
      throw new Error('token response missing access_token');
    }
    if (typeof t.refresh_token !== 'string' || !t.refresh_token) {
      throw new Error('token response missing refresh_token');
    }
    if (typeof t.expires_in !== 'number' || !(t.expires_in > 0)) {
      throw new Error('token response missing or invalid expires_in');
    }
    localStorage.setItem('access_token',     t.access_token);
    localStorage.setItem('refresh_token',    t.refresh_token);
    localStorage.setItem('token_expires_at', Date.now() + t.expires_in * 1000);
  }
  function clearTokens() {
    localStorage.removeItem('access_token');
    localStorage.removeItem('refresh_token');
    localStorage.removeItem('token_expires_at');
    sessionStorage.removeItem('pkce_verifier');
    sessionStorage.removeItem('oauth_state');
  }
  function isAuthenticated() {
    return !!getAccessToken();
  }

  // ── Login (redirect to identity) ───────────────────────────────────
  async function startLogin() {
    if (!CONFIG) throw new Error('Auth not bootstrapped');
    const verifier  = randomString(32);
    const state     = randomString(16);
    const challenge = await pkceChallenge(verifier);

    sessionStorage.setItem('pkce_verifier', verifier);
    sessionStorage.setItem('oauth_state',   state);

    const params = new URLSearchParams({
      response_type:         'code',
      client_id:             CONFIG.client_id,
      redirect_uri:          location.origin + '/',
      code_challenge:        challenge,
      code_challenge_method: 'S256',
      state:                 state,
    });
    location.href = CONFIG.identity_url + '/oauth/authorize?' + params;
  }

  // ── Callback (exchange code for tokens) ────────────────────────────
  // Returns true if a callback was handled; false if there was no code in the URL.
  async function maybeHandleCallback() {
    const params = new URLSearchParams(location.search);
    const code   = params.get('code');
    const state  = params.get('state');
    if (!code || !state) return false;

    const expected = sessionStorage.getItem('oauth_state');
    if (state !== expected) {
      throw new Error('OAuth state mismatch — possible CSRF; aborting');
    }
    const verifier = sessionStorage.getItem('pkce_verifier');
    if (!verifier) {
      throw new Error('PKCE verifier missing — start login again');
    }

    const resp = await fetch(CONFIG.identity_url + '/oauth/token', {
      method:  'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type:    'authorization_code',
        client_id:     CONFIG.client_id,
        code, redirect_uri: location.origin + '/',
        code_verifier: verifier,
      }),
    });
    const body = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      throw new Error('Token exchange failed: ' + (body.error || resp.status));
    }
    saveTokens(body);
    sessionStorage.removeItem('pkce_verifier');
    sessionStorage.removeItem('oauth_state');

    // Strip ?code & ?state from the URL so a refresh doesn't replay.
    history.replaceState(null, '', location.pathname + location.hash);
    return true;
  }

  // ── Refresh ────────────────────────────────────────────────────────
  // Coalesces concurrent calls so 5 simultaneous 401s only kick off one refresh.
  // Returns true on success, false on any failure (network, server error,
  // malformed response). Network failures are intentionally treated as
  // refresh-failures so the SPA can route to the login screen instead of
  // throwing an unhandled error from a transient blip.
  function refresh() {
    if (refreshInFlight) return refreshInFlight;
    refreshInFlight = (async () => {
      const rt = getRefreshToken();
      if (!rt) return false;
      let resp;
      try {
        resp = await fetch(CONFIG.identity_url + '/oauth/token', {
          method:  'POST',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: new URLSearchParams({
            grant_type:    'refresh_token',
            refresh_token: rt,
          }),
        });
      } catch (_) {
        return false; // network failure — caller will treat as session expired
      }
      if (!resp.ok) return false;
      let body;
      try { body = await resp.json(); }
      catch (_) { return false; }
      try { saveTokens(body); }
      catch (_) { return false; }
      return true;
    })().finally(() => { refreshInFlight = null; });
    return refreshInFlight;
  }

  // ── Authed fetch wrapper ───────────────────────────────────────────
  // Adds Authorization header. On 401, tries refresh-and-retry once.
  async function authedFetch(url, opts = {}) {
    const tok = getAccessToken();
    if (!tok) throw new Error('not authenticated');
    opts.headers = Object.assign({}, opts.headers, { Authorization: 'Bearer ' + tok });
    let resp = await fetch(url, opts);
    if (resp.status !== 401) return resp;

    const ok = await refresh();
    if (!ok) {
      clearTokens();
      throw new Error('session expired');
    }
    opts.headers.Authorization = 'Bearer ' + getAccessToken();
    return fetch(url, opts);
  }

  // ── Logout ─────────────────────────────────────────────────────────
  // Best-effort revocation on identity, then clear local state.
  // Identity's /api/v1/auth/logout is auth-gated — it identifies the
  // session via the access token and revokes the supplied refresh
  // token (or the whole family if none is supplied). Without the
  // Bearer header we'd 401 every time and never actually revoke.
  async function logout() {
    const tok = getAccessToken();
    const rt  = getRefreshToken();
    if (tok && rt) {
      try {
        await fetch(CONFIG.identity_url + '/api/v1/auth/logout', {
          method:  'POST',
          headers: {
            'Content-Type':  'application/json',
            'Authorization': 'Bearer ' + tok,
          },
          body: JSON.stringify({ refresh_token: rt }),
        });
      } catch (_) { /* network failure — local clear is the source of truth */ }
    }
    clearTokens();
  }

  // ── Bootstrap ──────────────────────────────────────────────────────
  // Must be called first. Returns the resolved config so callers can
  // pass it on to the API layer.
  async function bootstrap() {
    const resp = await fetch('/spa-config.json');
    if (!resp.ok) throw new Error('failed to load /spa-config.json: ' + resp.status);
    CONFIG = await resp.json();
    if (!CONFIG.identity_url || !CONFIG.client_id) {
      throw new Error('SPA config missing identity_url or client_id');
    }
    return CONFIG;
  }

  // getConfig returns the cached bootstrap config so callers (e.g. the
  // username-display path) don't need to refetch /spa-config.json.
  function getConfig() {
    if (!CONFIG) throw new Error('Auth not bootstrapped');
    return CONFIG;
  }

  return {
    bootstrap,
    getConfig,
    startLogin,
    maybeHandleCallback,
    refresh,
    authedFetch,
    logout,
    isAuthenticated,
    getAccessToken,
    clearTokens,
  };
})();
