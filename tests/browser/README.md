# VOXMail browser tests

These Playwright tests run in CI on pushes and pull requests and can also run
locally against a disposable VOXMail deployment. They
use a real Chromium browser and exercise the rendered console, not only HTTP
responses. The settings scenario needs an installed voice metadata pair, so
use `scripts/browser-test.sh` to build, start, and stop a fresh offline app:

```sh
npm ci --prefix tests/browser
(cd tests/browser && npx playwright install chromium)
scripts/browser-test.sh
```

Pass Playwright filters after the script name to run a focused browser check,
for example `scripts/browser-test.sh specs/accessibility.spec.js`.

The script uses a private `/tmp/voxmail-browser-ci.*` directory, a loopback HTTP
listener, and offline mode to prevent SIP, sync, and alert workers from running.
The harness chooses an available loopback port and a unique result/report
directory for each run. Set `VOXMAIL_BROWSER_PORT` to request a specific local
port; a port already in use is rejected before starting the fixture. It invokes
`tests/browser/prepare.sh`; the fixture only satisfies the server's installed-voice metadata
contract; it is intentionally not a usable Piper model and does not provide
speech/runtime evidence.

The main scenario runs serially because it creates the first
administrator and a second user in the disposable database. Do not point it at
a production or persistent database. Provider validation is mocked only for
the account form; the account save, ordering, settings, role gating, session
restore, offline recovery refusal, and CSP checks use the live server.

Recovery and authenticator-flow tests use controlled HTTP transport to exercise
failure/retry, cancellation, duplicate suppression, focus, 390px reflow, local
QR loading, and the backup-code download/acknowledgment without contacting a
provider. The main scenario also provisions and enables a real authenticator
through the live backend, then verifies expiry clears sensitive output before a
different user signs in. `TestTwoFactorQRProvisioningAndVerification` independently
scans the production PNG, checks its account/issuer/secret, verifies a real code,
and checks encryption, no-store responses, and validation retries.

Security-form checks cover persistent labels/autocomplete/descriptions,
UTF-8 password limits, PIN rules, confirmation mismatches, API field-error
association and retries, pending duplicate suppression, and expiry clearing at
desktop/mobile widths. API tests separately verify those errors keep a valid
session authenticated and never echo submitted credentials.

Navigation checks cover each task for administrator and user roles at desktop
and mobile widths, deep links, reload/history, preserved drafts, keyboard focus
and skip links, inactive sections, and unauthorized/unknown task fallback.

Accessibility checks use axe-core in Chromium at desktop and 390px widths for
setup, login/recovery, and every user and administrator console task. They
check WCAG 2.0/2.1/2.2 A/AA and axe best-practice rules, including text
contrast. The broader suite also covers real setup, expiry, different-user
login, role restrictions, keyboard operation, and failure recovery.

Operational checks cover SIP registration/failure/retry, explicit maintenance
mode, sync recency and failure details, unfinished records, stale snapshots,
automatic polling without draft/focus loss, failed manual sync refresh, and
expiry followed by a different user with late responses. Backend tests use a
real synchronization service and child process to verify live ownership,
durable history, error privacy, and restart recovery. The isolated native
shutdown fixture also verifies registered → provider 403 → registered snapshots
through the actual Baresip bridge and checks both blocked mbsync workers.
