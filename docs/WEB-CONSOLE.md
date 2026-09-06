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
- remote-to-local folder mappings;
- account-level call-alert enablement and alert folders.

Leaving either password blank while editing preserves the encrypted secret.
New accounts require both passwords. **Test IMAP host** is available after an
account is saved and only checks that account's configured host and port.

The account table provides Edit, Test, Delete, and up/down ordering controls.
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

## Alerts

Account alert switches and alert-folder lists are configured on each account.
Global call alerts and the alert number are configured in **Voice and call
alerts**. The phone Settings menu can toggle the global switch.

## Voice settings

Enter a Piper model such as `en_US-hfc_male-medium` and choose menu/email
speeds from 1 (slower) to 5 (faster). A selected voice is resolved below the
deployment voice directory and loaded only for an admitted call. Calls sharing
the same voice/speed share one warm runtime; another setting gets another key.

## Users

Administrators can create users with a role, login password, and telephone PIN,
and delete another user. An administrator cannot delete the current admin from
the current session.
