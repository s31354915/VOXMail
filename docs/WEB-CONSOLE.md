# Web console guide

The root page is a same-origin single-page console with session cookies and a
CSRF token on writes.

## First-run setup and sign-in

On an empty database, use **First-run setup** once: choose an administrator
username, a password of at least 12 characters, and a numeric telephone PIN of
4–12 digits. Setup returns to **Sign in** without creating a web session;
sign in with the credentials you just chose. If another tab completes setup,
the older form also returns to sign-in. Subsequent users use **Sign in**. TOTP-enabled users must also
enter their authenticator code. Only administrators see the Users panel.

Sign-in can return a temporary rate-limit error with a retry delay. An account
can have up to 16 simultaneous web sessions; if that limit is reached, sign out
of an existing session before signing in again. Existing sessions are preserved
when the server reaches its global limit. A rejected sign-in due to session
capacity does not consume an authenticator backup or mailbox recovery code.

The sign-in card displays one form at a time. **Reset password** opens a mailbox
recovery request; **Bypass TOTP** opens a request to sign in once without an
authenticator while keeping 2FA enabled. An accepted request opens code
confirmation for that mailbox and purpose. Password reset also requires a new
password; successful reset returns to sign-in. **Back to sign in** cancels the
pending browser request and clears recovery inputs. A late response cannot
reopen the canceled flow. Errors remain visible so you can retry. Offline mode
rejects recovery-email delivery with a clear maintenance message.

API clients retain automatic setup sign-in unless they send `sign_in: false`;
the console uses that opt-out to make the first-run transition deliberate.

## Mail accounts

Each account supports:

- display name, sender address, IMAP/SMTP host, port, username, and password;
- sync interval, initial RFC3339 cutoff, and retention days;
- display order;
- remote-to-local folder mappings and explicit Inbox/Sent/Drafts/Spam/Trash/Archive
  role mappings;
- account-level call-alert enablement and alert folders.

Leaving either password blank while editing preserves the encrypted secret.
New accounts require both passwords. **Test IMAP and SMTP** is available after
an account is saved and checks only that account's configured hosts and ports.

The account table provides Edit, Test, Delete, and up/down ordering controls.
The **Test IMAP and SMTP** action authenticates both services over verified TLS
(IMAPS or STARTTLS), discovers remote folders, and displays the discovered
names before the user completes mappings. SMTP port 25 is rejected by the
normal account path unless a separate trusted plaintext policy is added.
After saving an account, open **Folder choices** in its table row. The editor
shows retained mappings and folders discovered by synchronization or a successful
**Test saved settings**. For a new account, test its saved settings and reopen the
editor to map folders before a long initial sync finishes. Discovered names are
retained separately from synchronized directories; testing never rewrites existing
Maildir paths or UID metadata. Use the folder
checkboxes to select an action; selecting a checkbox alone changes no mapping.
Use **Map to local folder** to select a local alias from the dropdown, and use
the role selector to assign a role. Destinations used by other folders are disabled;
combine those folders as a logical group instead. **Custom local name** also allows
a new alias when needed. Existing mappings and role assignments are retained.

To combine duplicates, select their folders and choose **Combine selected**,
enter a display name, and explicitly choose the primary member. The primary is
the destination for writes to that group; browsing includes each member's
distinct messages. Provider names never cause automatic grouping or exclusion.
**Keep separate**, **Exclude from browsing**, and **Show selected** control the
selected folders. Each folder also has a **Phone menu** dropdown: choose
**Hide from phone menu** to ignore it or **Show folder** to restore it. Excluded
folders are not listed in the phone folder menu; stored and remote emails are
retained and synchronization continues. Existing
groups can be edited through **Edit group** or dissolved with **Keep all members
separate**.

**Save folder choices** commits mappings, roles and groups together. A failed
save preserves your edits and reports the error beside the editor. If another
configuration update makes the save stale, cancel and reopen Folder choices to
load the current version. Cancel discards unsaved editor changes.

The preview shows the phone menu after the next successful synchronization,
using local aliases and the retained legacy role fallbacks. Expand **Nested
folders** and **Press 9 for more folders** to inspect every submenu and page.
Inbox/Drafts/Sent/Trash retain keys 1–4 at the root; additional root folders start
at 5–8, and subsequent pages and nested menus use 1–8. An explicitly excluded role
stays unavailable until the owner assigns it again.

Aliases must be relative, non-empty, and unique locally. Keep alert-folder
names aligned with the remote names or local aliases produced by sync.
An alias cannot reuse another remote folder's configured or observed directory,
including unmapped folders downloaded by sync. Equivalent local paths count as
the same destination. Choose a distinct alias and use a logical group to combine
duplicates; sharing a physical directory would mix their UID state.

