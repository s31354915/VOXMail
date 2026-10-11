import { expect, test } from '@playwright/test';

async function signedIn(page, role = 'user') {
  const rows = [{ id: 'focus-mailbox', canonical_name: 'Focus mailbox', email: 'focus@example.test', imap_host: 'imap.example.test', imap_port: 993, display_order: 0 }];
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    if (path === '/api/v1/status') return route.fulfill({ json: { checked_at: '2026-10-07T00:00:00Z', readiness: 'ready', runtime: { process: 'running', bridge: 'connected', registration: 'registered', media: 'ready' } } });
    if (path === '/api/v1/accounts' && method === 'GET') return route.fulfill({ json: rows });
    if (path === '/api/v1/accounts/validate') return route.fulfill({ status: 200, json: { folders: ['INBOX'] } });
    if (path === '/api/v1/accounts' && method === 'POST') { rows.push({ ...route.request().postDataJSON(), id: 'saved-mailbox' }); return route.fulfill({ status: 201, json: rows.at(-1) }); }
    if (path === '/api/v1/contacts') return route.fulfill({ json: [{ id: 9, name: 'Ada Contact', email: 'ada@example.test', display_order: 0 }] });
    if (path === '/api/v1/me') return route.fulfill({ json: { id: 'feedback-user', username: 'feedback-user', role, two_factor_enabled: false } });
    if (path === '/api/v1/settings') return route.fulfill({ json: { alerts_available: role === 'admin', menu_speed: 3, email_speed: 2, tts_voice: 'en_US-hfc_male-medium' } });
    if (path === '/api/v1/voices') return route.fulfill({ json: [] });
    if (path === '/api/v1/voices/jobs/active') return route.fulfill({ json: { status: 'idle' } });
    if (path === '/api/v1/whitelist') return route.fulfill({ json: [] });
    if (path === '/api/v1/alert-numbers') return route.fulfill({ json: [] });
    if (path === '/api/v1/admin/alerts') return route.fulfill({ json: { available: true } });
    if (path === '/api/v1/sip') return route.fulfill({ json: { enabled: true, domain: 'sip.example.test', username: 'admin', password_set: true } });
    if (path === '/api/v1/users') return role === 'admin' ? route.fulfill({ json: [{ id: 'feedback-admin', username: 'feedback-admin', role: 'admin', enabled: true }, { id: 'feedback-user', username: 'feedback-user', role: 'user', enabled: true }] }) : route.fulfill({ status: 403, json: { error: 'administrator access required' } });
    return route.fulfill({ json: { status: 'ok', available: false } });
  });
  await page.goto('/#mailboxes');
  await expect(page.locator('#app')).toBeVisible();
}

