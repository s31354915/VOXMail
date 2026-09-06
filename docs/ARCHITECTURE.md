# VOXMail architecture and speech lifecycle

## Mail synchronization

The Go service renders a private mbsync configuration per account and invokes
the installed `mbsync` binary. The policy uses UID state, mirrors mail into
local Maildirs, creates local aliases for configured mappings, and uses
`Remove None` and `Expunge None` for routine synchronization. The index stores
metadata and attachment counts in SQLite; bodies remain in the Maildir.

## SIP and media boundary

baresip owns SIP signaling and RTP. The native `voxmail` module exposes a Unix
control socket for incoming-call, DTMF, and close events and private per-call
PCM paths for playback and capture. Go owns admission, PIN verification, IVR
state, database operations, synthesis, and transcription. C session lookup and
Go session cleanup are protected against concurrent close/event access.

## Static prompts

The image ships `assets/welcome.wav`, `assets/main-menu.wav`, and
`assets/static-prompts.json`. They were generated with Piper 1.3.0 using the
`en_US-hfc_male-medium` voice from the pinned Piper voice source. Whisper is
not used to make them. The bundled model SHA-256 is:

```text
d11e403a02bdf5a670c877b3dc56e0e1c8cece6fb30289586314dffdc0a78cb0
```

The signed-in main menu is static because its six choices never change with
the number of accounts. Account, folder, and contact prompts are dynamic.

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
2. Piper starts in JSON-input worker mode and synthesizes a warmup utterance.
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

## Settings ownership

SQLite stores user settings, account metadata, folder mappings, alert folders,
contacts, and caller whitelist entries. IMAP/SMTP passwords are encrypted by
the secret box and are not returned by the account API. The browser receives
safe account views. API validation covers relative aliases, ports, cutoff
timestamps, voice-name path characters, ownership, sessions, and CSRF writes.
