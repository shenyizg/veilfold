// Drives the desktop UI in TestDesktopUI the way a person would, and asks
// the Go side to check the folders on disk after every step that touches
// them. Each click is hit-tested, so a control that is covered by something
// else fails just as it would for a user.
'use strict';

(() => {
  const PW1 = 'first password 1';
  const PW2 = 'second Passw0rd';
  const PW3 = 'third pass phrase';
  let recoveryKey = '';
  let current = 'starting';

  const q = (sel, root = document) => root.querySelector(sel);
  const qa = (sel, root = document) => [...root.querySelectorAll(sel)];
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  const errors = [];
  window.addEventListener('error', (e) => errors.push(`${e.message} (${e.filename}:${e.lineno})`));
  window.addEventListener('unhandledrejection', (e) =>
    errors.push(`unhandled rejection: ${(e.reason && (e.reason.message || e.reason)) || e.reason}`));

  async function post(path, body) {
    const res = await fetch(path, { method: 'POST', body: JSON.stringify(body) });
    return res.json();
  }

  async function check(name, arg = '') {
    const r = await post('/test/check', { name, arg });
    if (r.error) throw new Error(`check ${name}(${arg}): ${r.error}`);
    return r.value;
  }

  // eventually retries a check that depends on work still in flight.
  async function eventually(name, arg) {
    const end = Date.now() + 10000;
    for (;;) {
      try {
        return await check(name, arg);
      } catch (e) {
        if (Date.now() > end) throw e;
      }
      await sleep(50);
    }
  }

  async function waitFor(what, fn, timeout = 30000) {
    const end = Date.now() + timeout;
    for (;;) {
      const v = fn();
      if (v) return v;
      if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
      await sleep(20);
    }
  }

  function assert(cond, message) {
    if (!cond) throw new Error(message);
  }

  const describe = (el) => (el ? `<${el.tagName.toLowerCase()} class="${el.getAttribute('class') || ''}">` : 'nothing');

  // click presses and releases the mouse over an element, as a user would:
  // the events go to whatever is on top at that point.
  function click(el, what = 'element', at) {
    assert(el, `${what} not found`);
    el.scrollIntoView({ block: 'center', inline: 'center' });
    const r = el.getBoundingClientRect();
    const x = at ? r.left + at.x : r.left + r.width / 2;
    const y = at ? r.top + at.y : r.top + r.height / 2;
    const hit = document.elementFromPoint(x, y);
    assert(hit && (hit === el || el.contains(hit)), `${what} cannot be clicked: ${describe(hit)} is on top of it`);
    assert(!el.disabled, `${what} is disabled`);
    const opts = { bubbles: true, cancelable: true, view: window, clientX: x, clientY: y, button: 0, detail: 1 };
    hit.dispatchEvent(new MouseEvent('mousedown', { ...opts, buttons: 1 }));
    hit.dispatchEvent(new MouseEvent('mouseup', opts));
    hit.dispatchEvent(new MouseEvent('click', opts));
  }

  function type(input, text) {
    assert(input, 'input not found');
    input.focus();
    input.value = text;
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }

  const submit = (form) => q('[type=submit]', form);
  const button = (text, root = document) => qa('button', root).find((b) => b.textContent.trim() === text);
  const card = (name) => qa('.item').find((c) => q('.item-name', c).textContent === name);
  const cardButton = (name, action) => card(name) && q(`[data-action="${action}"]`, card(name));
  const toasts = () => qa('#toasts .msg').map((m) => m.textContent);
  const toast = (text) => toasts().some((t) => t.includes(text));
  const modal = () => q('.modal');
  const idle = () => !q('.item.busy') && !q('#progress');
  const choice = (value) => q(`.choice input[value="${value}"]`).closest('.choice');
  const formError = (form, text) => {
    const el = q('.form-error.show', form);
    return el && el.textContent.includes(text);
  };
  const labelOf = (input) => q('span', input.closest('label'));

  // The app shows a result's toast before it has refreshed the folder list,
  // so states in the list are awaited rather than asserted right away.

  async function protectDialog() {
    return waitFor('the protect dialog to measure the folder', () => {
      const f = q('form[data-form=protect]');
      return f && !submit(f).disabled && f;
    });
  }

  async function menuItem(name, label) {
    click(cardButton(name, 'menu'), `menu button of ${name}`);
    const menu = await waitFor('the menu', () => q('.menu'));
    click(button(label, menu), `menu item “${label}”`);
  }

  async function stopProtecting(name) {
    await menuItem(name, 'Stop protecting…');
    await waitFor('the confirmation', () => modal() && modal().textContent.includes(`Stop protecting “${name}”?`));
    click(button('Stop protecting', modal()), 'Stop protecting button');
    await waitFor(`${name} to be removed`, () => toast(`“${name}” is no longer protected.`) && !card(name));
  }

  const steps = [
    ['first start shows the welcome screen', async () => {
      await waitFor('the create form', () => q('form[data-form=create]'));
      assert(q('h1').textContent === 'Welcome to Veilfold', `unexpected title ${q('h1').textContent}`);
    }],

    ['weak or mismatched passwords are refused', async () => {
      const f = q('form[data-form=create]');
      type(f.password, 'short');
      type(f.confirm, 'short');
      click(submit(f), 'Create vault');
      await waitFor('the length error', () => formError(f, 'Use at least 8 characters.'));
      type(f.password, PW1);
      type(f.confirm, PW1 + 'x');
      click(submit(f), 'Create vault');
      await waitFor('the mismatch error', () => formError(f, 'The passwords do not match.'));
      await check('vault-count', '0');
    }],

    ['create a vault with a recovery key', async () => {
      const f = q('form[data-form=create]');
      type(f.password, PW1);
      type(f.confirm, PW1);
      click(labelOf(f.recovery), 'recovery key option');
      assert(!f.recovery.checked, 'the recovery key option cannot be unticked');
      click(labelOf(f.recovery), 'recovery key option');
      assert(f.recovery.checked, 'the recovery key option cannot be ticked');
      click(submit(f), 'Create vault');
      const code = await waitFor('the recovery key', () => q('.recovery-box code'));
      recoveryKey = code.textContent.trim();
      assert(/^[0-9A-Z]{4}(-[0-9A-Z]{4}){7}$/.test(recoveryKey), `odd recovery key ${recoveryKey}`);
      await check('vault-count', '1');
    }],

    ['continuing needs confirmation that the key was saved', async () => {
      const f = q('form[data-form=recovery-saved]');
      assert(submit(f).disabled, 'Continue is enabled before the key was confirmed');
      click(q('[data-action=copy-recovery]'), 'Copy');
      await waitFor('“Copied”', () => q('[data-action=copy-recovery] span').textContent === 'Copied');
      click(labelOf(f.saved), 'confirmation box');
      click(submit(f), 'Continue');
      await waitFor('the empty main screen', () => q('.empty') && q('#summary').textContent === 'Nothing is protected yet');
    }],

    ['protect a folder with encryption and lock it', async () => {
      await check('pick', 'home/Taxes');
      click(button('Add folder'), 'Add folder');
      const f = await protectDialog();
      assert(q('.folder-summary .name').textContent === 'Taxes', 'wrong folder in the dialog');
      assert(q('input', choice('encrypt')).checked, 'Encrypt is not preselected');
      click(q('.desc', choice('hide')), 'Hide choice');
      assert(q('input', choice('hide')).checked, 'clicking the Hide choice does not select it');
      assert(choice('hide').classList.contains('selected'), 'the Hide choice is not highlighted');
      click(q('.title', choice('encrypt')), 'Encrypt choice');
      assert(q('input', choice('encrypt')).checked, 'clicking the Encrypt choice does not select it');
      const lockNow = f.lockNow;
      click(labelOf(lockNow), '“Lock the folder now”');
      assert(!lockNow.checked, '“Lock the folder now” cannot be unticked');
      click(labelOf(lockNow), '“Lock the folder now”');
      assert(lockNow.checked, '“Lock the folder now” cannot be ticked');
      click(submit(f), 'Protect');
      await waitFor('the lock to finish', () => toast('“Taxes” is encrypted and locked.') && idle());
      await waitFor('Taxes to be shown as locked', () => q('.pill.locked', card('Taxes')));
      await check('locked', 'Taxes');
    }],

    ['unlock it again', async () => {
      click(cardButton('Taxes', 'unlock'), 'Unlock');
      await waitFor('the unlock to finish', () => toast('“Taxes” is unlocked.') && idle());
      await waitFor('Taxes to be shown as unlocked', () => q('.pill.unlocked', card('Taxes')));
      await check('intact', 'Taxes');
    }],

    ['hide a second folder', async () => {
      await check('pick', 'home/Photos');
      click(button('Add folder'), 'Add folder');
      const f = await protectDialog();
      click(q('.title', choice('hide')), 'Hide choice');
      click(submit(f), 'Protect');
      await waitFor('the folder to be hidden', () => toast('“Photos” is hidden and locked.') && idle());
      await check('hidden', 'Photos');
    }],

    ['lock all from the top bar', async () => {
      click(q('[data-action=lock-all]'), 'Lock all');
      await waitFor('every folder to be locked', () => toast('The folder is locked.') && idle());
      await waitFor('every folder to be shown as locked', () => qa('.item').every((c) => q('.pill.locked', c)));
      await check('locked', 'Taxes');
      await check('hidden', 'Photos');
    }],

    ['change the default protection in the settings', async () => {
      click(q('[data-action=settings]'), 'Settings');
      const sel = await waitFor('the settings', () => q('select[data-setting=defaultMode]'));
      sel.value = 'hide';
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      await eventually('default-mode', 'hide');
    }],

    ['a wrong current password is refused', async () => {
      click(button('Change…', modal()), 'Change… (password)');
      const f = await waitFor('the password form', () => q('form[data-form=change-password]'));
      type(f.current, 'not the password');
      type(f.password, PW2);
      type(f.confirm, PW2);
      click(submit(f), 'Change password');
      await waitFor('the error', () => formError(f, 'The current password is not correct.'));
    }],

    ['change the password', async () => {
      const f = q('form[data-form=change-password]');
      type(f.current, PW1);
      click(submit(f), 'Change password');
      await waitFor('the confirmation', () => toast('Password changed.') && !modal());
    }],

    ['clicks inside a dialog keep it open, the backdrop closes it', async () => {
      click(q('[data-action=settings]'), 'Settings');
      await waitFor('the settings', () => modal());
      click(q('.modal-head h3'), 'dialog title');
      click(q('.settings-row .desc'), 'dialog text');
      assert(modal(), 'clicking inside the dialog closed it');
      click(q('.backdrop'), 'backdrop', { x: 6, y: 6 });
      assert(!modal(), 'clicking the backdrop did not close the dialog');
    }],

    ['close the vault when everything is locked', async () => {
      click(q('[data-action=close-vault]'), 'Close vault');
      await waitFor('the unlock screen', () => q('form[data-form=unlock]'));
      await check('closed');
    }],

    ['the old password no longer opens the vault', async () => {
      const f = q('form[data-form=unlock]');
      type(f.password, PW1);
      click(submit(f), 'Open vault');
      await waitFor('the error', () => formError(f, 'does not open any vault'));
    }],

    ['show the password, then open the vault with the new one', async () => {
      const f = q('form[data-form=unlock]');
      click(q('[data-action=toggle-visibility]', f), 'show password');
      assert(f.password.type === 'text', 'the password is not shown');
      click(q('[data-action=toggle-visibility]', f), 'hide password');
      assert(f.password.type === 'password', 'the password is not hidden again');
      type(f.password, PW2);
      click(submit(f), 'Open vault');
      await waitFor('the folders', () => card('Taxes') && card('Photos'));
    }],

    ['stop protecting a locked, hidden folder', async () => {
      await stopProtecting('Photos');
      await check('intact', 'Photos');
      await check('unlisted', 'Photos');
    }],

    ['a folder that cannot be protected is explained', async () => {
      window.testBridge.drop([await check('path', 'data')]);
      await waitFor('the explanation', () => modal() && modal().textContent.includes('This folder cannot be protected'));
      click(button('OK', modal()), 'OK');
      assert(!modal(), 'OK did not close the dialog');
    }],

    ['drop a folder and protect it without locking it', async () => {
      window.testBridge.drop([await check('path', 'home/Notes')]);
      const f = await protectDialog();
      assert(q('input', choice('hide')).checked, 'the default protection from the settings is not preselected');
      click(labelOf(f.lockNow), '“Lock the folder now”');
      assert(!f.lockNow.checked, '“Lock the folder now” cannot be unticked');
      click(submit(f), 'Protect');
      await waitFor('the folder to be added', () => toast('“Notes” is protected. Lock it whenever you like.') && card('Notes'));
      await check('unlocked', 'Notes');
    }],

    ['switch it to encrypt mode from its menu', async () => {
      await menuItem('Notes', 'Use encrypt mode instead');
      await waitFor('the mode change', () => toast('“Notes” will be encrypted when you lock it.') && card('Notes').textContent.includes('Encrypt mode'));
    }],

    ['closing with an unlocked folder offers to lock it first', async () => {
      click(q('[data-action=close-vault]'), 'Close vault');
      await waitFor('the question', () => modal() && modal().textContent.includes('One folder is still unlocked'));
      click(button('Lock all and close', modal()), 'Lock all and close');
      await waitFor('the unlock screen', () => q('form[data-form=unlock]'));
      await check('closed');
      await check('gone', 'Notes');
    }],

    ['a mistyped recovery key is refused', async () => {
      click(button('Forgot password?'), 'Forgot password?');
      const f = await waitFor('the recovery form', () => q('form[data-form=recover]'));
      type(f.key, 'ABCD-EFGH-JKMN');
      click(submit(f), 'Continue');
      await waitFor('the error', () => formError(f, 'not valid'));
    }],

    ['recover with the recovery key and choose a new password', async () => {
      const f = q('form[data-form=recover]');
      type(f.key, recoveryKey.toLowerCase().replaceAll('-', ' '));
      click(submit(f), 'Continue');
      const r = await waitFor('the new password form', () => q('form[data-form=reset]'));
      type(r.password, PW3);
      type(r.confirm, PW3);
      click(submit(r), 'Save password');
      await waitFor('the folders', () => toast('Your new password is set.') && card('Taxes') && card('Notes'));
      await check('locked', 'Notes');
    }],

    ['unlocking where the original place is taken restores elsewhere', async () => {
      await check('occupy', 'Taxes');
      await check('pick', 'restore');
      click(cardButton('Taxes', 'unlock'), 'Unlock');
      await waitFor('the question', () => modal() && modal().textContent.includes('The original location is not available'));
      click(button('Choose location…', modal()), 'Choose location…');
      await waitFor('the restore', () => toast('“Taxes” is unlocked at its new location.') && idle());
      await check('restored-elsewhere', 'Taxes');
    }],

    ['cancelling a lock half-way changes nothing', async () => {
      await check('pick', 'home/Big');
      click(button('Add folder'), 'Add folder');
      const f = await protectDialog();
      click(q('.title', choice('encrypt')), 'Encrypt choice');
      click(labelOf(f.lockNow), '“Lock the folder now”');
      click(submit(f), 'Protect');
      await waitFor('the folder to be added', () => toast('“Big” is protected.') && card('Big'));
      await check('pause-next-lock');
      click(cardButton('Big', 'lock'), 'Lock');
      // The backend holds the lock at its first encryption progress, after
      // the folder has been moved into the store.
      const cancel = await waitFor('the lock to be encrypting', () =>
        /Encrypting|Verifying/.test((q('#progress-label') || {}).textContent) && q('#progress [data-action=cancel]'));
      click(cancel, 'Cancel');
      await waitFor('the lock to stop', () => toast('Cancelled. Nothing was changed.') && idle());
      await waitFor('Big to be shown as unlocked', () => q('.pill.unlocked', card('Big')));
      await check('cancelled', 'Big');
    }],

    ['stop protecting every folder', async () => {
      for (const name of ['Notes', 'Taxes', 'Big']) await stopProtecting(name);
      await waitFor('the empty list', () => q('.empty'));
      await check('final');
    }],
  ];

  async function main() {
    await waitFor('the app to start', () => q('#app form, #app .topbar'));
    for (const [name, fn] of steps) {
      current = name;
      await post('/test/log', { line: `step: ${name}` });
      qa('#toasts .toast').forEach((t) => t.remove());
      await fn();
      assert(!errors.length, `JavaScript errors: ${errors.join('; ')}`);
    }
    await post('/test/done', { ok: true });
  }

  main().catch((e) => post('/test/done', {
    ok: false, step: current, error: String((e && e.stack) || e), toasts: toasts(), errors,
  }));
})();
