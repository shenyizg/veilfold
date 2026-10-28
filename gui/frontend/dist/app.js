// Veilfold desktop UI. Plain JavaScript, no build step. The Go backend is
// reached through the bindings Wails injects as window.go.main.App, and
// pushes progress through window.runtime events.
'use strict';

(() => {
  // ------------------------------------------------------------------ helpers

  const $ = (sel, root = document) => root.querySelector(sel);
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const ICONS = {
    folder: '<path d="M3 7.5A2.5 2.5 0 0 1 5.5 5h3.6l2 2.2h7.4A2.5 2.5 0 0 1 21 9.7v7.8a2.5 2.5 0 0 1-2.5 2.5h-13A2.5 2.5 0 0 1 3 17.5z"/>',
    lock: '<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8 10.5V8a4 4 0 0 1 8 0v2.5"/>',
    unlock: '<rect x="5" y="10.5" width="14" height="10" rx="2.5"/><path d="M8 10.5V8a4 4 0 0 1 7.6-1.7"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
    logout: '<path d="M14 4h3.5A2.5 2.5 0 0 1 20 6.5v11a2.5 2.5 0 0 1-2.5 2.5H14"/><path d="M10 8l-4 4 4 4M6 12h9"/>',
    more: '<circle cx="5.5" cy="12" r="1.3" fill="currentColor"/><circle cx="12" cy="12" r="1.3" fill="currentColor"/><circle cx="18.5" cy="12" r="1.3" fill="currentColor"/>',
    eye: '<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/>',
    eyeOff: '<path d="M10 5.7A9.6 9.6 0 0 1 12 5.5c6 0 9.5 6.5 9.5 6.5a17 17 0 0 1-2.6 3.3M14.1 14.2A3 3 0 0 1 9.8 9.9M6.6 6.7C3.9 8.4 2.5 12 2.5 12S6 18.5 12 18.5c1.7 0 3.2-.5 4.4-1.2"/><path d="M4 4l16 16"/>',
    shield: '<path d="M12 3l7 3v5.5c0 4.5-3 8-7 9.5-4-1.5-7-5-7-9.5V6z"/>',
    shieldOff: '<path d="M8.5 4.5 12 3l7 3v5.5c0 1.5-.3 2.9-.9 4.1M5.7 6.5 5 6.8v4.7c0 4.5 3 8 7 9.5 1.9-.7 3.6-1.9 4.9-3.4"/><path d="M4 4l16 16"/>',
    external: '<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 14v4.5a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 4 18.5v-11A1.5 1.5 0 0 1 5.5 6H10"/>',
    restore: '<path d="M4 12a8 8 0 1 0 2.4-5.7L4 8.5"/><path d="M4 4v4.5h4.5"/>',
    x: '<path d="M6 6l12 12M18 6L6 18"/>',
    check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
    alert: '<path d="M10.3 4.6 3 17.5A2 2 0 0 0 4.7 20.5h14.6a2 2 0 0 0 1.7-3L13.7 4.6a2 2 0 0 0-3.4 0z"/><path d="M12 10v4M12 17v.01"/>',
    info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8v.01"/>',
    copy: '<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2"/>',
    back: '<path d="M19 12H5M11 6l-6 6 6 6"/>',
  };

  const icon = (name) =>
    `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ''}</svg>`;

  function formatBytes(n) {
    if (!n) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + ' ' + units[i];
  }

  const formatCount = (n) => new Intl.NumberFormat('en').format(n || 0);
  const plural = (n, one, many) => `${formatCount(n)} ${n === 1 ? one : many}`;

  function timeAgo(iso) {
    const t = Date.parse(iso || '');
    if (Number.isNaN(t) || t < 86400000) return '';
    const s = (Date.now() - t) / 1000;
    const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });
    if (s < 60) return 'just now';
    if (s < 3600) return rtf.format(-Math.round(s / 60), 'minute');
    if (s < 86400) return rtf.format(-Math.round(s / 3600), 'hour');
    if (s < 86400 * 30) return rtf.format(-Math.round(s / 86400), 'day');
    return 'on ' + new Date(t).toLocaleDateString('en', { year: 'numeric', month: 'short', day: 'numeric' });
  }

  const baseName = (p) => String(p).replace(/[\\/]+$/, '').split(/[\\/]/).pop();

  // ------------------------------------------------------------------ backend

  const backend = () => window.go && window.go.main && window.go.main.App;

  // Errors arrive as Error objects whose message is the JSON produced by
  // the backend's error formatter: {"code": "...", "message": "..."}.
  function toError(e) {
    const raw = e && typeof e === 'object' && 'message' in e ? e.message : String(e);
    try {
      const parsed = JSON.parse(raw);
      if (parsed && typeof parsed.message === 'string') return parsed;
    } catch (_) { /* not JSON */ }
    return { code: 'error', message: raw };
  }

  async function call(method, ...args) {
    const b = backend();
    if (!b || typeof b[method] !== 'function') throw { code: 'error', message: 'The app backend is not available.' };
    try {
      return await b[method](...args);
    } catch (e) {
      throw toError(e);
    }
  }

  const on = (event, fn) => window.runtime && window.runtime.EventsOn(event, fn);

  function friendly(e) {
    switch (e && e.code) {
      case 'wrong_password': return 'That password does not open any vault on this computer.';
      case 'wrong_recovery_key': return 'This recovery key is not valid for any vault on this computer. Check it for typing mistakes.';
      case 'password_in_use': return 'Another vault on this computer already uses this password. Please choose a different one.';
      case 'busy': return 'Veilfold is busy with another operation. Try again in a moment.';
      case 'cancelled': return 'Cancelled. Nothing was changed.';
      default: return (e && e.message) || String(e);
    }
  }

  // ------------------------------------------------------------------ state

  const state = {
    status: { platform: 'darwin', version: '', hasVault: false, open: false, dataDir: '' },
    view: 'loading',
    items: [],
    settings: { defaultMode: 'encrypt', autoLockMinutes: 0 },
    op: null,          // running operation: {kind, id, name, phase, done, total}
    modal: null,
    menuFor: null,     // item id whose menu is open
    recoveryKey: '',
    afterRecoveryKey: 'main',
    notes: [],
    lastActivity: Date.now(),
    autoLocking: false,
  };

  const byId = (id) => state.items.find((i) => i.id === id) || { id, name: 'the folder' };
  const fileManager = () => ({ darwin: 'Finder', windows: 'Explorer' }[state.status.platform] || 'file manager');

  // ------------------------------------------------------------------ toasts

  function toast(kind, message, ms) {
    const el = document.createElement('div');
    el.className = `toast ${kind}`;
    el.innerHTML = `${icon({ success: 'check', error: 'alert' }[kind] || 'info')}<div class="msg"></div>` +
      `<button class="icon-btn" aria-label="Dismiss">${icon('x')}</button>`;
    el.querySelector('.msg').textContent = message;
    el.querySelector('button').addEventListener('click', () => el.remove());
    $('#toasts').appendChild(el);
    const timeout = ms === undefined ? (kind === 'error' ? 9000 : 4500) : ms;
    if (timeout) setTimeout(() => el.remove(), timeout);
  }

  // ------------------------------------------------------------------ views

  function passwordField(name, label, { autofocus = false, strength = false } = {}) {
    return `<div class="field"><label for="f-${name}">${esc(label)}</label>
      <div class="input-wrap">
        <input class="input" id="f-${name}" name="${name}" type="password" autocomplete="off" spellcheck="false"
          ${autofocus ? 'autofocus' : ''} ${strength ? 'data-strength' : ''}>
        <button type="button" class="icon-btn" data-action="toggle-visibility" aria-label="Show password" tabindex="-1">${icon('eye')}</button>
      </div></div>`;
  }

  const strengthMeter = () =>
    '<div class="strength" data-score="0"><div class="bars"><i></i><i></i><i></i><i></i></div><span class="hint">At least 8 characters</span></div>';

  const backButton = (view) =>
    `<button class="btn ghost back" data-action="go" data-view="${view}">${icon('back')}<span>Back</span></button>`;

  const gate = (inner, { back, wide } = {}) =>
    `<section class="gate">${back ? backButton(back) : ''}<div class="gate-card ${wide ? 'wide' : ''}">
      <img class="logo" src="logo.svg" alt="">${inner}</div></section>`;

  const views = {
    loading: () => '<section class="gate"><div class="spinner" aria-label="Loading"></div></section>',

    unlock: () => gate(`
      <h1>Veilfold</h1>
      <p class="lead">Enter your password to open your vault.</p>
      <form data-form="unlock" autocomplete="off">
        <div class="form-error" role="alert"></div>
        ${passwordField('password', 'Password', { autofocus: true })}
        <button class="btn primary wide" type="submit">${icon('unlock')}<span>Open vault</span></button>
      </form>
      <div class="alt">
        <button class="link muted" data-action="go" data-view="recover">Forgot password?</button>
        <span class="sep">·</span>
        <button class="link muted" data-action="go" data-view="create">Create another vault</button>
      </div>
      <p class="fineprint">Several people can each have their own vault on this computer.<br>A password only opens its own vault.</p>`),

    create: () => {
      const first = !state.status.hasVault;
      return gate(`
        <h1>${first ? 'Welcome to Veilfold' : 'Create a vault'}</h1>
        <p class="lead">${first
          ? 'Hide and lock private folders behind a password. Start by choosing the password for your vault.'
          : 'A new vault has its own password and its own folders. Nobody can see what other vaults contain.'}</p>
        <form data-form="create" autocomplete="off">
          <div class="form-error" role="alert"></div>
          ${passwordField('password', 'Password', { autofocus: true, strength: true })}
          ${strengthMeter()}
          ${passwordField('confirm', 'Repeat password')}
          <label class="check"><input type="checkbox" name="recovery" checked>
            <span>Create a recovery key, so I can get back in if I forget my password <strong>(recommended)</strong></span></label>
          <button class="btn primary wide" type="submit">Create vault</button>
        </form>
        <p class="fineprint">The password is never stored. Without it or the recovery key, nobody can open encrypted folders — not even you.</p>`,
      { back: first ? null : 'unlock' });
    },

    'recovery-key': () => gate(`
      <h1>Save your recovery key</h1>
      <p class="lead">If you forget your password, this key is the only way back into your vault. Veilfold will not show it again.</p>
      <div class="recovery-box"><code>${esc(state.recoveryKey)}</code>
        <button class="btn small" data-action="copy-recovery">${icon('copy')}<span>Copy</span></button></div>
      <form data-form="recovery-saved">
        <label class="check"><input type="checkbox" name="saved">
          <span>I have written the key down or stored it in a password manager.</span></label>
        <button class="btn primary wide" type="submit" disabled>Continue</button>
      </form>`, { wide: true }),

    recover: () => gate(`
      <h1>Forgot your password?</h1>
      <p class="lead">Enter the recovery key you saved when you created the vault. Then choose a new password.</p>
      <form data-form="recover" autocomplete="off">
        <div class="form-error" role="alert"></div>
        <div class="field"><label for="f-key">Recovery key</label>
          <input class="input mono" id="f-key" name="key" spellcheck="false" autofocus
            placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"></div>
        <button class="btn primary wide" type="submit">Continue</button>
      </form>`, { back: 'unlock' }),

    reset: () => gate(`
      <h1>Choose a new password</h1>
      <p class="lead">Your vault is open. Set a new password to finish. Your recovery key keeps working.</p>
      <form data-form="reset" autocomplete="off">
        <div class="form-error" role="alert"></div>
        ${passwordField('password', 'New password', { autofocus: true, strength: true })}
        ${strengthMeter()}
        ${passwordField('confirm', 'Repeat new password')}
        <button class="btn primary wide" type="submit">Save password</button>
      </form>`),

    main: () => `<div class="shell">
      <header class="topbar">
        <div class="brand"><img src="logo.svg" alt="">Veilfold</div>
        <div class="spacer"></div>
        <div class="actions">
          <button class="btn" data-action="lock-all" data-busy-disable>${icon('lock')}<span>Lock all</span></button>
          <button class="icon-btn" data-action="settings" title="Settings" aria-label="Settings">${icon('settings')}</button>
          <button class="icon-btn" data-action="close-vault" title="Close vault" aria-label="Close vault" data-busy-disable>${icon('logout')}</button>
        </div>
      </header>
      <section class="content"><div class="content-inner">
        <div class="page-head">
          <div class="grow"><h2>Protected folders</h2><div class="sub" id="summary"></div></div>
          <button class="btn primary" data-action="add" data-busy-disable>${icon('plus')}<span>Add folder</span></button>
        </div>
        <div id="items"></div>
      </div></section>
    </div>`,
  };

  function render() {
    $('#app').innerHTML = (views[state.view] || views.loading)();
    const focus = $('#app [autofocus]');
    if (focus) focus.focus();
    if (state.view === 'main') renderItems();
  }

  function go(view) {
    state.view = view;
    state.menuFor = null;
    render();
  }

  // ------------------------------------------------------------------ item list

  const PHASES = { scan: 'Preparing…', encrypt: 'Encrypting', verify: 'Verifying', decrypt: 'Decrypting', cleanup: 'Finishing…' };

  function progressInfo(op) {
    const pct = op.total > 0 && op.phase !== 'scan' && op.phase !== 'cleanup'
      ? Math.min(100, Math.round((op.done * 100) / op.total)) : null;
    const fallback = { unlock: 'Unlocking…', remove: 'Restoring…' }[op.kind] || 'Locking…';
    return {
      pct,
      label: op.phase ? PHASES[op.phase] || op.phase : fallback,
      detail: pct === null ? '' : `${pct}% · ${formatBytes(op.done)} of ${formatBytes(op.total)}`,
    };
  }

  function progressRow(op) {
    const p = progressInfo(op);
    return `<div class="progress" id="progress">
      <div class="label"><span class="spinner"></span><span id="progress-label">${esc(p.label)}</span>
        <span id="progress-detail" class="meta-text">${esc(p.detail)}</span></div>
      <button class="btn small ghost" data-action="cancel">Cancel</button>
      <div class="bar ${p.pct === null ? 'indeterminate' : ''}"><i style="width:${p.pct === null ? 30 : p.pct}%"></i></div>
    </div>`;
  }

  function updateProgress() {
    const row = $('#progress');
    if (!state.op || !row) return renderItems();
    const p = progressInfo(state.op);
    $('#progress-label').textContent = p.label;
    $('#progress-detail').textContent = p.detail;
    const bar = row.querySelector('.bar');
    bar.classList.toggle('indeterminate', p.pct === null);
    bar.firstElementChild.style.width = (p.pct === null ? 30 : p.pct) + '%';
  }

  const pill = (cls, ic, text) => `<span class="pill ${cls}">${ic ? icon(ic) : ''}${esc(text)}</span>`;

  function itemCard(it) {
    const op = state.op && state.op.id === it.id ? state.op : null;
    const busy = Boolean(state.op);
    const sealed = it.storage === 'sealed';
    let mode;
    if (it.locked) mode = sealed ? pill('', 'shield', 'Encrypted') : pill('', 'eyeOff', 'Hidden');
    else mode = it.mode === 'encrypt' ? pill('', 'shield', 'Encrypt mode') : pill('', 'eyeOff', 'Hide mode');
    const meta = [];
    if (it.locked && sealed && it.size) meta.push(`${formatBytes(it.size)} · ${plural(it.files, 'file', 'files')}`);
    if (it.locked && timeAgo(it.locked_at)) meta.push(`locked ${timeAgo(it.locked_at)}`);
    const primary = it.locked
      ? `<button class="btn" data-action="unlock" data-id="${it.id}" ${busy ? 'disabled' : ''}>${icon('unlock')}<span>Unlock</span></button>`
      : `<button class="btn primary" data-action="lock" data-id="${it.id}" ${busy || it.problem ? 'disabled' : ''}>${icon('lock')}<span>Lock</span></button>`;
    return `<article class="item ${op ? 'busy' : ''}">
      <div class="tile ${it.locked ? 'locked' : ''}">${icon('folder')}<span class="badge">${icon(it.locked ? 'lock' : 'unlock')}</span></div>
      <div class="item-main">
        <div class="item-name" title="${esc(it.name)}">${esc(it.name)}</div>
        <div class="item-path" title="${esc(it.path)}"><bdi>${esc(it.path)}</bdi></div>
        <div class="item-meta">${it.locked ? pill('locked', '', 'Locked') : pill('unlocked', '', 'Unlocked')}${mode}
          ${meta.length ? `<span class="meta-text">${esc(meta.join(' · '))}</span>` : ''}</div>
      </div>
      <div class="item-actions">${primary}
        <button class="icon-btn" data-action="menu" data-id="${it.id}" aria-label="More actions" ${busy ? 'disabled' : ''}>${icon('more')}</button>
      </div>
      ${it.problem && !op ? `<div class="problem">${icon('alert')}<span>${esc(it.problem)}</span></div>` : ''}
      ${op ? progressRow(op) : ''}
    </article>`;
  }

  function renderItems() {
    const box = $('#items');
    if (!box) return;
    const items = state.items;
    const locked = items.filter((i) => i.locked).length;
    $('#summary').textContent = items.length
      ? `${plural(items.length, 'folder', 'folders')} · ${formatCount(locked)} locked`
      : 'Nothing is protected yet';
    if (!items.length) {
      box.innerHTML = `<div class="empty"><div class="art">${icon('folder')}</div>
        <h3>Protect your first folder</h3>
        <p>Drop a folder here or choose one. Locked folders disappear from their location until you unlock them here.</p>
        <button class="btn primary" data-action="add" data-busy-disable>${icon('plus')}<span>Choose a folder…</span></button></div>`;
    } else {
      box.innerHTML = `<div class="items">${items.map(itemCard).join('')}</div>
        <div class="dropzone">Drop folders here to protect them</div>`;
    }
    document.querySelectorAll('[data-busy-disable]').forEach((b) => { b.disabled = Boolean(state.op); });
    updateMenu();
  }

  async function refreshItems() {
    try {
      state.items = (await call('Items')) || [];
    } catch (e) {
      if (e.code === 'closed') return go(state.status.hasVault ? 'unlock' : 'create');
      toast('error', friendly(e));
    }
    renderItems();
  }

  // ------------------------------------------------------------------ menu

  function updateMenu() {
    document.querySelectorAll('.menu').forEach((m) => m.remove());
    if (!state.menuFor) return;
    const it = state.items.find((i) => i.id === state.menuFor);
    const btn = document.querySelector(`[data-action="menu"][data-id="${state.menuFor}"]`);
    if (!it || !btn || state.op) { state.menuFor = null; return; }
    const entry = (action, ic, label, cls = '') =>
      `<button class="${cls}" data-action="${action}" data-id="${it.id}">${icon(ic)}<span>${esc(label)}</span></button>`;
    const parts = [];
    if (it.busy) {
      // An earlier operation could not be completed; the folder can still
      // be restored somewhere else.
      parts.push(entry('unlock-to', 'restore', 'Restore to another location…'));
    } else if (!it.locked) {
      parts.push(entry('reveal', 'external', `Show in ${fileManager()}`));
      parts.push(it.mode === 'encrypt'
        ? entry('set-mode', 'eyeOff', 'Use hide mode instead')
        : entry('set-mode', 'shield', 'Use encrypt mode instead'));
    } else {
      parts.push(entry('unlock-to', 'restore', 'Unlock to another location…'));
    }
    parts.push('<hr>');
    if (it.locked && it.problem) parts.push(entry('forget', 'x', 'Forget this folder…', 'danger'));
    parts.push(entry('remove', 'shieldOff', 'Stop protecting…', 'danger'));

    const menu = document.createElement('div');
    menu.className = 'menu';
    menu.innerHTML = parts.join('');
    document.body.appendChild(menu);
    const r = btn.getBoundingClientRect();
    menu.style.position = 'fixed';
    menu.style.right = `${Math.max(8, window.innerWidth - r.right)}px`;
    const below = r.bottom + 6;
    menu.style.top = `${below + menu.offsetHeight > window.innerHeight - 8 ? Math.max(8, r.top - menu.offsetHeight - 6) : below}px`;
  }

  // ------------------------------------------------------------------ modals

  function openModal(m) {
    state.menuFor = null;
    updateMenu();
    state.modal = m;
    renderModal();
  }

  function closeModal() {
    state.modal = null;
    renderModal();
  }

  const option = (value, label, current) =>
    `<option value="${esc(value)}" ${String(value) === String(current) ? 'selected' : ''}>${esc(label)}</option>`;

  const modals = {
    add(m) {
      const ins = m.inspection;
      const name = (ins && ins.name) || baseName(m.path);
      let stats = '<span class="spinner" style="display:inline-block;vertical-align:-3px"></span> Measuring…';
      if (ins && ins.stats) {
        const s = ins.stats;
        stats = esc(`${ins.complete ? '' : 'At least '}${formatBytes(s.bytes)} · ${plural(s.files, 'file', 'files')}`);
      }
      const summary = `<div class="folder-summary"><div class="tile">${icon('folder')}</div>
        <div><div class="name">${esc(name)}</div><div class="path selectable">${esc(m.path)}</div>
        ${ins && ins.error ? '' : `<div class="stats">${stats}</div>`}</div></div>`;
      if (ins && ins.error) {
        return `<div class="modal-head"><h3>This folder cannot be protected</h3></div>
          <div class="modal-body">${summary}<div class="notice error">${icon('alert')}<span>${esc(ins.error)}</span></div></div>
          <div class="modal-foot"><button class="btn primary" data-action="close-modal">OK</button></div>`;
      }
      const encryptBlocked = Boolean(ins && ins.encrypt_error);
      const mode = encryptBlocked ? 'hide' : m.mode || state.settings.defaultMode;
      const choice = (value, title, tag, desc, disabled) => `<label class="choice ${mode === value ? 'selected' : ''} ${disabled ? 'disabled' : ''}">
          <input type="radio" name="mode" value="${value}" ${mode === value ? 'checked' : ''} ${disabled ? 'disabled' : ''}>
          <span class="title">${title}${tag}</span><span class="desc">${desc}</span></label>`;
      const warnings = ((ins && ins.warnings) || []).map((w) => `<div class="notice warn">${icon('alert')}<span>${esc(w)}</span></div>`).join('');
      return `<form data-form="protect">
        <div class="modal-head"><h3>Protect “${esc(name)}”</h3><p>Choose how Veilfold should protect this folder.</p></div>
        <div class="modal-body">${summary}
          <div class="choices">
            ${choice('encrypt', `${icon('shield')} Encrypt`, ' <span class="tag">Recommended</span>',
              encryptBlocked ? esc(ins.encrypt_error)
                : 'Contents and file names are encrypted with your password. Locking and unlocking take a moment for large folders and need free disk space.',
              encryptBlocked)}
            ${choice('hide', `${icon('eyeOff')} Hide`, ' <span class="tag muted">Instant</span>',
              'The folder is moved to a concealed place on the same drive. Instant for any size, but the contents are not encrypted.', false)}
          </div>
          ${warnings}
          <label class="check"><input type="checkbox" name="lockNow" ${m.lockNow === false ? '' : 'checked'}><span>Lock the folder now</span></label>
        </div>
        <div class="modal-foot">
          <button class="btn ghost" type="button" data-action="close-modal">Cancel</button>
          <button class="btn primary" type="submit" ${ins ? '' : 'disabled'}>${icon('lock')}<span>Protect</span></button>
        </div></form>`;
    },

    settings(m) {
      const s = state.settings;
      return `<div class="modal-head"><h3>Settings</h3></div>
        <div class="modal-body">
          <div class="settings-row"><div><div class="title">Default protection</div>
            <div class="desc">Preselected when you add a folder.</div></div>
            <select class="input" data-setting="defaultMode">${option('encrypt', 'Encrypt', s.defaultMode)}${option('hide', 'Hide', s.defaultMode)}</select></div>
          <div class="settings-row"><div><div class="title">Auto-lock</div>
            <div class="desc">Lock every folder and close the vault when you have not used Veilfold for a while.</div></div>
            <select class="input" data-setting="autoLockMinutes">${[[0, 'Off'], [5, 'After 5 minutes'], [15, 'After 15 minutes'], [30, 'After 30 minutes'], [60, 'After 1 hour']]
              .map(([v, l]) => option(v, l, s.autoLockMinutes)).join('')}</select></div>
          <div class="settings-row"><div><div class="title">Password</div>
            <div class="desc">Change the password of this vault.</div></div>
            <button class="btn" data-action="change-password">Change…</button></div>
          <div class="settings-row"><div><div class="title">Recovery key</div>
            <div class="desc">${m.hasRecovery ? 'Create a new recovery key. The current one stops working.' : 'This vault has no recovery key. Create one so you can never be locked out.'}</div></div>
            <button class="btn" data-action="new-recovery">${m.hasRecovery ? 'Replace…' : 'Create…'}</button></div>
          <div class="settings-row about"><div>Veilfold ${esc(state.status.version)} · MIT License<br>
            Data folder: <span class="selectable">${esc(state.status.dataDir)}</span></div>
            <button class="link" data-action="project-page">Project page</button></div>
        </div>
        <div class="modal-foot"><button class="btn primary" data-action="close-modal">Done</button></div>`;
    },

    password: () => `<form data-form="change-password" autocomplete="off">
      <div class="modal-head"><h3>Change password</h3><p>Your folders stay as they are; only the password changes.</p></div>
      <div class="modal-body"><div class="form-error" role="alert"></div>
        ${passwordField('current', 'Current password', { autofocus: true })}
        ${passwordField('password', 'New password', { strength: true })}
        ${strengthMeter()}
        ${passwordField('confirm', 'Repeat new password')}</div>
      <div class="modal-foot"><button class="btn ghost" type="button" data-action="close-modal">Cancel</button>
        <button class="btn primary" type="submit">Change password</button></div></form>`,

    'new-recovery': () => `<form data-form="new-recovery" autocomplete="off">
      <div class="modal-head"><h3>New recovery key</h3><p>Enter your password to create a new recovery key. Any previous key stops working.</p></div>
      <div class="modal-body"><div class="form-error" role="alert"></div>
        ${passwordField('password', 'Password', { autofocus: true })}</div>
      <div class="modal-foot"><button class="btn ghost" type="button" data-action="close-modal">Cancel</button>
        <button class="btn primary" type="submit">Create recovery key</button></div></form>`,

    confirm: (m) => `<div class="modal-head"><h3>${esc(m.title)}</h3>${m.text ? `<p>${esc(m.text)}</p>` : ''}</div>
      <div class="modal-foot">${m.buttons.map((b, i) =>
        `<button class="btn ${b.cls || ''}" data-action="confirm-choice" data-index="${i}">${esc(b.label)}</button>`).join('')}</div>`,
  };

  function renderModal() {
    const root = $('#modal-root');
    const m = state.modal;
    if (!m) { root.innerHTML = ''; return; }
    root.innerHTML = `<div class="backdrop" data-action="backdrop">
      <div class="modal ${m.wide ? 'wide' : ''}" role="dialog" aria-modal="true">${modals[m.type](m)}</div></div>`;
    const focus = root.querySelector('[autofocus]') || root.querySelector('.modal-foot .primary');
    if (focus) focus.focus();
  }

  function confirmDialog(title, text, buttons) {
    openModal({ type: 'confirm', title, text, buttons });
  }

  // ------------------------------------------------------------------ forms

  function setError(form, message) {
    const el = form.querySelector('.form-error');
    if (!el) { if (message) toast('error', message); return; }
    el.textContent = message || '';
    el.classList.toggle('show', Boolean(message));
  }

  async function withBusy(form, label, fn) {
    const btn = form.querySelector('[type=submit]');
    const html = btn.innerHTML;
    btn.disabled = true;
    btn.innerHTML = `<span class="spinner"></span><span>${esc(label)}</span>`;
    setError(form, '');
    try {
      await fn();
    } catch (e) {
      setError(form, friendly(e));
    } finally {
      if (btn.isConnected) { btn.disabled = false; btn.innerHTML = html; }
    }
  }

  function checkNewPassword(form) {
    const pw = form.password.value;
    if ([...pw].length < 8) return 'Use at least 8 characters.';
    if (pw !== form.confirm.value) return 'The passwords do not match.';
    return '';
  }

  function strength(pw) {
    if (!pw) return [0, 'At least 8 characters'];
    if ([...pw].length < 8) return [1, 'Too short'];
    const classes = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^A-Za-z0-9]/].filter((r) => r.test(pw)).length;
    let score = pw.length >= 16 ? 4 : pw.length >= 12 ? 3 : 2;
    if (classes >= 3 && score < 4) score++;
    if (classes <= 1 && score > 2) score--;
    return [score, ['', 'Too short', 'Fair', 'Good', 'Strong'][score]];
  }

  const forms = {
    unlock: (form) => withBusy(form, 'Opening…', async () => {
      const pw = form.password.value;
      if (!pw) throw { message: 'Enter your password.' };
      try {
        const res = await call('OpenVault', pw);
        await enterMain(res.notes);
      } catch (e) {
        form.password.select();
        throw e;
      }
    }),

    create: (form) => withBusy(form, 'Creating…', async () => {
      const problem = checkNewPassword(form);
      if (problem) throw { message: problem };
      const key = await call('CreateVault', form.password.value, form.recovery.checked);
      state.status.hasVault = true;
      if (key) {
        state.recoveryKey = key;
        state.afterRecoveryKey = 'main';
        go('recovery-key');
      } else {
        await enterMain();
      }
    }),

    'recovery-saved': async () => {
      state.recoveryKey = '';
      if (state.afterRecoveryKey === 'main') await enterMain();
    },

    recover: (form) => withBusy(form, 'Checking…', async () => {
      const res = await call('OpenWithRecoveryKey', form.key.value);
      state.notes = res.notes || [];
      go('reset');
    }),

    reset: (form) => withBusy(form, 'Saving…', async () => {
      const problem = checkNewPassword(form);
      if (problem) throw { message: problem };
      await call('ResetPassword', form.password.value);
      toast('success', 'Your new password is set.');
      await enterMain(state.notes);
      state.notes = [];
    }),

    protect: async (form) => {
      const m = state.modal;
      const mode = form.mode.value;
      const lockNow = form.lockNow.checked;
      closeModal();
      try {
        const it = await call('Protect', m.path, mode, lockNow);
        toast('success', lockNow
          ? `“${it.name}” is ${mode === 'encrypt' ? 'encrypted' : 'hidden'} and locked.`
          : `“${it.name}” is protected. Lock it whenever you like.`);
      } catch (e) {
        opError(e);
      }
      await refreshItems();
    },

    'change-password': (form) => withBusy(form, 'Changing…', async () => {
      const problem = checkNewPassword(form);
      if (problem) throw { message: problem };
      try {
        await call('ChangePassword', form.current.value, form.password.value);
      } catch (e) {
        if (e.code === 'wrong_password') throw { message: 'The current password is not correct.' };
        throw e;
      }
      closeModal();
      toast('success', 'Password changed.');
    }),

    'new-recovery': (form) => withBusy(form, 'Creating…', async () => {
      try {
        state.recoveryKey = await call('NewRecoveryKey', form.password.value);
      } catch (e) {
        if (e.code === 'wrong_password') throw { message: 'The password is not correct.' };
        throw e;
      }
      closeModal();
      state.afterRecoveryKey = 'main';
      go('recovery-key');
    }),
  };

  // ------------------------------------------------------------------ actions

  async function enterMain(notes = []) {
    state.view = 'main';
    render();
    try {
      state.settings = await call('Settings');
    } catch (_) { /* keep defaults */ }
    await refreshItems();
    // Repair notes can say where leftover data is; keep them until dismissed.
    (notes || []).forEach((n) => toast('info', n, 0));
    state.lastActivity = Date.now();
  }

  function opError(e) {
    if (e.code === 'cancelled') return toast('info', friendly(e));
    toast('error', friendly(e));
  }

  async function chooseAndAdd() {
    try {
      const path = await call('ChooseFolder', 'Choose a folder to protect');
      if (path) openAdd(path);
    } catch (e) {
      toast('error', friendly(e));
    }
  }

  async function openAdd(path) {
    openModal({ type: 'add', path, inspection: null, mode: null, wide: true });
    let ins;
    try {
      ins = await call('Inspect', path);
    } catch (e) {
      ins = { name: baseName(path), path, error: friendly(e) };
    }
    if (state.modal && state.modal.type === 'add' && state.modal.path === path) {
      state.modal.inspection = ins;
      renderModal();
    }
  }

  async function lockItem(id) {
    const it = byId(id);
    try {
      await call('Lock', id);
      toast('success', `“${it.name}” is locked.`);
    } catch (e) {
      opError(e);
    }
    await refreshItems();
  }

  async function unlockItem(id) {
    const it = byId(id);
    try {
      await call('Unlock', id);
      toast('success', `“${it.name}” is unlocked.`);
    } catch (e) {
      if (e.code === 'target_exists' || e.code === 'parent_missing') {
        confirmDialog('The original location is not available',
          e.code === 'target_exists'
            ? `Something named “${it.name}” already exists where the folder used to be. Restore it to another location instead?`
            : 'The folder that used to contain it no longer exists. Restore it to another location instead?',
          [
            { label: 'Cancel', cls: 'ghost', run: closeModal },
            { label: 'Choose location…', cls: 'primary', run: () => { closeModal(); unlockTo(id); } },
          ]);
      } else {
        opError(e);
      }
    }
    await refreshItems();
  }

  async function unlockTo(id) {
    const it = byId(id);
    try {
      if (await call('UnlockTo', id)) toast('success', `“${it.name}” is unlocked at its new location.`);
    } catch (e) {
      opError(e);
    }
    await refreshItems();
  }

  async function lockAll() {
    const n = state.items.filter((i) => !i.locked).length;
    if (!n) return toast('info', 'Every folder is already locked.');
    try {
      await call('LockAll');
      toast('success', n === 1 ? 'The folder is locked.' : `All ${n} folders are locked.`);
    } catch (e) {
      opError(e);
    }
    await refreshItems();
  }

  function removeItem(id) {
    const it = byId(id);
    confirmDialog(`Stop protecting “${it.name}”?`,
      it.locked
        ? 'It will be unlocked, put back at its location and removed from Veilfold. Nothing is deleted.'
        : 'It stays where it is and is removed from Veilfold. Nothing is deleted.',
      [
        { label: 'Cancel', cls: 'ghost', run: closeModal },
        {
          label: 'Stop protecting', cls: 'danger solid', run: async () => {
            closeModal();
            try {
              await call('Remove', id);
              toast('success', `“${it.name}” is no longer protected.`);
            } catch (e) {
              opError(e);
            }
            await refreshItems();
          },
        },
      ]);
  }

  function forgetItem(id) {
    const it = byId(id);
    confirmDialog(`Forget “${it.name}”?`,
      'Its protected data cannot be found, for example because a drive was erased. Veilfold will stop listing it. Data that still exists is never deleted.',
      [
        { label: 'Cancel', cls: 'ghost', run: closeModal },
        {
          label: 'Forget', cls: 'danger solid', run: async () => {
            closeModal();
            try { await call('Forget', id); } catch (e) { opError(e); }
            await refreshItems();
          },
        },
      ]);
  }

  async function setMode(id) {
    const it = byId(id);
    const next = it.mode === 'encrypt' ? 'hide' : 'encrypt';
    try {
      await call('SetMode', id, next);
      toast('success', `“${it.name}” will be ${next === 'encrypt' ? 'encrypted' : 'hidden'} when you lock it.`);
    } catch (e) {
      opError(e);
    }
    await refreshItems();
  }

  async function openSettings() {
    try {
      const [settings, hasRecovery] = await Promise.all([call('Settings'), call('HasRecoveryKey')]);
      state.settings = settings;
      openModal({ type: 'settings', hasRecovery });
    } catch (e) {
      toast('error', friendly(e));
    }
  }

  async function doCloseVault() {
    closeModal();
    try {
      await call('CloseVault');
      state.items = [];
      go('unlock');
    } catch (e) {
      toast('error', friendly(e));
    }
  }

  function closeVault() {
    const n = state.items.filter((i) => !i.locked).length;
    if (!n) return doCloseVault();
    confirmDialog(`${n === 1 ? 'One folder is' : `${n} folders are`} still unlocked`,
      'Closing the vault does not lock folders. Lock them first, so they are hidden from others using this computer?',
      [
        { label: 'Cancel', cls: 'ghost left', run: closeModal },
        { label: 'Close only', run: doCloseVault },
        {
          label: 'Lock all and close', cls: 'primary', run: async () => {
            closeModal();
            try {
              await call('LockAll');
            } catch (e) {
              opError(e);
              return refreshItems();
            }
            await doCloseVault();
          },
        },
      ]);
  }

  function confirmQuit(n) {
    confirmDialog(`${n === 1 ? 'One folder is' : `${n} folders are`} still unlocked`,
      'Lock them before quitting? Unlocked folders stay visible to anyone using this computer.',
      [
        { label: 'Cancel', cls: 'ghost left', run: closeModal },
        { label: 'Quit anyway', run: () => call('Quit', false).catch(opError) },
        {
          label: 'Lock all and quit', cls: 'primary', run: async () => {
            closeModal();
            try {
              await call('Quit', true);
            } catch (e) {
              opError(e);
              refreshItems();
            }
          },
        },
      ]);
  }

  async function copyText(text) {
    try {
      if (window.runtime && window.runtime.ClipboardSetText) {
        await window.runtime.ClipboardSetText(text);
        return true;
      }
      await navigator.clipboard.writeText(text);
      return true;
    } catch (_) {
      return false;
    }
  }

  const actions = {
    go: (el) => go(el.dataset.view),
    'toggle-visibility': (el) => {
      const input = el.parentElement.querySelector('input');
      const show = input.type === 'password';
      input.type = show ? 'text' : 'password';
      el.innerHTML = icon(show ? 'eyeOff' : 'eye');
      el.setAttribute('aria-label', show ? 'Hide password' : 'Show password');
      input.focus();
    },
    'copy-recovery': async (el) => {
      const ok = await copyText(state.recoveryKey);
      el.querySelector('span').textContent = ok ? 'Copied' : 'Copy failed';
      setTimeout(() => { if (el.isConnected) el.querySelector('span').textContent = 'Copy'; }, 1800);
    },
    add: chooseAndAdd,
    lock: (el) => lockItem(el.dataset.id),
    unlock: (el) => unlockItem(el.dataset.id),
    'unlock-to': (el) => { state.menuFor = null; updateMenu(); unlockTo(el.dataset.id); },
    menu: (el) => { state.menuFor = state.menuFor === el.dataset.id ? null : el.dataset.id; updateMenu(); },
    reveal: (el) => { state.menuFor = null; updateMenu(); call('Reveal', el.dataset.id).catch(opError); },
    'set-mode': (el) => { state.menuFor = null; updateMenu(); setMode(el.dataset.id); },
    remove: (el) => removeItem(el.dataset.id),
    forget: (el) => forgetItem(el.dataset.id),
    'lock-all': lockAll,
    settings: openSettings,
    'close-vault': closeVault,
    cancel: (el) => { el.disabled = true; call('Cancel'); },
    'close-modal': closeModal,
    'confirm-choice': (el) => {
      const b = state.modal && state.modal.buttons && state.modal.buttons[Number(el.dataset.index)];
      if (b) b.run();
    },
    'change-password': () => openModal({ type: 'password' }),
    'new-recovery': () => openModal({ type: 'new-recovery' }),
    'project-page': () => call('OpenProjectPage'),
  };

  // ------------------------------------------------------------------ wiring

  // Where the current mouse press started; the backdrop closes its dialog
  // only when a click starts and ends on the backdrop itself.
  let pressTarget = null;
  document.addEventListener('mousedown', (e) => { pressTarget = e.target; }, true);

  document.addEventListener('click', (e) => {
    const el = e.target.closest('[data-action]');
    if (state.menuFor && !e.target.closest('.menu') && !(el && el.dataset.action === 'menu')) {
      state.menuFor = null;
      updateMenu();
    }
    if (!el || el.disabled) return;
    if (el.dataset.action === 'backdrop') {
      // Clicks inside a dialog bubble up to its backdrop. They must keep
      // their default action: submitting forms, ticking boxes and choices.
      if (e.target === el && pressTarget === el) closeModal();
      return;
    }
    const fn = actions[el.dataset.action];
    if (fn) {
      e.preventDefault();
      fn(el, e);
    }
  });

  document.addEventListener('submit', (e) => {
    const form = e.target.closest('form[data-form]');
    if (!form) return;
    e.preventDefault();
    const fn = forms[form.dataset.form];
    if (fn) fn(form);
  });

  document.addEventListener('input', (e) => {
    if (e.target.matches('[data-strength]')) {
      const meter = e.target.closest('form').querySelector('.strength');
      const [score, hint] = strength(e.target.value);
      meter.dataset.score = score;
      meter.querySelector('.hint').textContent = hint;
    }
  });

  document.addEventListener('change', (e) => {
    const t = e.target;
    if (t.name === 'saved') {
      t.closest('form').querySelector('[type=submit]').disabled = !t.checked;
    } else if (t.name === 'mode' && state.modal && state.modal.type === 'add') {
      state.modal.mode = t.value;
      t.closest('.choices').querySelectorAll('.choice').forEach((c) =>
        c.classList.toggle('selected', c.querySelector('input').checked));
    } else if (t.name === 'lockNow' && state.modal && state.modal.type === 'add') {
      // Keep the choice when the dialog re-renders with the folder's size.
      state.modal.lockNow = t.checked;
    } else if (t.dataset.setting) {
      const key = t.dataset.setting;
      state.settings[key] = key === 'autoLockMinutes' ? Number(t.value) : t.value;
      call('SaveSettings', state.settings).catch((err) => toast('error', friendly(err)));
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (state.menuFor) {
      state.menuFor = null;
      updateMenu();
    } else if (state.modal) {
      closeModal();
    }
  });

  ['mousemove', 'mousedown', 'keydown', 'wheel'].forEach((ev) =>
    window.addEventListener(ev, () => { state.lastActivity = Date.now(); }, { passive: true }));

  // Auto-lock after inactivity, if enabled in the settings.
  setInterval(async () => {
    const minutes = state.settings.autoLockMinutes;
    if (state.view !== 'main' || !minutes || state.op || state.autoLocking) return;
    if (Date.now() - state.lastActivity < minutes * 60000) return;
    state.autoLocking = true;
    try {
      await call('LockAll');
    } catch (e) {
      toast('error', `Auto-lock could not lock every folder:\n${friendly(e)}`, 0);
    }
    try { await call('CloseVault'); } catch (_) { /* already closed */ }
    state.autoLocking = false;
    state.items = [];
    closeModal();
    go('unlock');
    toast('info', 'Veilfold locked your folders and closed the vault after a period of inactivity.', 0);
  }, 15000);

  function onDrop(paths) {
    document.body.classList.remove('dragging');
    if (state.view !== 'main' || state.modal || state.op || !paths || !paths.length) return;
    if (paths.length > 1) toast('info', 'Add one folder at a time — using the first one.');
    openAdd(paths[0]);
  }

  window.addEventListener('dragenter', (e) => {
    if (e.dataTransfer && [...e.dataTransfer.types].includes('Files')) document.body.classList.add('dragging');
  });
  window.addEventListener('dragleave', (e) => {
    if (e.clientX <= 0 || e.clientY <= 0 || e.clientX >= window.innerWidth || e.clientY >= window.innerHeight) {
      document.body.classList.remove('dragging');
    }
  });
  window.addEventListener('drop', () => document.body.classList.remove('dragging'));

  function wireBackendEvents() {
    // useDropTarget=false: accept folders dropped anywhere in the window.
    if (window.runtime && window.runtime.OnFileDrop) window.runtime.OnFileDrop((x, y, paths) => onDrop(paths), false);
    on('op:start', (info) => {
      state.op = { ...info, phase: '', done: 0, total: 0 };
      state.menuFor = null;
      if (!state.items.some((i) => i.id === info.id)) refreshItems();
      else renderItems();
    });
    on('op:progress', (p) => {
      if (!state.op || state.op.id !== p.id) return;
      Object.assign(state.op, p);
      updateProgress();
    });
    on('op:end', (id) => {
      if (state.op && state.op.id === id) state.op = null;
      renderItems();
    });
    on('quit:confirm', (n) => confirmQuit(n));
    on('quit:busy', () => toast('info', 'Veilfold is still working. Wait until it has finished, or cancel the operation first.'));
  }

  async function boot() {
    for (let i = 0; i < 150 && !backend(); i++) await new Promise((r) => setTimeout(r, 20));
    wireBackendEvents();
    try {
      state.status = await call('Status');
    } catch (e) {
      $('#app').innerHTML = gate(`<h1>Veilfold could not start</h1><p class="lead">${esc(friendly(e))}</p>`);
      return;
    }
    document.body.classList.add(`platform-${state.status.platform}`);
    if (state.status.open) {
      if (state.status.recovery) return go('reset');
      return enterMain();
    }
    go(state.status.hasVault ? 'unlock' : 'create');
  }

  boot();
})();
