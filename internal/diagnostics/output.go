package diagnostics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

const maxDiagnosticLine = 8192

// Output filters complete lines before copying child diagnostics to a log.
// It buffers at most one bounded line and is safe for shared stdout/stderr.
// Flush must be called after the child has stopped.
type Output struct {
	mu          sync.Mutex
	destination io.Writer
	ctx         context.Context
	component   string
	pending     []byte
	overflow    bool
}

func NewOutput(ctx context.Context, component string, destination io.Writer) *Output {
	return &Output{ctx: ctx, component: component, destination: destination}
}

func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if err := o.emit(); err != nil {
				return 0, err
			}
			continue
		}
		if len(o.pending) < maxDiagnosticLine {
			o.pending = append(o.pending, b)
		} else {
			o.overflow = true
		}
	}
	return len(p), nil
}

func (o *Output) Flush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pending) == 0 && !o.overflow {
		return nil
	}
	return o.emit()
}

func (o *Output) emit() error {
	line := string(bytes.TrimSpace(o.pending))
	o.pending = o.pending[:0]
	if o.overflow {
		line = "[oversized diagnostic line omitted]"
	}
	o.overflow = false
	line = safeDiagnosticLine(line)
	if o.destination != nil {
		_, err := io.WriteString(o.destination, line+"\n")
		return err
	}
	if Enabled() && line != "" {
		slog.DebugContext(o.ctx, "child diagnostic", "component", o.component, "detail", line)
	}
	return nil
}

func safeDiagnosticLine(line string) string {
	lower := strings.ToLower(line)
	for _, marker := range []string{"password", "passwd", "auth_pass", "authorization", "bearer ", "token", "secret", "pin="} {
		if strings.Contains(lower, marker) {
			return "[credential-bearing diagnostic omitted]"
		}
	}
	// Upstream Baresip's INFO output includes individual key values even in
	// normal mode. Keep the event observable without recording a caller PIN.
	if strings.Contains(lower, "dtmf") && !strings.Contains(lower, "value redacted") {
		return "[DTMF event: value redacted]"
	}
	return line
}
