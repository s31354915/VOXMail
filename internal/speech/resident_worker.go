package speech

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/operation"
	"github.com/voxmail/voxmail/internal/process"
)

//go:embed piper_resident.py
var piperResidentScript string

const (
	// residentStartupTimeout bounds the wait for the resident Python worker
	// to load its model and answer ready. Loading takes seconds; the
	// generous ceiling covers cold page caches and CPU contention with
	// Whisper warm-up and Baresip on small machines.
	residentStartupTimeout = 2 * time.Minute
	// residentStopTimeout bounds the graceful stop inside close(). Callers
	// release speech leases on the call-teardown path, so close never
	// blocks long: the reaper goroutine reaps the child regardless.
	residentStopTimeout = 500 * time.Millisecond
)

// piperSynthesizer is the runtime's TTS engine behind one or more speech
// runtimes. A speed of 0 means the synthesizer's configuration already
// carries its speed (baked Extra flags); 1-5 applies that call speed to the
// request so one resident model can serve every speech rate.
type piperSynthesizer interface {
	synthesize(ctx context.Context, text, output string, speed int) error
	close() error
	// alive reports whether the engine currently holds a loaded model. A
	// one-shot CLI worker never reports resident.
	alive() bool
}

var (
	residentScriptOnce sync.Once
	residentScriptPath string
	residentScriptErr  error
)

// residentScriptFile materializes the embedded worker script once per
// process. The image runs one binary, so the script is written to the
// shared temporary directory and reused by every resident worker.
func residentScriptFile() (string, error) {
	residentScriptOnce.Do(func() {
		path := filepath.Join(os.TempDir(), "voxmail-piper-resident.py")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			residentScriptPath = path
			return
		}
		tmp, err := os.CreateTemp(os.TempDir(), ".voxmail-piper-resident-*.py")
		if err != nil {
			residentScriptErr = err
			return
		}
		name := tmp.Name()
		if _, err := tmp.WriteString(piperResidentScript); err != nil {
			tmp.Close()
			os.Remove(name)
			residentScriptErr = err
			return
		}
		if err := tmp.Close(); err != nil {
			os.Remove(name)
			residentScriptErr = err
			return
		}
		if err := os.Chmod(name, 0500); err != nil {
			os.Remove(name)
			residentScriptErr = err
			return
		}
		if err := os.Rename(name, path); err != nil {
			os.Remove(name)
			residentScriptErr = err
			return
		}
		residentScriptPath = path
	})
	return residentScriptPath, residentScriptErr
}

// residentWorker keeps one Piper voice model loaded in a long-lived Python
// worker process. Requests and replies travel as JSON lines over the
// process's stdin/stdout so every dynamic prompt reuses the loaded model
// instead of paying Piper's model-load cost again. A resident worker that
// cannot start, or that dies mid-flight, falls back to the one-shot CLI
// path so a broken Python environment can never take prompts down.
type residentWorker struct {
	piper      Piper // one-shot fallback configuration
	python     string
	model      string
	configPath string

	mu        sync.Mutex
	closed    bool
	proc      *residentProcess
	fallback  *piperWorker
	nextReqID int
}

// newResidentWorker prepares, but does not start, the resident engine.
// Starting is deferred to the first synthesis so call activation never
// blocks on the model load; readiness gating happens inside synthesize.
func newResidentWorker(python string, piper Piper) *residentWorker {
	if piper.Model == "" {
		return nil
	}
	worker := &residentWorker{piper: piper, model: piper.Model, configPath: piper.Model + ".json"}
	if strings.TrimSpace(python) == "" {
		python = "python3"
	}
	worker.python = python
	if _, err := exec.LookPath(python); err != nil {
		return nil
	}
	return worker
}

type residentRequest struct {
	ID          int      `json:"id"`
	Text        string   `json:"text"`
	LengthScale *float64 `json:"length_scale,omitempty"`
	Out         string   `json:"out"`
}

