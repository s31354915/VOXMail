import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

const wcagTags = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'];

async function audit(page, state) {
  const results = await new AxeBuilder({ page }).withTags(wcagTags).analyze();
  return results.violations.map(violation => {
    const nodes = violation.nodes.map(node => `${node.target.join(', ')}: ${node.failureSummary}`).join('\n');
    return `${state}: ${violation.id} (${violation.impact}): ${violation.help}\n${nodes}`;
  });
}

async function signedOut(page, setupAvailable) {
  await page.route('**/api/v1/me', route => route.fulfill({ status: 401, json: { error: 'authentication required' } }));
  await page.route('**/api/v1', route => route.fulfill({ json: { setup_available: setupAvailable, offline_mode: false } }));
  await page.goto('/');
  await expect(page.locator(setupAvailable ? '#setup' : '#login')).toBeVisible();
}

async function signedIn(page, role) {
  await page.route('**/api/v1', route => route.fulfill({ json: { setup_available: false, offline_mode: false } }));
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/api/v1/me') return route.fulfill({ json: { id: 'a11y-user', username: 'a11y-user', role, two_factor_enabled: false } });
    if (path === '/api/v1/settings') return route.fulfill({ json: { alerts_available: true, menu_speed: 3, email_speed: 2, tts_voice: 'en_US-hfc_male-medium' } });
    if (path === '/api/v1/status') return route.fulfill({ json: { checked_at: '2026-10-07T00:00:00Z', readiness: 'ready', runtime: { process: 'running', bridge: 'connected', registration: 'registered', media: 'ready' } } });
    if (path === '/api/v1/sip') return route.fulfill({ json: { enabled: true, domain: 'sip.example.test', username: 'a11y-user', password_set: true } });
    if (path === '/api/v1/admin/alerts') return route.fulfill({ json: { available: true } });
    if (path === '/api/v1/voices/jobs/active') return route.fulfill({ json: { status: 'idle' } });
    if (path === '/api/v1/users' && role !== 'admin') return route.fulfill({ status: 403, json: { error: 'administrator access required' } });
    if (path === '/api/v1/alert-numbers' || path === '/api/v1/voices' || path === '/api/v1/accounts' || path === '/api/v1/contacts' || path === '/api/v1/whitelist' || path === '/api/v1/users') return route.fulfill({ json: [] });
    return route.fulfill({ json: { enabled: false, status: 'ok' } });
  });
  await page.goto('/#mailboxes');
  await expect(page.locator('#app')).toBeVisible();
}

for (const width of [1536, 390]) {
  test(`WCAG accessibility for setup and recovery views at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const violations = [];
    await signedOut(page, true);
    violations.push(...await audit(page, `setup at ${width}px`));
    await page.screenshot({ path: test.info().outputPath(`setup-${width}.png`), fullPage: true });

    await page.unroute('**/api/v1');
    await page.route('**/api/v1', route => route.fulfill({ json: { setup_available: false, offline_mode: false } }));
    await page.reload();
    await expect(page.locator('#login')).toBeVisible();
    violations.push(...await audit(page, `login at ${width}px`));

    await page.route('**/api/v1/recovery/request', route => route.fulfill({ json: { status: 'accepted' } }));
    await page.getByRole('button', { name: 'Reset password', exact: true }).click();
    violations.push(...await audit(page, `password recovery request at ${width}px`));
    await page.locator('#recovery-request [name="email"]').fill('a11y@example.test');
    await page.getByRole('button', { name: 'Send recovery code', exact: true }).click();
    await expect(page.locator('#recovery-confirm')).toBeVisible();
    violations.push(...await audit(page, `password recovery confirmation at ${width}px`));
    expect(violations.join('\n\n'), `Accessibility violations at ${width}px`).toBe('');
  });

  test(`WCAG accessibility for user and administrator console tasks at ${width}px`, async ({ page }) => {
    test.setTimeout(180_000);
    await page.setViewportSize({ width, height: 1024 });
    const violations = [];
    await signedIn(page, 'user');
    for (const task of ['mailboxes', 'caller-access', 'voice-alerts', 'security']) {
      await page.locator(`[data-task="${task}"]`).click();
      violations.push(...await audit(page, `user ${task} at ${width}px`));
    }

    const adminPage = await page.context().newPage();
    await adminPage.setViewportSize({ width, height: 1024 });
    await signedIn(adminPage, 'admin');
    for (const task of ['mailboxes', 'caller-access', 'voice-alerts', 'security', 'administration']) {
      await adminPage.locator(`[data-task="${task}"]`).click();
      violations.push(...await audit(adminPage, `admin ${task} at ${width}px`));
    }
    expect(await adminPage.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await adminPage.screenshot({ path: test.info().outputPath(`admin-console-${width}.png`), fullPage: true });
    await adminPage.close();
    expect(violations.join('\n\n'), `Accessibility violations at ${width}px`).toBe('');
  });
}
