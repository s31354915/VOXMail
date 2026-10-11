# Security reporting

Do not report exploitable vulnerabilities, credentials or private mail contents
in public issues, discussions or pull requests.

Use [GitHub private vulnerability reporting](https://github.com/s31354915/VOXMail/security/advisories/new)
when the repository Security tab offers **Report a vulnerability**. Reports are
private to the repository security maintainers and invited collaborators. If that
control is unavailable, open a public issue asking only for a private reporting
channel, without the affected endpoint, exploit, credentials or other sensitive
details. Wait for a private route before sending the report. No separate security
email address is currently advertised by this project.

The private reporting setting must be enabled by a repository administrator.
At the last configuration check on 2026-10-08 it was disabled, and the automation
token could not enable it. The fallback above applies until maintainers enable
that setting; this policy does not promise that an unavailable form accepts reports.

## Useful report details

In the private report, include the source revision or exact image digest,
architecture, affected configuration, expected and actual behavior, a minimal
disposable reproduction and impact. Include sanitized logs and relevant error
categories. Remove real mailbox contents, recordings, passwords, encryption
keys, session cookies, TOTP secrets and recovery tokens. Use test accounts and
fixture data rather than production credentials.

Test only systems and accounts you are authorized to access. Avoid destructive
remote-mail operations and live calls when demonstrating a bug; describe any
necessary side effects so maintainers can reproduce it in isolation.

## Scope and maintenance

VOXMail is beta software. Maintainers prioritize investigation against the current
main-branch code and current release artifacts; older images are not guaranteed
backported security fixes. Include the original affected revision even if you
also reproduce on current code. A moving `latest` tag is insufficient to identify
an affected artifact: record its digest.

Authentication/session/ownership boundaries, recovery flows, secret storage,
provider endpoint validation, mail parsing/mutations, SIP/native bridge handling
and release/dependency integrity are relevant security areas. For an upstream
dependency vulnerability, identify the dependency and version and whether/how
VOXMail exposes the affected path; coordinate with the upstream project's private
reporting policy as appropriate.

Maintainers will assess impact, reproduce the issue and coordinate a fix and
disclosure through the private channel. This project makes no guaranteed response
time, bounty or supported-version service commitment. Routine non-sensitive bugs
and contribution questions belong in public issues. See
[contribution guidance](CONTRIBUTING.md) and
[release readiness](docs/RELEASE-READINESS.md) for testing and deployment boundaries.
