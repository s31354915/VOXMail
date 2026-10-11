import { expect, test } from '@playwright/test';

// Recovery transport is controlled here so the browser can exercise every
// transition without delivering real mail. Token expiry, single-use redemption
// and provider/offline enforcement have separate backend acceptance.
async function signedOut(page) {
  await page.route('**/api/v1/me', route => route.fulfill({ status: 401, json: { error: 'authentication required' } }));
  await page.route('**/api/v1', route => route.fulfill({ json: { setup_available: false, offline_mode: false } }));
  await page.goto('/');
  await expect(page.locator('#login')).toBeVisible();
  await expect(page.locator('body')).toHaveAttribute('data-setup-available', 'false');
}

async function oneForm(page, id) {
  await expect(page.locator('#auth form:visible')).toHaveCount(1);
  await expect(page.locator(id)).toBeVisible();
}

for (const width of [1440, 390]) {
  test(`password recovery swaps a single form, retries errors and returns to login at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await signedOut(page);
    await oneForm(page, '#login');
    await page.locator('#login [name="password"]').fill('SecretToClear-123');
    await page.getByRole('button', { name: 'Reset password', exact: true }).focus();
    await page.keyboard.press('Enter');
    await oneForm(page, '#recovery-request');
    await expect(page.locator('#recovery-request [name="email"]')).toBeFocused();
    await expect(page.locator('#login [name="password"]')).toHaveValue('');
    const sent = [];
    await page.route('**/api/v1/recovery/request', async route => {
      sent.push(route.request().postDataJSON());
      await route.fulfill({ json: { status: 'accepted' } });
    });
    await page.locator('#recovery-request [name="email"]').fill('configured@example.test');
    await page.getByRole('button', { name: 'Send recovery code', exact: true }).click();
    await oneForm(page, '#recovery-confirm');
    expect(sent).toEqual([{ email: 'configured@example.test', purpose: 'reset_password' }]);
    await expect(page.locator('#auth-status')).toContainText('If the address is configured');
    await expect(page.locator('#recovery-confirm [name="email"]')).toHaveValue('configured@example.test');
    await expect(page.locator('#recovery-confirm [name="email"]')).toHaveAttribute('readonly', '');
    await expect(page.locator('#recovery-confirm [name="code"]')).toBeFocused();
    await expect(page.locator('#recovery-confirm [name="new_password"]')).toBeVisible();
    let attempts = 0;
    await page.route('**/api/v1/recovery/confirm', async route => {
      const body = route.request().postDataJSON();
      expect(body.purpose).toBe('reset_password');
      expect(body.email).toBe('configured@example.test');
      await route.fulfill(++attempts === 1
        ? { status: 401, json: { error: 'invalid or expired recovery code' } }
        : { json: { status: 'password_reset' } });
    });
    await page.locator('#recovery-confirm [name="code"]').fill('123456');
    await page.locator('#recovery-confirm [name="new_password"]').fill('ChangedPassword-123');
    await page.locator('#recovery-confirm button[type="submit"]').click();
    await expect(page.locator('#auth-status')).toContainText('invalid or expired recovery code');
    await oneForm(page, '#recovery-confirm');
    await expect(page.locator('#recovery-confirm button[type="submit"]')).toBeEnabled();
    await page.locator('#recovery-confirm [name="code"]').fill('654321');
    await page.locator('#recovery-confirm button[type="submit"]').click();
    await oneForm(page, '#login');
    await expect(page.locator('#auth-status')).toContainText('Password reset');
    await expect(page.locator('#login [name="username"]')).toBeFocused();
    await expect(page.locator('#recovery-confirm [name="code"]')).toHaveValue('');
    await expect(page.locator('#recovery-confirm [name="new_password"]')).toHaveValue('');
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test('TOTP bypass uses its own purpose, no new password, and clears canceled secrets', async ({ page }) => {
  await signedOut(page);
  await page.getByRole('button', { name: 'Bypass TOTP', exact: true }).click();
  await oneForm(page, '#recovery-request');
  await expect(page.locator('#recovery-request')).toContainText('Bypass TOTP');
  await page.route('**/api/v1/recovery/request', async route => {
    expect(route.request().postDataJSON().purpose).toBe('bypass_2fa');
    await route.fulfill({ json: { status: 'accepted' } });
  });
  await page.locator('#recovery-request [name="email"]').fill('configured@example.test');
  await page.getByRole('button', { name: 'Send recovery code', exact: true }).click();
  await oneForm(page, '#recovery-confirm');
  await expect(page.locator('#recovery-confirm [name="new_password"]')).toBeHidden();
  await page.locator('#recovery-confirm [name="code"]').fill('123456');
  await page.route('**/api/v1/recovery/confirm', async route => {
    const body = route.request().postDataJSON();
    expect(body.purpose).toBe('bypass_2fa');
    expect(body.new_password).toBeUndefined();
    await route.fulfill({ status: 401, json: { error: 'invalid or expired recovery code' } });
  });
  await page.locator('#recovery-confirm button[type="submit"]').click();
  await expect(page.locator('#auth-status')).toContainText('invalid or expired recovery code');
  await page.getByRole('button', { name: 'Back to sign in', exact: true }).click();
  await oneForm(page, '#login');
  await expect(page.locator('#login [name="username"]')).toBeFocused();
  await expect(page.locator('#recovery-confirm [name="code"]')).toHaveValue('');
  await expect(page.locator('#recovery-confirm [name="email"]')).toHaveValue('');
  await expect(page.locator('#auth-status')).toBeEmpty();
});

test('canceling pending recovery prevents a late response from reopening confirmation', async ({ page }) => {
  await signedOut(page);
  let release;
  const held = new Promise(resolve => { release = resolve; });
  let entered;
  const started = new Promise(resolve => { entered = resolve; });
  let finish;
  const settled = new Promise(resolve => { finish = resolve; });
  await page.route('**/api/v1/recovery/request', async route => {
    entered();
    await held;
    await route.fulfill({ json: { status: 'accepted' } }).catch(() => {});
    finish();
  });
  try {
    await page.getByRole('button', { name: 'Reset password', exact: true }).click();
    await page.locator('#recovery-request [name="email"]').fill('configured@example.test');
    await page.getByRole('button', { name: 'Send recovery code', exact: true }).click();
    await started;
    await expect(page.locator('#recovery-request button[type="submit"]')).toBeDisabled();
    await page.getByRole('button', { name: 'Back to sign in', exact: true }).click();
    release();
    await settled;
    await oneForm(page, '#login');
    await expect(page.locator('#auth-status')).toBeEmpty();
    await expect(page.locator('#recovery-request [name="email"]')).toHaveValue('');
  } finally {
    release();
  }
});

test('a verified TOTP bypass enters the console and clears the recovery form', async ({ page }) => {
  await signedOut(page);
	await page.route('**/api/v1/status', route => route.fulfill({ json: { checked_at: '2026-10-07T12:00:00Z', readiness: 'ready', runtime: { registration: 'registered', process: 'running', bridge: 'connected', media: 'ready' }, offline_mode: false } }));
  await page.route('**/api/v1/me', route => route.fulfill({ json: { id: 'fixture-user', username: 'recovered-user', role: 'user', two_factor_enabled: true } }));
  await page.route('**/api/v1/settings', route => route.fulfill({ json: { tts_voice: 'fixture', menu_speed: 3, email_speed: 2, alerts_available: false } }));
  for (const path of ['voices', 'accounts', 'contacts', 'whitelist']) {
    await page.route('**/api/v1/' + path, route => route.fulfill({ json: [] }));
  }
  await page.route('**/api/v1/users', route => route.fulfill({ status: 403, json: { error: 'administrator access required' } }));
  await page.route('**/api/v1/recovery/request', route => route.fulfill({ json: { status: 'accepted' } }));
  await page.route('**/api/v1/recovery/confirm', async route => {
    expect(route.request().postDataJSON()).toEqual({ email: 'configured@example.test', purpose: 'bypass_2fa', code: '123456' });
    await route.fulfill({ json: { status: 'signed_in', user: { id: 'fixture-user', username: 'recovered-user', role: 'user' } } });
  });
  await page.getByRole('button', { name: 'Bypass TOTP', exact: true }).click();
  await page.locator('#recovery-request [name="email"]').fill('configured@example.test');
  await page.getByRole('button', { name: 'Send recovery code', exact: true }).click();
  await page.locator('#recovery-confirm [name="code"]').fill('123456');
  await page.locator('#recovery-confirm button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#who')).toHaveText('recovered-user');
  await expect(page.locator('#users-panel')).toBeHidden();
  await expect(page.locator('#recovery-confirm [name="code"]')).toHaveValue('');
  await expect(page.locator('#recovery-confirm [name="email"]')).toHaveValue('');
});

test('two setup tabs reconcile completion into a single sign-in form', async ({ page, context }) => {
  let complete = false;
  await context.route('**/api/v1/me', route => route.fulfill({ status: 401, json: { error: 'authentication required' } }));
  await context.route('**/api/v1', route => route.fulfill({ json: { setup_available: !complete } }));
  await context.route('**/api/v1/setup', async route => {
    expect(route.request().postDataJSON().sign_in).toBe(false);
    if (complete) {
      await route.fulfill({ status: 409, json: { error: 'setup has already been completed' } });
    } else {
      complete = true;
      await route.fulfill({ status: 201, json: { id: 'first', username: 'first-admin', role: 'admin' } });
    }
  });
  const other = await context.newPage();
  for (const tab of [page, other]) {
    await tab.goto('/');
    await oneForm(tab, '#setup');
    await tab.locator('#setup [name="username"]').fill('first-admin');
    await tab.locator('#setup [name="password"]').fill('BootstrapPassword-123');
    await tab.locator('#setup [name="pin"]').fill('2468');
  }
  await page.locator('#setup button[type="submit"]').click();
  await oneForm(page, '#login');
  await expect(page.locator('#auth-status')).toContainText('Administrator created');
  await expect(page.locator('#login [name="password"]')).toBeFocused();
  await other.locator('#setup button[type="submit"]').click();
  await oneForm(other, '#login');
  await expect(other.locator('#auth-status')).toContainText('another tab');
  await expect(other.locator('#login [name="username"]')).toBeFocused();
  for (const tab of [page, other]) {
    await expect(tab.locator('body')).toHaveAttribute('data-setup-available', 'false');
    await expect(tab.locator('#setup [name="password"]')).toHaveValue('');
    await expect(tab.locator('#setup [name="pin"]')).toHaveValue('');
  }
});
