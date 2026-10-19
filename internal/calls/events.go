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

// A lost release event must not disable this key for the rest of the call.
// Two seconds was long enough to swallow a caller's fast repeat presses of
// the same digit (press 1 twice quickly to confirm, for example); the grace
// only needs to cover a stray duplicate callback, not a deliberate press.
const dtmfReleaseGrace = 250 * time.Millisecond

type activeTone struct {
	digit string
	seen  time.Time
}

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
	activeDTMF map[string]activeTone
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
		activeDTMF: make(map[string]activeTone),
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

	d.mu.Lock()
	if message.Type == "dtmf" {
		previous := d.activeDTMF[message.CallID]
		now := time.Now()
		if message.Phase != "" && previous.digit == message.Digit && now.Sub(previous.seen) < dtmfReleaseGrace {
			d.activeDTMF[message.CallID] = activeTone{message.Digit, now}
			d.trace(message, "duplicate ignored")
			d.mu.Unlock()
			return nil
		}
		d.activeDTMF[message.CallID] = activeTone{message.Digit, now}
		// Only accepted presses may interrupt audio. A duplicate callback must
		// not cancel the next menu while being discarded as input.
		d.service.interruptPrompt(message.CallID)
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
			// DTMF is level-triggered user input. The caller's newest press
			// matters more than a queued burst of older presses, so make
			// room by recycling the oldest job instead of silently dropping
			// the newest press. Control events are never discarded: the one
			// drained job is re-queued behind the burst.
			var drained callEventJob
			select {
			case drained = <-worker.queue:
			default:
			}
			if drained.message.Type != "" && drained.message.Type != "dtmf" {
				select {
				case worker.queue <- drained:
				default:
				}
			}
			select {
			case worker.queue <- callEventJob{message: message, navigationVersion: navigationVersion, hasNavigationStamp: hasNavigationStamp}:
				d.trace(message, "queued after making room")
				return nil
			default:
			}
			d.trace(message, "queue full")
			if d.service.Log != nil {
				d.service.Log.Warn("dropping excess DTMF events", "call_id", message.CallID)
			}
			return nil
		}
		// A full queue belongs to this one call, not to the shared bridge
		// connection. Hang up only this call and drop the event instead of
		// closing the connection under every other call.
		if d.service.Log != nil {
			d.service.Log.Warn("call event queue is full; hanging up the call", "call_id", message.CallID, "event_type", message.Type)
		}
		d.service.hangup(message.CallID)
		return nil
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
	// Bind the worker context once for the worker's lifetime. Rebinding on
	// every queued job re-acquired the service lock for each event and
	// churned the session's event context for no benefit: the worker context
	// never changes while this goroutine runs.
	d.service.bindEventContext(callID, worker.ctx, worker.cancel)
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
			if job.hasNavigationStamp && !d.service.navigationVersionIsCurrent(callID, job.navigationVersion) {
				d.trace(job.message, "stale navigation ignored")
				continue
			}
			if err := d.handle(worker.ctx, job.message); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				// One call's handler failure must not tear down the shared
				// bridge connection: every other active call rides on it.
				// Log the failure, hang up only this call, and retire only
				// this worker. If the hangup itself cannot be written, the
				// connection is broken for everyone and fail() reports it.
				if d.service.Log != nil {
					d.service.Log.Error("call event handler failed; hanging up the call", "call_id", callID, "event_type", job.message.Type, "error", err)
				}
				if hangupErr := d.service.hangup(callID); hangupErr != nil && !errors.Is(hangupErr, context.Canceled) {
					d.fail(hangupErr)
				}
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
