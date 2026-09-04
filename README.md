# VOXMail

VOXMail is a local, telephone-driven email assistant. The current repository
contains the production foundation: a Go service, encrypted-secret primitive,
SQLite schema, deterministic IVR/keypad/speech components, mbsync policy
generation, and the baresip control-module boundary.

## Run locally

Go 1.23+ is required.

```sh
export VOXMAIL_ENCRYPTION_KEY='replace-this-with-a-long-random-secret'
go run ./cmd/voxmail
curl http://127.0.0.1:8080/readyz
```

The database and runtime directories default to `/data`. For local development
use `VOXMAIL_DATA_DIR=$PWD/data`.

## Docker

Copy `.env.example` to `.env`, set a long random `VOXMAIL_ENCRYPTION_KEY`,
then:

```sh
docker compose up --build
```

The image builds baresip from pinned source, compiles the headless module set,
and installs the VOXMail shim. Account synchronization is handled by mbsync;
the generated channel uses `Patterns *`, UID-based state, bidirectional flags,
`Create Slave`, and `Expunge None`, so existing remote folders are mirrored
without VOXMail creating or permanently deleting remote mail during routine
sync. Piper and Whisper model provisioning is kept explicit so model licensing
and storage are visible to the operator.

## Status

The repository now contains the end-to-end foundation: setup/login with CSRF
protection, encrypted account secrets, a usable management console, mbsync
orchestration and Maildir indexing, SMTP composition, deterministic IVR
primitives, model provisioning, and a baresip Unix/PCM bridge. Set
`VOXMAIL_PROVISION_MODELS=1` on the first container start to fetch the Piper
and Whisper models; the download is skipped once the files are present.

The remaining deployment step is provider-specific SIP credentials and a live
call test (NAT/RTP behavior varies by provider). Routine IMAP sync uses actual
mbsync channels with `Patterns *`, `Create Slave`, and `Expunge None`.
