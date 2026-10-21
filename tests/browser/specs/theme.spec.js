import { expect, test } from '@playwright/test';

// The console stylesheet is chosen from VOXMAIL_THEME at serve time: the
// default look when unset, console-<theme>.css when set. The fixture server
// runs one theme per suite, so this asserts the active one end to end.
test('active console theme matches VOXMAIL_THEME', async ({ page }) => {
  await page.goto('/');
  await page.locator('#login').waitFor({ state: 'visible' });
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const expected = process.env.VOXMAIL_THEME === 'dark' ? 'rgb(14, 16, 20)' : 'rgb(250, 248, 243)';
  expect(bg).toBe(expected);
});
