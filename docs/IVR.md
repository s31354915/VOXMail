# VOXMail IVR and typing guide

The spoken wording varies with the selected voice, but the DTMF controls are
stable. `#` means confirm/continue in menus and finish a text field. `*`
repeats a prompt outside an editor.

## Call flow

1. An allowed caller hears the prerecorded welcome: “Welcome to VOXMail.
   Please enter your PIN, then press pound.”
2. Enter the numeric PIN and press `#`.
3. The fixed signed-in menu is:
   - `1` — unread mail across accounts;
   - `2` — all mail across accounts;
   - `3` — choose an account, then a synchronized folder;
   - `4` — choose a contact and compose to it;
   - `5` — compose a new message;
   - `6` — settings.

`#` backs out one menu level. `*` repeats the current menu. The top-level menu
is always six choices; account count does not alter its prerecorded audio.
Account, folder, and contact prompts enumerate current data dynamically.

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
truncated. MIME attachments are announced by count and remain available in
the local Maildir for normal email clients.

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

## Compose, reply, and forward

New compose asks for recipient, subject, and body, then reads a review prompt:

- `1` sends;
- `2` edits the body;
- `3` records the body by voice;
- `#` returns to the body.

Reply seeds the sender and `Re:` subject. Forward seeds the recipient and
`Fwd:` subject and adds a forwarded-message notice. Sending uses the selected
account when one was chosen through account navigation; otherwise it uses the
first ordered account.

## Voice composition

At review, press `3` and speak after the prompt. The bounded 15-second capture
is converted to 16 kHz mono WAV, sent through Whisper, and removed after
transcription. Recognized text is placed into the reviewable draft. On failure
the draft returns to keypad editing. Temporary files are private and are
removed on success, failure, or timeout.

## Settings

Under `6`, press `1` for voice-settings guidance or `2` to toggle global call
alerts. Detailed model, speed, alert-number, account-alert, and folder-alert
configuration is in the web console.
