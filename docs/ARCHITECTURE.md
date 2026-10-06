# VOXMail architecture and speech lifecycle

## Mail synchronization

The Go service resolves and policy-checks each user-supplied IMAP host once,
then renders a private mbsync configuration per account and invokes the
installed `mbsync` binary through the fixed-address `voxmail-connect` tunnel.
The original hostname remains in the configuration for TLS certificate
verification and SNI; the tunnel receives only the already-validated numeric
address and performs no DNS lookup. The policy uses UID state, mirrors mail into
local Maildirs, creates local aliases for configured mappings, and uses
`Remove None` and `Expunge None` for routine synchronization. Authenticated
IMAP mutations are used for read/unread, move, and Trash actions; the next
mbsync run reconciles the cache. Sync runs are persisted in SQLite, with
initial, incremental, and daily reconciliation classifications. Initial-cutoff
and account retention policies prune only local Maildir files after
synchronization and never delete remote mail automatically. Startup cleanup
removes only known stale staging/call artifacts; it never recursively prunes
message bodies, models, drafts, or historical audit data.

The index stores metadata and attachment counts in SQLite; bodies remain in the
Maildir. Explicit folder roles are stored separately from display aliases, so
unread totals only use the Inbox role and never guess that a folder is Spam or
Trash from its name.

## SIP and media boundary

baresip owns SIP signaling and RTP. The native `voxmail` module exposes a Unix
control socket for incoming-call, DTMF, and close events and private per-call
PCM paths for playback and capture. Go owns admission, PIN verification, IVR
state, database operations, synthesis, and transcription. C session lookup and
Go session cleanup are protected against concurrent close/event access.

The baresip/libre call pointers and call lifecycle APIs are event-loop-owned.
The control-socket reader only validates and enqueues commands; `ua_connect`,
`call_answer`, `call_hangup`, and `call_set_handlers` run from the baresip loop
through the pipe handler. The command queue is bounded at 256 entries. A full
queue rejects the command with a version-2 `command_error` carrying code 429;
an unavailable/shutting-down queue uses code 503. A stale call ID is harmless
and returns code 481. At most 32 commands are executed per loop turn, after
which the handler reschedules itself so SIP timers and unrelated callbacks get
service. The pipe and listening socket are nonblocking; accepted control
descriptors have one close owner, while shutdown from another thread only
interrupts the owner with `shutdown(2)`.

## Static prompts

The image ships `assets/welcome.wav`, `assets/main-menu.wav`, and
`assets/static-prompts.json`. They were generated with Piper 1.3.0 using the
`en_US-hfc_male-medium` voice from the pinned Piper voice source. Whisper is
not used to make them. The bundled model SHA-256 is:

```text
d11e403a02bdf5a670c877b3dc56e0e1c8cece6fb30289586314dffdc0a78cb0
```

The signed-in main menu is static because its three choices never change with
the number of accounts. Account, folder, contact, and attachment prompts are
dynamic.

At startup VOXMail compares prompt text, model name, and model digest. A
matching manifest returns without touching Piper, so ordinary idle startup
does not warm speech resources. If the configured model exists but differs,
both recordings are generated in a private staging directory and replaced only
after both succeed; the manifest is written last. If the replacement model is
unavailable, the shipped recordings remain usable and a warning is logged.

For a per-user custom voice, the fixed static main prompt is used only when its
voice matches the call's selected voice. Otherwise that call uses its warmed
runtime, so a web voice setting is not silently ignored.

## Call-scoped model lifecycle

No Piper worker or Whisper warmup starts merely because the service is running.
After a caller passes admission and the call is answered:

1. VOXMail acquires a runtime keyed by voice model and menu speed.
2. Piper runs a bounded one-shot synthesis for the warmup utterance; process
   completion and WAV validation, not file size, establish readiness.
3. Whisper validates and warms against a short silent WAV.
4. The prerecorded greeting plays immediately while warmup proceeds.
5. The fixed post-PIN menu can play from static audio; dynamic prompts wait on
   the ready signal and then use the resident Piper worker.
6. Concurrent calls with the same key share the warm runtime.
7. When the final lease releases, the Piper process closes, the warmup context
   is cancelled, and call-scoped temporary files are removed.

Whisper's CLI is invoked for transcription after the warm check. Caller
recordings are bounded, private, and deleted after transcription, including
failure paths.

## Outbound alert calls

New unread mail in an account with call alerts enabled and non-empty alert
folders is surfaced to the user's phone. The alert service runs on a 20-second
ticker, groups pending messages per user, and dials the user's alert number via
the same baresip control socket used for inbound events. The candidate query
joins the normalized folder list, requires an enabled user/global/account
configuration and an active destination, keeps at most ten candidates per
user, and applies a global 100-row cap. The spoken alert intentionally
contains only a message count and folder; sender, subject, and body text are
not disclosed. Folder membership is checked again in Go (`alertFolderMatch`).

Before dialing, the service transactionally claims the selected messages. A
failed bridge write or failed call releases the claims; only a positive call
completion marks them announced. A durable per-user next-dial timestamp, plus
the in-process inflight set, prevents alert storms across restarts and during
overlapping rounds. Test calls use the same durable cooldown but do not claim
mail.

Outgoing calls share the socket with incoming ones. `bridge.Client.Dial` writes
a `dial` command with a `request_id` and `to` URI; the shim carries that ID
through `call_outgoing` and later `call_established`, which the calls service
correlates with a map of pending dial requests (`calls.DialRequest`). A
single-request fallback remains for older protocol-v1 shims, but ambiguous
multiple requests are never guessed. Sessions created for an outgoing call
play the per-user alert (or test) prompt and hang up. Caller-initiated sessions
are unaffected.

The native shim owns the per-call media endpoints. The Go service writes and
reads signed little-endian 16-bit PCM at 8 kHz, mono; Baresip is configured to
use that same format. Capture extraction is bounded to the requested speech
window and preserves complete sample frames. Playback and capture FIFOs are
closed as part of call teardown, while the service serializes writes per call.

`Service.TestAlert` (web `POST /api/v1/alerts/test`, console button **Send test
alert call**) dials the configured alert number with a canned message. It
requires a non-empty alert phone and an SIP registrar, and it never claims mail.

## Settings ownership

SQLite stores user settings, account metadata, folder mappings, alert folders,
contacts, and caller whitelist entries. IMAP/SMTP passwords are encrypted by
the secret box and are not returned by the account API. The browser receives
safe account views. API validation covers relative aliases, ports, cutoff
timestamps, voice-name path characters, ownership, sessions, and CSRF writes.
