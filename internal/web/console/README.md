# Console source

The console uses standard JavaScript modules with explicit imports and exports.
`src/state.js` owns mutable session/operation state; `src/core.js` owns the API,
session cleanup, shared DOM helpers, and mutation feedback. Task behavior and
its event bindings live in authentication, mailboxes, caller-access, voices,
security, navigation, administration, and operations modules. `src/bootstrap.js`
coordinates initial loading. `src/main.js` installs bindings after all module
state has initialized, then starts discovery of the current session/setup state.

Keep API requests behind `api()`, and retain session/authentication epochs when
changing asynchronous flows. Private state stays inside the generated bundle;
modules must not publish credentials or new state on `window`.

After editing source, regenerate the checked-in bundle:

```sh
npm ci --prefix internal/web/console
npm run --prefix internal/web/console build
npm run --prefix internal/web/console check
scripts/browser-test.sh
```

Use `npm run --prefix internal/web/console format` to format source and styles.
The lockfile pins build tools. CI rejects a generated bundle that differs from
its module sources. Application builds require only Go; Node is a development
and verification dependency.

Go embeds the generated `static/console.bundle.js`, `console/console.css`, and
HTML template, then assembles the page once. The response's CSP includes hashes
of the exact assembled script and style bytes. There are no runtime module
requests, inline event attributes, eval, external script/style origins, or
`unsafe-inline` exceptions. Static console images use the existing asset route.
The browser suite verifies the assembled page under its real security headers.
