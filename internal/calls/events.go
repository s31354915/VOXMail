package calls

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/bridge"
)

const callEventQueueSize = 32

type callEventJob struct {
	message bridge.Message
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

	mu      sync.Mutex
	workers map[string]*callEventWorker
	errs    chan error
	// handleFn is nil in production. Tests use it to deterministically hold
	// one event and prove queue ordering, isolation, and cancellation without
	// needing a live SIP call.
	handleFn func(context.Context, bridge.Message) error
}

func newCallEventDispatcher(ctx context.Context, service *Service, conn net.Conn) *callEventDispatcher {
	return &callEventDispatcher{
		service: service,
		conn:    conn,
		ctx:     ctx,
		workers: make(map[string]*callEventWorker),
		errs:    make(chan error, 1),
	}
}

func (d *callEventDispatcher) dispatch(message bridge.Message) error {
	if d == nil || d.service == nil {
		return errors.New("call event dispatcher is unavailable")
	}
	if message.CallID == "" {
		return nil
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
	worker := d.workers[message.CallID]
	if worker == nil {
		worker = d.newWorkerLocked(message.CallID)
	}
	select {
	case worker.queue <- callEventJob{message: message}:
		d.mu.Unlock()
		return nil
	default:
		d.mu.Unlock()
		if message.Type == "dtmf" {
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
		// Baresip's call_dtmf_h callback reports one completed key, not a
		// start/end pair. The native shim labels that single event phase=end;
		// reject any start-level event so one key cannot be processed twice.
		if message.Phase != "" && message.Phase != "end" {
			return nil
		}
		return d.service.handleDTMF(d.conn, message)
	}
	return nil
}

func (d *callEventDispatcher) stopWorker(callID string) {
	d.mu.Lock()
	worker := d.workers[callID]
	delete(d.workers, callID)
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
