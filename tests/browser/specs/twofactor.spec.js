import { expect, test } from '@playwright/test';
import { readFile } from 'node:fs/promises';

const key = 'JBSWY3DPEHPK3PXP';
// Flow-only transport fixture. The HTTP backend test independently decodes the
// production PNG and verifies the scanned URI against the enabled secret.
const image = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a0ioAAAAASUVORK5CYII=';
const codes = Array.from({ length: 10 }, (_, i) => `BACKUP-${i + 1}`);

async function signedIn(page) {
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const json = path === '/api/v1/me' ? { id: 'twofactor-user', username: 'twofactor-user', role: 'user', two_factor_enabled: false }
      : path === '/api/v1/settings' ? { alerts_available: false, menu_speed: 3, email_speed: 2 }
      : [];
    await route.fulfill({ status: path === '/api/v1/users' ? 403 : 200, json });
  });
  await page.route('**/api/v1/security/2fa/setup', route => route.fulfill({ json: { secret: key, qr_data_uri: image, provisioning_uri: `otpauth://totp/VOXMail:test?secret=${key}&issuer=VOXMail` } }));
  await page.goto('/#security');
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#twofa-state')).toContainText('not enabled');
}

async function scan(page) {
  await page.locator('#twofa-form [name="current_password"]').fill('DisposablePassword-123');
  await page.locator('#twofa-setup').click();
  await expect(page.locator('#twofa-qr')).toBeVisible();
  await expect(page.locator('#twofa-next')).toBeFocused();
  await expect(page.locator('#twofa-form [name="secret"]')).toHaveValue(key);
  await expect(page.locator('#twofa-enable')).toBeHidden();
}

for (const width of [1536, 390]) {
  test(`QR setup verifies, retries, downloads and requires acknowledgment at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1024 });
    const external = [];
    page.on('request', request => { if (!request.url().startsWith(new URL(test.info().project.use.baseURL).origin) && !request.url().startsWith('data:')) external.push(request.url()); });
    await signedIn(page);
    let setupAttempts = 0;
    await page.route('**/api/v1/security/2fa/setup', async route => {
      await route.fulfill(++setupAttempts === 1 ? { status: 400, json: { error: 'current password is incorrect' } } : { json: { secret: key, qr_data_uri: image } });
    });
    await page.locator('#twofa-form [name="current_password"]').fill('WrongPassword-123');
    await page.locator('#twofa-setup').click();
    await expect(page.locator('#twofa-status')).toContainText('current password is incorrect');
    await expect(page.locator('#twofa-status')).toBeFocused();
    await expect(page.locator('#app')).toBeVisible();
    await scan(page);
    await page.locator('#twofa-next').press('Enter');
    await expect(page.locator('#twofa-form [name="code"]')).toBeFocused();
    await expect(page.locator('#twofa-qr')).toBeHidden();
    let attempts = 0;
    await page.route('**/api/v1/security/2fa/enable', async route => {
      expect(route.request().postDataJSON().secret).toBe(key);
      await route.fulfill(++attempts === 1 ? { status: 400, json: { error: 'password or authenticator code is incorrect' } } : { json: { backup_codes: codes, status: 'enabled' } });
    });
    await page.locator('#twofa-form [name="code"]').fill('000000');
    await page.locator('#twofa-enable').click();
    await expect(page.locator('#twofa-status')).toContainText('authenticator code is incorrect');
    await expect(page.locator('#twofa-enable')).toBeEnabled();
    await page.locator('#twofa-form [name="code"]').fill('123456');
    await page.locator('#twofa-enable').click();
    await expect(page.locator('#twofa-backup-codes')).toBeFocused();
    await expect(page.locator('#twofa-backup-codes')).toContainText(codes[9]);
    await expect(page.locator('#twofa-done')).toBeDisabled();
    await expect(page.locator('#twofa-qr')).not.toHaveAttribute('src');
    await expect(page.locator('#twofa-form [name="secret"]')).toHaveValue('');
    await expect(page.locator('#twofa-form [name="current_password"]')).toHaveValue('');
    const downloadPromise = page.waitForEvent('download');
    await page.locator('#twofa-download').click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toBe('voxmail-backup-codes.txt');
    const downloaded = await readFile(await download.path(), 'utf8');
    for (const code of codes) expect(downloaded).toContain(code);
    await expect(page.locator('#twofa-done')).toBeDisabled();
    await page.locator('#twofa-saved').check();
    await page.locator('#twofa-done').press('Enter');
    await expect(page.locator('#twofa-backup-codes')).toBeEmpty();
    await expect(page.locator('#twofa-backup')).toBeHidden();
    await expect(page.locator('#twofa-disable')).toBeFocused();
    await expect(page.locator('#twofa-state')).toContainText('is enabled');
    expect(external).toEqual([]);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test('canceling unverified setup clears the QR and key and restores focus', async ({ page }) => {
  await signedIn(page);
  await scan(page);
  await page.locator('#twofa-cancel').click();
  await expect(page.locator('#twofa-form [name="secret"]')).toHaveValue('');
  await expect(page.locator('#twofa-form [name="current_password"]')).toHaveValue('');
  await expect(page.locator('#twofa-qr')).not.toHaveAttribute('src');
  await expect(page.locator('#twofa-setup')).toBeFocused();
  await expect(page.locator('#twofa-backup')).toBeHidden();
  await expect(page.locator('#twofa-state')).toContainText('not enabled');
});

test('pending setup suppresses duplicates and late responses after session expiry', async ({ page }) => {
  await signedIn(page);
  let release, entered;
  const pending = new Promise(resolve => { release = resolve; });
  const started = new Promise(resolve => { entered = resolve; });
  let calls = 0;
  await page.route('**/api/v1/security/2fa/setup', async route => { calls++; entered(); await pending; await route.fulfill({ json: { secret: key, qr_data_uri: image } }).catch(() => {}); });
  await page.locator('#twofa-form [name="current_password"]').fill('DisposablePassword-123');
  await page.locator('#twofa-setup').click();
  await started;
  await expect(page.locator('#twofa-form')).toHaveAttribute('aria-busy', 'true');
  await expect(page.locator('#twofa-setup')).toBeDisabled();
  await page.locator('#twofa-setup').evaluate(button => button.click());
  expect(calls).toBe(1);
  await page.route('**/api/v1/contacts', route => route.fulfill({ status: 401, json: { error: 'authentication required' } }));
  await page.locator('[data-task="caller-access"]').click();
  await page.locator('#contact-form [name="name"]').fill('Expire fixture');
  await page.locator('#contact-form [name="email"]').fill('expire@example.test');
  await page.locator('#contact-form button[type="submit"]').click();
  await expect(page.locator('#login')).toBeVisible();
  release();
  await expect(page.locator('#twofa-qr')).not.toHaveAttribute('src');
  await expect(page.locator('#twofa-form [name="secret"]')).toHaveValue('');
  await expect(page.locator('#twofa-status')).toBeEmpty();
});
