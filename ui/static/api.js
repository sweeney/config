// api.js — thin wrapper around the config service's /api/v1/config/*.
// Uses Auth.authedFetch so 401s automatically trigger refresh-and-retry.

window.ConfigAPI = (function () {
  'use strict';

  function api(path) { return '/api/v1/config' + path; }

  async function readJSON(resp) {
    const txt = await resp.text();
    try { return txt ? JSON.parse(txt) : null; }
    catch (_) { return txt; }
  }

  async function check(resp) {
    if (resp.ok) return readJSON(resp);
    const body = await readJSON(resp);
    const err = (body && body.message) || (body && body.error) || ('HTTP ' + resp.status);
    const e = new Error(err);
    e.status = resp.status;
    e.body   = body;
    throw e;
  }

  return {
    list:     async ()             => check(await Auth.authedFetch(api(''))),
    get:      async (ns)           => {
      const resp = await Auth.authedFetch(api('/' + encodeURIComponent(ns)));
      const document = await check(resp);
      return {
        document,
        readRole:  resp.headers.get('X-Read-Role'),
        writeRole: resp.headers.get('X-Write-Role'),
      };
    },
    put:      async (ns, doc) => check(await Auth.authedFetch(api('/' + encodeURIComponent(ns)), {
      method:  'PUT',
      headers: { 'Content-Type': 'application/json' },
      body:    typeof doc === 'string' ? doc : JSON.stringify(doc),
    })),
    create:   async (input)        => check(await Auth.authedFetch(api('/namespaces'), {
      method:  'POST',
      headers: { 'Content-Type': 'application/json' },
      body:    JSON.stringify(input),
    })),
    updateACL: async (ns, acl) => check(await Auth.authedFetch(api('/namespaces/' + encodeURIComponent(ns)), {
      method:  'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body:    JSON.stringify(acl),
    })),
    delete:   async (ns) => check(await Auth.authedFetch(api('/' + encodeURIComponent(ns)), {
      method: 'DELETE',
    })),
  };
})();
