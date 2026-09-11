package ivr

// StateSpec is the contract for one call state. Dynamic list states use an
// acceptance predicate because their valid number of items is data-driven.
type StateSpec struct {
	State      State
	Prompt     string
	Valid      func(string) bool
	Back       State
	Timeout    State
	BargeIn    bool
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
	"pin":                   {State: "pin", Prompt: "Enter your PIN, then press pound.", Valid: nil, Back: "closed", Timeout: "closed", Repeatable: true},
	"main":                  {State: "main", Prompt: "Press 1 for email, 2 for settings, or 3 for information and instructions.", Valid: fixed("1", "2", "3"), Repeatable: true},
	"accounts":              {State: "accounts", Prompt: "Press 0 for all unread mail, choose an account, or press pound to go back.", Valid: digits(0, 9), Back: "main", Repeatable: true},
	"account_menu":          {State: "account_menu", Prompt: "Press 1 to listen to mail, 2 to send an email, 3 to refresh, or 4 for account settings.", Valid: fixed("1", "2", "3", "4"), Back: "accounts", Repeatable: true},
	"draft_menu":            {State: "draft_menu", Prompt: "Press 1 for a new email, 2 to resume a saved draft, or pound to go back.", Valid: fixed("1", "2"), Back: "account_menu", Repeatable: true},
	"draft_list":            {State: "draft_list", Prompt: "Choose a saved draft, press 9 for more, or pound to go back.", Valid: digits(1, 9), Back: "draft_menu", Repeatable: true},
	"draft_edit":            {State: "draft_edit", Prompt: "Press 1 to edit recipients, 2 for the subject, 3 for the body, or 4 for attachments.", Valid: fixed("1", "2", "3", "4"), Back: "review", Repeatable: true},
	"recipient_edit":        {State: "recipient_edit", Prompt: "Press 1 to replace To recipients, 2 to clear Cc, 3 to clear Bcc, or 4 to add a recipient.", Valid: fixed("1", "2", "3", "4"), Back: "draft_edit", Repeatable: true},
	"attachment_edit":       {State: "attachment_edit", Prompt: "Press 1 to add, 2 to remove the last, or 3 to re-record the last attachment.", Valid: fixed("1", "2", "3"), Back: "draft_edit", Repeatable: true},
	"account_settings":      {State: "account_settings", Prompt: "Press 1 to toggle call alerts, or 2 to choose alert folders. Press pound to go back.", Valid: fixed("1", "2"), Back: "account_menu", Repeatable: true},
	"account_alert_folders": {State: "account_alert_folders", Prompt: "Choose alert folders, press 9 for more, or pound when finished.", Valid: digits(1, 9), Back: "account_settings", Repeatable: true},
	"folders":               {State: "folders", Prompt: "Choose a folder, press 0 for Inbox at the root, or press pound to go back.", Valid: digits(0, 9), Back: "account_menu", Repeatable: true},
	"folder_action":         {State: "folder_action", Prompt: "Press 1 to listen to messages here, or 2 to open nested folders.", Valid: fixed("1", "2"), Back: "folders", Repeatable: true},
	"list":                  {State: "list", Prompt: "Press 1 to read, 2 for next, 3 for previous, 4 to delete, 5 to reply, or pound to go back.", Valid: fixed("1", "2", "3", "4", "5", "6"), Repeatable: true},
	"read":                  {State: "read", Prompt: "Press 1 to listen, 2 to mark read or unread, 3 to reply, 4 to reply all, 5 to forward, 6 to delete, 7 to move, 8 for attachments, 9 for more options, 0 for next, or pound to return.", Valid: fixed("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"), Back: "list", BargeIn: true, Repeatable: true},
	"attachment_menu":       {State: "attachment_menu", Prompt: "Choose an attachment, press 0 for more, or pound to return.", Valid: digits(0, 9), Back: "read", BargeIn: true, Repeatable: true},
	"attachment_playback":   {State: "attachment_playback", Prompt: "Playing the attachment. Press pound to stop.", Valid: digits(0, 9), Back: "read", BargeIn: true, Repeatable: true},
	"move_menu":             {State: "move_menu", Prompt: "Choose a destination folder, or press pound to return.", Valid: digits(1, 9), Back: "read", Repeatable: true},
	"more_options":          {State: "more_options", Prompt: "More options. Press 1 to add the sender to contacts, 2 for the date, 3 for recipients, 4 for the subject, or pound to return.", Valid: fixed("1", "2", "3", "4"), Back: "read", Repeatable: true},
	"contact_name":          {State: "contact_name", Prompt: "Enter a contact name using the keypad, then press pound.", Valid: nil, Back: "more_options", Repeatable: true},
	"contact_confirm":       {State: "contact_confirm", Prompt: "Press 1 to save this contact or 2 to cancel.", Valid: fixed("1", "2"), Back: "more_options", Repeatable: true},
	"field_confirm":         {State: "field_confirm", Prompt: "Press 1 to accept or 2 to re-enter.", Valid: fixed("1", "2"), Back: "compose", Repeatable: true},
	"confirm_delete":        {State: "confirm_delete", Prompt: "Delete this message? Press 1 to confirm or 2 to cancel.", Valid: fixed("1", "2"), Back: "list", Repeatable: true},
	"compose":               {State: "compose", Prompt: "Enter the recipient using multi tap, then press pound.", Valid: nil, Back: "main", Repeatable: true},
	"recipient_menu":        {State: "recipient_menu", Prompt: "Press 1 to add To, 2 to add Cc, 3 to add Bcc, or 4 to continue.", Valid: fixed("1", "2", "3", "4"), Back: "compose", Repeatable: true},
	"recipient_input":       {State: "recipient_input", Prompt: "Enter the email address using multi tap, then press pound.", Valid: nil, Back: "recipient_menu", Repeatable: true},
	"subject_method":        {State: "subject_method", Prompt: "For the subject, press 1 to type with the keypad or 2 to speak.", Valid: fixed("1", "2"), Back: "recipient_menu", Repeatable: true},
	"subject":               {State: "subject", Prompt: "Enter the subject using multi tap, then press pound.", Valid: nil, Back: "compose", Repeatable: true},
	"body_method":           {State: "body_method", Prompt: "For the message body, press 1 to type with the keypad or 2 to speak.", Valid: fixed("1", "2"), Back: "subject", Repeatable: true},
	"body":                  {State: "body", Prompt: "Enter the message using multi tap, then press pound.", Valid: nil, Back: "subject", Repeatable: true},
	"review":                {State: "review", Prompt: "Press 1 to send, 2 to save as draft, 3 to cancel, 4 to edit the message, 5 to add an audio attachment, or pound to go back.", Valid: fixed("1", "2", "3", "4", "5"), Back: "body", Repeatable: true},
	"forward_options":       {State: "forward_options", Prompt: "Forward the original message with its attachments? Press 1 for yes or 2 for no.", Valid: fixed("1", "2"), Back: "read", Repeatable: true},
	"audio_recording":       {State: "audio_recording", Prompt: "Playing or recording an audio attachment. Press 0 when finished.", Valid: fixed("0"), Back: "review", BargeIn: true, Repeatable: true},
	"recording":             {State: "recording", Prompt: "Speak your message after the tone. Press 0 when finished, or pound to cancel.", Valid: fixed("0"), Back: "body_method", Repeatable: true},
	"recording_subject":     {State: "recording_subject", Prompt: "Speak the subject after the tone. Press 0 when finished, or pound to cancel.", Valid: fixed("0"), Back: "subject_method", Repeatable: true},
	"settings":              {State: "settings", Prompt: "Press 1 for voice settings, 2 to toggle call alerts, 3 for contacts, or pound to go back.", Valid: fixed("1", "2", "3"), Back: "main", Repeatable: true},
	"contacts":              {State: "contacts", Prompt: "Choose a contact or press pound to go back.", Valid: digits(0, 9), Back: "settings", Repeatable: true},
	"info":                  {State: "info", Prompt: "VOXMail reads synchronized email over the phone. Press star to repeat, or pound to return.", Valid: fixed("1"), Back: "main", Repeatable: true},
}

func init() {
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
