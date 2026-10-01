package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coverletter/internal/logging"
	"coverletter/internal/obs"
)

func newTestPool(concurrency, queueSize int) *Pool {
	return New(concurrency, queueSize, logging.Discard(), obs.New())
}

func TestPoolRunsTasks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newTestPool(2, 4)
	pool.Start(ctx)

	var ran atomic.Int32
	for i := 0; i < 4; i++ {
		if err := pool.Submit(ctx, Task{Name: "task", Run: func(context.Context) error {
			ran.Add(1)
			return nil
		}}); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}

	waitFor(t, func() bool { return ran.Load() == 4 })

	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestTrySubmitReportsBackpressure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	pool := newTestPool(1, 1)
	pool.Start(ctx)

	// Occupy the single worker.
	if err := pool.TrySubmit(ctx, Task{Name: "blocking", Run: func(context.Context) error {
		<-release
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit(blocking) error = %v", err)
	}
	// Fill the queue. Capacity is concurrency + queueSize = 2.
	if err := pool.TrySubmit(ctx, Task{Name: "queued", Run: func(context.Context) error {
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit(queued) error = %v", err)
	}

	// A third task has nowhere to go, and the caller must be told rather than
	// having the work buffered indefinitely.
	err := pool.TrySubmit(ctx, Task{Name: "overflow", Run: func(context.Context) error { return nil }})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("TrySubmit(overflow) error = %v, want ErrQueueFull", err)
	}

	close(release)
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSubmitBlocksUntilCapacity(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	pool := newTestPool(1, 1)
	pool.Start(ctx)

	if err := pool.TrySubmit(ctx, Task{Name: "blocking", Run: func(context.Context) error {
		<-release
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit() error = %v", err)
	}
	// Fill the one remaining queue slot.
	if err := pool.TrySubmit(ctx, Task{Name: "queued", Run: func(context.Context) error {
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit(queued) error = %v", err)
	}

	// This one has nowhere to go yet and must wait rather than fail.
	submitted := make(chan error, 1)
	go func() {
		submitted <- pool.Submit(ctx, Task{Name: "waiting", Run: func(context.Context) error { return nil }})
	}()

	select {
	case err := <-submitted:
		t.Fatalf("Submit() returned %v while the pool was saturated; it should wait", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-submitted:
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit() did not return after capacity became available")
	}

	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSubmitRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1, 1)
	pool.Start(context.Background())
	defer func() { _ = pool.Shutdown(context.Background()) }()

	release := make(chan struct{})
	defer close(release)
	if err := pool.TrySubmit(context.Background(), Task{Name: "blocking", Run: func(context.Context) error {
		<-release
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit() error = %v", err)
	}
	// Occupy the only queue slot so the pool is genuinely saturated.
	if err := pool.TrySubmit(context.Background(), Task{Name: "queued", Run: func(context.Context) error {
		return nil
	}}); err != nil {
		t.Fatalf("TrySubmit(queued) error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := pool.Submit(ctx, Task{Name: "cancelled", Run: func(context.Context) error { return nil }})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestPanicInTaskDoesNotKillWorker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newTestPool(1, 2)
	pool.Start(ctx)

	if err := pool.Submit(ctx, Task{Name: "panics", Run: func(context.Context) error {
		panic("boom")
	}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The same worker must still be able to run the next task; otherwise the
	// pool would silently lose capacity.
	var ran atomic.Bool
	if err := pool.Submit(ctx, Task{Name: "after-panic", Run: func(context.Context) error {
		ran.Store(true)
		return nil
	}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitFor(t, ran.Load)

	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestTaskFailureIsIsolated(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newTestPool(1, 4)
	pool.Start(ctx)

	sentinel := errors.New("task failed")
	var second atomic.Bool

	if err := pool.Submit(ctx, Task{Name: "fails", Run: func(context.Context) error { return sentinel }}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if err := pool.Submit(ctx, Task{Name: "succeeds", Run: func(context.Context) error {
		second.Store(true)
		return nil
	}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitFor(t, second.Load)

	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSubmitAfterShutdownIsRejected(t *testing.T) {
	t.Parallel()

	pool := newTestPool(1, 1)
	pool.Start(context.Background())

	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	err := pool.Submit(context.Background(), Task{Name: "late", Run: func(context.Context) error { return nil }})
	if !errors.Is(err, ErrPoolStopped) {
		t.Fatalf("Submit() after shutdown error = %v, want ErrPoolStopped", err)
	}
	// Shutdown is idempotent.
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
}

func TestConcurrentSubmissionsRespectBound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const concurrency = 2
	pool := newTestPool(concurrency, 8)
	pool.Start(ctx)

	var mu sync.Mutex
	var inFlight, maxInFlight int
	var completed atomic.Int32

	for i := 0; i < 20; i++ {
		err := pool.Submit(ctx, Task{Name: "concurrent", Run: func(context.Context) error {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			completed.Add(1)
			return nil
		}})
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
	}

	waitFor(t, func() bool { return completed.Load() == 20 })

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > concurrency {
		t.Errorf("observed %d concurrent tasks, want at most %d", maxInFlight, concurrency)
	}
}

// waitFor polls a condition until it holds or the test times out.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was not met within the timeout")
}
