# VOXMail operations guide

## Configure the deployment

Create `.env` from the example and set a unique key:

```dotenv
VOXMAIL_ENCRYPTION_KEY=<at least 32 random bytes>
```

For telephone service, configure the provider credentials in the web console's
**SIP connection** panel (registrar, SIP username, password, local bind port,
registrar port, transport, registration interval). The `voxmail` process writes the baresip configuration
and supervises the call client, so a save applies immediately. Until a
registrar is configured and enabled there, baresip does not register and no
calls are accepted.

`VOXMAIL_SIP_ACCOUNT` exists as a deployment override for the account line
baresip would otherwise generate from the console settings:

```dotenv
VOXMAIL_SIP_ACCOUNT=<sip:YOUR_NUMBER:YOUR_PASSWORD@YOUR_PROVIDER>;regint=300
```

Provider-specific account syntax, outbound proxy, codecs, and NAT behavior are
provider-dependent. Local SIP binding and registrar ports default independently
to 5060, and RTP uses UDP
10000–10100; both ranges must be reachable from the provider. Do not commit
provider credentials. A registration interval of `0` means “use the default”
and is persisted/rendered as 300 seconds; the browser and API use the same
normalization.

## Models

The default model download is explicit:

```dotenv
VOXMAIL_PROVISION_MODELS=1
```

On first start this downloads the default Piper voice and Whisper base English
model into `/data/voices` and `/data/whisper`. They persist in the named
`voxmail-data` volume. Remove the flag after provisioning if you want startup
configuration to be unambiguous.

Custom Piper voices must be installed as `<voice-name>.onnx` plus its Piper
JSON beside it in `/data/voices`. Enter the voice name, with or without the
`.onnx` suffix, in the web console. A missing model is reported when a call
tries to warm it; callers never cause arbitrary downloads.

Administrator voice installations have durable job records. A queued or
running job that is present when the server restarts is exposed as a failed
interrupted job rather than disappearing; this prevents a partial model
activation from being reported as successful. Terminal job records are retained
for diagnosis until the normal job-history cleanup policy removes them.

Whisper is configured with `VOXMAIL_STT_BINARY` and `VOXMAIL_STT_MODEL`.
Voice selection and menu/email speed are user settings.

## Tags and upgrades

Compose defaults to the moving `:latest` manifest. For a controlled upgrade,
set a release tag or digest in `.env`, then pull and recreate:

```sh
docker compose pull
docker compose up -d --force-recreate
docker compose ps
docker compose logs --tail 100 voxmail
```

The Compose service has conservative defaults of 2 GiB memory, 2 CPUs, 256
processes, and 4096/8192 open files. Increase these limits deliberately when
running larger Whisper/Piper models or allowing more simultaneous calls; do
not remove the process and file-descriptor limits without replacing them with
equivalent host controls. The container has a 60-second graceful-stop window
so active sync and calls can clean up before Docker restarts it.

The GitHub Action can be started manually or by pushing to `main` with
`(publish)` in the head commit message. It tests the exact source commit,
requires the matching CI checks, builds native amd64 and arm64 images, records
their registry digests, scans both images, and combines those digests on GHCR.
Marked pushes publish the exact pushed commit as `latest`; manual test or
prerelease tags do not advance `latest` unless the explicit production-
promotion input is selected. See the root README for the workflow inputs and
generated tags. Fixed HIGH/CRITICAL scanner findings block publication. Any
future exception must be reviewed, state the reason and owner, and include a
short expiry date; do not add a permanent blanket ignore.

### Reproducible image updates

The Dockerfile pins the Debian Bookworm, Go 1.26 Bookworm, and Python 3.11
slim Bookworm base manifests by digest. The tag remains beside each digest as
the human-readable compatibility label. Native dependency sources are pinned
separately by immutable commit arguments, and the build writes those resolved
commits plus model hashes into `/usr/local/share/voxmail/provenance`.

To intentionally refresh a base image or runtime distribution:

1. Choose a supported tag and inspect its multi-platform manifest with
   `docker buildx imagetools inspect IMAGE:TAG`; record the full `sha256:`
   manifest digest, not a short platform-layer digest.
2. Update the tag and digest together in `Dockerfile`, record the resolution
   date and reason in the change, and keep build-stage and final-stage
   distributions compatible. Do not replace a digest with `latest` or a
   mutable version tag.
3. Run `docker build --check`, Go vet/race tests, the native CTest suite, and
   the isolated E2E build for the exact source commit. Run
   `scripts/runtime-smoke.sh` with the real pinned Piper and Whisper models,
   then run `scripts/real-model-smoke.sh` with the same model paths. The latter
   synthesizes a known phrase, converts it to the supported Whisper WAV shape,
   and checks that the real model recognizes the expected words. Capture
   executable/module dependency checks and model hashes.
4. Publish architecture images through the workflow, review the SBOM and
   vulnerability scan, capture both architecture digests, and use the
   resulting multi-architecture manifest digest for deployment or rollback.
   Keep the previous digest until the restore drill and readiness checks pass.

The current base pins were resolved on 2026-10-06. A digest update is a
dependency change: review ABI/loader output, speech CLI behavior, baresip
module loading, mbsync validation, and both amd64 and arm64 smoke results
before accepting it.

Versioned image publications (every tag other than `latest`) require the
runtime and real-model smoke checks on both captured architecture digests before
any release alias is promoted. The real-model acceptance workflow also builds
and tests both architectures weekly or on manual dispatch. Normal push/PR CI
runs Chromium against a disposable offline app, controlled TLS IMAP/SMTP
integration, and the local SIP peer alongside the API E2E suite.

