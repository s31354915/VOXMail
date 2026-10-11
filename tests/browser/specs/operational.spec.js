import { expect, test } from '@playwright/test';

function snapshot(registration = 'failed') {
  return { checked_at: '2026-10-07T10:11:12Z', readiness: registration === 'registered' ? 'ready' : 'not_ready',
    reason: registration === 'registered' ? '' : 'SIP registration is failed', offline_mode: false,
    runtime: { process: 'running', bridge: 'connected', registration, media: 'ready' },
    ...(registration === 'failed' ? { sip_failure: { category: 'authentication', reason: 'The SIP registrar rejected authentication or access.', action: 'Check the SIP username, password, registrar, and provider account permissions.', diagnostic: 'SIP response 403' } } : {}) };
}

function mailbox(state = 'failed') {
  return { id: 'operational-mailbox', canonical_name: 'Work mailbox', email: 'work@example.test', imap_host: 'imap.example.test', imap_port: 993, display_order: 0,
    last_successful_sync: { id: 10, finished_at: '2026-10-06T12:00:00Z', success: true, state: 'succeeded' },
    last_sync: { id: 11, kind: 'manual', started_at: '2026-10-07T09:00:00Z', state, success: state === 'succeeded',
      ...(state === 'running' || state === 'unfinished' ? {} : { finished_at: '2026-10-07T09:10:00Z' }),
      ...(state === 'failed' ? { failure: { category: 'timeout', reason: 'The operation timed out.', action: 'Check provider connectivity and server logs, then retry.', diagnostic: 'Child process exit status 7' } } : {}) } };
}

async function fixture(page, { role = 'admin', state = 'failed' } = {}) {
  const data = { service: snapshot(), mail: [mailbox(state), { id: 'never', canonical_name: 'New mailbox', email: 'new@example.test' }], statusCode: 200, mailCode: 200, signedIn: true, username: 'status-user', requests: 0 };
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/v1/status') { data.requests++; await route.fulfill({ status: data.statusCode, json: data.statusCode === 200 ? data.service : { error: 'provider snapshot unavailable' } }); return; }
    if (path === '/api/v1/accounts') { await route.fulfill({ status: data.mailCode, json: data.mailCode === 200 ? data.mail : { error: 'sync snapshot unavailable' } }); return; }
    if (path === '/api/v1/me' && !data.signedIn) { await route.fulfill({ status: 401, json: { error: 'authentication required' } }); return; }
    if (path === '/api/v1/login') { data.signedIn = true; await route.fulfill({ json: { id: 'status-user-2', username: data.username, role: 'user' } }); return; }
    const json = path === '/api/v1/me' ? { id: 'status-user', username: data.username, role, two_factor_enabled: false }
      : path === '/api/v1/settings' ? { alerts_available: false, menu_speed: 3, email_speed: 2 }
      : path === '/api/v1/admin/alerts' ? { available: false }
      : path === '/api/v1/sip' ? { enabled: true, domain: 'sip.example.test', username: 'sip-user', password_set: true } : [];
    await route.fulfill({ json });
  });
  await page.goto('/#mailboxes');
  await expect(page.locator('[data-sync-status="operational-mailbox"]')).toBeVisible();
  await expect(page.locator('#runtime-registration')).toHaveText('Failed');
  return data;
}

