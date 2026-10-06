import { expect, test } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHmac } from 'node:crypto';

test.describe.configure({ mode: 'serial' });

const admin = { username: 'browser-admin', password: 'BrowserTestPassword-123', pin: '2468' };
const user = { username: 'browser-user', password: 'BrowserUserPassword-123', pin: '1357' };

function decodeBase32(value) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let buffer = 0;
  let bits = 0;
  const output = [];
  for (const character of value.toUpperCase().replace(/=+$/, '')) {
    const digit = alphabet.indexOf(character);
    if (digit < 0) throw new Error(`invalid base32 character: ${character}`);
    buffer = (buffer << 5) | digit;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      output.push((buffer >> bits) & 0xff);
    }
  }
  return Buffer.from(output);
}

function totpCode(secret, timestamp = Date.now()) {
  const step = BigInt(Math.floor(timestamp / 1000 / 30));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(step);
  const digest = createHmac('sha1', decodeBase32(secret)).update(counter).digest();
  const offset = digest[digest.length - 1] & 0x0f;
  const binary = ((digest[offset] & 0x7f) << 24) |
    (digest[offset + 1] << 16) |
    (digest[offset + 2] << 8) |
    digest[offset + 3];
  return String(binary % 1_000_000).padStart(6, '0');
}

async function stableTotpCode(page, secret, previous = '') {
  for (;;) {
    const remaining = 30 - (Math.floor(Date.now() / 1000) % 30);
    const code = totpCode(secret);
    if (remaining >= 5 && code !== previous) return code;
    await page.waitForTimeout(500);
  }
}

async function fillAccount(page, name, email, order) {
  await page.locator('#new-account').click();
  const form = page.locator('#account-form');
  await form.locator('[name="canonical_name"]').fill(name);
  await form.locator('[name="email"]').fill(email);
  await form.locator('[name="sender_name"]').fill(name);
  await form.locator('[name="imap_host"]').fill('imap.example.test');
  await form.locator('[name="imap_user"]').fill(email);
  await form.locator('[name="imap_password"]').fill('imap-password');
  await form.locator('[name="smtp_host"]').fill('smtp.example.test');
  await form.locator('[name="smtp_user"]').fill(email);
  await form.locator('[name="smtp_password"]').fill('smtp-password');
  await form.locator('[name="display_order"]').fill(String(order));
  await form.locator('[name="folder_map"]').fill('INBOX=Inbox\nSent=Sent');

  const validate = async route => {
    if (route.request().method() === 'POST') {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ folders: ['INBOX', 'Sent'] }) });
    } else {
      await route.continue();
    }
  };
  await page.route('**/api/v1/accounts/validate', validate);
  await form.locator('button[type="submit"]').click();
  await expect(page.locator('#accounts')).toContainText(name);
  await page.unroute('**/api/v1/accounts/validate', validate);
}

async function fillContact(page, name, email) {
  const form = page.locator('#contact-form');
  await form.locator('[name="name"]').fill(name);
  await form.locator('[name="email"]').fill(email);
  await form.locator('[name="display_order"]').fill('0');
  await form.locator('button[type="submit"]').click();
  await expect(page.locator('#contacts')).toContainText(name);
}

async function logoutAndWait(page) {
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'domcontentloaded' }),
    page.locator('#logout').click(),
  ]);
  await expect(page.locator('#login')).toBeVisible();
}

