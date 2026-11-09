package ivr

const MainMenuPrompt = "Main menu. Press 1 for email, 2 for settings, or 3 for help. Star repeats the choices."

// StateSpec is the contract for one call state. Dynamic list states use an
// acceptance predicate because their valid number of items is data-driven.
type StateSpec struct {
	State      State
	Prompt     string
	Valid      func(string) bool
	Back       State
	Timeout    State
	Repeatable bool
}

func fixed(keys ...string) func(string) bool {
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return func(key string) bool { return set[key] }
}

func digits(min, max int) func(string) bool {
	return func(key string) bool {
		if len(key) != 1 || key[0] < '0' || key[0] > '9' {
			return false
		}
		value := int(key[0] - '0')
		return value >= min && value <= max
	}
}

var specs = map[State]StateSpec{
	"closed":                {State: "closed", Prompt: "This call is closed.", Valid: fixed(), Timeout: "closed", Repeatable: true},
	"pin":                   {State: "pin", Prompt: "Enter your PIN, then press pound.", Valid: nil, Back: "closed", Timeout: "closed", Repeatable: true},
	"account_choice":        {State: "account_choice", Prompt: "Please choose the VOXMail account.", Valid: digits(1, 9), Back: "closed", Timeout: "closed", Repeatable: true},
	"verification":          {State: "verification", Prompt: "One moment.", Valid: nil, Back: "closed", Timeout: "closed", Repeatable: true},
	"main":                  {State: "main", Prompt: MainMenuPrompt, Valid: fixed("1", "2", "3"), Repeatable: true},
	"accounts":              {State: "accounts", Prompt: "Press 0 for all unread mail, choose an account, press 9 for more if available, or press pound to go back.", Valid: digits(0, 9), Back: "main", Repeatable: true},
	"account_menu":          {State: "account_menu", Prompt: "Press 1 to listen to mail, 2 to send an email, 3 to refresh, 4 for account settings when available, or 5 for the unread count.", Valid: fixed("1", "2", "3", "4", "5"), Back: "accounts", Repeatable: true},
	"draft_menu":            {State: "draft_menu", Prompt: "Press 1 for a new email, 2 to resume a saved draft, or pound to go back.", Valid: fixed("1", "2"), Back: "account_menu", Repeatable: true},
	"draft_list":            {State: "draft_list", Prompt: "Choose a saved draft, press 9 for more if available, or pound to go back.", Valid: digits(1, 9), Back: "draft_menu", Repeatable: true},
	"draft_edit":            {State: "draft_edit", Prompt: "Edit message. Press 1 for recipients, 2 for the subject, 3 for the body, or 4 for attachments. Pound returns to review.", Valid: fixed("1", "2", "3", "4"), Back: "review", Repeatable: true},
	"recipient_edit":        {State: "recipient_edit", Prompt: "Recipients. Press 1 to replace the main recipients, 2 to remove copy recipients, 3 to remove hidden copy recipients, or 4 to add a recipient. Pound goes back.", Valid: fixed("1", "2", "3", "4"), Back: "draft_edit", Repeatable: true},
	"attachment_edit":       {State: "attachment_edit", Prompt: "Press 1 to add, 2 to remove the last, or 3 to re-record the last attachment.", Valid: fixed("1", "2", "3"), Back: "draft_edit", Repeatable: true},
	"account_settings":      {State: "account_settings", Prompt: "Press 1 to toggle call alerts, or 2 to choose alert folders. Press pound to go back.", Valid: fixed("1", "2"), Back: "account_menu", Repeatable: true},
	"account_alert_folders": {State: "account_alert_folders", Prompt: "Choose alert folders, press 9 for more if available, or pound when finished.", Valid: digits(1, 9), Back: "account_settings", Repeatable: true},
	"folders":               {State: "folders", Prompt: "At the root, press 1 for Inbox, 2 for Drafts, 3 for Sent, or 4 for Trash. Press 5 through 8 for other folders and 9 for more. Press pound to go back.", Valid: digits(1, 9), Back: "account_menu", Repeatable: true},
	"folder_action":         {State: "folder_action", Prompt: "Press 1 to listen to messages here, or 2 to open nested folders.", Valid: fixed("1", "2"), Back: "folders", Repeatable: true},
	"list":                  {State: "list", Prompt: "Press 1 to read, 2 for next, 3 for previous, 4 to delete, 5 to reply, 6 to forward, 7 for all message options, or pound to go back.", Valid: fixed("1", "2", "3", "4", "5", "6", "7"), Repeatable: true},
	"list_actions":          {State: "list_actions", Prompt: "Message options. Press 1 to read, 2 to mark read or unread, 3 to reply, 4 to reply all, 5 to forward, 6 to delete, 7 to move, 8 for attachments, 9 for more options, 0 for the next message, or pound to return.", Valid: fixed("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"), Back: "list", Repeatable: true},
	"read":                  {State: "read", Prompt: "Message controls. Press 0 for the next message, 1 to read again, 3 to reply, or 6 to delete. Press 2 to mark read or unread, 4 to reply all, 5 to forward, 7 to move, 8 for attachments, 9 for more options, or pound to return.", Valid: fixed("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"), Back: "list", Repeatable: true},
	"attachment_menu":       {State: "attachment_menu", Prompt: "Choose an attachment, press 0 for more if available, or pound to return.", Valid: digits(0, 9), Back: "read", Repeatable: true},
	"attachment_playback":   {State: "attachment_playback", Prompt: "Playing the attachment. Press any key to stop.", Valid: digits(0, 9), Back: "read", Repeatable: true},
	"move_menu":             {State: "move_menu", Prompt: "Choose a destination folder, press 0 for more if available, or pound to return.", Valid: digits(0, 9), Back: "read", Repeatable: true},
	"more_options":          {State: "more_options", Prompt: "More options. Press 1 to add the sender to contacts, 2 for the date, 3 for recipients, 4 for the subject, or pound to return.", Valid: fixed("1", "2", "3", "4"), Back: "read", Repeatable: true},
	"contact_name":          {State: "contact_name", Prompt: "Type a name for this contact. " + TextKeypadInstructions, Valid: nil, Back: "more_options", Repeatable: true},
	"contact_confirm":       {State: "contact_confirm", Prompt: "Press 1 to save this contact or 2 to cancel.", Valid: fixed("1", "2"), Back: "more_options", Repeatable: true},
	"field_confirm":         {State: "field_confirm", Prompt: "Press 1 to accept, 2 to edit, or 3 to start this entry over.", Valid: fixed("1", "2", "3"), Back: "compose", Repeatable: true},
	"confirm_delete":        {State: "confirm_delete", Prompt: "Delete this message? Press 1 to confirm or 2 to cancel.", Valid: fixed("1", "2"), Back: "list", Repeatable: true},
	"compose":               {State: "compose", Prompt: "Type the recipient's email address. " + EmailKeypadInstructions, Valid: nil, Back: "main", Repeatable: true},
	"recipient_pick":        {State: "recipient_pick", Prompt: "Press 1 to type an address, or 2 to choose a contact.", Valid: fixed("1", "2"), Back: "main", Repeatable: true},
	"contact_pick":          {State: "contact_pick", Prompt: "Choose a contact, press 9 for more if available, or pound to go back.", Valid: digits(1, 9), Back: "recipient_pick", Repeatable: true},
	"recipient_menu":        {State: "recipient_menu", Prompt: "Press 1 to add a main recipient, 2 to add a copy recipient, 3 to add a hidden copy recipient, or 4 to continue.", Valid: fixed("1", "2", "3", "4"), Back: "compose", Repeatable: true},
	"recipient_input":       {State: "recipient_input", Prompt: "Type the email address. " + EmailKeypadInstructions, Valid: nil, Back: "recipient_menu", Repeatable: true},
	"subject_method":        {State: "subject_method", Prompt: "For the subject, press 1 to type with the keypad, 2 to speak, or 3 to skip the subject.", Valid: fixed("1", "2", "3"), Back: "recipient_menu", Repeatable: true},
	"subject":               {State: "subject", Prompt: "Type the subject. " + TextKeypadInstructions, Valid: nil, Back: "compose", Repeatable: true},
	"body_method":           {State: "body_method", Prompt: "For the message body, press 1 to type with the keypad or 2 to speak.", Valid: fixed("1", "2"), Back: "subject", Repeatable: true},
	"body":                  {State: "body", Prompt: "Type your message. " + TextKeypadInstructions, Valid: nil, Back: "subject", Repeatable: true},
	"review":                {State: "review", Prompt: "Message review. Press 1 to send, 2 to save a draft, 3 to cancel, 4 to edit, 5 to add an audio attachment, or 6 to hear your message. Pound goes back.", Valid: fixed("1", "2", "3", "4", "5", "6"), Back: "body", Repeatable: true},
	"forward_options":       {State: "forward_options", Prompt: "Forward the original message with its attachments? Press 1 for yes or 2 for no.", Valid: fixed("1", "2"), Back: "read", Repeatable: true},
	"audio_recording":       {State: "audio_recording", Prompt: "Record an audio attachment after these instructions. Press 0 to finish or pound to cancel.", Valid: fixed("0"), Back: "review", Repeatable: true},
	"recording":             {State: "recording", Prompt: "Record your message after these instructions, for up to 2 minutes. Press 0 to finish or pound to cancel.", Valid: fixed("0"), Back: "body_method", Repeatable: true},
	"recording_subject":     {State: "recording_subject", Prompt: "Record your subject after these instructions, for up to 2 minutes. Press 0 to finish or pound to cancel.", Valid: fixed("0"), Back: "subject_method", Repeatable: true},
	"settings":              {State: "settings", Prompt: "Press 1 for voice settings, 2 to toggle call alerts when available, 3 for contacts, or pound to go back.", Valid: fixed("1", "2", "3"), Back: "main", Repeatable: true},
	"contacts":              {State: "contacts", Prompt: "Choose a contact, press 9 for more if available, or press pound to go back.", Valid: digits(1, 9), Back: "settings", Repeatable: true},
}

