import { expect, test } from '@playwright/test';

async function previewFixture(page) {
  await page.addInitScript(() => {
    window.Audio = class {
      constructor(src) { this.src = src; window.fixturePreviewAudio = this; }
      async play() { window.fixturePreviewPlaying = true; }
      pause() {}
    };
  });
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname;
    const json = path === '/api/v1/me' ? { id: 'preview-user', username: 'preview-user', role: 'user' }
      : path === '/api/v1/settings' ? { tts_voice: 'en_US-hfc_male-medium', menu_speed: 3, email_speed: 2, alerts_available: false }
      : path === '/api/v1/status' ? { runtime: {}, readiness: 'ready' }
      : [];
    await route.fulfill({ json });
  });
  await page.goto('/#voice-alerts');
  await expect(page.locator('#preview-voice')).toBeVisible();
}

test('voice preview polls its job, fetches owned audio and restores controls after playback', async ({ page }) => {
  await previewFixture(page);
  let complete = false, audioRequests = 0, statusRequests = 0;
  await page.route('**/api/v1/voices/preview', route => route.fulfill({ status: 202, json: { job_id: 'preview-job', status: 'running' } }));
  await page.route('**/api/v1/voices/previews/preview-job', async route => {
    statusRequests++;
    await route.fulfill({ json: { status: complete ? 'complete' : 'running' } });
  });
  await page.route('**/api/v1/voices/previews/preview-job/audio', async route => {
    audioRequests++;
    await route.fulfill({ contentType: 'audio/wav', body: 'controlled audio' });
  });
  await page.locator('#preview-voice').click();
  await expect(page.locator('#voice-status')).toHaveText('Generating preview…');
  await expect(page.locator('#preview-voice')).toBeDisabled();
  expect(audioRequests).toBe(0);
  complete = true;
  await expect(page.locator('#voice-status')).toHaveText('Playing preview.');
  expect(statusRequests).toBeGreaterThan(0);
  expect(audioRequests).toBe(1);
  expect(await page.evaluate(() => window.fixturePreviewPlaying)).toBe(true);
  await page.evaluate(() => window.fixturePreviewAudio.onended());
  await expect(page.locator('#preview-voice')).toBeEnabled();
  await expect(page.locator('#voice-announcement')).toHaveText('Voice preview finished.');
});

test('preview job failure remains beside the form and permits retry', async ({ page }) => {
  await previewFixture(page);
  await page.route('**/api/v1/voices/preview', route => route.fulfill({ status: 202, json: { job_id: 'failed-preview' } }));
  await page.route('**/api/v1/voices/previews/failed-preview', route => route.fulfill({ json: { status: 'failed', failure: { category: 'timeout', reason: 'The operation timed out.' } } }));
  await page.locator('#preview-voice').click();
  await expect(page.locator('#settings-form-operation-feedback')).toContainText('Voice preview failed: The operation timed out.');
  await expect(page.locator('#preview-voice')).toBeEnabled();
  expect(await page.evaluate(() => !!window.fixturePreviewPlaying)).toBe(false);
});
