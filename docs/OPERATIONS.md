# VOXMail operations guide

## Configure the deployment

Create `.env` from the example and set a unique key:

```dotenv
VOXMAIL_ENCRYPTION_KEY=<at least 32 random bytes>
```

For telephone service, configure the provider credentials in the web console's
**SIP connection** panel (registrar, SIP username, password, port, transport,
registration interval). The `voxmail` process writes the baresip configuration
and supervises the call client, so a save applies immediately. Until a
registrar is configured and enabled there, baresip does not register and no
calls are accepted.

`VOXMAIL_SIP_ACCOUNT` exists as a deployment override for the account line
baresip would otherwise generate from the console settings:

```dotenv
VOXMAIL_SIP_ACCOUNT=<sip:YOUR_NUMBER:YOUR_PASSWORD@YOUR_PROVIDER>;regint=300
```

Provider-specific account syntax, outbound proxy, codecs, and NAT behavior are
provider-dependent. SIP uses TCP/UDP port 5060, and RTP uses UDP
10000–10100; both ranges must be reachable from the provider. Do not commit
provider credentials.

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

The manually triggered GitHub Action builds native amd64 and arm64 images,
combines them on GHCR, and advances `latest` for non-`latest` releases. It
does not run automatically. See the root README for the exact workflow inputs
and generated tags.

## Health and first-run checks

```sh
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

`healthz` confirms the HTTP process responds. `readyz` additionally checks
SQLite. The browser shows **First-run setup** only while the database contains
no users; after setup, sign in normally.

## Data and backups

All application state is under `/data`:

- `sqlite/voxmail.db` — users, settings, metadata, indexes, and audit records;
- `mail/` — synchronized Maildirs;
- `config/mbsync/` — generated per-account sync policy;
- `voices/` and `whisper/` — model files;
- `prompts/` — static recordings and generated prompt cache;
- `recordings/` — short-lived voice-composition work files;
- `run/` and `logs/` — runtime state and logs.

Stop the service or use a filesystem-consistent volume snapshot before copying
SQLite and Maildirs. Store the encryption key separately from the data archive
but back it up; without it encrypted account passwords cannot be recovered.

## mbsync safety model

VOXMail renders one configuration and channel per account. Routine sync uses
UID state, `Sync All`, `Create Slave`, `Remove None`, and `Expunge None`.
Remote folders are not deleted by routine synchronization. Folder aliases are
written as explicit local Maildir paths.

## Troubleshooting

### The web UI stays at setup

Confirm the container is using the same persistent volume. A new volume is a
new database and will correctly show setup again.

### Mail does not appear

Check logs, account host/user/ports, and whether the account password was
saved. The account test checks only that account's configured IMAP host.

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
