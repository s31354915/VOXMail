package lifecycle

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApplicationCancelsRequestsAndWorkersBeforeCallDrain(t *testing.T) {
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := NewApplication(stop, false)
	defer app.StopRuntime()
	workerStopped := make(chan struct{})
	bridgeStarted := make(chan struct{})
	bridgeStopped := make(chan struct{})
	app.Start(ProviderWorker, func(ctx context.Context) { <-ctx.Done(); close(workerStopped) })
	app.Start(CallBridgeWorker, func(ctx context.Context) { close(bridgeStarted); <-ctx.Done(); close(bridgeStopped) })
	<-bridgeStarted
	requestStarted := make(chan struct{})
	requestStopped := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		BaseContext: func(net.Listener) context.Context { return app.WorkerContext() },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(requestStarted)
			<-r.Context().Done()
			close(requestStopped)
			w.WriteHeader(http.StatusServiceUnavailable)
		}),
	}
	defer server.Close()
	app.Start(LocalWorker, func(context.Context) { _ = server.Serve(listener) })
	clientDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
		if response != nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel() // Signal cancellation reaches manual operations before main joins.
	app.BeginShutdown()
	for name, done := range map[string]<-chan struct{}{"provider worker": workerStopped, "HTTP request": requestStopped} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal(name + " retained canceled work")
		}
	}
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if app.RuntimeContext().Err() != nil {
		t.Fatal("signal stopped call bridge before call drain")
	}
	select {
	case <-bridgeStopped:
		t.Fatal("bridge stopped before drain")
	default:
	}
	if app.Start(LocalWorker, func(context.Context) { t.Error("late worker ran") }) {
		t.Fatal("shutdown admitted new worker")
	}
	deadline, release := context.WithTimeout(context.Background(), time.Second)
	defer release()
	if err := server.Shutdown(deadline); err != nil {
		t.Fatal(err)
	}
	app.StopRuntime()
	app.Wait()
	select {
	case <-bridgeStopped:
	default:
		t.Fatal("bridge was not joined")
	}
}

func TestOfflineApplicationRejectsProviderAndCallWorkers(t *testing.T) {
	app := NewApplication(context.Background(), true)
	defer app.StopRuntime()
	var remote atomic.Int32
	for _, kind := range []WorkerKind{ProviderWorker, CallBridgeWorker} {
		if app.Start(kind, func(context.Context) { remote.Add(1) }) {
			t.Fatal("offline admitted provider work", kind)
		}
	}
	local := make(chan struct{})
	if !app.Start(LocalWorker, func(ctx context.Context) { close(local); <-ctx.Done() }) {
		t.Fatal("offline rejected maintenance")
	}
	<-local
	app.StopRuntime()
	app.Wait()
	if remote.Load() != 0 {
		t.Fatal("offline worker contacted provider")
	}
}

func TestApplicationConcurrentAdmissionCannotEscapeJoin(t *testing.T) {
	for cycle := 0; cycle < 100; cycle++ {
		app := NewApplication(context.Background(), false)
		var admitted, finished atomic.Int32
		var callers sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 20; i++ {
			callers.Add(1)
			go func() {
				defer callers.Done()
				<-start
				if app.Start(ProviderWorker, func(ctx context.Context) { <-ctx.Done(); finished.Add(1) }) {
					admitted.Add(1)
				}
			}()
		}
		close(start)
		app.StopRuntime()
		app.Wait()
		callers.Wait()
		if finished.Load() != admitted.Load() {
			t.Fatalf("cycle %d: joined %d of %d workers", cycle, finished.Load(), admitted.Load())
		}
		if app.Start(LocalWorker, func(context.Context) {}) {
			t.Fatal("joined application accepted work")
		}
	}
}

func TestApplicationJoinWaitsForWorkerCleanup(t *testing.T) {
	app := NewApplication(context.Background(), false)
	cleanupStarted, release := make(chan struct{}), make(chan struct{})
	app.Start(LocalWorker, func(ctx context.Context) { <-ctx.Done(); close(cleanupStarted); <-release })
	app.StopRuntime()
	<-cleanupStarted
	joined := make(chan struct{})
	go func() { app.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("join finished while worker retained dependency")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("cleanup was not joined")
	}
}
