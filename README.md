# VOXMail

> **Beta software:** VOXMail is not yet stable. Until a stable release is
> announced, upgrades may introduce breaking configuration, database, API,
> IVR, or deployment changes. Back up `/data`, pin the image you deploy, and
> test upgrades before using them for important mail or telephone service.

VOXMail is a self-hosted telephone email assistant. It synchronizes IMAP mail
into private local Maildirs, indexes it locally, and gives approved callers a
PIN-protected SIP/DTMF interface for reading, composing, replying, forwarding,
and managing mail. A same-origin web console handles setup and configuration
that would be unsafe or impractical over a telephone.

The published container is designed for `linux/amd64` and `linux/arm64`. It
contains the Go service, the embedded Baresip call client and VOXMail native
module, `mbsync`, Piper, Whisper CLI, FFmpeg, and static first-call audio.
Speech models are stored in the persistent data volume and are downloaded only
when explicitly provisioned or installed.

## Before you deploy

VOXMail handles mailbox credentials, telephone access, speech recordings, and
mail content. Treat the Docker host and the `/data` volume as sensitive.

- Generate a unique encryption key and store a protected copy outside the
  volume. Without it, encrypted IMAP/SMTP/SIP credentials cannot be recovered.
- Keep the web console on host loopback, an SSH tunnel, or a deliberately
  configured HTTPS reverse proxy. Do not expose an empty database directly to
  an untrusted network: the first setup request creates the administrator.
- Add only trusted telephone identities to the caller whitelist. Caller ID is
  an admission hint, not a cryptographic identity; the PIN remains required.
- Pin a release tag or digest for production. `latest` is a moving convenience
  tag, not a change-control mechanism.
- Back up `/data` and the encryption key separately before upgrades.

## Documentation

- [Operations](docs/OPERATIONS.md) — installation, models, upgrades, backups,
  health checks, networking, troubleshooting, and release procedures.
- [Settings reference](docs/SETTINGS.md) — environment variables, API fields,
  defaults, limits, ownership, and restart behavior.
- [Web console](docs/WEB-CONSOLE.md) — setup, accounts, callers, contacts,
  users, SIP, alerts, security, and voice settings.
- [IVR guide](docs/IVR.md) — call flow, DTMF controls, multi-tap text entry,
  mail actions, drafts, and voice composition.
- [Architecture](docs/ARCHITECTURE.md) — synchronization safety, SIP/media
  ownership, model lifecycle, alerts, and data boundaries.
- [Release readiness](docs/RELEASE-READINESS.md) — what has been verified,
  what remains deployment-dependent, and the promotion checklist.
- [Browser test guide](tests/browser/README.md) — the isolated Chromium
  scenario for the web console.

## Quick start with Docker Compose

The default Compose file pulls the multi-architecture image from GHCR and
publishes the web console only on host loopback.

```sh
cp .env.example .env
openssl rand -base64 32
# Put the generated value in VOXMAIL_ENCRYPTION_KEY in .env.

docker compose pull
docker compose up -d
docker compose logs -f voxmail
```

Open `http://127.0.0.1:8080/` on the Docker host. On an empty named volume,
the sign-in page offers one-time **First-run setup**. Create the administrator
from loopback or an authenticated administrative tunnel before making the
console reachable to other networks. Once a user exists, setup is permanently
disabled for that database and normal sign-in is required.

The default image is:

```text
ghcr.io/s31354915/voxmail:latest
```

For controlled deployment, set an immutable release tag or digest in `.env`:

```dotenv
VOXMAIL_IMAGE=ghcr.io/s31354915/voxmail:v2.1.0
# or:
# VOXMAIL_IMAGE=ghcr.io/s31354915/voxmail@sha256:...
```

Then pull and recreate the service:

