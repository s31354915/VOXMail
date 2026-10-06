# VOXMail IVR and typing guide

The spoken wording varies with the selected voice, but the DTMF controls are
stable. `#` means confirm/continue in menus and finish a text field. `*`
repeats a prompt outside an editor.

## Call flow

1. An allowed caller hears the prerecorded welcome: “Welcome to VOXMail.
   Please enter your PIN, then press pound.”
2. Enter the numeric PIN and press `#`.
3. The signed-in menu has three choices:
   - `1` — Email. The next menu offers `0` for all unread Inbox mail and
     paged account selection;
   - `2` — settings, including voice guidance, the global alert toggle, and
     contacts;
   - `3` — information and instructions.

After selecting an account, `1` opens synchronized folders, `2` opens the
compose/draft flow, `3` refreshes the account view, and `4` opens account alert
settings.

`#` backs out one menu level. `*` repeats the current menu. The top-level menu
always has three choices; account count does not alter its prerecorded audio.
Account, folder, contact, attachment, and draft prompts enumerate current data
dynamically.

## Mail actions

In a message list:

- `1` reads the current message;
- `2` moves next;
- `3` moves previous;
- `4` opens delete confirmation;
- `5` starts a reply;
- `6` starts a forward;
- `#` returns to the folder or main menu.

Reading speaks sender, subject, and body. Long bodies are bounded and marked
truncated. MIME attachments are announced by count. `8` lists playable audio
attachments, including the audio track of video containers; the selected item
is extracted privately and normalized through FFmpeg to 8 kHz mono signed PCM
for the call. Playback can be interrupted with DTMF or `#` and temporary files
are removed afterward.

While reading a message, `1` repeats it, `2` toggles read/unread, `3` replies,
`4` replies to all, `5` forwards, `6` deletes, `7` moves it, `8` opens playable
attachments, `9` opens more options, and `0` advances to the next message.

## Multi-tap text entry

Press a key repeatedly to cycle its characters. Press a different key to
commit the previous character and start the next one. Press `*` while a
character is pending to commit it. Press `#` to commit the pending character
and finish the field. Press `*` with no character pending to delete the
previous character.

| Key | Characters |
| --- | --- |
| `0` | `0`, space |
| `1` | `1 @ . ? & + - _ = / : ; , $ % ( ) ! # * ' "` |
| `2` | `a b c 2` |
| `3` | `d e f 3` |
| `4` | `g h i 4` |
| `5` | `j k l 5` |
| `6` | `m n o 6` |
| `7` | `p q r s 7` |
| `8` | `t u v 8` |
| `9` | `w x y z 9` |

For example, type `ada@example.com` by cycling `2`, `3`, `2`, committing with
`*`, cycling `1` twice for `@`, and continuing with the table. Contacts are
recommended for frequent recipients.

Voice text fields are bounded before they are accepted: email addresses are
limited to 254 Unicode runes, subjects to 998, message bodies to 12,000, and
contact names to 128. When a limit is reached, additional characters are
rejected until `*` deletes one; the partially entered value is never silently
expanded beyond its bound. Inactivity closes the call, and credential
revocation makes an already-held call terminal even if a queued DTMF event
arrives afterward.

## Compose, reply, and forward

New compose asks for recipient, subject, and body, then reads a review prompt:

- `1` sends;
- `2` edits the body;
- `3` records the body by voice;
- `#` returns to the body.

Reply seeds the sender and `Re:` subject. Forward asks whether to include the
original raw message and attachments, then seeds the recipient and `Fwd:`
subject. Sending uses the selected account when one was chosen through account
navigation; otherwise it uses the first ordered account. Review also offers
recording one or more WAV audio attachments.

Saving a draft always updates the private local draft. On the first save, VOXMail
also makes a best-effort snapshot in the mapped remote Drafts folder when that
folder and an IMAP connection are available. IMAP saves are append-only: after
the local draft has an ID, later saves do not append another remote copy, so
the local draft remains canonical and the remote copy is only an initial
snapshot. A remote failure does not prevent the local save and is not retried
automatically, which avoids duplicate remote drafts after a timeout or restart.

## Voice composition

At review, press `5` to record an audio attachment. The bounded capture
is converted to 16 kHz mono WAV, sent through Whisper, and removed after
transcription. Recognized text is placed into the reviewable draft. On failure
the draft returns to keypad editing. Temporary files are private and are
removed on success, failure, or timeout.

## Settings

Under `2`, press `1` for voice-settings guidance, `2` to toggle global call
alerts, or `3` for contacts. Detailed model, speed, alert-number,
account-alert, and folder-alert configuration is in the web console.
