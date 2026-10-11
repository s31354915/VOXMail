package lifecycle

import (
	"context"
	"sync"
)

// WorkerKind states whether a worker can contact providers and whether it must
// survive the initial cancellation long enough to drain active calls.
type WorkerKind uint8

const (
	LocalWorker WorkerKind = iota
	ProviderWorker
	CallBridgeWorker
)

// Application owns worker admission and two shutdown scopes. Maintenance,
// synchronization, alerts and HTTP request contexts stop when the signal
// context is canceled. The call bridge and SIP supervisor remain alive until
// StopRuntime, after call drain and HTTP/job completion. Every admitted worker
// is joined before its shared database or filesystem dependencies are closed.
type Application struct {
	offline     bool
	runtime     context.Context
	workers     context.Context
	stopRuntime context.CancelFunc
	stopWorkers context.CancelFunc
	mu          sync.Mutex
	closing     bool
	wg          sync.WaitGroup
}

func NewApplication(stop context.Context, offline bool) *Application {
	if stop == nil {
		stop = context.Background()
	}
	runtime, stopRuntime := context.WithCancel(context.WithoutCancel(stop))
	workers, stopWorkers := context.WithCancel(stop)
	return &Application{offline: offline, runtime: runtime, workers: workers, stopRuntime: stopRuntime, stopWorkers: stopWorkers}
}

func (a *Application) RuntimeContext() context.Context { return a.runtime }
func (a *Application) WorkerContext() context.Context  { return a.workers }

// Start admits and owns a worker. Offline applications admit only local work.
// Admission and WaitGroup registration share the shutdown lock, so a concurrent
// start cannot escape the join or add work after shutdown has begun.
func (a *Application) Start(kind WorkerKind, run func(context.Context)) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if run == nil || kind > CallBridgeWorker || a.closing || a.workers.Err() != nil || (a.offline && kind != LocalWorker) {
		return false
	}
	ctx := a.workers
	if kind == CallBridgeWorker {
		ctx = a.runtime
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		run(ctx)
	}()
	return true
}

func (a *Application) BeginShutdown() {
	a.mu.Lock()
	a.closing = true
	a.stopWorkers()
	a.mu.Unlock()
}

// StopRuntime ends the remaining call/SIP scope once callers have drained it.
func (a *Application) StopRuntime() {
	a.BeginShutdown()
	a.stopRuntime()
}

// Wait closes admission and joins every worker. Call StopRuntime first when a
// bridge worker is active; Wait never closes dependencies under a running job.
func (a *Application) Wait() {
	a.BeginShutdown()
	a.wg.Wait()
}
