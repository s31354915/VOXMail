package diagnostics

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

func TestVerboseProcessModes(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			t.Setenv("VOXMAIL_VERBOSE", enabled)
			for component, flag := range map[string]string{"mbsync": "--verbose", "piper": "--debug", "ffmpeg": "verbose", "baresip": "-v"} {
				args := ProcessArguments(component, []string{"input"})
				found := false
				for _, arg := range args {
					if arg == flag {
						found = true
					}
				}
				if found != (enabled == "true") {
					t.Fatalf("%s args=%v", component, args)
				}
			}
			args := ProcessArguments("whisper", []string{"-m", "model", "--no-prints"})
			if strings.Contains(strings.Join(args, " "), "--no-prints") != (enabled == "false") {
				t.Fatalf("whisper args=%v", args)
			}
		})
	}
}

func TestProcessDiagnosticsAreCorrelatedAndKeepInputsPrivate(t *testing.T) {
	original := slog.Default()
	defer slog.SetDefault(original)
	for _, enabled := range []string{"false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			t.Setenv("VOXMAIL_VERBOSE", enabled)
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, HandlerOptions())))
			command := exec.CommandContext(context.Background(), "sh", "-c", "echo password=private-password >&2; exit 7")
			output, err := CombinedOutput(context.Background(), "fixture", command)
			if err == nil || !bytes.Contains(output, []byte("private-password")) {
				t.Fatalf("process result=%q err=%v", output, err)
			}
			if strings.Contains(logs.String(), "private-password") || strings.Contains(logs.String(), "password=") {
				t.Fatalf("process inputs leaked: %s", logs.String())
			}
			if enabled == "false" && logs.Len() != 0 {
				t.Fatalf("normal mode trace=%s", logs.String())
			}
			if enabled == "true" {
				for _, field := range []string{"operation started", "operation finished", "operation_id", "duration_ms", `"exit_code":7`, `"status":"failed"`} {
					if !strings.Contains(logs.String(), field) {
						t.Fatalf("missing %q in %s", field, logs.String())
					}
				}
			}
		})
	}
}
