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

The manually triggered GitHub Action tests the selected source commit, requires
the matching CI checks, builds native amd64 and arm64 images, records their
registry digests, scans both images, and combines those digests on GHCR. It
does not run automatically. Test/prerelease tags do not advance `latest`; that
alias requires an explicit production-promotion input. See the root README for
the workflow inputs and generated tags. Fixed HIGH/CRITICAL scanner findings
block publication. Any future exception must be reviewed, state the reason and
owner, and include a short expiry date; do not add a permanent blanket ignore.

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

The current base pins were resolved on 2026-09-28. A digest update is a
dependency change: review ABI/loader output, speech CLI behavior, baresip
module loading, mbsync validation, and both amd64 and arm64 smoke results
before accepting it.

The real-model smoke test is opt-in and must never use placeholder model files:

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

The native SIP boundary has a separate opt-in localhost peer test. It runs the
real Baresip binary and the real `voxmail` module against a disposable Python
peer, then verifies digest-authenticated registration, both call directions,
request-ID correlation, negotiated RTP audio in both directions, RFC 4733
telephone events, repeated hangup handling, bridge reconnect, and a Baresip
restart. It does not contact a provider and does not replace provider/NAT,
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

## Health and first-run checks

```sh
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

`healthz` confirms the HTTP process responds. `readyz` additionally checks
SQLite, the mail-sync worker, and—when SIP calls are enabled—the baresip bridge
and configured Piper/Whisper model files. A temporary `not_ready` response
after startup is expected while the workers initialize. The browser shows
**First-run setup** only while the database contains no users; after setup,
sign in normally. Perform setup through host loopback or an authenticated
administrative tunnel before exposing the console remotely. There is no
separate bootstrap token: the supported security boundary is the restricted
initial endpoint plus the store's atomic one-winner setup transaction. A
deployment that exposes an empty database directly to an untrusted network can
allow that network to create the first administrator.

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
| `sqlite/voxmail.db` | Credentials, settings, indexes, audit, sync and mutation history | Expired security rows and terminal durable history are pruned at startup using `VOXMAIL_HISTORY_RETENTION_DAYS` and `VOXMAIL_HISTORY_MAX_ROWS`; pending, queued, and uncertain rows are retained |
| `mail/` | Synchronized message bodies and attachments | Per-account local retention after sync; remote mail is never removed by that policy |
| `config/mbsync/` | Generated account-specific synchronization configuration | The account file is removed during owner-authorized account deletion; temporary staging files are handled by startup cleanup |
| `drafts/` | Private draft attachment generations | Exact account-owned draft directories and DB-referenced attachment paths are removed during account deletion; unowned staging files remain subject to the startup stale-stage policy |
| `quarantine/` | Recovery copies of locally stale mail | Account-scoped reconciliation quarantine is retained for operator recovery until that account is deleted or an explicit storage policy removes it |
| `voices/`, `whisper/` | Downloaded speech models | Persistent until an administrator replaces/removes them |
| `prompts/` | Shipped/static audio and generated speech cache | Generated cache is capped at 512 files/128 MiB and seven days; static assets persist |
| `recordings/`, `run/voxmail/` | Caller capture, temporary conversion files, and call FIFOs | Known stale temporary files and call paths older than 24 hours are removed at startup; active files are never recursively scanned |
| `logs/` and container stdout | Baresip/application diagnostics | Baresip file logs rotate at 64 MiB; Compose JSON logs retain three 10 MiB files |

The service emits a warning below 1 GiB of free space. Treat that warning as
an operational incident: do not delete arbitrary files from `/data`, because
Maildirs, encrypted credentials, drafts, and model files have different
recovery implications. Historical audit, synchronization, mutation, and
terminal SMTP-submission rows are pruned at startup by the configured age and
per-table row cap. The default is 365 days and 100,000 terminal rows per
table. Pending/queued/uncertain rows are deliberately excluded because they
may still be needed for reconciliation, retry decisions, or SMTP idempotency.
Lower the settings only after confirming the deployment's audit and recovery
requirements; a backup remains the source of historical records after pruning.

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
before metadata is finalized. Cleanup is owner-root and symlink checked; a
filesystem failure leaves a durable cleanup job for retry. Temporary draft
staging directories that are not attributable to a committed account draft
are intentionally handled by the startup stale-stage cleanup rather than by
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
and restore. The backup helper stops a running service, mounts the source
read-only, verifies the database and data directories, and restarts the service
afterward:

```sh
scripts/backup.sh
```

The archive includes SQLite/WAL state, synchronized Maildirs, models, prompt
cache, recordings, drafts, generated mbsync configuration, and logs. The
encryption key is not in the archive: back it up separately. Without the same
key, encrypted account passwords cannot be recovered.

Run the non-destructive restore drill against a pinned image digest. It creates
an isolated, labeled volume, restores the archive, starts the image with calls
disabled, and checks `/healthz`; it removes the drill volume afterward unless
`VOXMAIL_KEEP_RESTORE_DRILL=1` is set:

```sh
VOXMAIL_ENCRYPTION_KEY='<the original key>' \
  scripts/restore-drill.sh backups/voxmail-YYYYmmddTHHMMSSZ.tar.gz \
  ghcr.io/s31354915/voxmail@sha256:<release-digest>
```

For a production restore, stop the service, create a new isolated volume, and
extract only after verifying the archive checksum. Preserve ownership and
permissions from the archive; the image runs as UID 10001. Start the pinned
image with the same `VOXMAIL_ENCRYPTION_KEY`, test `/readyz`, sign in, and
confirm an account sync before retiring the old volume. Keep at least one
prior archive until the restored deployment has been verified.

For rollback, set `VOXMAIL_IMAGE` to the previous immutable release tag or
digest, run `docker compose pull`, then
`docker compose up -d --force-recreate`. Database migrations are designed to
be forward-compatible, so take a volume backup before upgrading and restore
the backup rather than downgrading a database if a release explicitly says
its schema is not backward-compatible.

## mbsync safety model

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