for (const width of [1536, 390]) {
  test(`opened forms and operation results announce and return focus at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await signedIn(page);
    const add = page.locator('#new-account');
    await add.click();
    await expect(page.locator('#account-form [name="canonical_name"]')).toBeFocused();
    await page.locator('#cancel-account').click();
    await expect(add).toBeFocused();

    await add.click();
    const form = page.locator('#account-form');
    for (const [name, value] of Object.entries({ canonical_name: 'New mailbox', email: 'new@example.test', imap_host: 'imap.example.test', imap_user: 'fixture', imap_password: 'disposable-password', smtp_host: 'smtp.example.test', smtp_user: 'fixture', smtp_password: 'disposable-password' })) await form.locator(`[name="${name}"]`).fill(value);
    await form.locator('button[type="submit"]').click();
    await expect(page.locator('#app-status')).toContainText('Account saved.');
    await expect(page.locator('#app-status')).toBeFocused();
    await expect(page.locator('#app-status')).toHaveAttribute('role', 'status');
    await expect(page.locator('#app-status')).toHaveAttribute('aria-live', 'polite');

    await page.locator('[data-task="caller-access"]').click();
    await page.locator('[data-edit-contact="9"]').click();
    await expect(page.locator('#contact-form [name="name"]')).toBeFocused();
    await page.locator('#cancel-contact').click();
    await expect(page.locator('#contact-form [name="name"]')).toBeFocused();
    await page.locator('[data-edit-contact="9"]').click();
    await page.locator('#contact-form [name="name"]').fill('Updated contact');
    await page.locator('#contact-form button[type="submit"]').click();
    await expect(page.locator('#app-status')).toContainText('Contact saved.');
    await expect(page.locator('#app-status')).toBeFocused();
    await page.locator('#refresh-status').click();
    await expect(page.locator('#refresh-status-announcement')).toBeFocused();
    await expect(page.locator('#refresh-status-announcement')).toBeVisible();
    await expect(page.locator('#refresh-status-announcement')).toContainText('Service status and mailbox sync status refreshed.');
    await expect(page.locator('#contact-form [name="name"]')).toBeEmpty();
  });
}

test('mailbox failures stay beside the form and preserve retryable values', async ({ page }) => {
  await signedIn(page);
  await page.locator('#new-account').click();
  const form = page.locator('#account-form');
  for (const [name, value] of Object.entries({ canonical_name: 'Retry mailbox', email: 'retry@example.test', imap_host: 'imap.example.test', imap_user: 'fixture', imap_password: 'disposable-password', smtp_host: 'smtp.example.test', smtp_user: 'fixture', smtp_password: 'disposable-password' })) await form.locator(`[name="${name}"]`).fill(value);
  await page.route('**/api/v1/accounts/validate', route => route.fulfill({ status: 502, json: { error: 'IMAP login rejected' } }));
  await form.locator('button[type="submit"]').click();
  const feedback = page.locator('#account-form-operation-feedback');
  await expect(feedback).toContainText('Mailbox save or refresh failed: IMAP login rejected');
  await expect(feedback).toBeFocused();
  await expect(page.locator('#app-status')).not.toContainText('IMAP login rejected');
  await expect(form).toBeVisible();
  await expect(form.locator('[name="canonical_name"]')).toHaveValue('Retry mailbox');
});

test('voice settings failures stay beside the affected settings form', async ({ page }) => {
  await signedIn(page);
  await page.route('**/api/v1/settings', async route => {
    if (route.request().method() === 'PUT') return route.fulfill({ status: 503, json: { error: 'Settings service unavailable' } });
    return route.fallback();
  });
  await page.locator('[data-task="voice-alerts"]').click();
  const form = page.locator('#settings-form');
  await form.locator('[name="menu_speed"]').fill('4');
  await form.locator('button[type="submit"]').click();
  const feedback = page.locator('#settings-form-operation-feedback');
  await expect(feedback).toContainText('Voice and alert settings could not be saved: Settings service unavailable');
  await expect(feedback).toBeFocused();
  await expect(form.locator('[name="menu_speed"]')).toHaveValue('4');
  await expect(page.locator('#app-status')).not.toContainText('Settings service unavailable');
});

test('mutation controls suppress duplicate submits and recover after server failures', async ({ page }) => {
  await signedIn(page, 'admin');
  await page.locator('[data-task="caller-access"]').click();
  const form = page.locator('#contact-form');
  await form.locator('[name="name"]').fill('Retryable contact');
  await form.locator('[name="email"]').fill('retryable@example.test');
  let release, entered;
  const held = new Promise(resolve => { release = resolve; });
  const started = new Promise(resolve => { entered = resolve; });
  let submissions = 0;
  await page.route('**/api/v1/contacts', async route => {
    if (route.request().method() !== 'POST') return route.fallback();
    submissions++;
    entered();
    await held;
    await route.fulfill({ status: 503, json: { error: 'Contact storage unavailable' } });
  });
  try {
    await form.locator('button[type="submit"]').click();
    await started;
    await expect(form).toHaveAttribute('aria-busy', 'true');
    await expect(form.locator('button[type="submit"]')).toBeDisabled();
    await form.evaluate(element => element.requestSubmit());
    expect(submissions).toBe(1);
    release();
    await expect(page.locator('#contact-form-operation-feedback')).toContainText('Contact storage unavailable');
    await expect(form).toHaveAttribute('aria-busy', 'false');
    await expect(form.locator('button[type="submit"]')).toBeEnabled();
    await expect(form.locator('[name="name"]')).toHaveValue('Retryable contact');
  } finally {
    release();
  }

  let releaseDelete, deleteEntered;
  const heldDelete = new Promise(resolve => { releaseDelete = resolve; });
  const startedDelete = new Promise(resolve => { deleteEntered = resolve; });
  await page.route('**/api/v1/contacts/9', async route => {
    deleteEntered();
    await heldDelete;
    await route.fulfill({ status: 503, json: { error: 'Contact deletion unavailable' } });
  });
  page.once('dialog', dialog => dialog.accept());
  try {
    const remove = page.locator('[data-delete-contact="9"]');
    await remove.click();
    await startedDelete;
    await expect(page.locator('#contacts')).toHaveAttribute('aria-busy', 'true');
    await expect(remove).toBeDisabled();
    releaseDelete();
    await expect(page.locator('#contacts-operation-feedback')).toContainText('Contact deletion unavailable');
    await expect(page.locator('#contacts')).toHaveAttribute('aria-busy', 'false');
    await expect(page.locator('[data-delete-contact="9"]')).toBeEnabled();
  } finally {
    releaseDelete();
  }
});

test('admin recovers an active voice install after reload, cancels it, and can retry', async ({ page }) => {
  const active = { job_id: 'voice-recovery-job', voice: 'en_US-lessac-low', stage: 'installing', status: 'running', progress: 35 };
  let retryJob = null;
  await signedIn(page, 'admin');
  await page.route('**/api/v1/voices/jobs/active', route => route.fulfill({ json: active }));
  await page.route('**/api/v1/voices/jobs/voice-recovery-job/cancel', async route => {
    active.status = 'failed'; active.stage = 'failed'; active.progress = 100; active.error = 'voice installation canceled';
    await route.fulfill({ status: 202, json: { status: 'cancellation requested' } });
  });
  await page.route('**/api/v1/voices/jobs/voice-recovery-job', route => route.fulfill({ json: active }));
  await page.route('**/api/v1/voices/install', async route => {
    retryJob = { job_id: 'voice-retry-job', voice: 'en_US-lessac-low', stage: 'complete', status: 'complete', progress: 100 };
    await route.fulfill({ status: 202, json: { job_id: retryJob.job_id, status: 'queued' } });
  });
  await page.route('**/api/v1/voices/jobs/voice-retry-job', route => route.fulfill({ json: retryJob }));
  await page.reload();
  await page.locator('[data-task="voice-alerts"]').click();
  await expect(page.locator('#voice-progress')).toBeVisible();
  await expect(page.locator('#voice-progress')).toHaveJSProperty('value', 35);
  await expect(page.locator('#cancel-voice-install')).toBeVisible();
  await expect(page.locator('#cancel-voice-install')).toBeEnabled();
  await page.locator('#cancel-voice-install').click();
  await expect(page.locator('#voice-status')).toContainText('Canceled. You can retry.', { timeout: 4000 });
  await expect(page.locator('#cancel-voice-install')).toBeHidden();
  await expect(page.locator('#install-voice')).toBeEnabled();
  await page.locator('#settings-form [name="tts_voice"]').fill('en_US-lessac-low');
  await page.locator('#install-voice').click();
  await expect(page.locator('#voice-status')).toContainText('Installed. Save this voice to use it.');
  await expect(page.locator('#install-voice')).toBeEnabled();
  expect(retryJob.status).toBe('complete');
});

test('changed synchronization states are announced once, without repeated polling announcements', async ({ page }) => {
  let syncState;
  await signedIn(page);
  await page.locator('#refresh-status').click();
  await expect(page.locator('#sync-freshness')).toContainText('Sync status checked');
  await expect(page.locator('#operational-panel')).toHaveAttribute('aria-busy', 'false');
  syncState = { id: 44, kind: 'manual', started_at: '2026-10-07T09:00:00Z', finished_at: '2026-10-07T09:01:00Z', success: false, state: 'failed', failure: { category: 'authentication', reason: 'The provider rejected authentication.', action: 'Check the saved account credentials.' } };
  await page.route('**/api/v1/accounts', async route => {
    if (route.request().method() !== 'GET') return route.fallback();
    await route.fulfill({ json: [{ id: 'focus-mailbox', canonical_name: 'Focus mailbox', email: 'focus@example.test', imap_host: 'imap.example.test', imap_port: 993, display_order: 0, last_sync: syncState }] });
  });
  await page.locator('#refresh-status').click();
  await expect(page.locator('#sync-change-announcement')).toContainText('Focus mailbox: synchronization failed.');
  await expect(page.locator('[data-sync-status="focus-mailbox"]')).toContainText('The provider rejected authentication.');
  const message = await page.locator('#sync-change-announcement').textContent();
  await page.evaluate(() => {
    window.syncAnnouncementMutations = 0;
    new MutationObserver(records => { window.syncAnnouncementMutations += records.length; })
      .observe(document.querySelector('#sync-change-announcement'), { childList: true, characterData: true, subtree: true });
  });
  await Promise.all([page.waitForResponse('**/api/v1/accounts'), page.locator('#refresh-status').click()]);
  await expect(page.locator('#operational-panel')).toHaveAttribute('aria-busy', 'false');
  await expect(page.locator('#sync-change-announcement')).toHaveText(message);
  expect(await page.evaluate(() => window.syncAnnouncementMutations)).toBe(0);
});

test('a changed SIP failure is exposed through a single polite live region', async ({ page }) => {
  await signedIn(page);
  await page.route('**/api/v1/status', route => route.fulfill({ json: {
    checked_at: '2026-10-07T00:00:01Z', readiness: 'not_ready', reason: 'SIP registration is failed', offline_mode: false,
    runtime: { process: 'running', bridge: 'connected', registration: 'failed', media: 'ready' },
    sip_failure: { category: 'authentication', reason: 'The SIP registrar rejected authentication or access.', action: 'Check the SIP username and password.', diagnostic: 'SIP response 403' },
  } }));
  await page.locator('#refresh-status').click();
  await expect(page.locator('#runtime-registration')).toHaveText('Failed');
  await expect(page.locator('#sip-failure')).toHaveAttribute('role', 'status');
  await expect(page.locator('#sip-failure')).toContainText('SIP response 403');
  await expect(page.locator('#refresh-status-announcement')).toBeFocused();
    await expect(page.locator('#refresh-status-announcement')).toBeVisible();
});
