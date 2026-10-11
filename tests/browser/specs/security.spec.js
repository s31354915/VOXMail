import { expect, test } from '@playwright/test';

async function signedIn(page) {
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const json = path === '/api/v1/me' ? { id: 'security-user', username: 'security-user', role: 'user', two_factor_enabled: false }
      : path === '/api/v1/settings' ? { alerts_available: false, menu_speed: 3, email_speed: 2 } : [];
    await route.fulfill({ status: path === '/api/v1/users' ? 403 : 200, json });
  });
  await page.goto('/#security');
  await expect(page.locator('#twofa-state')).toContainText('not enabled');
}

for (const width of [1536, 390]) {
  test(`security labels, descriptions, byte/PIN validation and retries at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1024 });
    await signedIn(page);
    for (const id of ['password-form', 'pin-form', 'twofa-form']) {
      const form = page.locator('#' + id);
      for (const input of await form.locator('input[name]').all()) {
        expect(await input.evaluate(el => el.labels.length)).toBeGreaterThan(0);
        const descriptions = (await input.getAttribute('aria-describedby')).split(/\s+/);
        for (const description of descriptions) expect(await page.locator('#' + description).count()).toBe(1);
      }
      await expect(form.locator('[name="current_password"]')).toHaveAttribute('autocomplete', 'current-password');
    }
    const password = page.locator('#password-form');
    await expect(password.locator('[name="new_password"]')).toHaveAttribute('autocomplete', 'new-password');
    await password.locator('[name="current_password"]').fill('CurrentPassword-123');
    await password.locator('[name="new_password"]').fill('é'.repeat(40));
    await password.locator('[name="confirm_password"]').fill('é'.repeat(40));
    await password.locator('button[type="submit"]').click();
    await expect(password.locator('[name="new_password"]')).toBeFocused();
    await expect(page.locator('#password-form-new_password-error')).toContainText('UTF-8 bytes');
    await expect(password.locator('[name="new_password"]')).toHaveAttribute('aria-invalid', 'true');
    await password.locator('[name="new_password"]').fill('ValidNewPassword-123');
    await expect(page.locator('#password-form-new_password-error')).toBeEmpty();
    await password.locator('[name="confirm_password"]').fill('WrongConfirmation-123');
    await password.locator('button[type="submit"]').click();
    await expect(password.locator('[name="confirm_password"]')).toBeFocused();
    await expect(page.locator('#password-form-confirm_password-error')).toContainText('do not match');
    await password.locator('[name="confirm_password"]').fill('ValidNewPassword-123');
    let attempts = 0;
    await page.route('**/api/v1/security/password', async route => {
      const body = route.request().postDataJSON();
      expect(body).toEqual({ current_password: 'CurrentPassword-123', new_password: 'ValidNewPassword-123' });
      await route.fulfill(++attempts === 1
        ? { status: 400, json: { error: 'current password is incorrect', fields: { current_password: 'current password is incorrect' } } }
        : { json: { status: 'changed' } });
    });
    await password.locator('button[type="submit"]').click();
    await expect(password.locator('[name="current_password"]')).toBeFocused();
    await expect(page.locator('#password-form-current_password-error')).toContainText('incorrect');
    await expect(password.locator('[name="new_password"]')).toHaveValue('ValidNewPassword-123');
    await password.locator('button[type="submit"]').click();
    await expect(page.locator('#password-status')).toContainText('Password changed');
    await expect(page.locator('#password-status')).toBeFocused();
    await expect(password.locator('[name="new_password"]')).toHaveValue('');
    await expect(page.locator('#app')).toBeVisible();
    const pin = page.locator('#pin-form');
    await pin.locator('[name="current_password"]').fill('CurrentPassword-123');
    await pin.locator('[name="new_pin"]').fill('12ab');
    await pin.locator('[name="confirm_pin"]').fill('12ab');
    await pin.locator('button[type="submit"]').click();
    await expect(pin.locator('[name="new_pin"]')).toBeFocused();
    await expect(page.locator('#pin-form-new_pin-error')).toContainText('4–12 digits');
    await pin.locator('[name="new_pin"]').fill('2468');
    await pin.locator('[name="confirm_pin"]').fill('1357');
    await pin.locator('button[type="submit"]').click();
    await expect(pin.locator('[name="confirm_pin"]')).toBeFocused();
    await expect(page.locator('#pin-form-confirm_pin-error')).toContainText('do not match');
    await page.locator('#pin-cancel').click();
    await expect(pin.locator('[name="current_password"]')).toBeFocused();
    await expect(pin.locator('[name="new_pin"]')).toHaveValue('');
    await expect(page.locator('#pin-form-confirm_pin-error')).toBeEmpty();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test('authenticator API field errors identify the editable field and clear on retry', async ({ page }) => {
  await signedIn(page);
  await page.route('**/api/v1/security/2fa/setup', route => route.fulfill({ status: 400, json: { error: 'current password is incorrect', fields: { current_password: 'current password is incorrect' } } }));
  await page.locator('#twofa-form [name="current_password"]').fill('WrongPassword-123');
  await page.locator('#twofa-setup').click();
  await expect(page.locator('#twofa-form [name="current_password"]')).toBeFocused();
  await expect(page.locator('#twofa-form-current_password-error')).toContainText('incorrect');
  await page.locator('#twofa-form [name="current_password"]').fill('CorrectPassword-123');
  await expect(page.locator('#twofa-form-current_password-error')).toBeEmpty();
  await expect(page.locator('#twofa-form [name="current_password"]')).not.toHaveAttribute('aria-invalid');
});

test('pending security submission is sent once and an expired session clears errors and input', async ({ page }) => {
  await signedIn(page);
  let release, started;
  const pending = new Promise(resolve => { release = resolve; });
  const entered = new Promise(resolve => { started = resolve; });
  let requests = 0;
  await page.route('**/api/v1/security/pin', async route => { requests++; started(); await pending; await route.fulfill({ status: 401, json: { error: 'authentication required' } }); });
  const form = page.locator('#pin-form');
  await form.locator('[name="current_password"]').fill('CurrentPassword-123');
  await form.locator('[name="new_pin"]').fill('2468');
  await form.locator('[name="confirm_pin"]').fill('2468');
  await form.locator('button[type="submit"]').click();
  await entered;
  await expect(form).toHaveAttribute('aria-busy', 'true');
  await expect(form.locator('button[type="submit"]')).toBeDisabled();
  await form.evaluate(el => el.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
  expect(requests).toBe(1);
  release();
  await expect(page.locator('#login')).toBeVisible();
  await expect(form.locator('[name="current_password"]')).toHaveValue('');
  await expect(form.locator('[name="new_pin"]')).toHaveValue('');
  await expect(form.locator('[name="confirm_pin"]')).toHaveValue('');
  await expect(page.locator('#pin-status')).toBeEmpty();
  await expect(form).toHaveAttribute('aria-busy', 'false');
});
