// Package worker provides a bounded background task pool.
//
// Cover letter generation is asynchronous: the HTTP handler must return as soon
// as the request is durably recorded. A naive "go func() per request" would let
// unbounded goroutines pile up against a slow model provider, so tasks go
// through a fixed queue drained by a fixed number of workers. When the queue is
// full the caller is told to retry rather than having work buffered
// indefinitely.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"coverletter/internal/apperror"
	"coverletter/internal/obs"
)

// ErrQueueFull is returned by TrySubmit when the queue has no free slot. It is
// the apperror sentinel so callers can branch on it without importing HTTP
// vocabulary.
var ErrQueueFull = apperror.ErrQueueFull

// ErrPoolStopped is returned by Submit after Shutdown has begun.
var ErrPoolStopped = errors.New("worker: pool is shutting down")

// Task is a unit of background work. The context passed to it is derived from
// the pool lifetime and cancelled on shutdown.
type Task struct {
	Name string
	Run  func(ctx context.Context) error
}

// Pool is a bounded worker pool.
//
// Admission and scheduling are separate concerns, so they use separate
// primitives. `slots` is the admission gate: a token is taken when a task is
// accepted and returned only when it has finished running, which caps the total
// outstanding work at concurrency + queueSize. `tasks` is just the hand-off
// channel to the workers. Using the channel's own capacity as the bound would be
// wrong, because workers drain it eagerly and the queue depth would never be
// observable.
type Pool struct {
	logger      *slog.Logger
	metrics     *obs.Registry
	queueSize   int
	concurrency int

	// slots bounds outstanding work; one token per accepted task.
	slots chan struct{}
	// tasks hands work to the workers.
	tasks chan Task

	// inFlight counts tasks currently executing.
	inFlight atomic.Int64

	// wake is closed on shutdown to release every worker blocked on receive.
	wake chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
}

// New creates a pool with the given concurrency and queue depth. Start must be
// called before submitting work.
func New(concurrency, queueSize int, logger *slog.Logger, metrics *obs.Registry) *Pool {
	if concurrency <= 0 {
		concurrency = 1
	}
	if queueSize <= 0 {
		queueSize = 1
	}
	return &Pool{
		logger:      logger,
		metrics:     metrics,
		queueSize:   queueSize,
		concurrency: concurrency,
		slots:       make(chan struct{}, concurrency+queueSize),
		tasks:       make(chan Task, concurrency+queueSize),
		wake:        make(chan struct{}),
	}
}

// Start launches the workers. It is safe to call once; subsequent calls are
// no-ops.
func (p *Pool) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	poolCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	for i := 0; i < p.concurrency; i++ {
		p.wg.Add(1)
		go p.run(poolCtx)
	}
	p.logger.Info("worker pool started",
		slog.Int("workers", p.concurrency),
		slog.Int("queue_size", p.queueSize),
	)
}

func (p *Pool) run(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			return
		case task := <-p.tasks:
			p.observeQueueDepth()
			p.inFlight.Add(1)

			p.execute(ctx, task)

			p.inFlight.Add(-1)
			// Release the admission slot only once the task is fully done.
			<-p.slots
			p.observeQueueDepth()
		}
	}
}

func (p *Pool) execute(ctx context.Context, task Task) {
	defer p.recoverPanic(task)

	start := time.Now()
	p.metrics.Inc("worker.tasks.total")
	err := task.Run(ctx)
	p.metrics.Observe("worker.task.duration", time.Since(start))

	attrs := []any{
		slog.String("task", task.Name),
		slog.Duration("duration", time.Since(start)),
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			p.metrics.Inc("worker.tasks.cancelled")
			p.logger.Warn("worker task cancelled", append(attrs, slog.String("error", err.Error()))...)
			return
		}
		p.metrics.Inc("worker.tasks.failed")
		p.logger.Error("worker task failed", append(attrs, slog.String("error", err.Error()))...)
		return
	}
	p.logger.Info("worker task completed", attrs...)
}

func (p *Pool) recoverPanic(task Task) {
	if r := recover(); r != nil {
		p.metrics.Inc("worker.tasks.panicked")
		// A panic in one task must not take down the worker goroutine, or the
		// pool would silently lose capacity.
		p.logger.Error("worker task panicked",
			slog.String("task", task.Name),
			slog.String("panic", fmt.Sprint(r)),
		)
	}
}

// Submit enqueues a task, waiting for capacity if the pool is busy. It returns
// when the task is queued, when ctx is cancelled, or once shutdown has begun.
// Callers that must fail fast rather than wait should use TrySubmit.
func (p *Pool) Submit(ctx context.Context, task Task) error {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return ErrPoolStopped
	}

	if err := p.acquire(ctx, true); err != nil {
		return err
	}
	if err := p.handOff(ctx, task); err != nil {
		<-p.slots
		return err
	}
	p.recordEnqueued()
	return nil
}

// TrySubmit enqueues a task only if a slot is immediately available, so a
// saturated pool surfaces as backpressure to the caller rather than a request
// that hangs.
func (p *Pool) TrySubmit(ctx context.Context, task Task) error {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return ErrPoolStopped
	}

	if err := p.acquire(ctx, false); err != nil {
		return err
	}
	if err := p.handOff(ctx, task); err != nil {
		<-p.slots
		return err
	}
	p.recordEnqueued()
	return nil
}

// acquire takes an admission token, optionally waiting for one.
func (p *Pool) acquire(ctx context.Context, wait bool) error {
	if !wait {
		select {
		case p.slots <- struct{}{}:
			return nil
		default:
			return ErrQueueFull
		}
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.wake:
		return ErrPoolStopped
	}
}

// handOff publishes a task to the workers. The task channel is sized to the
// admission gate, so this send cannot block once a token is held.
func (p *Pool) handOff(ctx context.Context, task Task) error {
	select {
	case p.tasks <- task:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.wake:
		return ErrPoolStopped
	}
}

func (p *Pool) recordEnqueued() {
	p.metrics.Inc("worker.queue.enqueued")
	p.observeQueueDepth()
}

func (p *Pool) observeQueueDepth() {
	p.metrics.Set("worker.queue.depth", int64(p.Depth()))
	p.metrics.Set("worker.tasks.in_flight", int64(p.InFlight()))
	p.metrics.Set("worker.tasks.outstanding", int64(len(p.slots)))
}

// Depth returns the number of tasks accepted but not yet picked up by a worker.
func (p *Pool) Depth() int { return len(p.slots) - p.InFlight() }

// InFlight returns the number of tasks currently executing.
func (p *Pool) InFlight() int { return int(p.inFlight.Load()) }

// Shutdown stops accepting work, wakes the workers and waits for in-flight tasks
// to finish or for ctx to expire.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	close(p.wake)
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("worker pool stopped")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker: shutdown timed out: %w", ctx.Err())
	}
}
