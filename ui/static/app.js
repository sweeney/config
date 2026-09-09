// app.js — config admin SPA bootstrap, hash router, and CRUD views.
//
// Routes (hash-based so the SPA works without server-side rewrite rules):
//
//   #/             → list of namespaces visible to the caller
//   #/new          → create a new namespace
//   #/edit/{ns}    → view + edit a namespace's document and ACL
//
// Everything renders into <main id="app">. Each handler returns an
// element that the router mounts in place of the previous view.

(function () {
  'use strict';

  const APP        = document.getElementById('app');
  const USER_INFO  = document.getElementById('user-info');
  const LOGOUT_BTN = document.getElementById('logout-btn');
  const TOAST      = document.getElementById('toast');

  // ── helpers ────────────────────────────────────────────────────────
  function el(tag, attrs, children) {
    const e = document.createElement(tag);
    if (attrs) for (const k in attrs) {
      if (k === 'class')      e.className = attrs[k];
      else if (k === 'text')  e.textContent = attrs[k];
      else if (k === 'html')  e.innerHTML = attrs[k];
      else if (k === 'on')    for (const ev in attrs.on) e.addEventListener(ev, attrs.on[ev]);
      else if (attrs[k] === false) { /* omit */ }
      else if (attrs[k] === true)  e.setAttribute(k, '');
      else                         e.setAttribute(k, attrs[k]);
    }
    if (children) children.forEach(c => c && e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c));
    return e;
  }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  function mount(node) { clear(APP); APP.appendChild(node); }

  let toastT;
  function toast(msg, kind) {
    TOAST.textContent = msg;
    TOAST.className = kind === 'error' ? 'error' : '';
    TOAST.hidden = false;
    clearTimeout(toastT);
    toastT = setTimeout(() => { TOAST.hidden = true; }, 3500);
  }

  function fmtTime(s) {
    if (!s) return '';
    const d = new Date(s);
    if (isNaN(d.getTime())) return s;
    return d.toLocaleString();
  }

  function badge(role) {
    return el('span', { class: 'badge ' + role, text: role });
  }

  // The API's own `message` is the right thing to show for most failures.
  // `confirm_required` is the exception: it is not really an error about the
  // data, it is the server asking for a step this form can point at.
  function errText(err) {
    const body = err && err.body;
    if (body && body.error === 'confirm_required') {
      return 'Publishing needs confirmation: type the namespace name into the confirmation box before saving.';
    }
    return err.message;
  }

  // ── views ──────────────────────────────────────────────────────────

  // Anonymous landing — only shown if no token in localStorage.
  function viewLogin() {
    const btn = el('button', { text: 'Sign in with Identity', on: { click: () => Auth.startLogin() } });
    return el('div', null, [
      el('h1', { text: 'Config Admin' }),
      el('div', { class: 'button-row' }, [btn]),
    ]);
  }

  async function viewList() {
    const root = el('div');
    root.appendChild(el('div', { class: 'button-row' }, [
      el('h1', { text: 'Namespaces' }),
    ]));
    root.appendChild(el('div', { class: 'button-row right' }, [
      el('a', { class: 'btn', href: '#/new', text: 'New namespace' }),
    ]));

    const status = el('div', { class: 'loading', text: 'Loading…' });
    root.appendChild(status);

    try {
      const items = await ConfigAPI.list();
      root.removeChild(status);
      if (!items || items.length === 0) {
        root.appendChild(el('div', { class: 'empty', text: 'No namespaces yet.' }));
        return root;
      }
      const ul = el('ul', { class: 'namespace-list' });
      for (const ns of items) {
        const left = el('div', null, [
          el('a', { class: 'name', href: '#/edit/' + encodeURIComponent(ns.name), text: ns.name }),
          el('div', { class: 'meta', text: 'updated ' + fmtTime(ns.updated_at) }),
        ]);
        const right = el('div', { class: 'badges' }, [
          el('span', { class: 'meta', text: 'read' }), badge(ns.read_role),
          el('span', { class: 'meta', text: 'write' }), badge(ns.write_role),
        ]);
        ul.appendChild(el('li', null, [left, right]));
      }
      root.appendChild(ul);
    } catch (e) {
      root.removeChild(status);
      root.appendChild(el('div', { class: 'empty', text: 'Failed to load: ' + e.message }));
    }
    return root;
  }

  // Roles are an ordered lattice: public < user < admin. `public` is a read
  // role only — a namespace may be readable without a token, but nothing is
  // ever anonymously writable, so it must never appear in the write list.
  const READ_ROLES  = ['admin', 'user', 'public'];
  const WRITE_ROLES = ['admin', 'user'];

  function roleSelect(name, value, roles) {
    const sel = el('select', { name: name });
    for (const role of roles) {
      const opt = el('option', { value: role, text: role });
      if (value === role) opt.selected = true;
      sel.appendChild(opt);
    }
    return sel;
  }
  function readRoleSelect(name, value)  { return roleSelect(name, value, READ_ROLES); }
  function writeRoleSelect(name, value) { return roleSelect(name, value, WRITE_ROLES); }

  // ── publish confirmation ───────────────────────────────────────────
  //
  // A read role of `public` means the document can be fetched with no
  // Authorization header at all, and that is not reversible in any way that
  // matters: revoking stops future reads, but whatever has already been
  // fetched is out. So the API refuses to publish unless the request body
  // carries `confirm_public` equal to the namespace name, and this builds the
  // matching UI — a warning, plus a box the operator types the name into.
  //
  //   ┌────────────────────────────────────────────────────────────────┐
  //   │ DO NOT PRE-FILL THIS INPUT.                                    │
  //   │ Not with the namespace name, not from the name field, not on   │
  //   │ focus, not on select-public, not "as a convenience". The only  │
  //   │ thing this guard is worth is the human keystrokes. An          │
  //   │ auto-filled box confirms nothing: it turns the server check    │
  //   │ into a formality that an unrelated edit satisfies by accident, │
  //   │ which is precisely the accident the guard exists to stop.      │
  //   └────────────────────────────────────────────────────────────────┘
  //
  // opts:
  //   readSel     — the read-role <select> to watch
  //   submitBtn   — button to hold disabled until the typed name matches
  //   currentRole — the namespace's stored read role, or null when creating
  //   nameFn      — returns the namespace name as currently entered/known
  function publishGuard(opts) {
    let currentRole = opts.currentRole;

    const input = el('input', {
      type: 'text',
      // Deliberately hostile to anything that would fill this in on the
      // operator's behalf — browser autofill and password managers included.
      autocomplete:   'off',
      autocapitalize: 'off',
      autocorrect:    'off',
      spellcheck:     'false',
      placeholder:    'type the namespace name',
    });
    const hint = el('div', { class: 'form-help' });
    const box  = el('div', { class: 'publish-confirm', hidden: true }, [
      el('h3', { text: 'This will publish the namespace' }),
      el('p', { text: 'A read role of “public” makes this document readable by anyone who knows the URL, with no token at all. Revoking the role later stops new reads — it does not un-publish copies that have already been fetched.' }),
      el('label', { text: 'Type the namespace name to confirm' }),
      input,
      hint,
    ]);

    // True only for the *transition* into public. An already-public namespace
    // edited for some other reason is not publishing, and neither is revoking.
    function publishing() {
      return opts.readSel.value === 'public' && currentRole !== 'public';
    }
    function satisfied() {
      if (!publishing()) return true;
      const want = opts.nameFn();
      return want !== '' && input.value === want;
    }
    function sync() {
      const on = publishing();
      box.hidden = !on;
      // Drop anything typed once the box is hidden, so a confirmation can
      // never sit out of sight and re-arm itself if the role flips back.
      if (!on) input.value = '';
      const want = on ? opts.nameFn() : '';
      hint.textContent = !on          ? ''
        : want === ''                 ? 'Enter a namespace name above first.'
        : input.value === want        ? ''
        :                               'Must match “' + want + '” exactly.';
      opts.submitBtn.disabled = !satisfied();
    }

    opts.readSel.addEventListener('change', sync);
    input.addEventListener('input', sync);
    sync();

    return {
      box:        box,
      sync:       sync,
      publishing: publishing,
      // The raw typed value. Never substitute nameFn() here — see above.
      value:      () => input.value,
      // Call after a successful save: the role we just sent is now the stored
      // one, so a follow-up edit must not re-ask for the same confirmation.
      commit:     () => { currentRole = opts.readSel.value; },
    };
  }

  function viewNew() {
    const root = el('div');
    root.appendChild(el('h1', { text: 'New namespace' }));
    root.appendChild(el('p', { class: 'form-help', text: 'Names must match ^[a-z0-9_-]{1,64}$. Documents must be JSON objects (≤ 64KB).' }));

    const nameInp = el('input', { type: 'text', name: 'name', placeholder: 'e.g. mqtt_topics', autofocus: true });
    const readSel = readRoleSelect('read_role', 'admin');
    const writeSel = writeRoleSelect('write_role', 'admin');

    const editorHost = el('div');
    const editor = JSONEditor.create(editorHost, { value: '{}\n' });

    const errBox = el('div', { class: 'form-error' });
    const submitBtn = el('button', { type: 'submit', text: 'Create' });
    const cancelBtn = el('a', { class: 'btn btn-secondary', href: '#/', text: 'Cancel' });

    // A namespace being created has no prior read role, so choosing `public`
    // here is always a publish and always needs confirming.
    const guard = publishGuard({
      readSel:     readSel,
      submitBtn:   submitBtn,
      currentRole: null,
      nameFn:      () => nameInp.value.trim(),
    });
    // The confirmation is bound to the name, and the name is still being
    // typed: renaming after confirming invalidates the confirmation.
    nameInp.addEventListener('input', guard.sync);

    const form = el('form', {
      on: { submit: async (e) => {
        e.preventDefault();
        errBox.textContent = '';
        let parsed;
        try { parsed = editor.getValueOrThrow(); }
        catch (err) { errBox.textContent = err.message; return; }
        submitBtn.disabled = true;
        try {
          const body = {
            name:       nameInp.value.trim(),
            read_role:  readSel.value,
            write_role: writeSel.value,
            document:   parsed.obj,
          };
          // Sent only for a real transition into public, and only ever the
          // value the operator typed. The API answers `confirm_required` if
          // it is missing or does not echo the namespace name.
          if (guard.publishing()) body.confirm_public = guard.value();
          await ConfigAPI.create(body);
          toast('Namespace created');
          location.hash = '#/edit/' + encodeURIComponent(nameInp.value.trim());
        } catch (err) {
          errBox.textContent = errText(err);
        } finally {
          // Re-enables only if the form is still in a submittable state —
          // an unconfirmed publish stays disabled.
          guard.sync();
        }
      } },
    });
    form.appendChild(el('label', { text: 'Name', for: 'name' }));
    form.appendChild(nameInp);
    form.appendChild(el('label', { text: 'Read role' }));
    form.appendChild(readSel);
    form.appendChild(guard.box);
    form.appendChild(el('label', { text: 'Write role (must satisfy read role)' }));
    form.appendChild(writeSel);
    form.appendChild(el('label', { text: 'Initial document' }));
    form.appendChild(editorHost);
    form.appendChild(errBox);
    form.appendChild(el('div', { class: 'button-row' }, [submitBtn, cancelBtn]));
    root.appendChild(form);
    return root;
  }

  async function viewEdit(name) {
    const root = el('div');
    root.appendChild(el('h1', { text: name }));

    const status = el('div', { class: 'loading', text: 'Loading…' });
    root.appendChild(status);

    let doc, readRole, writeRole;
    try {
      ({ document: doc, readRole, writeRole } = await ConfigAPI.get(name));
    } catch (e) {
      root.removeChild(status);
      if (e.status === 404) {
        root.appendChild(el('div', { class: 'empty', text: 'Namespace not found (or you do not have read access).' }));
        return root;
      }
      root.appendChild(el('div', { class: 'empty', text: 'Failed to load: ' + e.message }));
      return root;
    }
    root.removeChild(status);

    // ─ Document edit ─
    root.appendChild(el('h2', { text: 'Document' }));
    const editorHost = el('div');
    const editor = JSONEditor.create(editorHost, { value: JSON.stringify(doc, null, 2) + '\n' });
    root.appendChild(editorHost);

    const saveBtn   = el('button', { type: 'button', text: 'Save' });
    const revertBtn = el('button', { type: 'button', class: 'btn-secondary', text: 'Revert' });
    const docErr    = el('div', { class: 'form-error' });

    saveBtn.addEventListener('click', async () => {
      docErr.textContent = '';
      let parsed;
      try { parsed = editor.getValueOrThrow(); }
      catch (err) { docErr.textContent = err.message; return; }
      saveBtn.disabled = true;
      try {
        const r = await ConfigAPI.put(name, parsed.source);
        toast(r && r.changed === false ? 'No change' : 'Saved');
      } catch (err) {
        docErr.textContent = err.message;
        toast('Save failed: ' + err.message, 'error');
      } finally {
        saveBtn.disabled = false;
      }
    });
    revertBtn.addEventListener('click', async () => {
      try {
        const fresh = await ConfigAPI.get(name);
        editor.setValue(JSON.stringify(fresh.document, null, 2) + '\n');
        toast('Reverted to stored version');
      } catch (err) {
        toast('Revert failed: ' + err.message, 'error');
      }
    });
    root.appendChild(el('div', { class: 'button-row' }, [saveBtn, revertBtn]));
    root.appendChild(docErr);

    // ─ ACL ─
    root.appendChild(el('h2', { text: 'Access control' }));
    const aclReadSel  = readRoleSelect('read_role',  readRole  || 'admin');
    const aclWriteSel = writeRoleSelect('write_role', writeRole || 'admin');
    const aclErr = el('div', { class: 'form-error' });
    const aclBtn = el('button', { type: 'button', text: 'Update ACL' });
    // The PATCH always sends both roles, so changing only the write role
    // re-sends read_role unchanged. Passing the namespace's stored read role
    // as currentRole is what keeps that quiet: the guard fires on the
    // transition into public, not on the value being public, so neither a
    // write-only edit of an already-public namespace nor a revoke asks for
    // confirmation.
    const aclGuard = publishGuard({
      readSel:     aclReadSel,
      submitBtn:   aclBtn,
      currentRole: readRole,
      nameFn:      () => name,
    });
    aclBtn.addEventListener('click', async () => {
      aclErr.textContent = '';
      aclBtn.disabled = true;
      try {
        const acl = { read_role: aclReadSel.value, write_role: aclWriteSel.value };
        if (aclGuard.publishing()) acl.confirm_public = aclGuard.value();
        await ConfigAPI.updateACL(name, acl);
        aclGuard.commit();
        toast('ACL updated');
      } catch (err) {
        aclErr.textContent = errText(err);
        toast('ACL update failed: ' + errText(err), 'error');
      } finally {
        aclGuard.sync();
      }
    });
    const aclGrid = el('div', { class: 'card' }, [
      el('label', { text: 'Read role' }), aclReadSel,
      aclGuard.box,
      el('label', { text: 'Write role (must satisfy read role)' }), aclWriteSel,
      el('div', { class: 'button-row' }, [aclBtn]),
      aclErr,
    ]);
    root.appendChild(aclGrid);

    // ─ Delete ─
    root.appendChild(el('h2', { text: 'Danger zone' }));
    const deleteBtn = el('button', { type: 'button', class: 'btn-danger', text: 'Delete namespace' });
    deleteBtn.addEventListener('click', async () => {
      if (!confirm('Delete namespace ' + name + '? This cannot be undone.')) return;
      try {
        await ConfigAPI.delete(name);
        toast('Deleted');
        location.hash = '#/';
      } catch (err) {
        toast('Delete failed: ' + err.message, 'error');
      }
    });
    root.appendChild(el('div', { class: 'card' }, [
      el('p', { class: 'form-help', text: 'Deleting a namespace is permanent and triggers a backup of the post-delete state.' }),
      el('div', { class: 'button-row' }, [deleteBtn]),
    ]));

    return root;
  }

  // ── router ─────────────────────────────────────────────────────────
  async function route() {
    if (!Auth.isAuthenticated()) {
      USER_INFO.textContent = '';
      LOGOUT_BTN.hidden = true;
      mount(viewLogin());
      return;
    }
    LOGOUT_BTN.hidden = false;

    const hash = location.hash || '#/';
    let view;
    try {
      if (hash === '#/' || hash === '')        view = await viewList();
      else if (hash === '#/new')               view = viewNew();
      else if (hash.startsWith('#/edit/'))     view = await viewEdit(decodeURIComponent(hash.slice('#/edit/'.length)));
      else                                     view = el('div', { class: 'empty', text: 'Unknown route. ' }, [
        el('a', { href: '#/', text: 'Back' })
      ]);
      mount(view);
    } catch (e) {
      if (e.message === 'session expired' || e.message === 'not authenticated') {
        Auth.clearTokens();
        await route();
        return;
      }
      mount(el('div', { class: 'empty', text: 'Error: ' + e.message }));
    }
  }

  // ── bootstrap ──────────────────────────────────────────────────────
  (async function init() {
    LOGOUT_BTN.addEventListener('click', async () => {
      await Auth.logout();
      location.hash = '#/';
      route();
    });
    window.addEventListener('hashchange', route);

    try {
      await Auth.bootstrap();
    } catch (e) {
      mount(el('div', { class: 'empty', text: 'Cannot reach config service: ' + e.message }));
      return;
    }

    try {
      const handled = await Auth.maybeHandleCallback();
      if (handled) toast('Signed in');
    } catch (e) {
      Auth.clearTokens();
      toast(e.message, 'error');
    }

    // Display username if we have a token (cheap call to identity).
    if (Auth.isAuthenticated()) {
      try {
        // Reuse the cached bootstrap config rather than refetching
        // /spa-config.json. We could also decode the JWT for the
        // username, but a network call exercises the auth path on
        // every page load and surfaces a stale token immediately.
        const cfg = Auth.getConfig();
        const meResp = await Auth.authedFetch(cfg.identity_url + '/api/v1/auth/me');
        if (meResp.ok) {
          const me = await meResp.json();
          USER_INFO.textContent = me.username + ' (' + me.role + ')';
        }
      } catch (e) {
        // 'session expired' from authedFetch means refresh failed; we
        // must clear tokens so route() will land on the login view
        // instead of leaving the UI half-authed (logout-button visible
        // but no real session).
        if (e && (e.message === 'session expired' || e.message === 'not authenticated')) {
          Auth.clearTokens();
        }
        // Other errors (network blip, identity 5xx) are non-fatal:
        // the username just won't render.
      }
    }

    await route();
  })();
})();
