# Web console inventory (pre-redesign baseline)

Purpose: a complete map of what the current web console does, so the
UX/WebUI-overhaul redesign loses no functionality. Written Oct 21, 2026
against main 3dd6998. Every item below must exist in the redesigned UI.

## Architecture

- Single-page app, no framework: `internal/web/static/index.html` is the
  only page (served by `GET /`); styles from `internal/web/console/console.css`, or from `internal/web/console/console-<theme>.css` when `VOXMAIL_THEME=<theme>` (themes are drop-in stylesheets, e.g. `console-dark.css`; unknown or malformed names fall back to the default stylesheet)
  (~2,150 lines) are injected via the `{{CONSOLE_STYLES}}` placeholder;
  `internal/web/console/src/*.js` (14 ES modules) are bundled by
  `internal/web/console/build.mjs` into `static/console.bundle.js`.
- All data flows through a JSON API under `/api/v1` with session cookies;
  every state-changing request must carry the CSRF token in
  `X-CSRF-Token` (exposed by `GET /api/v1/me` for an authenticated session).
- Static assets served from `/assets/{name}` (brand images, icons).
- `body[data-setup-available]` and the `#auth` section's `data-state`
  drive which auth view shows; `#app` is hidden until signed in.

## Auth surface (before sign-in)

Views controlled by `authentication.js` (`setAuthMode`, `openRecovery`,
`confirmRecovery`, `renderAuthState`):

- Loading state ("checking whether this installation needs setup…").
- First-run setup form (`#setup`): username, password (12–72 bytes),
  telephone PIN (4–12 digits). Shown only when setup is available.
- Login form (`#login`): username, password, optional authenticator
  (TOTP) code and optional backup code.
- Recovery tools (`#recovery-tools`): "Reset password" and "Bypass TOTP"
  buttons; `#recovery-back` returns to sign-in.
- Recovery request form (`#recovery-request`): mailbox address; sends a
  code to the configured mailbox (10-minute expiry).
- Recovery confirm form (`#recovery-confirm`): mailbox address (readonly),
  6-digit code, new password.

Redesign note: the user asked for the login page to show options instead
of three always-visible forms. The mode logic (`setAuthMode`) is the
single place that toggles these; a tabbed/option-based flow must keep
all six states above reachable, including first-run setup detection and
both recovery purposes (`reset_password`, and the bypass-TOTP path).

## Post-signin shell

- Topbar: console wordmark (swaps to an admin wordmark on the
  administration task), `#who` pill (username/role), Sign out.
- Skip link to `#console-content`; task nav with hash routes:
  `#mailboxes`, `#caller-access`, `#voice-alerts`, `#security`,
  `#administration` (admin-only; forced back to mailboxes otherwise).
- Operational panel (always visible): service status card with
  Application / SIP registration / Call process / Call bridge / Speech
  media facts, manual "Refresh status" button, and background polling
  (`operations.js`: `refreshOperationalStatus`, `scheduleOperationalPoll`,
  SIP failure notice in `#sip-failure`).
- Live-region conventions: `role=status` aria-live blocks for auth,
  app status, sync announcements, voice announcements; a screen-reader
  refresh announcement. Keep all of these accessible in the redesign.

## Tasks and their functionality

### Mailboxes (`mailboxes.js`, `folder-editor.js`, `folder-preview.js`)

- Account form (`#account-form`): account name, email, sender name,
  display order; IMAP host/port/security (implicit TLS or STARTTLS)/
  user/password ("blank keeps existing"); SMTP host/port/security/user/
  password; sync interval (min), reconciliation interval (min), initial
  cutoff (RFC3339, optional), retention days (optional), per-account
  "Alert on this account" checkbox; hidden `config_version` for
  optimistic concurrency and hidden `folder_map`.
- Accounts list (`#accounts`): per-account edit (fills the form),
  reorder up/down (`PUT /api/v1/accounts/order`), manual sync +
  cancel-sync, folder choices (opens the folder editor), delete;
  sync freshness line and change announcements; per-account sync state
  rendering (`syncDetails`) including failures.
- Folder editor (`#folder-editor`): per-account folder mapping
  (`remote=local` pairs), folder roles, duplicate combining, folder
  discovery/preview (`folder-preview.js`); save flow calls back into
  `mailboxes.js` after save.
- Add account (`#new-account`) resets the form.

### Caller access (`caller-access.js`)

- Contacts: create/edit/delete with name, email, display order;
  reorder up/down; list rendering (`renderContacts`).
- Caller whitelist: add phone or SIP URI, list, delete.

### Voice & alerts (`voices.js`)