type residentReply struct {
	ID    int    `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type residentEvent struct {
	line string
	err  error
}

// residentProcess owns one running worker child: its pipes, its reply
// channel, and its exit signal.
type residentProcess struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	replies chan residentEvent
	stderr  *process.Buffer
	exited  chan struct{}
	ready   bool
}

func (w *residentWorker) synthesize(ctx context.Context, text, output string, speed int) (retErr error) {
	if w == nil {
		return fmt.Errorf("piper worker is nil")
	}
	if text == "" || output == "" {
		return fmt.Errorf("piper text and output are required")
	}
	stage, category := "settings", operation.Configuration
	defer func() { retErr = operation.Wrap("piper-resident", stage, category, retErr) }()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("piper worker is closed")
	}
	if err := safeOutput(output); err != nil {
		return err
	}
	normalized := NormalizeForSpeech(text)
	if normalized == "" {
		return fmt.Errorf("cannot synthesize empty text")
	}
	deadline := time.Now().Add(piperSynthesisTimeout)
	if ctx == nil {
		ctx = context.Background()
	}
	if wait, ok := ctx.Deadline(); ok && wait.Before(deadline) {
		deadline = wait
	}
	var lengthScale *float64
	if speed >= 1 && speed <= 5 && !hasLengthScaleExtra(w.piper.Extra) {
		value := speedLengthScale(speed)
		lengthScale = &value
	}
	// One transport failure (child crash, timeout, broken pipe) is retried
	// once on a fresh child; a reply-level synthesis error is deterministic
	// and surfaces immediately.
	for attempt := 0; attempt < 2; attempt++ {
		proc, err := w.ensureProcessLocked()
		if err != nil {
			break
		}
		out, err := w.requestLocked(ctx, proc, normalized, lengthScale, output, deadline)
		if err == nil {
			stage, category = "validate_output", operation.Protocol
			if err := validateWAV(out); err != nil {
				os.Remove(out)
				return fmt.Errorf("piper produced invalid WAV: %w", err)
			}
			stage, category = "publish_output", operation.Failed
			if err := os.Rename(out, output); err != nil {
				os.Remove(out)
				return fmt.Errorf("publish synthesized WAV: %w", err)
			}
			return nil
		}
		if !errors.Is(err, errResidentTransport) {
			return err
		}
		w.killProcessLocked(proc)
	}
	return w.fallbackLocked(ctx, text, output, speed)
}

// errResidentTransport marks failures that a fresh child may not hit again.
var errResidentTransport = errors.New("resident piper worker transport failure")

// speedLengthScale mirrors SpeedExtra: Piper's length_scale is inverse
// speech rate, speed 3 is Piper's own natural pace.
func speedLengthScale(speed int) float64 {
	return 1.375 - 0.125*float64(normalizeSpeed(speed))
}

func hasLengthScaleExtra(extra []string) bool {
	for i, value := range extra {
		if value == "--length_scale" && i+1 < len(extra) {
			return true
		}
	}
	return false
}

func (w *residentWorker) ensureProcessLocked() (*residentProcess, error) {
	if w.proc != nil {
		select {
		case <-w.proc.exited:
			w.proc = nil
		default:
			return w.proc, nil
		}
	}
	proc, err := w.spawnLocked()
	if err != nil {
		return nil, err
	}
	w.proc = proc
	return proc, nil
}

func (w *residentWorker) spawnLocked() (*residentProcess, error) {
	script, err := residentScriptFile()
	if err != nil {
		return nil, err
	}
	execCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(execCtx, w.python, "-u", script, "--model", w.model)
	if info, statErr := os.Stat(w.configPath); statErr == nil && !info.IsDir() {
		cmd.Args = append(cmd.Args, "--config", w.configPath)
	}
	process.Configure(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr := process.NewBuffer(process.MaxOutput)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	proc := &residentProcess{
		cmd:     cmd,
		cancel:  cancel,
		stdin:   stdin,
		replies: make(chan residentEvent, 8),
		stderr:  stderr,
		exited:  make(chan struct{}),
	}
	go func() {
		reader := bufio.NewReader(stdout)
		for {
			line, readErr := reader.ReadString('\n')
			if line != "" {
				proc.replies <- residentEvent{line: line}
			}
			if readErr != nil {
				proc.replies <- residentEvent{err: readErr}
				break
			}
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(proc.exited)
	}()
	return proc, nil
}

func (w *residentWorker) requestLocked(ctx context.Context, proc *residentProcess, text string, lengthScale *float64, output string, deadline time.Time) (string, error) {
	if !proc.ready {
		if err := w.awaitReadyLocked(proc, deadline); err != nil {
			return "", err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".piper-resident-*.wav")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	w.nextReqID++
	request := residentRequest{ID: w.nextReqID, Text: text, LengthScale: lengthScale, Out: tmpPath}
	payload, err := json.Marshal(request)
	if err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if _, err := proc.stdin.Write(append(payload, '\n')); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("%w: %v", errResidentTransport, err)
	}
	for {
		select {
		case event := <-proc.replies:
			if event.err != nil {
				os.Remove(tmpPath)
				return "", fmt.Errorf("%w: %v", errResidentTransport, stderrTail(proc))
			}
			var reply residentReply
			if err := json.Unmarshal([]byte(event.line), &reply); err != nil {
				os.Remove(tmpPath)
				return "", fmt.Errorf("%w: unreadable worker reply: %v", errResidentTransport, err)
			}
			if reply.ID != request.ID {
				continue
			}
			if !reply.OK {
				os.Remove(tmpPath)
				return "", fmt.Errorf("piper: %s", reply.Error)
			}
			return tmpPath, nil
		case <-ctx.Done():
			os.Remove(tmpPath)
			return "", fmt.Errorf("%w: %v", errResidentTransport, ctx.Err())
		case <-time.After(time.Until(deadline)):
			os.Remove(tmpPath)
			return "", fmt.Errorf("%w: synthesis exceeded %s", errResidentTransport, piperSynthesisTimeout)
		}
	}
}

func (w *residentWorker) awaitReadyLocked(proc *residentProcess, deadline time.Time) error {
	for {
		select {
		case event := <-proc.replies:
			if event.err != nil {
				return fmt.Errorf("%w: worker exited during startup: %v", errResidentTransport, stderrTail(proc))
			}
			var message map[string]any
			if err := json.Unmarshal([]byte(event.line), &message); err != nil {
				continue
			}
			if ready, _ := message["ready"].(bool); ready {
				proc.ready = true
				return nil
			}
			if failure, _ := message["error"].(string); failure != "" {
				return fmt.Errorf("%w: worker failed to load model: %s", errResidentTransport, failure)
			}
		case <-time.After(time.Until(deadline)):
			return fmt.Errorf("%w: worker did not become ready within %s", errResidentTransport, residentStartupTimeout)
		}
	}
}

func stderrTail(proc *residentProcess) string {
	if proc == nil || proc.stderr == nil {
		return ""
	}
	text := strings.TrimSpace(proc.stderr.String())
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return " (" + strings.Join(lines, "; ") + ")"
}

func (w *residentWorker) killProcessLocked(proc *residentProcess) {
	if w.proc == proc {
		w.proc = nil
	}
	if proc == nil {
		return
	}
	proc.cancel()
	select {
	case <-proc.exited:
	case <-time.After(2 * time.Second):
	}
}

func (w *residentWorker) fallbackLocked(ctx context.Context, text, output string, speed int) error {
	if w.fallback == nil {
		worker, err := startPiperWorker(ctx, w.piper)
		if err != nil {
			return err
		}
		w.fallback = worker
	}
	return w.fallback.synthesize(ctx, text, output, speed)
}

func (w *residentWorker) alive() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	proc := w.proc
	if proc == nil {
		return false
	}
	select {
	case <-proc.exited:
		return false
	default:
	}
	return proc.ready
}

func (w *residentWorker) close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closed = true
	proc := w.proc
	w.proc = nil
	fallback := w.fallback
	w.mu.Unlock()
	if proc != nil {
		proc.cancel()
		select {
		case <-proc.exited:
		case <-time.After(residentStopTimeout):
		}
	}
	if fallback != nil {
		fallback.close()
	}
	return nil
}