If discovery finds a folder whose automatic destination conflicts with a saved
mapping, **Folder choices** opens with a conflict notice. The phone preview stays
unavailable until the conflict is resolved. Each mapping dropdown offers a fresh
`Separate/<remote folder>` destination. For example, if Gmail Sent Mail already
uses `Sent`, map the newly discovered `Sent` folder to `Separate/Sent`, then save.
Existing destinations remain reserved to protect previously synchronized state;
logical grouping can combine their presentation after physical mappings are distinct.

### Draft synchronization policy

Drafts are stored locally as the canonical editable copy. The first local save
may append an initial snapshot to the account's mapped remote Drafts folder.
Because IMAP APPEND creates a new message rather than updating an existing
one, later saves for the same local draft ID never append another remote copy;
they update only the local draft. If the first remote append fails, the local
save still succeeds and VOXMail does not retry that append automatically, so a
network timeout cannot create duplicates after a later save or restart.

## Contacts and callers

Contacts have name, email, and display order. Create, edit, delete, and use
the up/down controls to reorder them. Contacts are IVR shortcuts; they do not
grant caller access.

The caller whitelist stores the stable phone/SIP identity allowed to reach the
PIN prompt. Remove entries when access is revoked. Caller ID is not a
cryptographic identity, so the PIN remains mandatory.

## SIP connection

The **SIP connection** panel (administrators only) manages the deployment-wide
provider credentials for the embedded call client:

- domain/registrar, SIP username, and password (blank keeps the existing one);
- local SIP bind port and registrar port, each defaulting to **5060**;
- transport (`udp`, `tcp`, or `tls`) and registration interval in seconds.

Saving persists the settings and restarts the call client so the change takes
effect immediately. Until a registrar is configured and enabled, baresip does
not register and no calls are accepted. The password is stored encrypted; the
panel only reports whether one is set. Non-administrators never see this
panel.

### Live service and synchronization status

The **Service status** card is available to every signed-in user. It shows SIP
registration separately from the call process, bridge connection, and speech
media readiness. Offline/maintenance mode is explicit. Registration failures
include the SIP response code when available and guidance for what to check.
These are runtime facts, rather than a claim that saving settings registered
successfully.

Each mailbox shows its latest sync state, start/finish times, last successful
sync time, and a run ID. A later failed attempt does not replace the last
success time. Failure categories cover authentication, TLS, connectivity, DNS,
timeout/cancellation, storage, and indexing. An unfinished record is labeled
running only when the synchronization service owns active work. Application
startup finalizes abandoned records as interrupted before accepting new work.

Status refreshes every ten seconds while the tab is visible and can be retried
with **Refresh status**. Refreshes preserve unsaved forms and keyboard focus.
If a refresh fails, the existing snapshot remains visible with a stale-data
message. Changed sync and SIP states are announced once through polite live
regions. A manual refresh announces its result and receives focus; background
polls do not steal focus or repeatedly announce unchanged rows. Mutation
results use a focusable polite status message. Opening account forms focuses the
first field, editing a contact focuses its name, and canceling a hidden account
form returns focus to Add account. Session expiry cancels requests/polls and
clears status diagnostics.

`GET /api/v1/status` requires authentication and returns `checked_at`,
`offline_mode`, `readiness`, `runtime`, and an optional structured `sip_failure`.
`GET /api/v1/accounts` returns only the user's mailboxes, with `last_sync` and
`last_successful_sync`. Run views include `state` and optional `failure` with
`category`, `reason`, `action`, and a recognized response/exit-code diagnostic.
Raw provider transcripts are retained in server diagnostics, not returned by
these views, because they may contain passwords or tokens. Public readiness
does not expose those private failure details.

## Alerts

Account alert switches and alert-folder lists are configured on each account.
Global call alerts and the alert number are configured in **Voice and call
alerts**. The phone Settings menu can toggle the global switch.

**Send test alert call** immediately places a call to the configured alert
number with a canned test message, without waiting for a mail round. It verifies
the whole outbound path: pending-mail scan is skipped and no message is claimed.
If no alert phone number or no SIP registrar is configured, the console reports
the specific problem instead.

## Voice settings

Administrators can enter any published Piper voice name, such as
`en_US-lessac-low`, or an HTTPS URL ending in `.onnx`, then choose **Install
selected Piper voice**. Named voices resolve in the upstream Piper repository;
custom URLs download their matching `.onnx.json` file from the same path.
The API also accepts a separate metadata URL and optional SHA-256 checksums.
The bundled catalog retains its pinned sources and checksums.

Installation runs as a job, showing its stage and any failure reason. The model
and metadata are staged together, checked with Piper, and activated using their
recorded checksums. Invalid, interrupted or canceled downloads leave the previous
pair active. Downloads require public HTTPS endpoints; the model limit is 4 GiB
and the metadata limit is 16 MiB. Installation is unavailable in offline mode.

Regular users see installed voices and can preview or save them, but cannot
install models. After installation completes, choose **Save voice settings**
and menu/email speeds from 1 (slower) to 5 (faster).
A selected voice is resolved below the
deployment voice directory and loaded only for an admitted call. Calls sharing
the same voice/speed share one warm runtime; another setting gets another key.

