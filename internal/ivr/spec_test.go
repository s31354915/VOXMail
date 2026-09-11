package ivr

import "testing"

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
