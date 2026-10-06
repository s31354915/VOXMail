package ivr

import (
	"strings"
	"testing"
)

func TestEveryDefinedStateHasNavigationContract(t *testing.T) {
	for _, spec := range States() {
		if spec.State == "" || spec.Prompt == "" || !spec.Repeatable || spec.Timeout == "" {
			t.Fatalf("incomplete state spec: %+v", spec)
		}
	}
}

func TestPinStateAndTimeoutContract(t *testing.T) {
	spec, ok := Spec("pin")
	if !ok || spec.Back != "closed" || spec.Timeout != "closed" {
		t.Fatalf("PIN timeout contract is wrong: %+v", spec)
	}
}

func TestStateSpecAcceptsOnlyFixedKeys(t *testing.T) {
	if !Accepts("main", "1") || Accepts("main", "9") {
		t.Fatal("main menu key contract is wrong")
	}
	if !Accepts("recording", "0") || Accepts("recording", "1") {
		t.Fatal("recording stop contract is wrong")
	}
	if !Accepts("move_menu", "0") || Accepts("contacts", "0") {
		t.Fatal("pagination/contact key contract is wrong")
	}
	if prompt, ok := Prompt("list"); !ok || !strings.Contains(prompt, "6 to forward") {
		t.Fatalf("message-list prompt omitted forwarding: %q", prompt)
	}
	if prompt, ok := Prompt("info"); !ok || !strings.Contains(prompt, "Press 1 or star to repeat") {
		t.Fatalf("info prompt omitted its numeric repeat key: %q", prompt)
	}
	if !Accepts("body", "9") {
		t.Fatal("editor state should defer validation to keypad input")
	}
	if Accepts("unknown", "1") {
		t.Fatal("unknown state must not accept DTMF")
	}
}

func TestBackUsesStateContract(t *testing.T) {
	if back, ok := Back("read"); !ok || back != "list" {
		t.Fatalf("read back target=%q,%v, want list,true", back, ok)
	}
	if _, ok := Back("main"); ok {
		t.Fatal("root state unexpectedly has a back target")
	}
}

func TestAccountAlertStatesHaveNavigationContracts(t *testing.T) {
	settings, ok := Spec("account_settings")
	if !ok || settings.Back != "account_menu" || !Accepts("account_settings", "1") || Accepts("account_settings", "9") {
		t.Fatalf("account settings contract is wrong: %+v", settings)
	}
	folders, ok := Spec("account_alert_folders")
	if !ok || folders.Back != "account_settings" || !Accepts("account_alert_folders", "9") || Accepts("account_alert_folders", "0") {
		t.Fatalf("account alert folders contract is wrong: %+v", folders)
	}
}

func TestEveryStateHasTableDrivenInputNavigationAndCancellationTrace(t *testing.T) {
	validKeys := map[State]string{
		"closed": "",
		"pin":    "1", "main": "1", "accounts": "0", "account_menu": "1",
		"draft_menu": "1", "draft_list": "1", "draft_edit": "1",
		"recipient_edit": "1", "attachment_edit": "1", "account_settings": "1",
		"account_alert_folders": "1", "folders": "0", "folder_action": "1",
		"list": "1", "read": "0", "attachment_menu": "0",
		"attachment_playback": "0", "move_menu": "1", "more_options": "1",
		"contact_name": "1", "contact_confirm": "1", "field_confirm": "1",
		"confirm_delete": "1", "compose": "1", "recipient_menu": "1",
		"recipient_input": "1", "subject_method": "1", "subject": "1",
		"body_method": "1", "body": "1", "review": "1",
		"forward_options": "1", "audio_recording": "0", "recording": "0",
		"recording_subject": "0", "settings": "1", "contacts": "1", "info": "1",
	}
	for _, spec := range States() {
		valid, ok := validKeys[spec.State]
		if !ok {
			t.Fatalf("state %q has no trace fixture", spec.State)
		}
		if spec.State != StateClosed && !Accepts(spec.State, valid) {
			t.Errorf("state %q rejected valid trace key %q", spec.State, valid)
		}
		if (spec.Valid != nil || spec.State == StateClosed) && Accepts(spec.State, "X") {
			t.Errorf("state %q accepted invalid trace key", spec.State)
		}
		first, firstOK := Prompt(spec.State)
		second, secondOK := Prompt(spec.State)
		if !firstOK || !secondOK || first != second || !spec.Repeatable {
			t.Errorf("state %q repeat trace is not stable", spec.State)
		}
		if spec.Back != "" {
			if back, ok := Back(spec.State); !ok || back != spec.Back {
				t.Errorf("state %q back=%q,%v, want %q,true", spec.State, back, ok, spec.Back)
			}
		}
		timeout, ok := Spec(spec.Timeout)
		if !ok || timeout.State != spec.Timeout {
			t.Errorf("state %q timeout target=%q is not declared", spec.State, spec.Timeout)
		}
		flow := NewSession("trace-" + string(spec.State))
		flow.State = spec.State
		flow.Authenticated = true
		flow.Close()
		if flow.State != StateClosed || flow.Authenticated {
			t.Errorf("state %q cancellation did not revoke terminal session", spec.State)
		}
	}
}