for (const width of [1536, 390]) {
  test(`SIP and mailbox states, failure details, stale snapshots and retries at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1024 });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const data = await fixture(page, { role: 'user' });
    await expect(page.locator('#sip-failure')).toContainText('SIP response 403');
    await expect(page.locator('#sip-failure')).toContainText('Check the SIP username');
    const sync = page.locator('[data-sync-status="operational-mailbox"]');
    await expect(sync).toContainText('Sync failed');
    await expect(sync).toContainText('The operation timed out.');
    await expect(sync).toContainText('Check provider connectivity');
    if (width === 390) {
      const bounds = await sync.boundingBox();
      expect(bounds.x).toBeGreaterThanOrEqual(0);
      expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
      await expect(sync.locator('.operational-failure')).toBeVisible();
      expect(await sync.locator('.operational-failure p').evaluateAll(elements => elements.every(el => el.scrollWidth <= el.clientWidth))).toBe(true);
    }
    await expect(sync).toContainText('Last successful sync:');
    await expect(sync.locator('time').last()).toHaveAttribute('datetime', '2026-10-06T12:00:00Z');
    await expect(page.locator('[data-sync-status="never"]')).toHaveText('Never synchronized');
    data.service = snapshot('registering'); data.mail[0] = mailbox('running');
    await page.locator('#refresh-status').click();
    await expect(page.locator('#runtime-registration')).toHaveText('Registering…');
    await expect(page.locator('#sip-failure')).toBeHidden();
    await expect(sync).toContainText('Synchronizing…');
    await expect(sync.locator('.operational-failure')).toHaveCount(0);
    data.statusCode = 503; data.mailCode = 500;
    await page.locator('#refresh-status').click();
    await expect(page.locator('#operational-status')).toContainText('may be stale');
    await expect(page.locator('#sync-freshness')).toContainText('may be stale');
    await expect(page.locator('#runtime-registration')).toHaveText('Registering…');
    await expect(sync).toContainText('Synchronizing…');
    data.statusCode = 200; data.mailCode = 200; data.service = snapshot('registered'); data.mail[0] = mailbox('succeeded');
    await page.locator('#refresh-status').click();
    await expect(page.locator('#runtime-registration')).toHaveText('Registered');
    await expect(page.locator('#operational-status')).toBeEmpty();
    await expect(page.locator('#sync-freshness')).not.toContainText('stale');
    await expect(sync).toContainText('Synchronized');
    data.service.offline_mode = true; data.service.runtime = { registration: 'disabled', process: 'disabled', bridge: 'disconnected', media: 'not_required' };
    data.mail[0] = mailbox('unfinished');
    await page.locator('#refresh-status').click();
    await expect(page.locator('#runtime-readiness')).toHaveText('Offline / maintenance');
    await expect(page.locator('#runtime-media')).toHaveText('Not required');
    await expect(sync).toContainText('Unfinished sync — worker not confirmed');
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    expect(errors).toEqual([]);
  });
}

test('automatic status polling keeps a mailbox draft and keyboard focus', async ({ page }) => {
  await page.clock.install();
  const data = await fixture(page, { state: 'running' });
  await page.locator('#new-account').click();
  const input = page.locator('#account-form [name="canonical_name"]');
  await input.fill('Keep this unsaved draft');
  await input.focus();
  const before = data.requests;
  data.service = snapshot('registered'); data.mail[0] = mailbox('failed');
  await page.clock.runFor(10100);
  await expect(page.locator('#runtime-registration')).toHaveText('Registered');
  expect(data.requests).toBe(before + 1);
  await expect(input).toBeFocused();
  await expect(input).toHaveValue('Keep this unsaved draft');
  await expect(page.locator('[data-sync-status="operational-mailbox"]')).toContainText('The operation timed out.');
});

test('failed manual synchronization refreshes the durable cause and previous success', async ({ page }) => {
  const data = await fixture(page, { state: 'succeeded' });
  await page.route('**/api/v1/accounts/operational-mailbox/sync', async route => {
    data.mail[0] = mailbox('failed');
    await route.fulfill({ status: 502, json: { error: 'The operation timed out. Check provider connectivity and server logs, then retry.' } });
  });
  await page.locator('[data-sync-account="operational-mailbox"]').click();
  await expect(page.locator('#accounts-operation-feedback')).toContainText('Work mailbox: Mailbox operation failed: The operation timed out.');
  await expect(page.locator('#app-status')).not.toContainText('The operation timed out.');
  await expect(page.locator('[data-sync-status="operational-mailbox"]')).toContainText('Sync failed');
  await expect(page.locator('[data-sync-status="operational-mailbox"] time').last()).toHaveAttribute('datetime', '2026-10-06T12:00:00Z');
});

test('running manual sync shows progress, cancels, and restores the retry action', async ({ page }) => {
  const data = await fixture(page, { state: 'running' });
  const sync = page.locator('[data-sync-status="operational-mailbox"]');
  await expect(sync.locator('progress[aria-label="Mailbox synchronization in progress"]')).toBeVisible();
  const cancel = page.getByRole('button', { name: 'Cancel sync' });
  await expect(cancel).toBeVisible();
  await page.route('**/api/v1/accounts/operational-mailbox/sync/cancel', async route => {
    data.mail[0] = mailbox('failed');
    data.mail[0].last_sync.failure = { category: 'canceled', reason: 'The synchronization was canceled.', action: 'Retry the mailbox synchronization when ready.' };
    await route.fulfill({ status: 202, json: { status: 'cancellation requested' } });
  });
  await cancel.click();
  await expect(sync).toContainText('Sync failed');
  await expect(sync).toContainText('The synchronization was canceled.');
  await expect(sync.getByRole('button', { name: 'Cancel sync' })).toHaveCount(0);
  await expect(page.locator('[data-sync-account="operational-mailbox"]')).toBeEnabled();
  await expect(page.locator('#app-status')).toContainText('Cancellation requested');
  data.mail[0] = mailbox('succeeded');
  await page.locator('[data-sync-account="operational-mailbox"]').click();
  await expect(sync).toContainText('Synchronized');
  await expect(page.locator('[data-sync-account="operational-mailbox"]')).toBeEnabled();
});

test('expiry stops status polls, clears diagnostics and rejects late snapshots before another user signs in', async ({ page }) => {
  const data = await fixture(page);
  let entered, release;
  const started = new Promise(resolve => { entered = resolve; });
  const pending = new Promise(resolve => { release = resolve; });
  await page.route('**/api/v1/status', async route => {
    entered(); await pending;
    await route.fulfill({ json: { ...snapshot(), sip_failure: { reason: 'Old user private reason', action: 'Old user action' } } }).catch(() => {});
  });
  await page.locator('#refresh-status').click(); await started;
  data.signedIn = false;
  await page.route('**/api/v1/contacts', route => route.fulfill({ status: 401, json: { error: 'authentication required' } }));
  await page.locator('[data-task="caller-access"]').click();
  await page.locator('#contact-form [name="name"]').fill('Expire the session');
  await page.locator('#contact-form [name="email"]').fill('expire@example.test');
  await page.locator('#contact-form button[type="submit"]').click();
  await expect(page.locator('#login')).toBeVisible();
  await expect(page.locator('#sip-failure')).toBeEmpty();
  await expect(page.locator('#accounts')).toBeEmpty();
  await expect(page.locator('#operational-updated')).toBeEmpty();
  release();
  await page.unroute('**/api/v1/status');
  await page.unroute('**/api/v1/contacts');
  // Restore the broad fixture route's new-user response after removing the late-response override.
  await page.route('**/api/v1/status', route => route.fulfill({ json: snapshot('registered') }));
  data.username = 'second-user'; data.mail = [];
  await page.locator('#login [name="username"]').fill('second-user');
  await page.locator('#login [name="password"]').fill('SecondUserPassword-123');
  await page.locator('#login button[type="submit"]').click();
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#runtime-registration')).toHaveText('Registered');
  await expect(page.locator('#sip-failure')).toBeHidden();
  await expect(page.locator('#app')).not.toContainText('Old user private reason');
  await expect(page.locator('#accounts')).not.toContainText('Work mailbox');
});

test('accepted synchronization remains tracked after navigating away and reports completion', async ({ page }) => {
  const data = await fixture(page, { state: 'succeeded' });
  let starts = 0;
  await page.route('**/api/v1/accounts/operational-mailbox/sync', async route => {
    starts++;
    data.mail[0] = mailbox('running');
    await route.fulfill({ status: 202, json: { status: 'running', run_id: 11, account_id: 'operational-mailbox' } });
  });
  const start = page.locator('[data-sync-account="operational-mailbox"]');
  await start.click();
  await expect(page.locator('#app-status')).toContainText('Synchronization started.');
  await expect(start).toBeDisabled();
  await expect(page.locator('[data-cancel-sync="operational-mailbox"]')).toBeVisible();
  await page.goto('/#security');
  await page.goto('/#mailboxes');
  await expect(start).toBeDisabled();
  data.mail[0] = mailbox('succeeded');
  await page.locator('#refresh-status').click();
  await expect(page.locator('[data-sync-status="operational-mailbox"]')).toContainText('Synchronized');
  await expect(start).toBeEnabled();
  expect(starts).toBe(1);
});


test('background deletion cleanup reports pending, failure and recovery', async ({ page }) => {
  const data = await fixture(page, { role: 'user' });
  data.service.cleanup = { pending: 1, failed: 0 };
  await page.locator('#refresh-status').click();
  await expect(page.locator('#operational-status')).toContainText('1 stored-file cleanup job(s) pending.');
  data.service.cleanup = { pending: 0, failed: 1 };
  await page.locator('#refresh-status').click();
  await expect(page.locator('#operational-status')).toContainText('Check storage permissions and server logs');
  await expect(page.locator('#operational-status')).toHaveClass(/error/);
  data.service = snapshot('registered');
  data.service.cleanup = { pending: 0, failed: 0 };
  await page.locator('#refresh-status').click();
  await expect(page.locator('#operational-status')).toBeEmpty();
});
