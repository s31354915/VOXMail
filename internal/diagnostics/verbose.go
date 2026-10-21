// Package diagnostics controls deployment-wide diagnostic output. Process
// input arguments, stdin and transcripts are excluded from operation traces.
// Child diagnostic lines are filtered before being copied into logs.
package diagnostics

import (
	"context"
	"errors"
	op "github.com/voxmail/voxmail/internal/operation"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func Enabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("VOXMAIL_VERBOSE")))
	return err == nil && enabled
}

// LogLevel resolves the deployment log level. VOXMAIL_LOG_LEVEL accepts
// debug, info, warn and error (case-insensitive); VOXMAIL_VERBOSE=true is the
// legacy switch that forces debug. An unset or unknown value means info.
func LogLevel() slog.Level {
	if Enabled() {
		return slog.LevelDebug
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VOXMAIL_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// HandlerOptions also works before configuration is loaded, allowing startup
// diagnostics to use the same deployment switch.
func HandlerOptions() *slog.HandlerOptions {
	return &slog.HandlerOptions{Level: LogLevel()}
}

var operations atomic.Uint64

// ProcessArguments enables the supported diagnostic mode for each pinned CLI.
// Protocol traces and speech debug dumps are excluded because they can contain
// authentication material or recorded audio.
func ProcessArguments(component string, args []string) []string {
	if !Enabled() {
		return args
	}
	switch component {
	case "mbsync":
		return append([]string{"--verbose"}, args...)
	case "piper":
		return append([]string{"--debug"}, args...)
	case "ffmpeg":
		return append([]string{"-loglevel", "verbose"}, args...)
	case "baresip":
		return append([]string{"-v", "-T"}, args...)
	case "whisper":
		out := make([]string, 0, len(args))
		for _, arg := range args {
			if arg != "--no-prints" {
				out = append(out, arg)
			}
		}
		return out
	}
	return args
}

// Start records a correlated operation and returns its completion recorder.
// Callers supply identifiers only; raw provider/process error strings remain
// at their existing reporting boundaries instead of being copied into traces.
func Start(ctx context.Context, component, operation string, fields ...any) func(error) {
	if !Enabled() {
		return func(error) {}
	}
	id := operations.Add(1)
	started := time.Now()
	attrs := append([]any{"component", component, "operation", operation, "operation_id", id}, fields...)
	slog.DebugContext(ctx, "operation started", attrs...)
	return func(err error) {
		status := "success"
		if err != nil {
			status = "failed"
		}
		exitCode := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exitCode = exit.ExitCode()
		}
		done := append(append([]any(nil), attrs...), "duration_ms", time.Since(started).Milliseconds(), "status", status, "exit_code", exitCode, "error_category", string(op.Classify(err)))
		if ctx.Err() != nil {
			done = append(done, "context_error", ctx.Err().Error())
		}
		slog.DebugContext(ctx, "operation finished", done...)
	}
}

// Run enables child diagnostics without changing stdout/stderr ownership.
func Run(ctx context.Context, component string, command *exec.Cmd) error {
	command.Args = append(command.Args[:1:1], ProcessArguments(component, command.Args[1:])...)
	finish := Start(ctx, component, "process")
	var output *Output
	if Enabled() {
		output = NewOutput(ctx, component, nil)
		if command.Stderr == nil {
			command.Stderr = output
		} else {
			// Preserve exec's shared-writer optimization: splitting a combined
			// capture into separate writers would race on its buffer.
			shared := command.Stdout != nil && command.Stdout == command.Stderr
			writer := io.MultiWriter(command.Stderr, output)
			command.Stderr = writer
			if shared {
				command.Stdout = writer
			}
		}
	}
	err := command.Run()
	if output != nil {
		_ = output.Flush()
	}
	traceErr := err
	if err != nil && ctx.Err() != nil {
		traceErr = errors.Join(err, ctx.Err())
	}
	finish(traceErr)
	return err
}

func CombinedOutput(ctx context.Context, component string, command *exec.Cmd) ([]byte, error) {
	command.Args = append(command.Args[:1:1], ProcessArguments(component, command.Args[1:])...)
	finish := Start(ctx, component, "process")
	output, err := command.CombinedOutput()
	if Enabled() {
		diagnostics := NewOutput(ctx, component, nil)
		_, _ = diagnostics.Write(output)
		_ = diagnostics.Flush()
	}
	traceErr := err
	if err != nil && ctx.Err() != nil {
		traceErr = errors.Join(err, ctx.Err())
	}
	finish(traceErr)
	return output, err
}
