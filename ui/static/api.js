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
    // input: { name, read_role, write_role, document, confirm_public? }.
    // Serialised whole, so `confirm_public` — required when read_role is
    // `public` — rides along without special handling here. Without it the
    // API answers 400 {"error":"confirm_required"}, surfaced as err.body.error.
    create:   async (input)        => check(await Auth.authedFetch(api('/namespaces'), {
      method:  'POST',
      headers: { 'Content-Type': 'application/json' },
      body:    JSON.stringify(input),
    })),
    // acl: { read_role, write_role, confirm_public? }. Same as create: the
    // confirmation is just another body field, required only when this call
    // moves read_role to `public`.
    updateACL: async (ns, acl) => check(await Auth.authedFetch(api('/namespaces/' + encodeURIComponent(ns)), {
      method:  'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body:    JSON.stringify(acl),
    })),
    delete:   async (ns) => check(await Auth.authedFetch(api('/' + encodeURIComponent(ns)), {
      method: 'DELETE',
    })),
    // Admin-only, whatever the namespace's own ACL says — a public namespace
    // does not have a public history. Non-admins get 403 (401 anonymously),
    // surfaced here as err.status like any other failure.
    //
    // Actions are `create`, `acl_change`, `document_write` and `delete`. Only
    // the first three carry role fields — a `document_write` records that the
    // contents changed, who changed them and when, never the body itself.
    //
    // Entries come back oldest-first, and a namespace that never existed (or
    // has since been deleted) answers `200 []` rather than 404: an empty array
    // means "nothing recorded", never "no such namespace".
    audit:    async (ns) => check(await Auth.authedFetch(
      api('/namespaces/' + encodeURIComponent(ns) + '/audit'))),
  };
})();
