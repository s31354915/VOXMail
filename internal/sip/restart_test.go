package sip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type restartLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (l *restartLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Buffer.Write(data)
}

func (l *restartLog) events(t *testing.T) (delays []time.Duration, abandoned bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range bytes.Split(l.Buffer.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Message string        `json:"msg"`
			Delay   time.Duration `json:"delay"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Message == "waiting to restart baresip" {
			delays = append(delays, event.Delay)
		}
		if event.Message == "baresip restart abandoned after repeated failures" {
			abandoned = true
		}
	}
	return delays, abandoned
}

type childEvent struct {
	Phase string
	At    time.Time
	PID   int
}

func childEvents(t *testing.T, dir string) []childEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "child-events"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []childEvent
	lines := bytes.Split(data, []byte("\n"))
	// A running child can be in the middle of appending its final line.
	for _, line := range lines[:len(lines)-1] {
		var event childEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func startCrashFixture(t *testing.T) (*Baresip, string, *restartLog) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-baresip")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec \"$VOXMAIL_RESTART_TEST_BINARY\" -test.run=^TestSIPRestartChild$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOXMAIL_RESTART_TEST_BINARY", executable)
	t.Setenv("VOXMAIL_RESTART_TEST_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	log := &restartLog{}
	b := &Baresip{
		Binary: binary, ConfigDir: dir, enabled: true, runCtx: ctx, runCancel: cancel,
		restart: restartPolicy{initial: 30 * time.Millisecond, max: 120 * time.Millisecond, stable: 200 * time.Millisecond},
		Log:     slog.New(slog.NewJSONHandler(log, nil)),
	}
	b.mu.Lock()
	err = b.startLocked(ctx)
	b.mu.Unlock()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	return b, dir, log
}

func TestBaresipBackoffSurvivesStartsAndResetsAfterStableUptime(t *testing.T) {
	b, dir, log := startCrashFixture(t)
	deadline := time.Now().Add(5 * time.Second)
	var events []childEvent
	stableReset := false
	for time.Now().Before(deadline) {
		events = childEvents(t, dir)
		if len(events) == 9 {
			b.mu.Lock()
			stableReset = stableReset || (b.crashes == 0 && b.cmd != nil)
			b.mu.Unlock()
		}
		if len(events) == 13 { // six exiting children, then one running child
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(events) != 13 {
		t.Fatalf("did not observe seven starts: %+v", events)
	}
	if !stableReset {
		t.Fatal("failure history did not reset while the stable child was running")
	}
	want := []time.Duration{30, 60, 120, 120, 30, 60}
	for i := range want {
		want[i] *= time.Millisecond
	}
	delays, abandoned := log.events(t)
	if abandoned || fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("restart delays=%v abandoned=%v, want %v", delays, abandoned, want)
	}
	for i, delay := range want {
		exit, next := events[i*2+1], events[i*2+2]
		if exit.Phase != "exit" || next.Phase != "start" || next.At.Sub(exit.At) < delay {
			t.Fatalf("restart %d interval=%s, want at least %s; events=%+v", i, next.At.Sub(exit.At), delay, events)
		}
		if next.PID == exit.PID {
			t.Fatal("fixture did not start a new process")
		}
	}
	if events[9].At.Sub(events[8].At) < b.restart.stable {
		t.Fatal("reset was not preceded by stable child uptime")
	}
	b.mu.Lock()
	crashes, running := b.crashes, b.cmd != nil
	b.mu.Unlock()
	if crashes != 2 || !running {
		t.Fatalf("failure history after stable reset: crashes=%d running=%v", crashes, running)
	}
	b.Stop()
	time.Sleep(150 * time.Millisecond)
	if got := len(childEvents(t, dir)); got != 13 {
		t.Fatalf("Stop revived a child: event count=%d", got)
	}
}

func TestBaresipFailedSpawnsUseAccumulatedBackoffAndStopAfterBudget(t *testing.T) {
	b, dir, log := startCrashFixture(t)
	deadline := time.Now().Add(2 * time.Second)
	for len(childEvents(t, dir)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := os.Remove(b.Binary); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	var abandoned bool
	var delays []time.Duration
	for time.Now().Before(deadline) {
		delays, abandoned = log.events(t)
		if abandoned {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := []time.Duration{30 * time.Millisecond, 60 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond, 120 * time.Millisecond}
	if !abandoned || fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("failed-spawn delays=%v abandoned=%v, want %v", delays, abandoned, want)
	}
	if events := childEvents(t, dir); len(events) != 2 {
		t.Fatalf("unexpected children after removing executable: %+v", events)
	}
	status := b.Status()
	if status.Process != ProcessDegraded || status.LastError == "" {
		t.Fatalf("abandoned restart status=%+v", status)
	}
}

func TestBaresipSupersededRestartCannotLaunchChild(t *testing.T) {
	log := &restartLog{}
	b := &Baresip{Binary: "/missing/baresip", enabled: true, crashes: 1,
		restart: restartPolicy{initial: 30 * time.Millisecond}, Log: slog.New(slog.NewJSONHandler(log, nil))}
	done := make(chan struct{})
	go func() {
		b.respawn(context.Background(), 0)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if delays, _ := log.events(t); len(delays) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	b.mu.Lock()
	b.generation++
	b.process = ProcessStarting
	b.lastError = "replacement configuration"
	b.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("superseded restart did not finish")
	}
	if status := b.Status(); status.Process != ProcessStarting || status.LastError != "replacement configuration" {
		t.Fatalf("old restart overwrote replacement state: %+v", status)
	}
}

func TestDefaultRestartPolicyAndBoundedFailureHistory(t *testing.T) {
	p := (restartPolicy{}).policy()
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	for i, delay := range want {
		if got := p.delay(uint8(i + 1)); got != delay*time.Second {
			t.Fatalf("failure %d: delay=%s", i+1, got)
		}
	}
	b := &Baresip{}
	for i := 0; i < 10000; i++ {
		b.recordCrashLocked()
	}
	if b.crashes != 64 || p.delay(b.crashes) != 30*time.Second || p.stable != time.Minute {
		t.Fatalf("unbounded or incorrect defaults: crashes=%d policy=%+v", b.crashes, p)
	}
}

func TestStableUptimeDoesNotCountExitedProcessWhileOutputDrains(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b := &Baresip{cmd: cmd, crashes: 4}
	// Keep waitDone open to model cmd.Wait draining a slow output sink after
	// the OS has reaped the process. The supervisor pointer is still current.
	b.resetAfterStableUptime(cmd, make(chan struct{}), context.Background(), 0, time.Millisecond)
	if b.crashes != 4 {
		t.Fatal("exited child cleared crash history before output drain finished")
	}
}

// Real child processes start successfully, record their PID/time, and exit
// immediately. Child 4 proves the stable-uptime reset; child 6 remains alive
// so the parent can test explicit Stop without another natural exit.
func TestSIPRestartChild(t *testing.T) {
	dir := os.Getenv("VOXMAIL_RESTART_TEST_DIR")
	if dir == "" {
		return
	}
	index := 0
	for _, event := range childEvents(t, dir) {
		if event.Phase == "start" {
			index++
		}
	}
	record := func(phase string) {
		file, err := os.OpenFile(filepath.Join(dir, "child-events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewEncoder(file).Encode(childEvent{Phase: phase, At: time.Now(), PID: os.Getpid()})
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("write child event: %v %v", err, closeErr)
		}
	}
	record("start")
	if index == 4 {
		time.Sleep(500 * time.Millisecond)
	}
	if index == 6 {
		for {
			time.Sleep(time.Second)
		}
	}
	record("exit")
	os.Exit(1)
}