```sh
docker compose pull
docker compose up -d --force-recreate
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

`healthz` means the HTTP process responds. `readyz` also checks the database
and application readiness, and can remain not ready while synchronization,
speech, or the managed call client initializes.

## Telephone setup

Configure the provider in the administrator-only **SIP connection** panel:

- registrar/domain, SIP username, and password;
- local bind port and registrar port, independently configurable and defaulting
  to 5060;
- UDP, TCP, or TLS transport; and
- registration interval.

Saving applies the generated Baresip configuration and restarts the managed
call client. The default RTP range is UDP 10000–10100. Publish the signaling
and RTP ranges required by the provider, account for NAT, and validate the
provider's authentication, TLS, codec, and inbound-call behavior before
relying on the service.

The web console is the normal source of SIP credentials. The optional
`VOXMAIL_SIP_ACCOUNT` environment override exists for deployments that need a
complete Baresip account line; never commit that value.

## Mail and speech setup

Accounts are configured in the web console. Each account supports IMAP/SMTP
credentials, explicit folder roles and aliases, sync intervals, retention,
alert folders, and account ordering. IMAP and SMTP passwords are encrypted at
rest and are never returned by the account API.

Model provisioning is opt-in:

```dotenv
VOXMAIL_PROVISION_MODELS=1
```

The default Piper voice and Whisper base-English model are stored in the
persistent volume. Remove the flag after provisioning if you want startup to
be fully explicit. Custom voices must contain both a trusted voice name's
`.onnx` file and its matching Piper JSON metadata. Callers cannot cause
arbitrary model downloads.

The image ships a static greeting and fixed signed-in menu generated with the
pinned Piper voice. Dynamic account, folder, contact, attachment, and message
prompts are generated only for an admitted call. See the
[speech lifecycle](docs/ARCHITECTURE.md#static-prompts) for replacement and
cleanup behavior.

## Local development

Requirements are Go 1.23 or newer, a C toolchain for the native shim tests,
and the usual Docker/Compose tools for image and packaged-runtime checks.

```sh
export VOXMAIL_DATA_DIR="$PWD/data"
export VOXMAIL_ENCRYPTION_KEY="$(openssl rand -base64 32)"
go run ./cmd/voxmail
```

Useful checks:

```sh
go test ./...
go test -race ./...
go vet ./...

cmake -S baresip/shim -B /tmp/voxmail-native \
  -DVOXMAIL_BUILD_NATIVE_TESTS=ON
cmake --build /tmp/voxmail-native
ctest --test-dir /tmp/voxmail-native --output-on-failure
```

`make test` runs vet and the Go race suite. `make build` creates
`bin/voxmail`; `make run` refuses to start without
`VOXMAIL_ENCRYPTION_KEY`. The standalone native command tests do not require
Baresip private headers; the production module is built by the pinned Baresip
image stage.

The opt-in integration layers are documented in [Operations](docs/OPERATIONS.md):

- disposable IMAP/SMTP and mbsync checks;
- real-model Piper/Whisper checks;
- the packaged Baresip/SIP/RTP peer test;
- the isolated Chromium browser scenario; and
- backup/restore and runtime smoke checks.

Never point these tests at a production volume or provider account unless the
test explicitly says it is an authorized live validation.

## Publishing

Image publication is manual. The GitHub workflow is started from **Actions →
Publish VOXMail image** and accepts a source ref, output tag, and explicit
`promote_latest` choice. It does not publish automatically for ordinary pushes,
pull requests, or tags.

The workflow tests the selected source, builds amd64 and arm64 images without
QEMU emulation, attaches BuildKit provenance and an SBOM, scans both images,
records architecture digests, and combines them into a multi-architecture
manifest. Prerelease and test tags do not move `latest`; production promotion
must be an explicit decision. Fixed HIGH or CRITICAL vulnerability findings
block publication unless an approved, time-bounded exception exists.

Before promotion, keep the prior image digest and backup available, complete
the [release checklist](docs/RELEASE-READINESS.md), and verify the image on
the actual deployment host and provider network.

## Data, backup, and upgrade rules

All persistent state is under `/data`, including SQLite, Maildirs, generated
sync configuration, drafts, quarantine copies, prompts, recordings, logs, and
speech models. The service uses conservative cleanup rules: remote mail is not
deleted by local retention, uncertain reconciliation records are retained, and
temporary call/recording files are removed only when safely attributable.

Use the backup helper before upgrades:

```sh
scripts/backup.sh
```

The archive does not contain `VOXMAIL_ENCRYPTION_KEY`; preserve that secret
separately. Test a copy with the non-destructive restore drill before retiring
the old deployment:

```sh
VOXMAIL_ENCRYPTION_KEY='<original key>' \
  scripts/restore-drill.sh backups/voxmail-YYYYmmddTHHMMSSZ.tar.gz \
  ghcr.io/s31354915/voxmail@sha256:<release-digest>
```

For rollback, set `VOXMAIL_IMAGE` to the previous immutable digest and recreate
the service. Do not downgrade a database migration blindly; restore the
matching data backup when a release states that its schema is not backward
compatible.

## Known limitations

VOXMail is beta software and these are deliberate boundaries, not promises of
provider compatibility:

- SIP registration, NAT traversal, provider authentication syntax, TLS
  certificates, codec negotiation, and RTP reachability vary by provider.
- Local exact-image tests do not prove an external registrar, carrier, or
  production reverse proxy.
- Arm64 and the supported production browser-library combination must be
  validated on the target deployment host before promotion.
- Mailbox recovery can act as an alternate authentication factor because it
  requires access to the configured mailbox; deployments needing stronger
  assurance should disable or restrict it.
- SQLite is intentionally conservative about writers. Large installations
  need a production-shaped capacity test before increasing concurrency.
- Interrupted model installation is reported as failed and must be retried;
  partial model files are not silently activated.

See [release readiness](docs/RELEASE-READINESS.md) for the exact evidence
available in this checkout and the remaining deployment gates.
