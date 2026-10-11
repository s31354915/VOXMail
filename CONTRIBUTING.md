# Contributing to VOXMail

Describe the concrete problem, intended behavior and relevant reproduction when
opening an issue or pull request. Include the source revision and sanitized
diagnostics. For suspected vulnerabilities, use the repository's private security
reporting route rather than a public issue. Do not upload mailbox contents,
recordings, credentials, cookies, recovery links, encryption keys or production
volumes. Use disposable fixtures for testing. If the private reporting control is
unavailable, request a private channel without disclosing vulnerability details;
see [security reporting](SECURITY.md).

## Development setup

Use Go compatible with `go.mod`; CI currently selects Go 1.26. The console and
browser jobs use Node 22 with the checked-in npm lockfiles. Docker with BuildKit
is needed for native/container checks. Controlled provider integration needs the
real `mbsync` executable (`isync` on Debian/Ubuntu). Python runtime dependency
verification uses the pinned Python 3.11 container, not the host interpreter.

Read [architecture](docs/ARCHITECTURE.md), [settings](docs/SETTINGS.md) and
[operations](docs/OPERATIONS.md) before changing lifecycle, ownership, provider,
security or deployment behavior. Start with relevant package tests while working,
then run the applicable CI checks before requesting review:

```sh
go vet ./...
go test -race -count=1 ./...
go build -o /tmp/voxmail-e2e-check ./tests/e2e
scripts/docs-contract.sh
docker build --check -f Dockerfile .
```

These checks do not replace native, browser, provider or real-model acceptance.
Choose additional checks for the changed boundary using
[acceptance layers](docs/OPERATIONS.md#acceptance-layers-and-what-each-proves).
State which checks passed, which were not run and why; attach source/digest/platform
identities for image acceptance. Avoid claiming a configured workflow has passed
without its completed-run evidence.

## Console changes

Edit modules and styles under `internal/web/console`, then regenerate the embedded
bundle. Commit source and generated output together; do not hand-edit only the
bundle. Preserve strict CSP, accessibility, role boundaries and sensitive-state
clearing on navigation/session changes.

```sh
npm ci --prefix internal/web/console
npm run --prefix internal/web/console build
npm run --prefix internal/web/console check
npm ci --prefix tests/browser
(cd tests/browser && npx playwright install chromium)
scripts/browser-test.sh
```

The [browser guide](tests/browser/README.md) explains disposable fixtures and
filtered runs. Never point the setup/restart harness at a persistent database.

## Provider and native changes

Use explicit fixtures and bounded timeouts; keep private endpoint/CA overrides
inside test injection paths. Preserve remote UID/UIDVALIDITY identities and
uncertain SMTP outcomes. Run the [controlled provider suite](tests/providers/README.md)
for mail transport, synchronization and recovery changes.

Native bridge changes require review of thread/descriptor ownership, shutdown,
queue bounds and protocol compatibility with Go. The Docker native stage builds
and runs the shim CTest targets. Run the deterministic SIP/RTP peer for native or
call-boundary changes and real models for speech changes. Validate both native
architectures on the exact image digests before making a release claim.

## Dependencies and publication

Keep action references pinned to reviewed commits and runtime Python packages
hash-locked for both architectures. Use the documented
[Python lock generation and verification](docs/OPERATIONS.md) when changing roots
or the interpreter/base digest. Dependency automation creates review proposals;
it does not approve compatibility or replace acceptance evidence.

Pull requests should explain final behavior, relevant risks and validation. Do
not mix unrelated edits or commit secrets, generated runtime data, test reports,
dependency directories or local review notes. `docs/REVIEW-PROGRESS.md` is a local
working checklist and must remain uncommitted.

Publication is an explicit operation. A `(publish)` marker on a main-branch push
or manual publication invokes exact-source CI, scans and acceptance gates. Ordinary
contributions should not add that marker. Versioned aliases are immutable; an
identical-digest retry is allowed, while changed artifacts need a new tag. See
[release operations](docs/OPERATIONS.md) and
[release readiness](docs/RELEASE-READINESS.md) before publication.

## Maintainer ownership

[CODEOWNERS](.github/CODEOWNERS) assigns the current repository owner to all
files, with explicit coverage for workflows, dependency/publication helpers,
native code and both sides of the bridge/lifecycle boundary. Owners must have
repository write access. Update those assignments when responsibility changes.

Workflow review includes token permissions, immutable action/dependency refs,
untrusted input, exact-source acceptance, failure paths and alias writes. Native
review includes thread/descriptor ownership, event queues, callback/shutdown races,
frame bounds and Go/native protocol compatibility. Changes to either side must
include matching boundary validation; generated bundles or passing Go tests alone
do not prove native safety.

GitHub applies CODEOWNERS from the pull request's base branch and requests eligible
owners when a pull request is ready for review. Required owner approval is a
separate branch-protection/ruleset setting; this file does not itself block merges.
Maintainers should require owner review for protected release branches and keep
ownership-file changes within that review boundary. Do not describe those settings
as enforced until their repository configuration is verified.
See [GitHub's code owner rules](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)
for base-branch selection, owner eligibility and required-review configuration.

## License for contributions

Contributions to VOXMail project code are accepted under the [MIT License](LICENSE).
Preserve the copyright notices and licenses of third-party code and assets.
