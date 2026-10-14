import { expect, test } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';

async function fixture(page, initial = {}) {
  let version = 7;
  let policy = initial.policy || { groups: [], excluded: [] };
  let mapping = { INBOX: 'Inbox', '[Gmail]/Inbox': 'GmailInbox', Drafts: 'Drafts', Sent: 'Sent', Trash: 'Trash' };
  mapping = initial.mapping || mapping;
  let roles = initial.roles || { inbox: 'INBOX', drafts: 'Drafts', sent: 'Sent', trash: 'Trash' };
  const folders = initial.folders || { ...mapping, Projects: 'Projects', '[Gmail]/All Mail': '[Gmail]/All Mail' };
  const submissions = [];
  const row = () => ({ id: 'mail', canonical_name: 'Personal mailbox', email: 'owner@example.test', imap_host: 'imap.example.test', imap_port: 993, config_version: version, folder_map: mapping, folder_roles: roles });
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname, method = route.request().method();
    if (path === '/api/v1/accounts/mail/folder-policy') {
      if (method === 'GET') return route.fulfill({ json: { config_version: version, policy, folders, folder_map: mapping, folder_roles: roles, mailboxes: [], folder_bindings: initial.bindings || {} } });
      const body = route.request().postDataJSON();
      submissions.push(body);
      if (body.config_version !== version) return route.fulfill({ status: 409, json: { error: 'account settings changed; reload before saving folder choices' } });
      policy = body.policy; mapping = body.folder_map; roles = body.folder_roles; version++;
      return route.fulfill({ json: { status: 'saved', config_version: version } });
    }
    if (path === '/api/v1/accounts') return route.fulfill({ json: [row()] });
    if (path === '/api/v1/me') return route.fulfill({ json: { id: 'owner', username: 'owner', role: 'user' } });
    if (path === '/api/v1/settings') return route.fulfill({ json: { alerts_available: false, tts_voice: 'en_US-hfc_male-medium', menu_speed: 3, email_speed: 2 } });
    if (path === '/api/v1/status') return route.fulfill({ json: { readiness: 'ready', runtime: {} } });
    if (path === '/api/v1/voices/jobs/active') return route.fulfill({ json: { status: 'idle' } });
    return route.fulfill({ json: [] });
  });
  await page.goto('/#mailboxes');
  await page.locator('[data-folders-account="mail"]').click();
  await expect(page.locator('#folder-editor')).toBeVisible();
  return { submissions };
}

test('preview matches local aliases, fixed legacy roles, nested keys and every more page', async ({ page }) => {
  const mapping = { INBOX: 'InboxCustom', Drafts: 'DraftCache', Sent: 'Archive/SentCache' };
  const folders = { ...mapping, 'Projects/2026': 'Projects/2026', 'Projects/2027': 'Projects/2027', A: 'A', B: 'B', C: 'C', D: 'D', E: 'E', F: 'F' };
  await fixture(page, { mapping, folders, roles: {} });
  const preview = page.locator('#folder-preview');
  const rootRows = preview.locator(':scope > .mailbox');
  await expect(rootRows).toHaveCount(8);
  for (const [key, folder] of [[1, 'InboxCustom'], [2, 'DraftCache'], [3, 'Archive/SentCache'], [4, ''], [5, 'A'], [6, 'Archive'], [7, 'B'], [8, 'C']]) {
    await expect(preview.locator(`:scope > [data-preview-key="${key}"]`)).toHaveAttribute('data-preview-folder', folder);
  }
  const more = preview.locator(':scope > [data-preview-more=""]');
  await more.locator(':scope > summary').click();
  await expect(more.locator(':scope > [data-preview-key="1"]')).toHaveAttribute('data-preview-folder', 'D');
  const nested = more.locator('[data-preview-nested="Projects"]');
  await nested.locator(':scope > summary').click();
  await expect(nested.locator('[data-preview-key="1"]')).toHaveAttribute('data-preview-folder', 'Projects/2026');
  await expect(nested.locator('[data-preview-key="2"]')).toHaveAttribute('data-preview-folder', 'Projects/2027');
  const editor = page.locator('#folder-editor');
  const alias = editor.locator('details.local-alias').filter({ has: page.locator('[data-folder-alias="Drafts"]') });
  await alias.locator('summary').click();
  await editor.locator('[data-folder-alias="Drafts"]').fill('ChangedDrafts');
  await editor.locator('[data-folder-alias="Drafts"]').press('Tab');
  await expect(preview.locator(':scope > [data-preview-key="2"]')).toHaveAttribute('data-preview-folder', 'ChangedDrafts');
  await expect(editor.getByText('Preview after the next successful sync.', { exact: false })).toBeVisible();
  const audit = await new AxeBuilder({ page }).include('#folder-editor').analyze();
  expect(audit.violations).toEqual([]);
});

