# VOXMail settings reference

This is the checked-in contract for deployment environment variables and the
main authenticated API settings. Values are validated by the server; changing
an HTML control or sending JSON directly does not bypass those limits.

## Deployment environment

| Variable | Default | Bounds/behavior | Restart required |
| --- | --- | --- | --- |
| `VOXMAIL_DATA_DIR` | `/data` | Root for database, Maildirs, models, prompts, recordings, and runtime files | Yes |
| `VOXMAIL_HTTP_ADDR` | `127.0.0.1:8080` | HTTP listener; in Compose the host publication is separately fixed by `ports` | Yes |
| `VOXMAIL_DB_PATH` | `$VOXMAIL_DATA_DIR/sqlite/voxmail.db` | SQLite application database path | Yes |
| `VOXMAIL_ENCRYPTION_KEY` | none | Required; at least 32 bytes. Keep it separate from `/data` backups | Yes |
| `VOXMAIL_CONTROL_SOCKET` | `$VOXMAIL_DATA_DIR/run/baresip.sock` | Go/native baresip control socket | Yes |
| `VOXMAIL_MAX_CALLS` | `10` | Integer from 1 through 100 | Yes |
| `VOXMAIL_STT_BINARY` | `whisper-cli` | Whisper executable | Yes |
| `VOXMAIL_STT_MODEL` | `$VOXMAIL_DATA_DIR/whisper/ggml-base.en.bin` | Whisper model path | Yes |
| `VOXMAIL_PIPER_BINARY` | `piper` | Piper executable | Yes |
| `VOXMAIL_PIPER_MODEL` | `$VOXMAIL_DATA_DIR/voices/en_US-hfc_male-medium.onnx` | Default Piper model path | Yes |
| `VOXMAIL_VOICE_DIR` | `$VOXMAIL_DATA_DIR/voices` | Installed voice directory | Yes |
| `VOXMAIL_RECORDINGS_DIR` | `$VOXMAIL_DATA_DIR/recordings` | Temporary caller recordings | Yes |
| `VOXMAIL_GREETING_PATH` | `$VOXMAIL_DATA_DIR/prompts/welcome.wav` | Static greeting path | Yes |
| `VOXMAIL_BARESIP_BINARY` | `baresip` | Baresip executable | Yes |
| `VOXMAIL_BARESIP_CONFIG` | `$VOXMAIL_DATA_DIR/config/baresip` | Generated baresip configuration directory | Yes |
| `VOXMAIL_TRUSTED_PROXY_CIDRS` | empty | Comma-separated valid CIDRs; only these immediate peers may supply trusted forwarding headers | Yes |
| `VOXMAIL_IMAGE` | `ghcr.io/s31354915/voxmail:latest` in Compose | Prefer an immutable release tag or digest for controlled upgrades | Recreate |
| `VOXMAIL_VOLUME_NAME` | `voxmail-data` in Compose | Named Docker volume used by backup/restore | Recreate |
| `VOXMAIL_PROVISION_MODELS` | `0` in Compose | Explicitly provisions default speech models when enabled | Recreate |
| `VOXMAIL_HISTORY_RETENTION_DAYS` | `365` | Startup retention for terminal audit/sync/mutation/submission history; 1–3650 days | Restart |
| `VOXMAIL_HISTORY_MAX_ROWS` | `100000` | Per-table cap for terminal history after age pruning; 100–10000000 rows | Restart |
| `VOXMAIL_SIP_ACCOUNT` | empty | Optional baresip account-line override; console settings are the normal source | Restart/apply |

The Compose file sets the container listener to `:8080` because Docker
publishes it as `127.0.0.1:8080`. Changing only `VOXMAIL_HTTP_ADDR` does not
change that host exposure. Change the Compose host mapping or use a reverse
proxy only after reviewing TLS and trusted-proxy settings.

## Authenticated API settings

The browser uses these endpoints under `/api/v1`; all write requests require a
session and CSRF token. Responses never return encrypted mailbox or SIP
passwords.

| Endpoint | Fields and defaults/bounds | Ownership/role |
| --- | --- | --- |
| `PUT /settings` | `tts_voice` defaults to `en_US-hfc_male-medium` and must be an installed safe voice name. `menu_speed` defaults to 3 and `email_speed` to 2; each normalizes to 1–5. `alert_phone` is normalized or cleared. Alert fields are preserved while alerts are globally unavailable. | Own user; admin and ordinary users |
| `POST /accounts` | IMAP/SMTP ports default to 993/465 and must be 1–65535; SMTP 25 is rejected by the normal path. Sync defaults to 5 minutes; reconciliation defaults to 1440 minutes. Retention, display order, folder names, mappings, and alert folders are bounded and owner-scoped. New accounts require both passwords; blank passwords on edits preserve stored secrets. | Own accounts; authenticated users |
| `PUT /sip` | `local_port` and `registrar_port` default to 5060 and must each be 1–65535. Legacy `port` is accepted as an alias for both. `transport` is `udp`, `tcp`, or `tls` (invalid values normalize to `udp`). `reg_interval` defaults to 300 and accepts 0–86400; 0 is persisted/rendered as 300. Enabling requires a valid domain and username. | Administrator only; deployment-wide |
| `POST /voices/install` | Voice must be in the trusted catalog. Installation is asynchronous and administrator-only. | Administrator only |
| `POST /voices/preview` | Installed voice only; speed normalizes to 1–5; generated WAV is capped at 16 MiB and the operation has a two-minute timeout. | Authenticated user |
| `POST /alerts/test` | Uses the owner's active alert destination and shared durable cooldown; it does not claim mail. | Authenticated user when alerts are available |

## Account and identity limits

Web passwords require at least 12 characters and at most 72 bytes because of
the bcrypt input limit. Usernames are 3–64 characters, PINs are 4–12 digits,
and TOTP codes are six digits. Request bodies are capped at 1 MiB. These limits
are server-side and should be reflected by any alternate client.

## Apply and readiness behavior

Saving SIP settings persists encrypted credentials, rewrites the generated
baresip configuration, and restarts the managed call client. Environment
changes require a process/container restart. Voice installation is a job and
must reach `complete` before selecting the new model. `/healthz` means the HTTP
process responds; `/readyz` additionally checks the database and application
readiness callback, and may remain `not_ready` during startup.
