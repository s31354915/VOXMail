# Release readiness and known limitations

VOXMail is beta software. A passing local test suite is necessary but is not a
production compatibility claim: providers, browsers, CPU architectures,
reverse proxies, container runtimes, and workload sizes still need validation
in the environment where the image will run.

This page is the public release checklist. It intentionally separates evidence
already produced in the repository from checks that require a deployment, an
authorized provider, or a hosted release system.

## Current disposition

Evidence last refreshed: **2026-10-06**.

### Verified in the repository workspace

- The complete Go test suite passes normally and under the race detector.
- `go vet ./...`, shell syntax checks, documentation contracts, and focused
  mail/config/speech/security tests pass.
- The standalone native shim tests pass repeatedly under ordinary constraints,
  restricted file-descriptor limits, and AddressSanitizer/UndefinedBehavior-
  Sanitizer builds. LeakSanitizer is disabled for the sanitizer run because
  this host cannot initialize it; that is an environment limitation, not a
  clean leak result.
- The pinned amd64 Baresip/re image was built and its native eight-test CTest
  suite passed. Five consecutive opt-in SIP/RTP peer runs covered registration,
  inbound and outbound calls, audio, DTMF, reconnect, close races, delayed
  bridge operation, and restart.
- A disposable Dovecot server was exercised with TLS IMAPS and STARTTLS, and
  `mbsync` synchronized against both modes with the real configured client.
- A real Piper/Whisper round trip, static-prompt validation, and model
  replacement safeguards passed using the pinned model assets.
- The browser scenario passed with real Chromium for accessibility names,
  keyboard navigation, account/setting flows, TOTP, logout clearing, CSP, and
  application restart. This used a disposable database and temporary browser
  runtime dependencies.
- A local TLS reverse proxy verified trusted-forwarding behavior, secure cookie
  handling, and shared client throttling. Direct untrusted forwarding headers
  were ignored.
- Startup cancellation was probed during model provisioning with the exact
  child-process cancellation fixture; both the application and child exited.
- Two sequential amd64 native image builds using pinned re/Baresip v4.10.0 and
  v4.11.0 references produced distinct library generations, Baresip versions,
  and matching provenance records. This verifies revision-scoped warm-cache
  behavior on amd64.

### Not yet a release claim

The following are intentionally deployment gates rather than defects hidden by
the local suite:

- authorized external IMAP/SMTP provider interoperability, including provider
  failures, retries, TLS variants, and destructive remote-operation recovery;
- real registrar and carrier behavior for SIP UDP, TCP, TLS, IPv4, IPv6,
  reserved-character credentials, NAT, codec negotiation, inbound calls, and
  RTP reachability;
- packaged native thread/address sanitizer runs, descriptor pressure, queue
  overload, two-call concurrency, long calls, and sustained reconnect tests;
- arm64 image build and runtime checks, including the pinned model binaries,
  Baresip, FFmpeg, `mbsync`, and speech activation during an existing call;
- supported browser-library installation on the target deployment host and
  concurrent/reordered-account flows at deployment scale;
- populated production-like backup/restore and SIGTERM drills inside the exact
  release container, including child-process and durable-state checks;
- SMTP delivery and recovery through the production reverse proxy/session-store
  topology;
- vulnerability scan, SBOM/provenance review, registry publication, manifest
  digest capture, rollback, and artifact-permission verification; and
- a sustained resource soak combining calls, reconnects, synchronization,
  speech selection, mail indexing, and alert scheduling.

## Promotion checklist

An operator should not promote a beta image until all applicable rows below
have evidence attached to the release record.

| Gate | Required evidence | Pass condition |
| --- | --- | --- |
| Source and image identity | Source commit, base-image digests, dependency refs, SBOM, provenance, architecture digests | Every artifact is traceable and the deployed digest is immutable |
| Automated quality | Go tests, race tests, vet, native CTest, shell/docs checks | No unexplained failures; failures are fixed or explicitly release-blocking |
| Mail providers | Authorized IMAP/SMTP matrix and failure-injection results | TLS/authentication, sync, mutation, submission, retry, and recovery behavior match the deployment contract |
| Telephone providers | Authorized SIP matrix with inbound/outbound audio and DTMF | Registration, calls, RTP, teardown, and reconnect pass on every supported transport advertised for the release |
| Container runtime | Exact image on the target Docker/OCI runtime and host | Health/readiness, permissions, signal handling, child cleanup, and persistent data behavior pass |
| Architectures | Independent amd64 and arm64 builds and smoke runs | Both architectures use the intended versions and pass runtime smoke checks |
| Web access | Supported browser and reverse-proxy topology | TLS, cookies, forwarding trust, CSP, login, recovery, TOTP, and accessibility behavior pass |
| Recovery | Fresh backup, restore, rollback, and key-recovery exercise | Mail, settings, models, credentials, and durable state are usable after recovery |
| Capacity | Soak and resource measurements | CPU, memory, descriptors, storage, and call/sync latency remain within declared limits |
| Security | Vulnerability review and secrets/log audit | No unreviewed blocking findings or credential/message leakage |

## Accepted operational boundaries

1. SIP registration, NAT traversal, provider authentication, TLS certificates,
   codec negotiation, and RTP reachability are provider and network dependent.
   The local SIP peer proves the native boundary and protocol shape only.
2. The first administrator setup is an atomic one-winner database operation,
   but it is not a network bootstrap-token protocol. Restrict an empty
   deployment to a trusted administrative path until setup is complete.
3. Mailbox recovery is an alternate authentication factor because control of
   the configured mailbox can produce a recovery session. Disable or restrict
   it where that assurance level is unsuitable.
4. Backups require the original encryption key. The key is intentionally not
   included in the data archive.
5. Local retention removes local Maildir data only; routine synchronization
   does not delete remote mail. Explicit read, move, and Trash actions are
   authenticated remote mutations and must be tested with the provider.
6. SQLite is deliberately conservative about concurrent writers. A larger
   deployment should measure contention before changing the limits.
7. Interrupted model installation is failed and retryable. A partial model is
   never silently activated.

## Release and rollback rules

Build and publish through the manual workflow described in the root
[README](../README.md). Keep the previous immutable digest and a verified
`/data` backup until the new deployment has passed readiness, account sync,
telephone, and recovery checks. For rollback, redeploy the previous image
digest with the matching data schema; do not blindly downgrade a migrated
database.

Any exception to a release gate must name an owner, state the affected users or
deployments, explain the risk, and have an expiry or a replacement validation
date. “Not tested” must never be reported as “passed.”
