# VOXMail browser tests

These are opt-in Playwright tests against a disposable VOXMail deployment. They
use a real Chromium browser and exercise the rendered console, not only HTTP
responses. The settings scenario needs an installed voice metadata pair, so
prepare a browser-only fixture in an isolated temporary data directory first:

```sh
npm install
npx playwright install chromium
data_dir=$(mktemp -d /tmp/voxmail-browser-data.XXXXXX)
tests/browser/prepare.sh "$data_dir"
VOXMAIL_DATA_DIR="$data_dir" VOXMAIL_BROWSER_BASE_URL=http://127.0.0.1:18080 npm test
```

Start the clean app with `VOXMAIL_DATA_DIR` set to the same directory and setup
available. The fixture only satisfies the server's installed-voice metadata
contract; it is intentionally not a usable Piper model and does not provide
speech/runtime evidence.

The suite intentionally uses one serial scenario because it creates the first
administrator and a second user in the disposable database. Do not point it at
a production or persistent database. Provider validation is mocked only for
the account form; the account save, ordering, settings, role gating, session
restore, recovery response, and CSP checks use the live server.
