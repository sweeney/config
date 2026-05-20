// editor.js — pimped <textarea> JSON editor.
//
// Affordances:
//   - Monospace styling (set in CSS)
//   - Tab inserts 2 spaces (instead of changing focus)
//   - Format button: pretty-prints valid JSON
//   - Live validation: parses on input, shows the parser's error message
//     and highlights the offending line in the toolbar status
//   - Validate-on-save guard via getValueOrThrow()
//
// Deliberately NOT a full editor — no syntax highlighting, no folding.
// CodeMirror is a follow-up if admins ask for it.

window.JSONEditor = (function () {
  'use strict';

  function el(tag, attrs, children) {
    const e = document.createElement(tag);
    if (attrs) for (const k in attrs) {
      if (k === 'class')      e.className = attrs[k];
      else if (k === 'text')  e.textContent = attrs[k];
      else                    e.setAttribute(k, attrs[k]);
    }
    if (children) children.forEach(c => e.appendChild(c));
    return e;
  }

  function deriveLineColumn(src, byteOffset) {
    let line = 1, col = 1;
    for (let i = 0; i < byteOffset && i < src.length; i++) {
      if (src.charCodeAt(i) === 10) { line++; col = 1; }
      else col++;
    }
    return { line, col };
  }

  // Parses error messages like "Unexpected token ',' ... at position 42"
  // (V8/Chromium) or with a position via SyntaxError stack on Firefox.
  // Falls back to the raw message if no position info is available.
  function annotate(src, err) {
    const m = String(err.message || '').match(/position (\d+)/);
    if (m) {
      const { line, col } = deriveLineColumn(src, parseInt(m[1], 10));
      return `JSON parse error at line ${line}, col ${col}: ${err.message}`;
    }
    return 'JSON parse error: ' + err.message;
  }

  function create(host, opts) {
    opts = opts || {};
    host.classList.add('editor-wrap');

    // Toolbar
    const status = el('span', { class: 'status' });
    const formatBtn = el('button', { type: 'button', class: 'btn-secondary', text: 'Format' });
    const tools = el('div', { class: 'lhs' }, [formatBtn]);
    const toolbar = el('div', { class: 'editor-toolbar' }, [tools, status]);

    // Textarea
    const ta = el('textarea', { class: 'editor', spellcheck: 'false', autocomplete: 'off', autocorrect: 'off', autocapitalize: 'off' });
    if (opts.value !== undefined) ta.value = opts.value;

    host.appendChild(toolbar);
    host.appendChild(ta);

    function setStatus(text, kind) {
      status.textContent = text;
      status.className = 'status' + (kind ? ' ' + kind : '');
    }

    function validate() {
      if (!ta.value.trim()) { setStatus('empty'); return null; }
      try {
        JSON.parse(ta.value);
        setStatus('valid JSON', 'ok');
        return true;
      } catch (e) {
        setStatus(annotate(ta.value, e), 'error');
        return false;
      }
    }

    function format() {
      try {
        const obj = JSON.parse(ta.value || '{}');
        ta.value = JSON.stringify(obj, null, 2) + '\n';
        validate();
      } catch (e) {
        setStatus(annotate(ta.value, e), 'error');
      }
    }

    // Tab inserts 2 spaces.
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Tab' && !e.shiftKey) {
        e.preventDefault();
        const start = ta.selectionStart, end = ta.selectionEnd;
        ta.value = ta.value.slice(0, start) + '  ' + ta.value.slice(end);
        ta.selectionStart = ta.selectionEnd = start + 2;
      }
    });

    ta.addEventListener('input', validate);
    formatBtn.addEventListener('click', format);

    // Initial status
    validate();

    return {
      element: ta,
      getValue: () => ta.value,
      // Throws on invalid JSON; returns the parsed object plus the source string.
      getValueOrThrow: () => {
        const val = ta.value.trim() || '{}';
        const obj = JSON.parse(val);
        if (Object.prototype.toString.call(obj) !== '[object Object]') {
          throw new Error('document must be a JSON object (got ' + (Array.isArray(obj) ? 'array' : typeof obj) + ')');
        }
        return { obj, source: val };
      },
      setValue: (v) => { ta.value = v; validate(); },
      isValid:  validate,
      format,
      focus:    () => ta.focus(),
    };
  }

  return { create };
})();
