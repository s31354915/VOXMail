# Web console guide

The root page is a same-origin single-page console with session cookies and a
CSRF token on writes.

## First-run setup and sign-in

On an empty database, use **First-run setup** once: choose an administrator
username, a password of at least 12 characters, and a numeric telephone PIN of
4–12 digits. Subsequent users use **Sign in**. TOTP-enabled users must also
enter their authenticator code. Only administrators see the Users panel.

## Mail accounts

Each account supports:

- display name, sender address, IMAP/SMTP host, port, username, and password;
- sync interval, initial RFC3339 cutoff, and retention days;
- display order;
- remote-to-local folder mappings and explicit Inbox/Sent/Drafts/Spam/Trash
  role mappings;
- account-level call-alert enablement and alert folders.

Leaving either password blank while editing preserves the encrypted secret.
New accounts require both passwords. **Test IMAP host** is available after an
account is saved and only checks that account's configured host and port.

The account table provides Edit, Test, Delete, and up/down ordering controls.
The **Test IMAP and SMTP** action authenticates both services over verified TLS
(IMAPS or STARTTLS), discovers remote folders, and displays the discovered
names before the user completes mappings. SMTP port 25 is rejected by the
normal account path unless a separate trusted plaintext policy is added.
Enter one mapping per line, with the remote name on the left:

```text
INBOX=Inbox
Archive=Old Mail
[Gmail]/Sent Mail=Sent
```

Aliases must be relative, non-empty, and unique locally. Keep alert-folder
names aligned with the remote names or local aliases produced by sync.

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
- SIP port for registration and inbound calls, default **5060**;
- transport (`udp`, `tcp`, or `tls`) and registration interval in seconds.

Saving persists the settings and restarts the call client so the change takes
effect immediately. Until a registrar is configured and enabled, baresip does
not register and no calls are accepted. The password is stored encrypted; the
panel only reports whether one is set. Non-administrators never see this
panel.

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

Enter a trusted Piper model such as `en_US-hfc_male-medium` and choose
menu/email speeds from 1 (slower) to 5 (faster). The **Install selected
trusted voice** action downloads only catalog entries over pinned HTTPS URLs,
then the model can be activated. A selected voice is resolved below the
deployment voice directory and loaded only for an admitted call. Calls sharing
the same voice/speed share one warm runtime; another setting gets another key.

## Security and recovery

The Account security panel supports password changes, phone-PIN changes, TOTP
setup/enable/disable, and one-time backup codes. Mailbox recovery sends a
short-lived, rate-limited, single-use OTP to a configured mailbox for either a
password reset or one authenticated web session that bypasses TOTP. Recovery
requests intentionally return the same response for known and unknown
addresses.

## Users

Administrators can create users with a role, login password, and telephone PIN,
and delete another user. An administrator cannot delete the current admin from
the current session.