test('covers setup, refresh, roles, account ordering, alerts, recovery, and CSP', async ({ page }) => {
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error));

  const landing = await page.goto('/');
  expect(landing).not.toBeNull();
  expect(landing.headers()['content-security-policy']).toContain("script-src");
  await expect(page.locator('#setup')).toBeVisible();

  await page.locator('#setup [name="username"]').fill(admin.username);
  await page.locator('#setup [name="password"]').fill(admin.password);
  await page.locator('#setup [name="pin"]').fill(admin.pin);
  await page.locator('#setup button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#users-panel')).toBeVisible();
  await expect(page.locator('#sip-panel')).toBeVisible();

  await fillAccount(page, 'First account', 'first@example.test', 0);
  await fillAccount(page, 'Second account', 'second@example.test', 1);
  const firstDown = page.locator('#accounts button[data-down-account]').first();
  await firstDown.click();
  await expect(page.locator('#accounts tbody tr').first()).toContainText('Second account');

  // Exercise the transaction boundary with two simultaneous complete-list
  // writes. Either request may win, but neither may leave a partial/duplicate
  // order behind.
  const accountIDs = await page.locator('#accounts button[data-edit-account]').evaluateAll(buttons => buttons.map(button => button.dataset.editAccount));
  expect(accountIDs).toHaveLength(2);
  const concurrentOrders = await page.evaluate(async ids => {
    const csrfResponse = await fetch('/api/v1/me');
    const csrf = csrfResponse.headers.get('X-CSRF-Token');
    return Promise.all([ids, [...ids].reverse()].map(order => fetch('/api/v1/accounts/order', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify({ ids: order }),
    }).then(response => response.status)));
  }, accountIDs);
  expect(concurrentOrders).toEqual([200, 200]);
  await page.reload();
  await expect(page.locator('#accounts tbody tr')).toHaveCount(2);

  const savedTestPayloads = [];
  const captureSavedTest = async route => {
    if (route.request().method() === 'POST') {
      savedTestPayloads.push(route.request().postDataJSON());
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ folders: ['INBOX', 'Sent'] }) });
    } else {
      await route.continue();
    }
  };
  await page.route('**/api/v1/accounts/test', captureSavedTest);
  const savedAccountID = await page.locator('#accounts button[data-test-account]').first().getAttribute('data-test-account');
  expect(savedAccountID).toBeTruthy();
  await page.locator('#accounts button[data-test-account]').first().click();
  await expect.poll(() => savedTestPayloads.length).toBe(1);
  expect(savedTestPayloads[0]).toEqual({ account_id: savedAccountID });
  await page.unroute('**/api/v1/accounts/test', captureSavedTest);

  // A new-account test must send the values currently in the form, including
  // an unsaved host, rather than silently testing a saved account.
  await page.locator('#new-account').click();
  const unsavedForm = page.locator('#account-form');
  await unsavedForm.locator('[name="canonical_name"]').fill('Unsaved test account');
  await unsavedForm.locator('[name="email"]').fill('unsaved@example.test');
  await unsavedForm.locator('[name="imap_host"]').fill('edited-imap.example.test');
  await unsavedForm.locator('[name="imap_user"]').fill('unsaved@example.test');
  await unsavedForm.locator('[name="imap_password"]').fill('imap-password');
  await unsavedForm.locator('[name="smtp_host"]').fill('edited-smtp.example.test');
  await unsavedForm.locator('[name="smtp_user"]').fill('unsaved@example.test');
  await unsavedForm.locator('[name="smtp_password"]').fill('smtp-password');
  const validationPayloads = [];
  const captureValidation = async route => {
    if (route.request().method() === 'POST') {
      validationPayloads.push(route.request().postDataJSON());
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ folders: ['INBOX'] }) });
    } else {
      await route.continue();
    }
  };
  await page.route('**/api/v1/accounts/validate', captureValidation);
  await page.locator('#test-account').click();
  await expect.poll(() => validationPayloads.length).toBe(1);
  expect(validationPayloads[0].imap_host).toBe('edited-imap.example.test');
  expect(validationPayloads[0].smtp_host).toBe('edited-smtp.example.test');
  await page.unroute('**/api/v1/accounts/validate', captureValidation);
  await page.locator('#cancel-account').click();

  await page.locator('#global-alerts-available').uncheck();
  await page.locator('#save-global-alerts').click();
  await expect(page.locator('#alert-controls-extra')).toHaveClass(/hidden/);
  await expect(page.locator('#settings-form button[type="submit"]')).toBeVisible();
  await page.locator('#settings-form [name="tts_voice"]').fill('en_US-hfc_male-medium');
  await page.locator('#settings-form button[type="submit"]').click();
  await expect(page.locator('#app-status')).toContainText('saved');

  const unnamedVisibleSettingsControls = await page.locator('#settings-form input, #settings-form select, #settings-form button').evaluateAll(elements => elements
    .filter(element => element.offsetParent !== null && !element.disabled)
    .filter(element => {
      const label = element.labels?.length ? Array.from(element.labels).map(node => node.textContent).join(' ') : '';
      const name = element.getAttribute('aria-label') || element.getAttribute('title') || label || element.textContent;
      return !String(name || '').trim();
    })
    .map(element => `${element.tagName}#${element.id}[name=${element.getAttribute('name') || ''}]`));
  expect(unnamedVisibleSettingsControls).toEqual([]);

  // The alert-only controls are display:none while unavailable. Keyboard
  // navigation must skip them and still reach the independent voice save.
  await page.locator('#settings-form [name="tts_voice"]').focus();
  const tabTargets = [];
  for (let i = 0; i < 12; i += 1) {
    await page.keyboard.press('Tab');
    tabTargets.push(await page.evaluate(() => {
      const element = document.activeElement;
      return {
        tag: element?.tagName || '',
        type: element?.getAttribute('type') || '',
        id: element?.id || '',
        name: element?.getAttribute('name') || '',
        hiddenAlert: !!element?.closest('#alert-controls, #alert-controls-extra'),
      };
    }));
  }
  expect(tabTargets.some(target => target.tag === 'BUTTON' && target.type === 'submit')).toBeTruthy();
  expect(tabTargets.some(target => target.hiddenAlert && target.id !== 'global-alerts-available')).toBeFalsy();

  await page.locator('#global-alerts-available').check();
  await page.locator('#save-global-alerts').click();
  await expect(page.locator('#alert-controls-extra')).not.toHaveClass(/hidden/);
  await page.locator('#global-alerts-available').uncheck();
  await page.locator('#save-global-alerts').click();
  await expect(page.locator('#alert-controls-extra')).toHaveClass(/hidden/);

  await page.locator('#user-form [name="username"]').fill(user.username);
  await page.locator('#user-form [name="role"]').selectOption('user');
  await page.locator('#user-form [name="password"]').fill(user.password);
  await page.locator('#user-form [name="pin"]').fill(user.pin);
  await page.locator('#user-form button[type="submit"]').click();
  await expect(page.locator('#users')).toContainText(user.username);

  await fillContact(page, 'Admin-only contact', 'admin-contact@example.test');

  await page.reload();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#alert-controls-extra')).toHaveClass(/hidden/);

  if (process.env.VOXMAIL_BROWSER_RESTART_CONTAINER) {
    execFileSync('docker', ['restart', process.env.VOXMAIL_BROWSER_RESTART_CONTAINER], { stdio: 'inherit' });
    await expect.poll(async () => {
      try {
        return (await page.request.get('/healthz')).status();
      } catch (_) {
        return 0;
      }
    }, { timeout: 20_000 }).toBe(200);
    await page.reload();
    await expect(page.locator('#login')).toBeVisible();
    await expect(page.locator('#app')).toBeHidden();
    await page.locator('#login [name="username"]').fill(admin.username);
    await page.locator('#login [name="password"]').fill(admin.password);
    await page.locator('#login button[type="submit"]').click();
    await expect(page.locator('#app')).toBeVisible();
  }

  const secondTab = await page.context().newPage();
  await secondTab.goto('/');
  await expect(secondTab.locator('#app')).toBeVisible();

  await logoutAndWait(page);
  await secondTab.reload();
  await expect(secondTab.locator('#login')).toBeVisible();
  await secondTab.close();
  await page.locator('#login [name="username"]').fill(user.username);
  await page.locator('#login [name="password"]').fill(user.password);
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#accounts')).toContainText('No accounts configured.');
  await expect(page.locator('#accounts')).not.toContainText('First account');
  await expect(page.locator('#accounts')).not.toContainText('Second account');
  await expect(page.locator('#contacts')).toContainText('No contacts.');
  await expect(page.locator('#contacts')).not.toContainText('Admin-only contact');
  await expect(page.locator('#users-panel')).toHaveClass(/hidden/);
  await expect(page.locator('#sip-panel')).toHaveClass(/hidden/);
  await expect(page.locator('#alert-controls-extra')).toHaveClass(/hidden/);

  await fillContact(page, 'User-only contact', 'user-contact@example.test');
  await expect(page.locator('#contacts')).toContainText('User-only contact');

  await logoutAndWait(page);
  const clearedSensitiveFields = await page.locator('input[type="password"], input[name="pin"], input[name="totp"], input[name="backup_code"], input[name="code"], input[name="secret"]').evaluateAll(elements => elements
    .filter(element => element.value !== '')
    .map(element => `${element.tagName}#${element.id}[name=${element.getAttribute('name') || ''}]`));
  expect(clearedSensitiveFields).toEqual([]);
  await page.locator('#login [name="username"]').fill(admin.username);
  await page.locator('#login [name="password"]').fill(admin.password);
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#accounts')).toContainText('First account');
  await expect(page.locator('#accounts')).toContainText('Second account');
  await expect(page.locator('#contacts')).toContainText('Admin-only contact');
  await expect(page.locator('#contacts')).not.toContainText('User-only contact');

  await page.locator('#twofa-form [name="current_password"]').fill(admin.password);
  await page.locator('#twofa-setup').click();
  const secretField = page.locator('#twofa-form [name="secret"]');
  await expect(secretField).not.toHaveValue('');
  const totpSecret = await secretField.inputValue();
  expect(totpSecret).toMatch(/^[A-Z2-7]+$/);
  const enableCode = await stableTotpCode(page, totpSecret);
  await page.locator('#twofa-form [name="code"]').fill(enableCode);
  await page.locator('#twofa-enable').click();
  await expect(page.locator('#twofa-status')).toContainText('Enabled');

  await logoutAndWait(page);
  const acceptedCode = await stableTotpCode(page, totpSecret);
  await page.locator('#login [name="username"]').fill(admin.username);
  await page.locator('#login [name="password"]').fill(admin.password);
  await page.locator('#login [name="totp"]').fill(acceptedCode);
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();

  await logoutAndWait(page);
  await page.locator('#login [name="username"]').fill(admin.username);
  await page.locator('#login [name="password"]').fill(admin.password);
  await page.locator('#login [name="totp"]').fill(acceptedCode);
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#login')).toBeVisible();
  await expect(page.locator('#auth-status')).toContainText('authenticator');

  const nextCode = await stableTotpCode(page, totpSecret, acceptedCode);
  await page.locator('#login [name="totp"]').fill(nextCode);
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();

  await logoutAndWait(page);
  await expect(page.locator('#recovery-request')).toBeVisible();
  await page.locator('#recovery-request [name="email"]').fill('unknown@example.invalid');
  await page.locator('#recovery-request button[type="submit"]').click();
  await expect(page.locator('#auth-status')).toContainText('If the address is configured');
  expect(pageErrors).toEqual([]);
});
