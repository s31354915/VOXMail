package diagnostics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

func TestOutputFiltersFragmentedCredentialsAndDTMF(t *testing.T) {
	var dest bytes.Buffer
	output := NewOutput(context.Background(), "baresip", &dest)
	for _, fragment := range []string{"registration complete\npass", "word=private-value\naudio: send DTMF digit: '7'\n", "voxmail: DTMF callback call_id=fixture phase=start (value redacted)\n", strings.Repeat("x", maxDiagnosticLine+20) + "\n", "last line"} {
		if _, err := io.WriteString(output, fragment); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-value", "'7'", strings.Repeat("x", 20)} {
		if strings.Contains(dest.String(), secret) {
			t.Fatalf("unsafe output=%s", dest.String())
		}
	}
	for _, wanted := range []string{"registration complete", "credential-bearing", "phase=start", "oversized diagnostic", "last line"} {
		if !strings.Contains(dest.String(), wanted) {
			t.Fatalf("missing %q in %s", wanted, dest.String())
		}
	}
}

func TestVerboseRunPreservesCombinedCaptureAndStreamsSafeOutput(t *testing.T) {
	t.Setenv("VOXMAIL_VERBOSE", "true")
	original := slog.Default()
	defer slog.SetDefault(original)
	var logs, capture bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, HandlerOptions())))
	command := exec.CommandContext(context.Background(), "sh", "-c", "echo 'model loaded'; echo 'password=private-value' >&2; echo 'engine timing=20ms' >&2")
	command.Stdout, command.Stderr = &capture, &capture
	if err := Run(context.Background(), "fixture", command); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capture.String(), "private-value") {
		t.Fatal("original capture changed")
	}
	if strings.Contains(logs.String(), "private-value") {
		t.Fatal("credential leaked into diagnostics")
	}
	for _, want := range []string{"model loaded", "engine timing=20ms", "child diagnostic"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in %s", want, logs.String())
		}
	}
}