func init() {
	registerHelpPages()
	// Every live state has the same explicit safety policy until a more
	// specific timeout is needed: inactivity returns the call to the closed
	// state. Keeping this in the contract prevents a newly added state from
	// silently becoming an unbounded call.
	for state, spec := range specs {
		if spec.Timeout == "" {
			spec.Timeout = "closed"
			specs[state] = spec
		}
	}
}

func Spec(state State) (StateSpec, bool) {
	spec, ok := specs[state]
	return spec, ok
}

// Accepts reports only state-specific restrictions. A nil Valid function is
// an editor/dynamic-list state and accepts the digit before its handler checks
// ownership, page bounds, or the keypad grammar.
func Accepts(state State, key string) bool {
	spec, ok := Spec(state)
	if !ok {
		return false
	}
	if spec.Valid == nil {
		return true
	}
	return spec.Valid(key)
}

// Back returns the contract-defined destination for the pound navigation key.
// Handlers may still apply a data-dependent override for recursive menus.
func Back(state State) (State, bool) {
	spec, ok := Spec(state)
	if !ok || spec.Back == "" {
		return "", false
	}
	return spec.Back, true
}

func Prompt(state State) (string, bool) {
	spec, ok := Spec(state)
	return spec.Prompt, ok && spec.Prompt != ""
}

func States() []StateSpec {
	out := make([]StateSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec)
	}
	return out
}