To reproduce the model gate from a checkout with Go, Docker, and jq installed:

```sh
scripts/model-acceptance.sh ghcr.io/<owner>/voxmail@sha256:<digest> linux/amd64
```

This downloads the production checksum-verified Piper/Whisper fixtures into a
private temporary directory, mounts them read-only, and runs both smoke scripts
with the container network disabled. Run it natively for `linux/arm64` as well.
For protocol integration commands and isolation details, see
[controlled provider tests](../tests/providers/README.md) and
[browser tests](../tests/browser/README.md).

The standalone real-model smoke test can use already installed models and must
never use placeholder model files:

```sh
VOXMAIL_PIPER_MODEL=/path/to/voice.onnx \
VOXMAIL_WHISPER_MODEL=/path/to/ggml-base.en.bin \
VOXMAIL_REAL_MODEL_TEXT='This is a VOXMail speech smoke test.' \
VOXMAIL_REAL_MODEL_EXPECTED='speech smoke test' \
scripts/real-model-smoke.sh
```

`VOXMAIL_REAL_MODEL_EXPECTED` may contain a smaller stable set of words when a
different approved phrase or model is used. The test still requires non-empty
Whisper output and validates the generated WAV before transcription.

The native SIP boundary has a separate localhost peer test, enabled in CI and
available for local use. It runs the
real Baresip binary and the real `voxmail` module against a disposable Python
peer, then verifies digest-authenticated registration, both call directions,
request-ID correlation, negotiated RTP audio in both directions, RFC 4733
telephone events, repeated hangup handling, bridge reconnect, and a Baresip
restart with an active call. It also runs 12 additional startup/shutdown cycles
with empty, queued, and consumed events, rejects forced-kill or nonzero exits,
and checks active-dialog closure and per-call audio cleanup.
It does not contact a provider and does not replace provider/NAT,
codec, sanitizer, or long-running acceptance. Run it from a checkout only
after the native runtime is available, or from the final image:

```sh
scripts/sip-peer-smoke.sh
# equivalent image invocation:
docker run --rm --entrypoint /usr/local/bin/voxmail-sip-peer-smoke \
  voxmail:<tested-tag-or-digest>
```