test('preview receives retained source bindings without listing old caches twice', async ({ page }) => {
  await fixture(page, { mapping: { Alias: 'NewAlias' }, folders: { Alias: 'NewAlias' }, roles: {}, bindings: { Drafts: 'Alias', NewAlias: 'Alias' } });
  const preview = page.locator('#folder-preview');
  await expect(preview.locator(':scope > .mailbox')).toHaveCount(4);
  await expect(preview.locator(':scope > [data-preview-key="2"]')).toHaveAttribute('data-preview-folder', 'NewAlias');
  await expect(preview.locator('[data-preview-folder="Drafts"]')).toHaveCount(0);
});

test('provider names matching object properties remain selectable and map correctly', async ({ page }) => {
  const folders = JSON.parse('{"__proto__":"__proto__","constructor":"constructor"}');
  const { submissions } = await fixture(page, { mapping: {}, folders, roles: {} });
  const editor = page.locator('#folder-editor');
  const alias = editor.locator('details.local-alias').filter({ has: page.locator('[data-folder-alias="__proto__"]') });
  await alias.locator('summary').click();
  await expect(editor.locator('[data-folder-alias="__proto__"]')).toHaveValue('__proto__');
  await editor.locator('[data-folder-role="__proto__"]').selectOption('drafts');
  await editor.getByRole('button', { name: 'Save folder choices' }).click();
  await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
  expect(submissions[0].folder_map).toEqual(JSON.parse('{"__proto__":"__proto__"}'));
  expect(submissions[0].folder_roles).toEqual({ drafts: '__proto__' });
});

test('reserved INBOX casing preserves saved groups, primary, roles and mapping keys', async ({ page }) => {
  const { submissions } = await fixture(page, {
    mapping: { INBOX: 'FutureInbox', Alias: 'Alias', Sent: 'UpperSent', sent: 'LowerSent' },
    folders: { Inbox: 'ObservedInbox', Alias: 'Alias', Sent: 'UpperSent', sent: 'LowerSent' },
    roles: { inbox: 'INBOX' },
    policy: { groups: [{ id: 'pair', name: 'My inbox', primary: 'INBOX', folders: ['inbox', 'Alias'] }], excluded: [] },
  });
  const editor = page.locator('#folder-editor');
  const inboxRow = editor.locator('.folder-row').filter({ has: page.locator('[data-folder-select="Inbox"]') });
  await expect(inboxRow).toContainText('Group: My inbox');
  await expect(editor.locator('[data-folder-role="Inbox"]')).toHaveValue('inbox');
  await editor.locator('#folder-group-choice').selectOption('pair');
  await expect(editor.locator('[data-folder-primary="inbox"]')).toBeChecked();
  await editor.locator('[data-folder-select="Inbox"]').check();
  await editor.getByRole('button', { name: 'Add selected members' }).click();
  await expect(editor.locator('input[name=folder-primary]')).toHaveCount(2);
  await expect(editor.locator('[data-folder-primary="inbox"]')).toBeChecked();
  const alias = editor.locator('details.local-alias').filter({ has: page.locator('[data-folder-alias="Inbox"]') });
  await alias.locator('summary').click();
  await expect(editor.locator('[data-folder-alias="Inbox"]')).toHaveValue('FutureInbox');
  await editor.locator('[data-folder-alias="Inbox"]').fill('ChangedInbox');
  await editor.locator('[data-folder-alias="Inbox"]').press('Tab');
  await editor.getByRole('button', { name: 'Save folder choices' }).click();
  await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
  expect(submissions[0].folder_map).toEqual({ INBOX: 'ChangedInbox', Alias: 'Alias', Sent: 'UpperSent', sent: 'LowerSent' });
  expect(submissions[0].policy.groups[0].folders).toEqual(['inbox', 'Alias']);
  expect(submissions[0].policy.groups[0].primary).toBe('INBOX');
});

