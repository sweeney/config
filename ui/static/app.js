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

  function roleSelect(name, value) {
    const sel = el('select', { name: name });
    for (const role of ['admin', 'user']) {
      const opt = el('option', { value: role, text: role });
      if (value === role) opt.selected = true;
      sel.appendChild(opt);
    }
    return sel;
  }

  function viewNew() {
    const root = el('div');
    root.appendChild(el('h1', { text: 'New namespace' }));
    root.appendChild(el('p', { class: 'form-help', text: 'Names must match ^[a-z0-9_-]{1,64}$. Documents must be JSON objects (≤ 64KB).' }));

    const nameInp = el('input', { type: 'text', name: 'name', placeholder: 'e.g. mqtt_topics', autofocus: true });
    const readSel = roleSelect('read_role', 'admin');
    const writeSel = roleSelect('write_role', 'admin');

    const editorHost = el('div');
    const editor = JSONEditor.create(editorHost, { value: '{}\n' });

    const errBox = el('div', { class: 'form-error' });
    const submitBtn = el('button', { type: 'submit', text: 'Create' });
    const cancelBtn = el('a', { class: 'btn btn-secondary', href: '#/', text: 'Cancel' });

    const form = el('form', {
      on: { submit: async (e) => {
        e.preventDefault();
        errBox.textContent = '';
        let parsed;
        try { parsed = editor.getValueOrThrow(); }
        catch (err) { errBox.textContent = err.message; return; }
        submitBtn.disabled = true;
        try {
          await ConfigAPI.create({
            name:       nameInp.value.trim(),
            read_role:  readSel.value,
            write_role: writeSel.value,
            document:   parsed.obj,
          });
          toast('Namespace created');
          location.hash = '#/edit/' + encodeURIComponent(nameInp.value.trim());
        } catch (err) {
          errBox.textContent = err.message;
        } finally {
          submitBtn.disabled = false;
        }
      } },
    });
    form.appendChild(el('label', { text: 'Name', for: 'name' }));
    form.appendChild(nameInp);
    form.appendChild(el('label', { text: 'Read role' }));
    form.appendChild(readSel);
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
    const aclReadSel  = roleSelect('read_role',  readRole  || 'admin');
    const aclWriteSel = roleSelect('write_role', writeRole || 'admin');
    const aclErr = el('div', { class: 'form-error' });
    const aclBtn = el('button', { type: 'button', text: 'Update ACL' });
    aclBtn.addEventListener('click', async () => {
      aclErr.textContent = '';
      aclBtn.disabled = true;
      try {
        await ConfigAPI.updateACL(name, { read_role: aclReadSel.value, write_role: aclWriteSel.value });
        toast('ACL updated');
      } catch (err) {
        aclErr.textContent = err.message;
        toast('ACL update failed: ' + err.message, 'error');
      } finally {
        aclBtn.disabled = false;
      }
    });
    const aclGrid = el('div', { class: 'card' }, [
      el('label', { text: 'Read role' }), aclReadSel,
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
