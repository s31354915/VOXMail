# VOXMail settings reference

This is the checked-in contract for deployment environment variables and the
main authenticated API settings. Values are validated by the server; changing
an HTML control or sending JSON directly does not bypass those limits.

## Deployment environment

| Variable | Default | Bounds/behavior | Restart required |
| --- | --- | --- | --- |
| `VOXMAIL_DATA_DIR` | `/data` | Root for database, Maildirs, models, prompts, recordings, and runtime files | Yes |
| `VOXMAIL_HTTP_ADDR` | `127.0.0.1:8080` | HTTP listener; in Compose the host publication is separately fixed by `ports` | Yes |
| `VOXMAIL_OFFLINE_MODE` | `false` | Validated boolean. Suppresses provider/call workers and model provisioning, and rejects manual provider operations. Local inspection/configuration and maintenance remain available. Overrides persisted and forced SIP enablement. | Yes |
| `VOXMAIL_DB_PATH` | `$VOXMAIL_DATA_DIR/sqlite/voxmail.db` | SQLite application database path | Yes |
| `VOXMAIL_ENCRYPTION_KEY` | none | Required; at least 32 bytes. Keep it separate from `/data` backups | Yes |
| `VOXMAIL_CONTROL_SOCKET` | `$VOXMAIL_DATA_DIR/run/baresip.sock` | Go/native baresip control socket | Yes |
| `VOXMAIL_MAX_CALLS` | `10` | Integer from 1 through 100 | Yes |
| `VOXMAIL_MAX_CONCURRENT_SYNCS` | `2` | Integer from 1 through 8. How many due mail accounts the scheduled sweep syncs at the same time | Yes |
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
| `VOXMAIL_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error`, case-insensitive; invalid values reject startup | Yes |
| `VOXMAIL_IMAGE` | `ghcr.io/s31354915/voxmail:latest` in Compose | Prefer an immutable release tag or digest for controlled upgrades | Recreate |
| `VOXMAIL_VOLUME_NAME` | `voxmail-data` in Compose | Named Docker volume used by backup/restore | Recreate |
| `VOXMAIL_PROVISION_MODELS` | `0` in Compose | Explicitly provisions default speech models when enabled; suppressed in offline mode | Recreate |
| `VOXMAIL_HISTORY_RETENTION_DAYS` | `365` | Startup retention for terminal audit/sync/mutation/submission history; 1–3650 days | Restart |
| `VOXMAIL_HISTORY_MAX_ROWS` | `100000` | Per-table cap for terminal history after age pruning; 100–10000000 rows | Restart |
| `VOXMAIL_SIP_ACCOUNT` | empty | Optional baresip account-line override; console settings are the normal source | Restart/apply |
| `VOXMAIL_ENABLE_CALLS` | unset | Only the exact value `1` forces the managed SIP process on. `0`, unset, and other values remove that force; they do not force calls off. Offline mode overrides it. | Restart/apply |

The Compose file sets the container listener to `:8080` because Docker
publishes it as `127.0.0.1:8080`. Changing only `VOXMAIL_HTTP_ADDR` does not
change that host exposure. Change the Compose host mapping or use a reverse
proxy only after reviewing TLS and trusted-proxy settings.

### Call enablement precedence

`VOXMAIL_OFFLINE_MODE=true` forces provider and call activity off, even when
restored SQLite SIP settings are enabled, `VOXMAIL_ENABLE_CALLS=1`, or a
`VOXMAIL_SIP_ACCOUNT` override is present. Use offline mode for isolated restore
inspection; it also suppresses mail sync, alerts and other provider operations.

Outside offline mode, a nonempty trimmed `VOXMAIL_SIP_ACCOUNT` forces SIP on
using that account line. The exact value `VOXMAIL_ENABLE_CALLS=1` independently
forces the process on, even without a usable account; this does not guarantee
registration or readiness. With neither force present, the persisted SIP enabled
setting and a usable generated account determine whether the process starts.

Setting `VOXMAIL_ENABLE_CALLS=0` is therefore insufficient to stop an enabled
restored SIP account. To disable calls while continuing mail activity, remove the
account override, clear the force (`0` or unset), and disable SIP in the console.
Recreate the process/container for environment changes; saving SIP settings
applies persisted changes. Environment overrides do not erase the stored settings,
so removing offline mode or an override can re-expose their enabled behavior.

## Authenticated API settings

The browser uses these endpoints under `/api/v1`; all write requests require a
session and CSRF token. Responses never return encrypted mailbox or SIP
passwords.