for (const width of [1536, 390]) {
  test(`explicit grouping, primary, exclusions and persisted roles at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    const { submissions } = await fixture(page);
    const editor = page.locator('#folder-editor');
    await expect(editor.locator('input[type=checkbox]:checked')).toHaveCount(0);
    await expect(editor.locator('.folder-row')).toHaveCount(7);
    await expect(editor.locator('#folder-editor-title')).toBeFocused();
    for (const remote of ['INBOX', '[Gmail]/Inbox']) await editor.locator(`[data-folder-select="${remote}"]`).check();
    await editor.getByRole('button', { name: 'Combine selected', exact: true }).click();
    await expect(editor.locator('input[name=folder-primary]:checked')).toHaveCount(0);
    await editor.getByRole('button', { name: 'Save folder choices' }).click();
    await expect(editor.locator('#folder-editor-status')).toContainText('explicitly chosen primary');
    expect(submissions).toHaveLength(0);
    await editor.locator('#folder-group-name').fill('My inbox');
    await editor.locator('[data-folder-primary="[Gmail]/Inbox"]').check();
    await editor.locator('[data-folder-select="[Gmail]/All Mail"]').check();
    await editor.getByRole('button', { name: 'Exclude from browsing' }).click();
    await editor.getByRole('button', { name: 'Save folder choices' }).click();
    await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
    expect(submissions).toHaveLength(1);
    const audit = await new AxeBuilder({ page }).include('#folder-editor').analyze();
    expect(audit.violations).toEqual([]);
    await editor.screenshot({ path: `/tmp/voxmail-folder-editor-${width}.png` });
    expect(submissions[0]).toMatchObject({ config_version: 7, folder_map: { INBOX: 'Inbox' }, folder_roles: { inbox: 'INBOX' }, policy: { groups: [{ name: 'My inbox', primary: '[Gmail]/Inbox', folders: ['INBOX', '[Gmail]/Inbox'] }], excluded: ['[Gmail]/All Mail'] } });
    await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
    await expect(page.locator('[data-folders-account="mail"]')).toBeFocused();
    await page.locator('[data-folders-account="mail"]').click();
    await editor.locator('#folder-group-choice').selectOption(submissions[0].policy.groups[0].id);
    await expect(editor.locator('#folder-group-name')).toHaveValue('My inbox');
    await expect(editor.locator('[data-folder-primary="[Gmail]/Inbox"]')).toBeChecked();
    await editor.getByRole('button', { name: 'Keep all members separate' }).click();
    await editor.locator('[data-folder-select="[Gmail]/All Mail"]').check();
    await editor.getByRole('button', { name: 'Show selected' }).click();
    await editor.getByRole('button', { name: 'Save folder choices' }).click();
    await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
    expect(submissions[1]).toMatchObject({ config_version: 8, policy: { groups: [], excluded: [] } });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test('failed save retains choices and suppresses pending duplicate saves', async ({ page }) => {
  await fixture(page);
  const editor = page.locator('#folder-editor');
  await editor.locator('.folder-row').filter({ has: page.locator('[data-folder-alias="INBOX"]') }).locator('summary').click();
  await editor.locator('[data-folder-alias="INBOX"]').fill('ChosenLocalInbox');
  let release;
  let calls = 0;
  await page.route('**/api/v1/accounts/mail/folder-policy', async route => {
    if (route.request().method() !== 'PUT') return route.fallback();
    calls++;
    await new Promise(resolve => { release = resolve; });
    await route.fulfill({ status: 409, json: { error: 'account settings changed; reload before saving folder choices' } });
  });
  await editor.getByRole('button', { name: 'Save folder choices' }).click();
  await expect(editor).toHaveAttribute('aria-busy', 'true');
  await expect(editor.getByRole('button', { name: 'Save folder choices' })).toBeDisabled();
  await expect.poll(() => calls).toBe(1);
  release();
  await expect(editor.locator('#folder-editor-status')).toContainText('reload before saving');
  await expect(editor.locator('#folder-editor-status')).toBeFocused();
  await expect(editor.locator('[data-folder-alias="INBOX"]')).toHaveValue('ChosenLocalInbox');
  await expect(editor.getByRole('button', { name: 'Save folder choices' })).toBeEnabled();
});


test('folder selection and primary choices work with the keyboard', async ({ page }) => {
  const { submissions } = await fixture(page);
  const editor = page.locator('#folder-editor');
  for (const remote of ['INBOX', '[Gmail]/Inbox']) {
    await editor.locator(`[data-folder-select="${remote}"]`).focus();
    await page.keyboard.press('Space');
  }
  await editor.getByRole('button', { name: 'Combine selected', exact: true }).focus();
  await page.keyboard.press('Enter');
  await expect(editor.locator('#folder-group-name')).toBeFocused();
  await page.keyboard.type('Keyboard inbox');
  await page.keyboard.press('Tab');
  await page.keyboard.press('Space');
  await expect(editor.locator('input[name=folder-primary]:checked')).toHaveCount(1);
  await editor.getByRole('button', { name: 'Save folder choices' }).focus();
  await page.keyboard.press('Enter');
  await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
  expect(submissions[0].policy.groups[0]).toMatchObject({ name: 'Keyboard inbox', primary: 'INBOX' });
});

test('mapping and ignoring folders use dropdowns and survive reopening', async ({ page }) => {
  const { submissions } = await fixture(page);
  const editor = page.locator('#folder-editor');
  await expect(editor.locator('[data-folder-mapping="Projects"] option[value="Inbox"]')).toHaveJSProperty('disabled', true);
  await editor.locator('[data-folder-mapping="Projects"]').selectOption('Archive');
  await editor.locator('[data-folder-visibility="[Gmail]/All Mail"]').selectOption('ignore');
  await expect(editor.locator('#folder-preview')).not.toContainText('[Gmail]/All Mail');
  await editor.getByRole('button', { name: 'Save folder choices' }).click();
  await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
  expect(submissions[0].folder_map.Projects).toBe('Archive');
  expect(submissions[0].policy.excluded).toEqual(['[Gmail]/All Mail']);
  await editor.getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.locator('[data-folders-account="mail"]').click();
  await expect(editor.locator('[data-folder-mapping="Projects"]')).toHaveValue('Archive');
  await expect(editor.locator('[data-folder-visibility="[Gmail]/All Mail"]')).toHaveValue('ignore');
  await editor.locator('[data-folder-visibility="[Gmail]/All Mail"]').selectOption('show');
  await editor.getByRole('button', { name: 'Save folder choices' }).click();
  await expect(editor.locator('#folder-editor-status')).toHaveText('Folder choices saved.');
  expect(submissions[1].policy.excluded).toEqual([]);
});
