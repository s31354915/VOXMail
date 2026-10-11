package calls

import (
	"context"
	"errors"
	"github.com/voxmail/voxmail/internal/diagnostics"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
)

const callEventQueueSize = 32

type callEventJob struct {
	message            bridge.Message
	navigationVersion  uint64
	hasNavigationStamp bool
}

type callEventWorker struct {
	queue  chan callEventJob
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// callEventDispatcher keeps the bridge reader deliberately small. Each call
// has one ordered worker, while independent calls can process events in
// parallel. The queue is bounded so a noisy endpoint cannot create unlimited
// goroutines or memory.
type callEventDispatcher struct {
	service *Service
	conn    net.Conn
	ctx     context.Context

	mu         sync.Mutex
	workers    map[string]*callEventWorker
	activeDTMF map[string]string
	errs       chan error
	// handleFn is nil in production. Tests use it to deterministically hold
	// one event and prove queue ordering, isolation, and cancellation without
	// needing a live SIP call.
	handleFn func(context.Context, bridge.Message) error
}

func newCallEventDispatcher(ctx context.Context, service *Service, conn net.Conn) *callEventDispatcher {
	return &callEventDispatcher{
		service:    service,
		conn:       conn,
		ctx:        ctx,
		workers:    make(map[string]*callEventWorker),
		activeDTMF: make(map[string]string),
		errs:       make(chan error, 1),
	}
}

func (d *callEventDispatcher) dispatch(message bridge.Message) error {
	if d == nil || d.service == nil {
		return errors.New("call event dispatcher is unavailable")
	}
	if message.CallID == "" {
		return nil
	}
	var navigationVersion uint64
	var hasNavigationStamp bool
	if message.Type == "dtmf" {
		// The only meaningful input values are one keypad key on press. Treat
		// end markers, malformed values, and stale bridge payloads as transport
		// noise so they cannot trigger an IVR error prompt in the next state.
		if message.Phase == "end" {
			d.trace(message, "release ignored")
			d.mu.Lock()
			delete(d.activeDTMF, message.CallID)
			d.mu.Unlock()
			return nil
		}
		if (message.Phase != "" && message.Phase != "start") || !validDTMFKey(message.Digit) {
			d.trace(message, "invalid input ignored")
			return nil
		}
		navigationVersion, hasNavigationStamp = d.service.navigationVersion(message.CallID)
	}
	if message.Type == "call_closed" {
		// Closing a call is a control-plane event, not ordinary IVR work. Apply
		// cancellation immediately so an in-flight prompt stops even if the
		// call's DTMF queue is full.
		d.service.closeCall(message)
		d.stopWorker(message.CallID)
		return nil
	}
	if message.Type == "dtmf" {
		// Prompt playback runs on the ordered call worker. Interrupt it as soon
		// as the bridge receives a completed key so the worker can process that
		// key instead of making callers wait for the whole menu to finish.
		d.service.interruptPrompt(message.CallID)
	}

	d.mu.Lock()
	if message.Type == "dtmf" {
		if d.activeDTMF[message.CallID] == message.Digit {
			d.trace(message, "duplicate ignored")
			d.mu.Unlock()
			return nil
		}
		d.activeDTMF[message.CallID] = message.Digit
	}
	worker := d.workers[message.CallID]
	if worker == nil {
		worker = d.newWorkerLocked(message.CallID)
	}
	select {
	case worker.queue <- callEventJob{message: message, navigationVersion: navigationVersion, hasNavigationStamp: hasNavigationStamp}:
		d.mu.Unlock()
		d.trace(message, "queued")
		return nil
	default:
		d.mu.Unlock()
		if message.Type == "dtmf" {
			d.trace(message, "queue full")
			// DTMF is level-triggered user input; dropping excess digits is
			// safer than allowing an unbounded backlog to outlive the call.
			if d.service.Log != nil {
				d.service.Log.Warn("dropping excess DTMF events", "call_id", message.CallID)
			}
			return nil
		}
		err := errors.New("call event queue is full")
		d.fail(err)
		return err
	}
}

func (d *callEventDispatcher) newWorkerLocked(callID string) *callEventWorker {
	workerCtx, cancel := context.WithCancel(d.ctx)
	worker := &callEventWorker{
		queue:  make(chan callEventJob, callEventQueueSize),
		ctx:    workerCtx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	d.workers[callID] = worker
	go d.runWorker(callID, worker)
	return worker
}

func (d *callEventDispatcher) runWorker(callID string, worker *callEventWorker) {
	defer close(worker.done)
	for {
		select {
		case <-worker.ctx.Done():
			return
		default:
		}
		select {
		case <-worker.ctx.Done():
			return
		case job, ok := <-worker.queue:
			if !ok {
				return
			}
			d.service.bindEventContext(callID, worker.ctx, worker.cancel)
			if job.hasNavigationStamp && !d.service.navigationVersionIsCurrent(callID, job.navigationVersion) {
				d.trace(job.message, "stale navigation ignored")
				continue
			}
			if err := d.handle(worker.ctx, job.message); err != nil {
				d.fail(err)
				return
			}
		}
	}
}

func (d *callEventDispatcher) handle(ctx context.Context, message bridge.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if d.handleFn != nil {
		return d.handleFn(ctx, message)
	}
	switch message.Type {
	case "call_incoming":
		err := d.service.admit(d.conn, message)
		if err == nil {
			d.service.bindEventContext(message.CallID, ctx, nil)
		}
		return err
	case "call_outgoing":
		err := d.service.startOutgoing(d.conn, message)
		if err == nil {
			d.service.bindEventContext(message.CallID, ctx, nil)
		}
		return err
	case "call_established":
		d.service.reportCallAlertOutcome(message.CallID, AlertAnswered)
		d.service.onEstablished(message)
	case "call_ringing":
		d.service.reportCallAlertOutcome(message.CallID, AlertRinging)
	case "dtmf":
		// The native shim emits only the key callback and ignores the later
		// KEYCODE_REL callback. Process that start event once; accepting the
		// release marker as well would replay a bogus key after every press.
		if message.Phase != "" && message.Phase != "start" || !validDTMFKey(message.Digit) {
			return nil
		}
		return d.service.handleDTMF(d.conn, message)
	}
	return nil
}

func validDTMFKey(key string) bool {
	if len(key) != 1 {
		return false
	}
	return key[0] >= '0' && key[0] <= '9' || key == "*" || key == "#"
}

func (d *callEventDispatcher) stopWorker(callID string) {
	d.mu.Lock()
	worker := d.workers[callID]
	delete(d.workers, callID)
	delete(d.activeDTMF, callID)
	d.mu.Unlock()
	if worker == nil {
		return
	}
	worker.cancel()
	close(worker.queue)
}

func (d *callEventDispatcher) fail(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	select {
	case d.errs <- err:
	default:
	}
	if d.conn != nil {
		_ = d.conn.Close()
	}
}

func (d *callEventDispatcher) close() {
	d.mu.Lock()
	workers := make([]*callEventWorker, 0, len(d.workers))
	for callID, worker := range d.workers {
		delete(d.workers, callID)
		workers = append(workers, worker)
	}
	d.mu.Unlock()
	for _, worker := range workers {
		worker.cancel()
		close(worker.queue)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, worker := range workers {
		select {
		case <-worker.done:
		case <-deadline.C:
			return
		}
	}
}

func (d *callEventDispatcher) trace(message bridge.Message, outcome string) {
	if diagnostics.Enabled() && message.Type == "dtmf" {
		slog.Debug("DTMF dispatch", "call_id", message.CallID, "outcome", outcome)
	}
}