| Endpoint | Fields and defaults/bounds | Ownership/role |
| --- | --- | --- |
| `PUT /settings` | `tts_voice` defaults to `en_US-hfc_male-medium` and must be an installed safe voice name. `menu_speed` defaults to 3 and `email_speed` to 2; each normalizes to 1–5. `alert_phone` is normalized or cleared. Alert fields are preserved while alerts are globally unavailable. | Own user; admin and ordinary users |
| `POST /accounts` | IMAP/SMTP ports default to 993/465 and must be 1–65535; SMTP 25 is rejected by the normal path. Sync defaults to 5 minutes; reconciliation defaults to 1440 minutes. Retention, display order, folder names, mappings, and alert folders are bounded and owner-scoped. New accounts require both passwords; blank passwords on edits preserve stored secrets. | Own accounts; authenticated users |
| `GET /accounts/{id}/folder-policy` | Returns the saved grouping/exclusion policy with its configuration version, persisted `folder_map` and `folder_roles`, discovered/retained remote-to-local folder inventory and logical mailbox preview. Inventory can retain the synchronized local path while a mapping change awaits sync. No provider requests or credentials are returned. | Account owner |
| `PUT /accounts/{id}/folder-policy` | Requires an explicit `policy` and current positive `config_version`. Optionally provide both `folder_map` and `folder_roles` to save physical mappings, roles and logical choices in one transaction; omitting both preserves mappings and roles. Successful saves advance the version once. Unknown folders, invalid roles, overlapping selections and missing/outside primaries are rejected without partial writes; stale writes return 409. | Account owner; CSRF required |
| `PUT /sip` | `local_port` and `registrar_port` default to 5060 and must each be 1–65535. Legacy `port` is accepted as an alias for both. `transport` is `udp`, `tcp`, or `tls` (invalid values normalize to `udp`). `reg_interval` defaults to 300 and accepts 0–86400; 0 is persisted/rendered as 300. Enabling requires a valid domain and username. | Administrator only; deployment-wide |
| `POST /voices/install` | `voice` accepts a Piper name or HTTPS `.onnx` URL outside the bundled catalog. Optional `model_url`, `config_url`, `model_sha256`, and `config_sha256` support custom sources and checksum pins. Default metadata URL appends `.json` to the model path. Sources require public HTTPS endpoints; limits are 4 GiB for the model and 16 MiB for metadata. A staged pair is checked with Piper before activation; failed replacements preserve the previous pair. Installation is asynchronous and unavailable in offline mode. | Administrator only |
| `POST /voices/preview` | Installed voice only; speed normalizes to 1–5; generated WAV is capped at 16 MiB and the operation has a two-minute timeout. | Authenticated user |
| `POST /alerts/test` | Uses the owner's active alert destination and shared durable cooldown; it does not claim mail. | Authenticated user when alerts are available |

## Account and identity limits

Web passwords require at least 12 characters and at most 72 bytes because of
the bcrypt input limit. Usernames are 3–64 characters, PINs are 4–12 digits,
and TOTP codes are six digits. Request bodies are capped at 1 MiB. These limits
are server-side and should be reflected by any alternate client.

## Apply and readiness behavior

Folder policies contain `groups` with stable `id`, display `name`, selected remote
`folders` and explicit `primary`, plus `excluded` remote names. Empty policy keeps
every folder separate. Groups combine browsing across separate physical Maildirs;
they do not combine mbsync UID state or deduplicate Message-IDs. Exclusions hide
selected folders from logical browsing without deleting remote or indexed mail,
and do not turn off synchronization or previously configured call alerts.
These choices never follow Gmail/provider naming heuristics. Fetch the latest
policy/version after saving or after a conflict before making another edit.
When a mapped write role belongs to a group, its destination is the selected
primary remote folder and corresponding physical directory. An excluded role
is unavailable for writes until remapped. Saved role assignments remain intact;
grouping does not silently rewrite them. Pending mapping edits retain the last
synchronized directory identity for remote mutations, and ambiguous identities
stop the operation rather than selecting a folder by name.
Mapping saves also reject destinations already configured or observed for another
remote folder, including discovered unmapped folders and the implicit Inbox
destination. Canonically equivalent local paths are collisions. Logical grouping
never grants permission to share a physical directory or mbsync UID state.
Directory-to-remote bindings survive remaps and discovery pruning. Sync records
known configuration associations before launching mbsync, so partial downloads
retain their source identity even if reconciliation never finishes. A stale sync
configuration is rejected before launch. Reads include retained indexed directories
for the same remote mailbox; grouping different remote mailboxes still requires
explicit owner choices. Exclusions cover those historical directories too.
Unknown or ambiguous source bindings refuse remote mutation; retained bindings
never bypass UIDVALIDITY checks. Historical directories remain reserved against
reuse by another remote mailbox until the account and its data are removed.
Production sync first obtains an authenticated provider folder list and generates
an explicit mbsync pair for every mapped or discovered folder. Retained local
aliases are never interpreted as additional server folders. Discovery/connection
failures stop the run before mbsync launches and remain visible in sync status.
If a newly discovered remote folder would reuse another folder's historical
directory, sync stops until the owner chooses a distinct local alias.

Saving SIP settings persists encrypted credentials, rewrites the generated
baresip configuration, and restarts the managed call client. Environment
changes require a process/container restart. Voice installation is a job and
must reach `complete` before selecting the new model. `/healthz` means the HTTP
process responds; `/readyz` additionally checks the database and application
readiness callback, and may remain `not_ready` during startup. See
[health and first-run checks](OPERATIONS.md#health-and-first-run-checks) for
the enabled-SIP registration, worker/model and offline conditions. Readiness
does not assert that each account has synchronized successfully.

### Diagnostic logging

`VOXMAIL_VERBOSE` is a deployment boolean, default `false`. Set it to `true`
to enable Go debug traces and supported diagnostic modes for mbsync, Piper,
Whisper, FFmpeg, Baresip, and the native bridge. Invalid boolean values reject
startup.

`VOXMAIL_LOG_LEVEL` selects the Go log level without enabling child-process
diagnostic modes: `debug`, `info`, `warn`, or `error`, case-insensitive,
default `info`. `VOXMAIL_VERBOSE=true` still forces `debug`; invalid values
reject startup. See [verbose diagnostics](OPERATIONS.md#verbose-diagnostics) for
recreation and log collection commands.
