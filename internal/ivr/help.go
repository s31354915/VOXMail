package ivr

// Entry guidance is shared by the live editors and the help guide. Star is
// a separator, not a timed commit: pausing never selects the next character.
const KeypadInstructions = "Press a number key repeatedly to cycle through its letters, then its number, then back to the first letter. A different key starts the next character. Use star between characters on the same key. Star alone never deletes. Press star twice to delete the last character. Pound finishes."

const EmailKeypadInstructions = KeypadInstructions + " For at, press 1 twice, then star. For dot, press 1 three times, then star."

const TextKeypadInstructions = KeypadInstructions + " For a space, press 0 twice, then star. For a dot, press 1 three times, then star."

type helpPage struct {
	text     string
	back     State
	children []State // Keys 1 through len(children), in spoken order.
}

// Keep topics short enough to replay independently. Every leaf has the same
// controls; navigation is derived from this tree instead of a second switch.
var helpPages = map[State]helpPage{
	"info":                   {"Help. Press 1 for getting around, 2 for reading email, 3 for writing email, 4 for keypad typing, 5 for attachments, 6 for contacts and settings, 7 for accounts and folders, or 8 for troubleshooting.", "main", []State{"help_basics", "help_reading", "help_writing", "help_typing", "help_attachments", "help_settings", "help_accounts", "help_troubleshooting"}},
	"help_basics":            {"Getting around. Sign in with your PIN, then pound, also called the hash key. Star clears your PIN. At the main menu, press 1 for email, 2 for settings, or 3 for help. In menus, star repeats the choices and pound goes back. You can choose a menu option while it is speaking. While typing, star keeps each character, star twice deletes one, and pound finishes the entry. During recording, 0 finishes and pound cancels. Hang up to end the call.", "info", nil},
	"help_reading":           {"Reading email. Press 1 for browsing messages, 2 for controls while reading, or 3 for message details and changes.", "info", []State{"help_browsing", "help_read_controls", "help_message_actions"}},
	"help_browsing":          {"From the main menu, press 1 for email, then 0 for unread mail across your accounts. Or choose an account, press 1, and choose a folder. The list announces the sender and subject. Press 1 to read the message, 2 for the next, or 3 for the previous. Press 4 to delete, 5 to reply, or 6 to forward. At either end, next and previous wrap around the list. Pound returns to the previous menu.", "help_reading", nil},
	"help_read_controls":     {"While reading a message, press 1 to read it again, 2 to change its read status, 3 to reply, or 4 to reply to everyone. Press 5 to forward, 6 to delete, 7 to move, 8 for attachments, or 9 for details. Press 0 to hear the next message's sender and subject, or pound to return to the list. A keypress stops the reading and selects that action. Star repeats the controls.", "help_reading", nil},
	"help_message_actions":   {"Reading a message marks it as read. Press 2 while in that message to switch between read and unread. Delete asks for confirmation: 1 deletes, 2 cancels. Move asks you to choose a destination; 0 gives more destinations when available. For details, press 9 while in the message: 1 saves the sender as a contact, 2 reads the date, 3 reads recipients, and 4 repeats the subject.", "help_reading", nil},
	"help_writing":           {"Writing email. Press 1 for recipients, 2 for the subject and message, 3 for review and saved drafts, or 4 for replies and forwarding.", "info", []State{"help_recipients", "help_compose_body", "help_drafts", "help_replies"}},
	"help_recipients":        {"Choose an account, press 2 to send email, then 1 for a new message. Type the recipient's email address and press pound. Listen to the spelling: 1 accepts it, 2 edits it, and 3 starts that entry over. Then press 1 to add another main recipient, 2 to add a copy recipient, or 3 to add a hidden copy recipient. Press 4 to continue. For typing and symbols, go back to the main help menu and press 4.", "help_writing", nil},
	"help_compose_body":      {"For both the subject and message, choose 1 to type or 2 to speak. After typing, pound finishes the entry. Listen to the confirmation: 1 accepts, 2 edits, and 3 starts over. For speech, wait until the instructions finish, then speak. Press 0 to finish early or pound to cancel. The prompt tells you the recording limit, normally 30 seconds. Check the recognized text before sending; you can edit it from the review menu.", "help_writing", nil},
	"help_drafts":            {"At review, press 1 to send, 2 to save a draft, 3 to cancel, 4 to edit, 5 to record an audio attachment, or 6 to hear the message before sending. Cancellation asks you to press 3 again. To edit, choose 1 for recipients, 2 for the subject, 3 for the body, or 4 for attachments. To resume a saved draft later, choose its account, press 2, then 2 again. Saving a draft does not send it.", "help_writing", nil},
	"help_replies":           {"From the message list, 5 replies and 6 forwards. While reading, 3 replies to the sender, 4 replies to everyone, and 5 forwards. A reply starts with the recipients filled in. Forwarding asks whether to include the original message and its attachments, then asks for a recipient. You can review recipients, subject, body, and attachments before you send.", "help_writing", nil},
	"help_typing":            {"Keypad typing. Press 1 for letters and numbers, 2 for email symbols, 3 for correcting an entry, or 4 for a typing example.", "info", []State{"help_letters", "help_symbols", "help_editing", "help_typing_example"}},
	"help_letters":           {KeypadInstructions + " Key 2 has A, B, C. 3 has D, E, F. 4 has G, H, I. 5 has J, K, L. 6 has M, N, O. 7 has P, Q, R, S. 8 has T, U, V. 9 has W, X, Y, Z. Key 0 cycles between zero and space. You do not need to type quickly.", "help_typing", nil},
	"help_symbols":           {"Symbols use key 1. Start a new character, then press 1 once for the digit one, twice for at, three times for dot, four for question mark, five for ampersand, six for plus, seven for dash, eight for underscore, or nine for equals. Press star to keep the symbol. For zero, press 0 once. For a space, press 0 twice, then star. Press 1 to repeat this list, or 2 for the remaining symbols.", "help_typing", []State{"help_symbols", "help_symbols_more"}},
	"help_symbols_more":      {"Continuing key 1's cycle: ten presses gives slash, eleven colon, twelve semicolon, thirteen comma, fourteen dollar sign, fifteen percent, sixteen open parenthesis, seventeen close parenthesis, eighteen exclamation mark, nineteen hash, twenty asterisk, twenty-one apostrophe, and twenty-two quotation mark. The next press returns to the digit one. Press star to keep the symbol. Use key 1's cycle to type a hash or asterisk; the pound and star keys are editing controls.", "help_symbols", nil},
	"help_editing":           {"Star keeps the character you are choosing. Star never deletes on a single press. Press star twice to delete the last character, and each further double-press deletes another. Pausing does not finish a character: use a different key or star. Pound finishes the field and reads it back. Then 1 accepts, 2 lets you edit the existing text, and 3 clears that field so you can start again. An empty subject or body is announced as blank.", "help_typing", nil},
	"help_typing_example":    {"To type A, B: press 2 once, star, then 2 twice, star. To type the number two, press 2 four times, then star. To type at, press 1 twice, then star. To type dot, press 1 three times, then star. For example, A at B dot C O M is: 2, star; 1 twice, star; 2 twice, star; 1 three times, star; 2 three times, star; 6 three times, star; 6 once, then pound. Listen to the address before accepting it.", "help_typing", nil},
	"help_attachments":       {"Attachments. Press 1 for listening to received attachments, or 2 for recording and editing attachments.", "info", []State{"help_attachment_listen", "help_attachment_record"}},
	"help_attachment_listen": {"While reading a message, press 8 for attachments you can listen to. Choose the announced number. Press 0 for more attachments when offered. Any key stops attachment playback. Audio files and the sound from supported video files can be played. Documents and pictures cannot be read aloud here. You can open them in your email app. Pound returns to the message controls.", "help_attachments", nil},
	"help_attachment_record": {"At message review, press 5 to add a voice attachment. Wait until the instructions finish, then speak. The prompt tells you the recording limit. Press 0 to finish or pound to cancel. The recording is attached as audio, without turning it into text. To manage attachments, press 4 at review, then 4 again. Press 1 to add, 2 to remove the last attachment, or 3 to record another version. Removing asks for a second press of 2.", "help_attachments", nil},
	"help_settings":          {"Contacts and settings. Press 1 for contacts, 2 for voice settings, or 3 for call alerts.", "info", []State{"help_contacts", "help_voice", "help_alerts"}},
	"help_contacts":          {"To email a saved contact, press 2 at the main menu, then 3, and choose the contact. Press 9 for more contacts when offered. To save a message's sender, open the message, press 9, then 1. Type a name, finish with pound, and confirm the name and address. You can also manage contacts in the web console.", "help_settings", nil},
	"help_voice":             {"Choose your voice and speech speed in the web console. The phone's settings menu explains where to change them. You can choose separate speech speeds for menus and email. While listening to menus, you may press your choice before the prompt finishes. Star repeats the current menu.", "help_settings", nil},
	"help_alerts":            {"Call alerts let Vox Mail call you about new email. When enabled by your administrator, press 2 at the main menu, then 2 to switch your alerts on or off. For one account, choose it under email and press 4 for account settings. There, 1 switches that account's alerts on or off, and 2 chooses its alert folders. Selecting a folder switches its selection. Press 9 for more folders when offered, and pound when finished.", "help_settings", nil},
	"help_accounts":          {"Accounts and folders. From the main menu, press 1 and choose an account. Press 1 to browse its folders, 2 to compose, or 3 to refresh its mail. At the top of the folder list, 1 is Inbox, 2 Drafts, 3 Sent, and 4 Trash. Other folders use the announced keys; 9 gives more when offered. A folder with subfolders lets you choose 1 for its messages or 2 for its subfolders. Configure accounts and folder mappings in the web console.", "info", nil},
	"help_troubleshooting":   {"Troubleshooting. Press star in a menu to hear the choices again. If a key is unavailable, use an announced choice or pound to go back. For missing mail, choose the account and press 3 to refresh, then reopen its folder. If speech entry fails, try again or choose keypad entry. If an address is rejected, your typed text is kept: press star twice to delete the last character, and pound checks it again. Persistent account, connection, or sign-in problems may need your administrator's help.", "info", nil},
}

func registerHelpPages() {
	for state, page := range helpPages {
		keys := []string{"1"} // A leaf's numeric repeat is useful on phones without *.
		text := page.text
		if len(page.children) == 0 {
			text += " Press 1 or star to repeat, or pound to go back."
		} else {
			keys = nil
			for i := range page.children {
				keys = append(keys, string(rune('1'+i)))
			}
			text += " Star repeats these choices. Pound goes back."
		}
		specs[state] = StateSpec{State: state, Prompt: text, Valid: fixed(keys...), Back: page.back, Timeout: "closed", Repeatable: true}
	}
}

func IsHelpState(state State) bool { _, ok := helpPages[state]; return ok }

func HelpDestination(state State, key string) (State, bool) {
	page, ok := helpPages[state]
	if !ok || !Accepts(state, key) {
		return "", false
	}
	if len(page.children) == 0 {
		return state, true
	}
	return page.children[int(key[0]-'1')], true
}