`VOXMAIL_E2E_SIP_PEER=1 scripts/e2e.sh` runs the same isolated test in a
short-lived container before the normal HTTP/restart E2E flow. The peer's
authentication and RTP behavior follows [RFC 3261](https://www.rfc-editor.org/rfc/rfc3261),
[RFC 3551](https://www.rfc-editor.org/rfc/rfc3551), and
[RFC 4733](https://www.rfc-editor.org/rfc/rfc4733). A successful local run is
evidence for the packaged native boundary only; live registrar and provider
matrix rows remain validation gates.

### Acceptance layers and what each proves

Record the source revision, command, fixture configuration and results for each
applicable layer. A configured CI job describes a gate; a completed run and its
artifacts provide evidence. Passing one layer does not satisfy the others.

| Layer | Reproduction | Evidence and limits |
| --- | --- | --- |
| HTTP API and restart E2E | `scripts/e2e.sh` | Builds a disposable container, exercises authentication, validation, roles, cross-user isolation and persistence after restart. The default is API-only, with calls off and no fake speech models; it does not drive a browser or prove provider/SIP/speech compatibility. |
| Rendered browser | `scripts/browser-test.sh` after the browser prerequisites | Chromium exercises the real console, navigation, keyboard/accessibility, responsive layouts and session/role boundaries. It uses an offline disposable app, metadata-only voice fixture, mocked provider validation and controlled HTTP responses for selected error flows. These do not prove a usable speech model or external delivery. See the browser README for scenario boundaries. |
| Controlled IMAP/SMTP | `go test -race -count=1 -tags integration -timeout 5m -v ./tests/providers` with `mbsync` installed | Loopback protocol servers and private fixture CAs exercise real TLS/client/sync behavior, folders/attachments, disconnects, UIDVALIDITY, SMTP uncertainty and recovery delivery. Production private-endpoint restrictions remain enforced; the fixture transport is separate. External provider interoperability requires its own evidence. |
| Deterministic native SIP/RTP | `scripts/sip-peer-smoke.sh` with the native runtime, or `VOXMAIL_E2E_SIP_PEER=1 scripts/e2e.sh` | A disposable local peer verifies real Baresip/module registration, both call directions, audio, DTMF, reconnect and teardown. This is independent of the API-only phase and does not prove carrier/NAT behavior. |
| Real speech models | `scripts/model-acceptance.sh IMAGE@sha256:DIGEST linux/ARCH` | Checksum-verified real Piper/Whisper fixtures run with networking disabled and include a synthesis/transcription round trip. Browser metadata fixtures and API tests cannot substitute for this gate. |
| Exact published artifacts | Publication workflow on both native architectures | Build-output digests bind scans and runtime/model results to the source and platform, then form the published manifest. Local source tests or a tag's older success do not prove these exact artifacts pass. Review retained release, scan and acceptance records. |
| Deployment providers and topology | Authorized release-readiness matrix | Actual mail provider, registrar, carrier, TLS/proxy/session topology and target host checks supplement deterministic local evidence. Never point destructive disposable test harnesses at production state. |

Normal CI configures API/restart, browser, controlled provider and local SIP
checks; model acceptance is periodic/manual and required before versioned image
publication. Both-architecture acceptance is evaluated per immutable digest.
See [browser fixture details](../tests/browser/README.md),
[controlled provider details](../tests/providers/README.md), and the
[release readiness matrix](RELEASE-READINESS.md) for remaining deployment gates.

## Health and first-run checks

```sh
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

`GET /healthz` returns HTTP 200 with `status: ok` while the HTTP process can
respond, including during graceful shutdown. It does not check SQLite, provider
connections, SIP registration or speech models.

`GET /readyz` returns HTTP 200 with `status: ready` only after its local SQLite
health check and application checks pass. In normal mode these require the
mail-sync worker to be running. When SIP is enabled, they also require the
managed SIP process to be running, registration to be `registered`, the call
bridge to be connected, a nonempty Piper model with valid companion configuration,
and a nonempty Whisper model. Shutdown, database failure or a failed application
check returns HTTP 503 with `status: not_ready` and a `reason`. Startup can remain
not ready while these dependencies initialize. Runtime state fields, when
available, distinguish process, bridge, registration and media state.

The Compose health check uses `/readyz`; an unhealthy container can still serve
the console and diagnostic endpoints. Readiness checks worker/model availability,
not the success of every account's last sync, SMTP delivery, neural inference,
or a complete SIP/RTP call. Inspect authenticated operational status and run the
applicable acceptance tests for those outcomes. In offline mode the application
checks skip provider/call dependencies; SQLite and shutdown checks still apply.

For local maintenance, set `VOXMAIL_OFFLINE_MODE=1` and recreate the container.
This overrides enabled SIP/alert settings and forced SIP accounts, suppresses
model provisioning and provider workers, and rejects manual account tests,
sync, test calls, recovery-email delivery and voice installation with HTTP 503.
Local inspection, authentication, configuration edits, installed-voice preview
and periodic retention remain available. SIP edits are saved without applying
the call client. `/api/v1` and `/readyz` report `offline_mode`; readiness in this
mode checks the local database. Stop cancels active HTTP operations, admitted background syncs and
provider workers, drains calls while the bridge remains alive, joins handlers
and jobs, and closes the database last.

The browser shows
**First-run setup** only while the database contains no users; after setup,
sign in normally. Perform setup through host loopback or an authenticated
administrative tunnel before exposing the console remotely. There is no
separate bootstrap token: the supported security boundary is the restricted
initial endpoint plus the store's atomic one-winner setup transaction. A
deployment that exposes an empty database directly to an untrusted network can
allow that network to create the first administrator. Setup requests are
limited to five attempts per source address in 15 minutes, and at most two
credential-hashing requests run at once. Those limits reduce resource abuse;
they do not replace keeping the initial endpoint on a trusted administrative
path.

Sign-in reserves attempts before checking credentials. At most four sign-in
requests perform verification concurrently, with at most two for one username,
and at most 120 attempts begin per minute across the deployment. Five failed
attempts from a source address or its source/username pair impose a five-minute
cooldown; pending checks count toward that budget. There is no failed-password
cooldown shared across sources for a username. The limiter retains at most
4,096 source/pair keys, cleans idle state no more than once per minute, and
rejects untracked sources if full. Trust forwarding headers only from explicitly
configured proxies, as described below.

Web sessions expire after 12 hours. Storage accepts at most 4,096 sessions
globally and 16 per user, including authentication reservations. Overflow returns
HTTP 429 with `Retry-After` and preserves existing sessions. Sign out of an
existing session to release capacity. Expiry cleanup runs at most once per
minute during session access. A login or TOTP-bypass redemption reserves capacity
before consuming its one-use code; password changes atomically replace that
user's sessions without releasing their quota to other concurrent logins.

The baresip control bridge accepts at most 256 queued call commands and
returns a structured `command_error` with code 429 when that queue is full.
Commands are executed in bounded batches of 32 on the baresip loop; a stale
call ID returns 481, and a bridge that is shutting down returns 503. These
responses are expected backpressure signals, not permission to retry
indefinitely; callers should apply their normal bounded retry policy.

## Data and backups

All application state is under `/data`:

- `sqlite/voxmail.db` — users, settings, metadata, indexes, and audit records;
- `mail/` — synchronized Maildirs;
- `config/mbsync/` — generated per-account sync policy;
- `drafts/` — private draft attachment generations;
- `quarantine/` — owner-scoped recovery copies from conservative mail reconciliation;
- `voices/` and `whisper/` — model files;
- `prompts/` — static recordings and generated prompt cache;
- `recordings/` — short-lived voice-composition work files;
- `run/` and `logs/` — runtime state and logs.

The storage classes have different retention rules:

| Location/table | Contents | Automatic lifecycle |
| --- | --- | --- |
| `sqlite/voxmail.db` | Credentials, settings, indexes, audit, sync and mutation history | Expired security rows and terminal durable history are pruned at startup and every five minutes using `VOXMAIL_HISTORY_RETENTION_DAYS` and `VOXMAIL_HISTORY_MAX_ROWS`; pending, queued, and uncertain rows are retained |
| `mail/` | Synchronized message bodies and attachments | Per-account local retention after sync; remote mail is never removed by that policy |
| `config/mbsync/` | Generated account-specific synchronization configuration | The account file is removed during owner-authorized account deletion; temporary staging files are handled by bounded startup and periodic cleanup |
| `drafts/` | Private draft attachment generations | Exact account-owned draft directories and DB-referenced attachment paths are removed during account deletion; unowned staging files remain subject to the periodic stale-stage policy |
| `quarantine/` | Recovery copies of locally stale mail | Account-scoped reconciliation quarantine is retained for operator recovery until that account is deleted or an explicit storage policy removes it |
| `voices/`, `whisper/` | Downloaded speech models | Persistent until an administrator replaces/removes them |
| `prompts/` | Shipped/static audio and generated speech cache | Generated cache is capped at 512 files/128 MiB after synthesis; periodic cleanup expires idle cache after seven days and preserves active playback; static assets persist |
| `recordings/`, `run/voxmail/` | Caller capture, temporary conversion files, and call FIFOs | Known abandoned temporary files and call paths older than 24 hours are removed in bounded startup and periodic passes; ownership leases preserve active files and the running native child's audio tree |
| `logs/` and container stdout | Baresip/application diagnostics | Baresip's writer rotates during execution, retaining a current file and one previous file of at most 64 MiB each (128 MiB total); Compose JSON logs retain three 10 MiB files |

The Baresip limit is enforced while writing, including writes larger than the
limit, without restarting the process. Startup also trims oversized logs from
older versions to their newest 64 MiB. Renaming an output file externally does
not provide this guarantee: an existing writer can keep using the old file
descriptor.

The service emits a warning below 1 GiB of free space. Treat that warning as
an operational incident: do not delete arbitrary files from `/data`, because
Maildirs, encrypted credentials, drafts, and model files have different
recovery implications. Historical audit, synchronization, mutation, and
terminal SMTP-submission rows are pruned at startup and every five minutes by
the configured age and per-table row cap. The default is 365 days and 100,000 terminal rows per
table. Unfinished synchronization runs, active jobs, unknown states and
pending/queued/uncertain rows are deliberately excluded because they
may still be needed for reconciliation, retry decisions, or SMTP idempotency.
Lower the settings only after confirming the deployment's audit and recovery
requirements; a backup remains the source of historical records after pruning.

Maintenance runs serially, including in offline mode, with a five-second pass
budget and lifecycle cancellation. Each SQL statement removes at most 256 rows
and releases the database connection before the next statement; age and quota
checks can each remove one batch per table. Large backlogs converge over later
passes rather than holding a transaction until every expired row is gone. Sync
retention uses completion time. Terminal voice-installation jobs retain their
existing one-hour history policy; failed account-deletion jobs stay available
for recovery. The filesystem scanner reads at most 128 entries per known
directory per pass, retains cursors between passes, and visits at most 128 nodes
in one abandoned draft-staging tree. It ignores symlinks and does not recurse
into Maildirs, models or durable draft generations. Active operation leases
remain valid even if wall-clock time advances beyond the retention age.
Cancellation stops and awaits maintenance before the database closes. Failures
are logged and the next scheduled pass retries.

### Audit commit policy

Audit rows are diagnostic history, not the authorization or reconciliation
source of truth. Business state is committed by its owning transaction first;
the corresponding audit insert is best-effort afterward. A failed audit write
must not roll back a successful password/PIN/TOTP change, account mutation,
recovery completion, mail mutation, or sync result, and an audit row must never
be treated as proof that the business operation committed. Security-sensitive
operations still log a safe internal failure when their audit insert fails;
audit details contain action identifiers and bounded safe metadata, not
passwords, tokens, message bodies, or provider credentials. Deployments that
require legally atomic audit trails need a separately designed transactionally
coupled audit store rather than relying on this SQLite best-effort policy.

Account deletion captures the Maildir, generated mbsync account file, exact
draft attachment generations, and that account's reconciliation quarantine
before metadata is finalized. The request stops active synchronization and invalidates calls before removing
metadata, then returns with filesystem cleanup pending. A serialized local worker
runs in both normal and offline mode, resumes durable jobs after restart, and
only admits jobs whose account metadata is gone. Removal uses anchored directory
handles without following symlinks, reads directory entries in chunks of 128,
and checks cancellation between entries. Shutdown cancels and joins the worker
before closing SQLite; interrupted work remains eligible for the next startup.
Filesystem failures stay visible in the console's operational status and retry
automatically after five minutes. The cleanup-job API allows owners or admins to
request an earlier retry after correcting storage; raw paths/errors remain in
restricted server diagnostics. See WEB-CONSOLE.md for the status/retry contract. Temporary draft
staging directories that are not attributable to a committed account draft
are intentionally handled by the periodic stale-stage cleanup rather than by
another account's deletion request.

### Call-alert privacy and cooldown

Alert calls intentionally announce only a generic message count and folder;
they do not read sender names, subjects, message bodies, or mail contents.
This policy applies to voicemail and any other party who answers the configured
alert destination. Normal and test alerts share the persisted per-user cooldown,
so restarting the service does not reset the cooldown window. A failed dial
releases pending-message claims for a later retry.

Stop the service or use a filesystem-consistent volume snapshot before copying
SQLite and Maildirs. Store the encryption key separately from the data archive
but back it up; without it encrypted account passwords cannot be recovered.

The Compose volume has the stable default name `voxmail-data`, regardless of
the Compose project directory. Set `VOXMAIL_VOLUME_NAME` before `docker compose`
if multiple deployments share one Docker host; use the same value for backup
and restore. The backup helper enforces a private `0700` output directory,
installs its restart/cleanup trap before stopping a running service, mounts the
source read-only, and verifies the database and data directories. It writes a
unique staged archive as mode `0600`, records a SHA-256 sidecar and manifest,
publishes the metadata first and archive last, then restarts the service:

```sh
scripts/backup.sh
```

The archive includes SQLite/WAL state, synchronized Maildirs, models, prompt
cache, recordings, drafts, generated mbsync configuration, and logs. The
archive and sidecars are owned by the invoking host user. The encryption key
is not in the archive: back it up separately. Without the same key, encrypted
account passwords cannot be recovered.

Run the non-destructive restore drill against a pinned image digest. It creates
an isolated, labeled volume, restores the archive, starts the image in enforced
offline mode on Docker's `none` network, and checks `/healthz` and `/readyz`.
It also verifies SQLite integrity/foreign keys, representative users and mail,
attachments and credential decryption, and absence of Baresip. A separate volume
protects production files; offline mode suppresses restored provider settings;
the separate network barrier prevents external connections independently.
Volume isolation alone does not isolate the application: the copied database
still contains provider credentials, enabled SIP settings, account sync settings
and alert destinations. Starting an ordinary networked app on that volume can
register SIP, synchronize or mutate remote mail, and place alerts. A loopback-only
web port limits incoming console access; it does not block outbound activity.

The drill combines a fresh volume and disposable container with two independent
controls: `VOXMAIL_OFFLINE_MODE=1` suppresses provider/call workers and manual
provider operations, and Docker `--network none` blocks external connectivity
even if an application gate regresses. Model provisioning is explicitly off.
Checks execute through `docker exec`, so the drill publishes no host ports.
Its health/readiness result verifies local restored state in offline mode,
not provider registration, delivery, remote mutation or call usability.
It removes the drill volume afterward unless
`VOXMAIL_KEEP_RESTORE_DRILL=1` is set:

```sh
VOXMAIL_ENCRYPTION_KEY='<the original key>' \
  scripts/restore-drill.sh backups/voxmail-YYYYmmddTHHMMSSZ-<unique>.tar.gz \
  ghcr.io/s31354915/voxmail@sha256:<release-digest>
```

The restore drill verifies the archive's `.sha256` and `.manifest` sidecars
before extracting it. Timestamp-only archives from older versions remain
usable with a warning.
Keeping the drill volume preserves data for inspection; the drill still removes
its container. Any later container using that volume must explicitly retain both
offline mode and the network barrier until provider activity is intended.

For a production restore, stop the service, create a new isolated volume, and
extract only after verifying the archive checksum. Preserve ownership and
permissions from the archive; the image runs as UID 10001. Inspect it first using
the offline drill. When intentionally resuming production provider activity,
start the pinned image with the same `VOXMAIL_ENCRYPTION_KEY`, test `/readyz`, sign in, and
confirm an account sync before retiring the old volume. Keep at least one
prior archive until the restored deployment has been verified.

For rollback, set `VOXMAIL_IMAGE` to the previous immutable release tag or
digest, run `docker compose pull`, then
`docker compose up -d --force-recreate`. Database migrations are designed to
be forward-compatible, so take a volume backup before upgrading and restore
the backup rather than downgrading a database if a release explicitly says
its schema is not backward-compatible.

## mbsync safety model

If mbsync fails or reaches an explicitly configured runner timeout after
receiving messages, VOXMail indexes complete files already in the Maildir and
keeps the sync marked failed with its original error. This partial import does
not apply retention or the initial cutoff, purge missing-file index records,
reconcile remote identities, or acknowledge queued mutations. A later successful
sync performs normal reconciliation. Application shutdown, account deletion,
and explicit sync cancellation also stop indexing; downloaded files remain on
disk for a subsequent run. If indexing itself fails, both errors appear in the
durable sync outcome.

VOXMail renders one configuration and channel per account. Routine sync uses
UID state, `Sync All`, `Create Slave`, `Remove None`, and `Expunge None`.
Remote folders are not deleted by routine synchronization, and local retention
removes only local Maildir files after synchronization. `Expunge None` means
mbsync does not automatically expunge messages as part of that routine run; it
does not make every operation one-way. Authenticated web/phone actions such as
read-state changes, moves, and Trash operations intentionally mutate the
remote mailbox, and the next sync reconciles those remote changes into the
local cache. Folder aliases are written as explicit local Maildir paths.

The synchronization worker treats provider hosts as untrusted user input in a
multi-user deployment. Before mbsync starts, it resolves the IMAP hostname,
rejects loopback, private, link-local, metadata, multicast, unspecified, and
CGNAT addresses, connects to one allowed numeric address, and places that exact
address in a controlled `voxmail-connect` tunnel. mbsync still receives the
original hostname in `Host`, so TLS certificate verification and SNI use the
provider name; only the TCP destination is pinned. A DNS answer change after
preflight therefore cannot redirect that synchronization run to a reserved
address.

The same pinned address is used by VOXMail's authenticated IMAP reconciliation
path. The fixed tunnel helper performs no DNS lookup and independently rejects
reserved addresses. Private-provider endpoints are intentionally not enabled by
the production mbsync path; allowing them requires an explicitly isolated,
reviewed deployment change rather than a user-controlled setting.

## Troubleshooting

### The web UI stays at setup

Confirm the container is using the same persistent volume. A new volume is a
new database and will correctly show setup again.

### Mail does not appear

Check logs, account hosts/users/ports, and whether the account passwords were
saved. **Test IMAP and SMTP** checks only that account's configured services;
it does not prove that other accounts or the deployed provider are healthy.

## Reverse proxy trust

If a reverse proxy terminates TLS, set `VOXMAIL_TRUSTED_PROXY_CIDRS` to the
proxy's exact source CIDR(s), for example `127.0.0.1/32` or a private
container-network range. VOXMail ignores `X-Forwarded-Proto` and
`X-Forwarded-For` from unlisted peers. The proxy should overwrite, not append
to, the client forwarding headers before passing requests to VOXMail.

### Calls are not admitted

Confirm baresip registered (SIP connection panel or logs), the caller is
whitelisted, and the PIN is entered followed by `#`. Check baresip and VOXMail
logs. SIP needs TCP/UDP 5060 and RTP needs UDP 10000–10100 reachable from the
provider.

Unexpected Baresip exits wait 2, 4, 8, 16, then at most 30 seconds before
restarting. Successful process creation preserves this crash history; a child
must remain alive for one minute to reset it. A failure to start also increases
the delay, with five failed start attempts before automatic retries stop.
Reapplying SIP settings can retry an abandoned start. Stop and configuration
changes cancel or supersede pending restarts. Check the application log for the
planned restart delay and failure reason; a running process alone does not prove
successful registration.

### Speech is slow on the first call

The shipped welcome and fixed signed-in menu are static WAVs. Dynamic prompts
wait for the call-scoped Piper/Whisper warmup. Confirm the selected voice file
exists and that the container has enough memory. Resources close after the
last active call lease ends.

### A model was replaced

At startup VOXMail compares the static prompt manifest's model name and
SHA-256 with the configured Piper model. If the model exists and differs, both
static recordings are regenerated in a private staging directory and the
manifest is committed last. If the replacement is unavailable, the shipped
recordings remain usable and a warning is logged.

## Verbose diagnostics

Set `VOXMAIL_VERBOSE=true` in the deployment environment (or Compose `.env`)
and recreate the service with `docker compose up -d --force-recreate voxmail`.
Use `docker compose logs --timestamps voxmail` for JSON Go diagnostics and
process operation IDs, elapsed times, outcomes, and exit codes. Baresip and
native bridge output remains in `/data/logs/baresip.log`, under the existing
runtime size limit. Return to `VOXMAIL_VERBOSE=false` and recreate the service
after troubleshooting.

The switch selects mbsync's `--verbose`, Piper's `--debug`, FFmpeg's verbose
log level, Baresip's timestamped debug output, and Whisper's normal engine
statistics instead of suppressing prints. It does not enable SIP packet traces,
mbsync protocol dumps, or Whisper audio debug dumps. Process traces omit raw
arguments, request bodies, input speech and transcriptions. Child diagnostic
lines are bounded and credential-bearing lines are omitted; Baresip DTMF values
are also redacted, including in normal mode. IVR traces identify
the call, authentication state, menu state, and DTMF phase without printing keys.

Run `scripts/verbose-test.sh LOCAL_IMAGE_REFERENCE` to check both modes with
checksum-verified real models and the production Go execution paths. The
engine containers use no network and read-only model mounts. The separate SIP
peer checks use only local sockets and test registration, audio, DTMF, restart,
and shutdown with native diagnostics enabled and disabled. The supplied image
must be built from the current native source.

## Child-process execution limits

Piper, Whisper, FFmpeg and mbsync share the `internal/process` runner. Captured
stdout/stderr retain at most 1 MiB each and report truncation while continuing
to drain the pipe. Whisper rejects truncated transcription rather than using
partial text. Playback streams PCM through its existing ten-minute byte cap.

Piper synthesis and Whisper recognition each allow at most two minutes;
FFmpeg recording conversion allows 30 seconds and playback allows ten minutes.
A shorter caller deadline wins. mbsync intentionally has no default elapsed-time
limit, so large initial imports can complete; an explicitly configured runner
timeout and application/account/request cancellation still stop it.

On Linux, each child owns a process group. Cancellation kills that group, and
the runner bounds inherited-pipe joins to 500 ms and terminates leftover helpers
after the direct child finishes. Other platforms bound pipe joins and cancel
the direct child. Audio recording uses a separate normal-finish signal, so
pressing the finish key retains the call-owned context for conversion; hangup,
navigation cancellation and shutdown abort conversion and clean temporary files.

### Operational failure categories

Provider and child-process adapters attach a stable category and local
component/operation to failures while preserving the underlying error for
`errors.Is`/`errors.As`. Categories distinguish configuration, cancellation,
timeout, DNS, connection, TLS, authentication, outbound endpoint policy,
process exit, missing dependencies, disk space, storage permissions, mailbox
indexing/identity, protocol rejection, integrity verification, resource limits,
and uncertain remote outcomes. Verbose completion traces include the category;
wrapped child exit codes remain available in the trace.

An `uncertain` outcome takes precedence over a timeout or cancellation: a lost
SMTP DATA acknowledgement or disconnected IMAP mutation cannot prove whether
the provider applied the action. Reconcile remote state before retrying. An
accepted SMTP submission's cleanup failure remains separate from its outcome.

Sync records persist `error_category` independently of restricted raw
`error` diagnostics, preserving the category after restart. HTTP provider
failure responses expose fixed, safe `failure.category`, `failure.reason` and
`failure.action` fields alongside the existing HTTP error code and request ID.
The console also shows bounded numeric child-exit or SIP-response diagnostics.
Historical sync rows without a category use the previous compatibility renderer;
unknown categories receive a generic safe explanation. Classification of new
Go failures uses types and sentinels. Text-only adapters interpret known mbsync
signals or the native SIP response code at the boundary; provider prose never
becomes the user-facing explanation.


### HTTP deadlines and background jobs

The HTTP server uses a 90-second write timeout, replacing the former 35-minute
budget. Bounded provider checks and recovery SMTP submission remain normal
requests. Manual mailbox synchronization, voice installation, preview and account-file cleanup use
tracked background work; their execution contexts belong to the application,
not the requesting connection. Manual sync keeps its cancellation-only lifetime
for large imports and exposes its durable run ID/status. Preview has a two-minute
execution budget with bounded temporary results. See WEB-CONSOLE.md for endpoint,
ownership, admission and result-expiry behavior. Application shutdown cancels
and joins these jobs before shared storage closes.


### Workflow action revisions

Every external action in CI, publication and periodic model acceptance uses a
full immutable commit SHA. Version comments identify the upstream version line
for review; the comment does not participate in execution. Normal Go CI runs
`TestWorkflowActionsUseImmutableReferences`, which rejects mutable tag/branch
references in every workflow and requires digests for container actions. Local
actions inherit the checked-out source revision.

On 2026-10-08, the upstream tag refs resolved to these commits (Trivy's annotated
tag was peeled to its commit). Their action metadata was fetched at each
commit to verify it exists and inspect composite dependencies:

| Action | Upstream reference | Immutable commit |
| --- | --- | --- |
| actions/checkout | v4 | 11d5960a326750d5838078e36cf38b85af677262 |
| actions/setup-go | v5 | 40f1582b2485089dde7abd97c1529aa768e1baff |
| actions/setup-node | v4 | 49933ea5288caeca8642d1e84afbd3f7d6820020 |
| actions/upload-artifact | v4 | ea165f8d65b6e75b540449e92b4886f43607fa02 |
| actions/download-artifact | v4 | d3f86a106a0bac45b974a628896c90dbdf5c8093 |
| docker/login-action | v3 | c94ce9fb468520275223c153574b00df6fe4bcc9 |
| docker/setup-buildx-action | v3 | 8d2750c68a42422c14e847fe6c8ac0403b4cbd6f |
| docker/build-push-action | v6 | 10e90e3645eae34f1e60eeb005ba3a3d33f178e8 |
| aquasecurity/trivy-action | v0.36.0 | ed142fd0673e97e23eac54620cfb913e5ce36c25 |

Trivy's composite action also pins setup-trivy to
`3fb12ec12f41e471780db15c232d5dd185dcb514` (v0.2.6) and actions/cache to
`27d5ce7f107fe9357f9df03efb73ab90386fccae` (v5.0.5). setup-trivy in turn pins actions/cache restore/save to
`9255dc7a253b0ccc959486e2bca901246202afeb` (v5.0.1) and checkout to
`8e8c483db84b4bee98b60c0593521ed34d9990e8` (v6.0.1). Those leaf actions have no
further composite action references. Review both direct and composite action
revisions when updating pins. A commit pin freezes action code;
base images, downloaded tools and language dependencies have their own update
and integrity policies.


### Publication token permissions

The publication workflow defaults to `contents: read`, `actions: read` and
`packages: read`. Verification, CI coordination, scanning and exact-digest runtime
acceptance cannot upload images or move package aliases. Only `build-native`
(staging image upload) and `publish-manifest` (verified alias/manifest promotion)
receive job-scoped `packages: write`. Their explicit job permissions retain
checkout/artifact read access without granting repository-content writes.
Read-only package access remains available for pulling private staging images.
CI and periodic model acceptance grant no package-write permission. Normal Go
checks enforce this scope across the current workflows.


### Hashed Python runtime dependency lock

`requirements/python.in` records reviewed runtime roots, including Piper 1.8.0
and the setuptools/jaraco.context/wheel upgrades. `requirements/python.lock`
freezes the complete Python 3.11 dependency graph and approved amd64/arm64 wheel
SHA-256 hashes. Docker installs from that file with `--require-hashes` and
`--only-binary=:all:`, then runs `pip check`; missing dependencies, unapproved
archives and source distributions fail the build. The shipped provenance directory
contains both the lock and its SHA-256 checksum. Python/pip supplied by the base
remain covered by the immutable base-image digest.

Generate updates using the pinned Python base, because resolving on a different
interpreter can miss version-specific dependencies such as backports.tarfile.
The generator resolves amd64 first, resolves arm64 at the same exact versions,
and validates dependency markers and wheel identity for both targets. It writes
only the lock after both graphs pass. Review version/hash changes before using
them; regeneration can select newer transitive versions allowed by the roots.

From the repository root on an amd64 host:

```sh
docker run --rm --platform linux/amd64 \
  --mount type=bind,source="$PWD",target=/src --workdir /src \
  python:3.11-slim-bookworm@sha256:0a310eeecf4e1f5a0743f9a6520c90c88d089c903ca5fd283f501e3a805f5f89 \
  python scripts/lock-python.py
docker run --rm --platform linux/amd64 \
  --mount type=bind,source="$PWD",target=/src,readonly --workdir /src \
  python:3.11-slim-bookworm@sha256:0a310eeecf4e1f5a0743f9a6520c90c88d089c903ca5fd283f501e3a805f5f89 \
  python scripts/check-python-lock.py
```

The verifier downloads and hash-checks both complete wheel sets, installs the
native set into a fresh virtualenv without network access, checks dependencies,
imports Piper/NumPy/ONNX Runtime and checks Piper's CLI. An offline negative
control substitutes incorrect hashes and must fail with a hash-mismatch error.
All fixtures are disposable under `/tmp`. Normal Go CI also guards lock/root
consistency and the Docker install flags, while the Python-dependency CI job
runs the full verifier. Wheel validation for arm64 proves dependency resolution
and archive integrity; executing the arm64 libraries and synthesizing with real
models remain separate native/exact-image acceptance checks.

This follows [pip's secure-install guidance](https://pip.pypa.io/en/stable/topics/secure-installs/):
all requirements and dependencies need exact pins and local hashes, and binary-only
installation avoids an unreviewed source-build dependency graph.


### Reviewed dependency update proposals

`.github/dependabot.yml` groups Docker base image tags/digests, immutable action
pins, Go modules, both npm projects and Python runtime roots into one weekly
version-update PR. Python base major/minor updates are held for an explicit interpreter/lock
port; digest and compatible patch refreshes remain eligible. Dependabot discovers
Python roots through `requirements/requirements.txt`, which includes `python.in`.
It does not replace the custom cross-architecture lock: a root-version proposal
must regenerate and verify `python.lock` before its consistency check can pass.
For console tooling changes, regenerate the embedded bundle as part of review.

`Review Python lock updates` separately runs weekly or by manual dispatch. Its
read-only job records the source commit, derives the pinned Python base from
Dockerfile, resolves both target wheel sets, and runs the complete hashed-lock
verifier. A subsequent job receives the verified lock artifact at that source
revision and can commit only `requirements/python.lock` to the reserved
`automation/python-runtime-lock` proposal branch. It has repository/PR write
permission, but no package-write permission, and does not execute downloaded
Python dependencies. Checkout credentials are not persisted in either job.
Unchanged locks produce no new proposal; changed locks create/update a draft.

The proposal action is pinned to
`peter-evans/create-pull-request@22a9089034f40e5a961c8808d113e2c98fb63676` (v7).
It uses `draft: always-true`, so updates return the proposal to draft. Review the
version/hash diff and generated-run evidence before marking it ready. CI listens
to `ready_for_review` as well as opened/synchronize/reopened events. This human
transition starts CI even when the proposal was created using GITHUB_TOKEN,
whose generated PR/push events do not normally start further workflows. The
repository must permit GitHub Actions to create PRs; no PAT is required by the
configuration. Update proposals never enable auto-merge, invoke publication or
move image aliases. Maintainers still perform normal review and the applicable
native/model/provider/browser acceptance before merging or releasing.

These configurations take effect once present on the default branch. Review the
first scheduled/manual run and resulting proposal rather than treating local
configuration checks as evidence of a live GitHub run. Native source revision
arguments and model hash changes still require their dedicated compatibility and
provenance review; they are not silently refreshed by this lock updater.


### Retained vulnerability scan evidence

Each native image scan emits `trivy.json` using the existing fixed
HIGH/CRITICAL blocking policy and keeps its nonzero exit on findings. The artifact
`voxmail-scan-ARCH-RUN_ID-RUN_ATTEMPT` retains that JSON and `identity.json` for
30 days, including when findings prevent publication. Identity records the exact
source commit, immutable registry reference, selected platform, run/attempt,
policy, scanner outcome and report availability. Available reports include their
SHA-256 and byte length. The JSON is the policy-filtered report, not an assertion
that unreported severities or unfixed vulnerabilities do not exist.

Outcome capture and artifact upload run with `always()`; neither uses
`continue-on-error`. Malformed or missing reports fail validation and are recorded
as invalid/missing rather than an empty successful scan. A scanner failure with a
valid findings report remains a failed job with preserved evidence. Runtime tests
and alias promotion continue to require successful scans for both architectures.
If image resolution or identity preparation failed first, the job remains failed
and the runner log identifies why no report/identity artifact could be produced.
Export required release evidence before its artifact retention expires.

### Release identity and acceptance summary

Publication validates the downloaded scan and runtime evidence against the source
commit, immutable architecture digests, platforms and required scan/model policy
before changing public aliases. A successful job result alone is insufficient:
missing records, mismatched identities, changed report hashes, failed SIP tests or
skipped required real-model checks prevent promotion.

Each native runtime job retains `voxmail-acceptance-ARCH-RUN_ID-RUN_ATTEMPT` for
30 days. Its `acceptance.json` records the image digest, source, platform, SIP
outcome and real-model outcome, including failed or skipped steps. Real-model
acceptance is required for versioned releases; ordinary `latest` runs report it
as skipped. Scheduled model acceptance remains a separate workflow.

The read-only release-summary job runs after successful or failed prerequisite
jobs and writes a GitHub job summary plus `release.json` and `release.md` in
`voxmail-release-RUN_ID-RUN_ATTEMPT`, retained for 90 days. These record the source,
tested architecture digests/platforms, scan and acceptance results, job results,
published manifest digest when available, latest-promotion outcome, and exact
artifact names. Missing evidence is reported explicitly; completed scans remain
visible even when later runtime acceptance is missing. `acceptance_complete`
describes the prerequisite evidence, while `publish-manifest` and the manifest
digest describe publication. It does not claim that aliases were published when
the publication job failed or was skipped. Runner cancellation or checkout/upload
failure can prevent the summary artifact; retain the workflow log in that case.

### Immutable versioned release aliases

Every publication tag except the exact tag `latest` is immutable, including
prerelease tags and its `-amd64`/`-arm64` aliases. Repeating a publication is
allowed only when existing target aliases already reference the desired exact
digests. A conflicting digest requires a new release tag. `latest` and its
architecture aliases can advance after the normal acceptance gates; explicit
promotion of a versioned release to `latest` remains supported.

The publisher first creates a unique staging index from the accepted architecture
digests. It then lists registry tags and checks every existing versioned target
against its desired digest before writing any public alias. Authentication,
network, tag-listing and digest lookup errors abort publication. After promotion
each public alias is resolved and verified. This makes a partial registry-write
failure retryable with the same artifacts and prevents an existing conflicting
manifest from leaving newly moved architecture aliases. The helper uses the
pinned crane client's paginated tag listing and remote digest-based tagging:
[tag listing implementation](https://github.com/google/go-containerregistry/blob/v0.22.1/cmd/crane/cmd/list.go)
and [remote tag command](https://github.com/google/go-containerregistry/blob/v0.22.1/cmd/crane/cmd/tag.go).

Repository publication runs share the serialized queue. Registry tags do not
provide an atomic compare-and-set here: actors outside that queue with package
write access can bypass or race this policy. Keep public release writes within
this workflow; an external change or interrupted registry operation requires
inspection of the recorded digests rather than assuming a multi-tag transaction.