- Voice settings form (`#settings-form`): voice model name with
  datalist of installed voices, menu speed (1–5), email speed (1–5),
  enable call alerts checkbox, legacy/default alert phone.
- Piper voice install (admin-only button): install selected voice,
  cancel installation, progress bar, status line; the active install
  job is resumed on reload (`resumeActiveVoiceInstall`, admin only).
- Voice preview: preview button starts a preview job; audio playback
  via `#previewVoiceAudio`; job polling and cancel.
- Always-warm voices (admin-only section): list of pinned voices with
  keep-warm toggles (`PUT /api/v1/voices/warm/{voice}`), status line.
- Alert numbers: add number (with active checkbox), activate
  (`PUT /api/v1/alert-numbers/{id}/active`), delete.
- Alert availability (`applyAlertAvailability`): when global alerts are
  disabled, `.alert-control` and `#alert-controls` elements hide, the
  heading becomes "Voice settings", and the extra alert blocks hide.

### Security (`security.js`)

- Change password: current, new (12–72 bytes), confirm; warns that
  other web sessions are signed out.
- Change telephone PIN: current password, new PIN (4–12 digits).
- Two-factor authentication: render/manage TOTP (enable with setup
  secret + verification, disable with password), status line and
  busy/epoch state (`renderTwoFactor`, `loadTwoFactor`,
  `twoFactorAction`).
- Field-level validation errors (`validateSecurity`, `securityError`).

### Administration (admin-only) (`administration.js`)

- Users panel: create user (username, role user/admin, password 12+,
  PIN 4+), list users with role, delete user.
- Global alert availability: single checkbox controlling whether
  ordinary users see call-alert controls at all; save button.
- SIP connection panel: enable SIP checkbox, domain/registrar, SIP
  username, password ("blank keeps existing" + password-state span),
  local SIP bind port, registrar SIP port, transport (udp/tcp/tls),
  registration interval; "Saving applies the change to the running
  call client."

## Admin vs ordinary user differences (must be preserved)

- `window.isAdmin` comes from `GET /api/v1/me` (role === "admin").
- Administration task hidden for ordinary users; hash navigation to it
  is forced back to mailboxes.
- Users panel, global alert availability, and SIP panel load only for
  admins (`bootstrap.js` gates `loadGlobalAlerts` and `loadSIP`).
- Voice install button, always-warm voices section, and resume of an
  active install job are admin-only.
- Admin voice-name field shows a different placeholder/title than the
  ordinary user's.
- Global alert availability removes alert controls for ordinary users
  (see `applyAlertAvailability`) and stops per-account alert checkboxes
  rendering.

## API routes served by the console (server.go Handler())

Health/status: GET /healthz, GET /readyz, GET /api/v1, GET /api/v1/status.
Auth: POST /api/v1/setup, /login, /recovery/request, /recovery/confirm,
/logout; GET /api/v1/me.
Security: POST /api/v1/security/password, /security/pin,
/security/2fa/setup, /security/2fa/enable, /security/2fa/disable.
Users (admin): GET/POST /api/v1/users, DELETE /api/v1/users/{id}.
Accounts: GET/POST /api/v1/accounts, PUT /api/v1/accounts/order,
POST /api/v1/accounts/validate, /accounts/test,
POST /api/v1/accounts/{id}/sync, /sync/cancel,
GET /api/v1/accounts/{id}/mutations, GET/PUT
/api/v1/accounts/{id}/folder-policy, DELETE /api/v1/accounts/{id}.
Cleanup jobs: GET /api/v1/cleanup-jobs, POST .../retry.
Contacts: GET/POST /api/v1/contacts, PUT/DELETE /api/v1/contacts/{id}.
Whitelist: GET/POST /api/v1/whitelist, DELETE /api/v1/whitelist/{id}.
Settings: GET/PUT /api/v1/settings.
Admin alerts: GET/PUT /api/v1/admin/alerts.
Alert numbers: GET/POST /api/v1/alert-numbers,
PUT /api/v1/alert-numbers/{id}/active, DELETE /api/v1/alert-numbers/{id}.
Alerts: POST /api/v1/alerts/test.
Voices: GET /api/v1/voices, POST /api/v1/voices/install,
GET /api/v1/voices/jobs/active, POST /api/v1/voices/jobs/{id}/cancel,
GET /api/v1/voices/jobs/{id}, GET /api/v1/voices/warm,
PUT /api/v1/voices/warm/{voice}, POST /api/v1/voices/preview,
GET /api/v1/voices/previews/{id}, GET /api/v1/voices/previews/{id}/audio,
POST /api/v1/voices/previews/{id}/cancel.
SIP (admin): GET/PUT /api/v1/sip.
Assets/page: GET /assets/{name}, GET /.
