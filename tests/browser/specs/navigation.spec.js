import { expect, test } from '@playwright/test';

async function consoleFixture(page, role, task = 'mailboxes') {
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const value = path === '/api/v1/me' ? { id: 'navigation-user', username: 'navigation-user', role, two_factor_enabled: false }
      : path === '/api/v1/settings' ? { menu_speed: 3, email_speed: 2, alerts_available: false }
      : path === '/api/v1/admin/alerts' ? { available: false }
      : path === '/api/v1/sip' ? { enabled: false } : [];
    await route.fulfill({ status: path === '/api/v1/users' && role !== 'admin' ? 403 : 200, json: value });
  });
  await page.goto('/#' + task);
  await expect(page.locator('#app')).toBeVisible();
  await expect(page.locator('#twofa-state')).toContainText('not enabled');
}

for (const width of [1536, 390]) {
  for (const role of ['admin', 'user']) {
    test(`${role} task navigation, history, draft preservation and keyboard access at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1024 });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await consoleFixture(page, role);
      const tasks = ['mailboxes', 'caller-access', 'voice-alerts', 'security', ...(role === 'admin' ? ['administration'] : [])];
      await expect(page.locator('.task-nav a:visible')).toHaveCount(tasks.length);
      await page.locator('#new-account').click();
      await page.locator('#account-form [name="canonical_name"]').fill('Unsaved mailbox draft');
      for (const task of tasks) {
        const link = page.locator(`[data-task="${task}"]`);
        await link.focus();
        await page.keyboard.press('Enter');
        await expect(link).toHaveAttribute('aria-current', 'page');
        await expect(page.locator('#task-title')).toBeFocused();
        await expect(page.locator('[data-route]:visible')).toHaveCount(1);
        await expect(page.locator(`[data-route="${task}"]`)).toBeVisible();
        await expect(page).toHaveURL(new RegExp(`#${task}$`));
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        const hiddenFocusable = await page.locator('[data-route] input,[data-route] button').evaluateAll(elements => elements.filter(el => el.closest('.route-panel.hidden') && el.offsetParent !== null).length);
        expect(hiddenFocusable).toBe(0);
      }
      await page.locator('[data-task="mailboxes"]').click();
      await expect(page.locator('#account-form [name="canonical_name"]')).toHaveValue('Unsaved mailbox draft');
      await page.locator('[data-task="security"]').click();
      await page.reload();
      await expect(page.locator('[data-task="security"]')).toHaveAttribute('aria-current', 'page');
      await expect(page.locator('#security-panel')).toBeVisible();
      await page.locator('[data-task="caller-access"]').click();
      await page.goBack();
      await expect(page.locator('[data-task="security"]')).toHaveAttribute('aria-current', 'page');
      await page.goForward();
      await expect(page.locator('[data-task="caller-access"]')).toHaveAttribute('aria-current', 'page');
      await page.locator('.skip-link').focus();
      await page.keyboard.press('Enter');
      await expect(page.locator('#console-content')).toBeFocused();
      await expect(page.locator('[data-task="caller-access"]')).toHaveAttribute('aria-current', 'page');
      expect(errors).toEqual([]);
    });
  }
}

test('a regular user cannot select administration through a direct or changed hash', async ({ page }) => {
  await consoleFixture(page, 'user', 'administration');
  await expect(page).toHaveURL(/#mailboxes$/);
  await expect(page.locator('#admin-navigation')).toBeHidden();
  await expect(page.locator('[data-route="administration"]')).toBeHidden();
  await page.evaluate(() => { location.hash = 'administration'; });
  await expect(page.locator('[data-task="mailboxes"]')).toHaveAttribute('aria-current', 'page');
  await expect(page).toHaveURL(/#mailboxes$/);
  await page.evaluate(() => { location.hash = 'unknown-task'; });
  await expect(page).toHaveURL(/#mailboxes$/);
  await expect(page.locator('[data-route]:visible')).toHaveCount(1);
});