The opt-in `tests/voices/smoke.py --image IMAGE` check installs an upstream voice
outside the bundled catalog using real Piper, verifies the activated pair's
checksums and WAV output, and checks administrator/user permissions. It uses
a disposable Docker volume and fetches only the public voice model; no mailbox
or SIP provider is configured. `--binary PATH` can mount a current executable
read-only into an existing runtime image.

## Console navigation

The signed-in console has Mailboxes, Caller access, Voice & alerts, Security,
and administrator-only Administration sections. Navigation uses URL fragments
so direct links, reloads, and browser back/forward return to the selected task.
Changing sections preserves unsaved fields; sign-out and expiry still clear
private state. Unknown or unauthorized task links fall back to Mailboxes.
Task headings receive keyboard focus when navigation changes, a skip link
moves focus into the active content, and hidden sections leave the tab order.
On narrow screens the task links reflow above the content.

## Security and recovery

The Account security panel supports password changes, phone-PIN changes, TOTP
setup/enable/disable, and one-time backup codes. Setup renders its QR code locally
without sending the authenticator secret to a third party. Scan the code (or
enter the displayed key manually), then verify a six-digit code before enabling
protection. The final step offers a backup-code download and requires an explicit
saved-codes acknowledgment. Protection is already enabled while those codes are
shown; finishing clears them from the page. Canceling an unverified setup,
signing out, or session expiry clears the QR, key, code, and backup-code output.
An incorrect current password or verification code is a retryable validation
error; an expired session still signs the console out. Mailbox recovery sends a
short-lived, rate-limited, single-use OTP to a configured mailbox for either a
password reset or one authenticated web session that bypasses TOTP. Recovery
requests intentionally return the same response for known and unknown
addresses. The `bypass_2fa` option treats control of that mailbox as an
alternate authentication factor: anyone who can read the mailbox can obtain a
session without the authenticator app. This is especially important for
administrators, because an administrator mailbox compromise becomes a console
account compromise. Deployments with stronger assurance requirements should
disable or separately restrict mailbox recovery before exposing the console.
Web passwords are 12–72 UTF-8 bytes because the current bcrypt verifier rejects
longer inputs. Telephone PINs are 4–12 digits. Security forms retain visible
labels, appropriate autocomplete values, and associated descriptions while
editing. Password and PIN changes require matching confirmation values in the
console; those confirmation fields are not sent to the API. Field validation
appears beside its input and identifies it with `aria-invalid` and
`aria-describedby`; typing clears that field's old error. API validation adds an
optional `fields` map to the existing error/code/request-ID response without
returning submitted credentials. Password, PIN, and authenticator validation
errors return 400 while missing/expired authentication still returns 401.

## Users

Administrators can create users with a role, login password, and telephone PIN,
and delete another user. An administrator cannot delete the current admin from
the current session.

### Background synchronization and previews

Starting a manual mailbox sync returns HTTP 202 with a durable `run_id` and
`account_id`. The console reports that synchronization started, then polls the
existing account status without holding the request open. The same run remains
visible after navigation or sign-in again; running accounts disable duplicate
starts and keep the cancellation action available. The service admits at most
eight manual jobs at once and reports HTTP 429 when full. A request disconnect
ends its response, while the admitted job continues until completion, explicit
cancellation, account deletion, or application shutdown.

Voice preview also returns HTTP 202 with a `job_id`, `status_url` and `audio_url`.
The console polls the status and retrieves the WAV only after completion. Status,
audio and cancellation endpoints require the creating user's session; other
users, including administrators, cannot retrieve another user's preview. A
preview has a two-minute execution budget. At most four previews/results are
retained, each with at most 16 MiB of audio. Results expire after five minutes
and may be evicted earlier to admit a new preview. Preview results are temporary:
a restart or expired result requires generating a new preview. Failures appear
beside the voice settings, and the controls recover for retry.

The normal HTTP write timeout is 90 seconds. Large-mailbox syncs, voice installs
and previews run as background jobs and do not inherit that connection deadline.
Shutdown stops job admission, cancels work, and joins workers before closing the
database.


Account and user deletion return HTTP 202 with `cleanup_pending: true` after
stopping active synchronization, invalidating affected calls and removing
metadata. Stored-file removal runs separately, so mailbox size does not keep
the delete request open. The existing operational status area shows pending and
failed cleanup counts; owners see their jobs and administrators also see jobs
for deleted users. `GET /api/v1/cleanup-jobs` returns the latest 100 job statuses,
with account/owner identifiers, attempts, readiness and timestamps, without
private paths or raw errors. `POST /api/v1/cleanup-jobs/{id}/retry` admits a failed,
ready job for its owner or an administrator; it requires normal mutation/CSRF
authorization. Failed jobs also retry automatically after five minutes. A pending
job whose account metadata still exists waits for a repeated authorized delete
to finish worker coordination before any files are touched.
