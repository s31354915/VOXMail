package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestVoiceKeepWarmRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "warm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	rows, err := db.KeepWarmVoices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("KeepWarmVoices()=%v on a fresh store, want empty", rows)
	}

	if err := db.SetVoiceKeepWarm(ctx, "en_US-hfc_male-medium", true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetVoiceKeepWarm(ctx, "en_US-bryce-medium", true); err != nil {
		t.Fatal(err)
	}
	rows, err = db.KeepWarmVoices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows["en_US-hfc_male-medium"] || !rows["en_US-bryce-medium"] {
		t.Fatalf("KeepWarmVoices()=%v, want both voices true", rows)
	}

	if err := db.SetVoiceKeepWarm(ctx, "en_US-bryce-medium", false); err != nil {
		t.Fatal(err)
	}
	rows, err = db.KeepWarmVoices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows["en_US-hfc_male-medium"] || rows["en_US-bryce-medium"] {
		t.Fatalf("KeepWarmVoices()=%v after unpin, want only the default voice enabled", rows)
	}
}
