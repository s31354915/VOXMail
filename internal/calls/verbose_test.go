package calls

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/bridge"
	"github.com/voxmail/voxmail/internal/diagnostics"
)

func TestVerboseDTMFReportsDecisionsWithoutRecordingKeys(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	for _, mode := range []string{"false", "true"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VOXMAIL_VERBOSE", mode)
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, diagnostics.HandlerOptions())))
			d := newCallEventDispatcher(context.Background(), &Service{}, nil)
			for _, msg := range []bridge.Message{
				{Type: "dtmf", CallID: "fixture", Digit: "7", Phase: "start"},
				{Type: "dtmf", CallID: "fixture", Digit: "7", Phase: "start"},
				{Type: "dtmf", CallID: "fixture", Phase: "end"},
				{Type: "dtmf", CallID: "fixture", Digit: "private-pin", Phase: "start"},
			} {
				if err := d.dispatch(msg); err != nil {
					t.Fatal(err)
				}
			}
			d.close()
			if mode == "false" && logs.Len() != 0 {
				t.Fatalf("normal mode logs=%s", logs.String())
			}
			if mode == "true" {
				for _, outcome := range []string{"queued", "duplicate ignored", "release ignored", "invalid input ignored"} {
					if !strings.Contains(logs.String(), outcome) {
						t.Fatalf("missing %q in %s", outcome, logs.String())
					}
				}
			}
			if strings.Contains(logs.String(), "private-pin") || strings.Contains(logs.String(), `"digit"`) {
				t.Fatalf("DTMF value leaked: %s", logs.String())
			}
		})
	}
}
