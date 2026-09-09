# VOXMail

VOXMail is a self-hosted telephone email assistant. It synchronizes IMAP
mail into private Maildirs with `mbsync`, indexes messages locally, and gives
approved callers a PIN-protected SIP/DTMF interface for reading, composing,
replying, forwarding, and managing mail. A browser console handles the
configuration that is awkward or unsafe to do over a phone.

The production image is multi-architecture (`linux/amd64` and `linux/arm64`),
pull-based, and published to GHCR. It contains the Go service, baresip bridge,
mbsync, Piper, Whisper CLI, ffmpeg, and static first-call audio. Model files
live in the persistent data volume and are opt-in to download.

## Documentation

- [Installation and operations](docs/OPERATIONS.md) — Docker, tags, secrets,
  models, backups, upgrades, health checks, and troubleshooting.
- [Web console](docs/WEB-CONSOLE.md) — setup, accounts, folder aliases,
  alerts, callers, contacts, users, and voice settings.
- [IVR and typing guide](docs/IVR.md) — menus, DTMF controls, multi-tap entry,
  voice composition, and mail actions.
- [Architecture and speech lifecycle](docs/ARCHITECTURE.md) — mbsync safety,
  SIP/media boundaries, call-scoped model loading, static prompts, and model
  replacement behavior.

## Fastest Docker start

```sh
cp .env.example .env
openssl rand -base64 32
# Put the generated value in VOXMAIL_ENCRYPTION_KEY in .env.

docker compose pull
docker compose up -d
docker compose logs -f voxmail
```

Open `http://127.0.0.1:8080/` on the host running Docker. The UI binds to the
host loopback by default, so reach it from a remote machine over an SSH tunnel
or by changing the bind address. On an empty data volume the sign-in page
includes the one-time **First-run setup** form. Once the first administrator
exists, that form is disabled permanently for that database and normal sign-in
is used.

The default Compose service pulls; it does not build. It uses:

```text
ghcr.io/s31354915/voxmail:latest
```

Override the image in `.env` for a release tag or digest:

```dotenv
VOXMAIL_IMAGE=ghcr.io/s31354915/voxmail:v2.1.0
# or, for a reproducible deployment:
# VOXMAIL_IMAGE=ghcr.io/s31354915/voxmail@sha256:...
```

Then run `docker compose pull` followed by `docker compose up -d`.
`pull_policy: always` checks the registry whenever Compose creates or
recreates the service. `latest` is intentionally mutable; pin a tag or digest
for production change control.

## Local development

Go 1.23+ is required. The service expects an encryption key of at least 32
bytes and stores data under `/data` by default. For a workstation:

```sh
export VOXMAIL_DATA_DIR="$PWD/data"
export VOXMAIL_ENCRYPTION_KEY="$(openssl rand -base64 32)"
go run ./cmd/voxmail
```

Useful checks:

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
go test ./...
go vet ./...
```

`make run` also requires `VOXMAIL_ENCRYPTION_KEY`; no development key is
embedded in the repository.

## Publishing an image

Publishing is manual only. The GitHub Action does not run on pushes, pull
requests, or tags. In GitHub:

1. Open **Actions → Publish VOXMail image**.
2. Click **Run workflow**.
3. Enter a tag such as `v2.1.0` or `test-2026-09-06`.
4. Run it on the default branch.

Two native jobs build independently on `ubuntu-24.04` (amd64) and
`ubuntu-24.04-arm` (arm64). There is no QEMU emulation. Each job pushes an
architecture tag, and a final job combines them into:

```text
ghcr.io/s31354915/voxmail:<tag>
ghcr.io/s31354915/voxmail:<tag>-amd64
ghcr.io/s31354915/voxmail:<tag>-arm64
```

For every tag other than `latest`, the workflow also moves the `latest` alias
to that same two-architecture manifest. To publish `latest` directly, enter
`latest`; it does not create a second alias operation.

## Security baseline

- Use a unique random `VOXMAIL_ENCRYPTION_KEY`; losing it makes encrypted mail
  credentials unrecoverable.
- Keep `.env` out of source control and protect the Docker host.
- Add only trusted telephone identities to the caller whitelist. Caller ID is
  an admission hint, not a cryptographic identity; the PIN is still required.
- Use HTTPS or a private network/reverse proxy for the web console.
- Pin a digest when reproducibility matters.
- Back up `/data` as a unit, including SQLite, encrypted account secrets,
  Maildirs, prompts, and model files.

## What is included

The web console manages multiple accounts, ordering, IMAP folder aliases,
alert folders, account alert switches, caller whitelist entries, contacts and
their order, users, alert numbers, speech models, and speech speeds. The IVR
supports PIN login, unread/all-mail views, account and folder navigation,
attachment announcements, read/delete/reply/forward, keypad composition,
contact shortcuts, settings, and voice-recorded composition.

The first greeting and fixed signed-in menu are shipped as WAV files. The
bundled recordings were generated with Piper 1.3.0 using
`en_US-hfc_male-medium` from the pinned Piper voice source; they are not
generated by Whisper. See [the speech lifecycle notes](docs/ARCHITECTURE.md)
for the exact model fingerprint and replacement rules.

SIP registration, NAT traversal, and provider-specific RTP behavior remain
provider-dependent. The web console's **SIP connection** panel stores the
registrar, SIP username/password, port, transport, and registration interval
and applies them to the embedded call client; port 5060 maps through for
registration and inbound calls, and UDP 10000–10100 covers the configured
baresip RTP range.
