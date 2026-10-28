// Package workerpool provides a bounded, reusable worker pool.
package workerpool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("workerpool: pool is closed")

// Task is a single inference request. Delay is used by the demo backend.
type Task struct {
	ID     uint64
	Prompt string
	Delay  time.Duration
}

// Result describes the completion of one accepted task.
type Result struct {
	TaskID      uint64        `json:"task_id"`
	WorkerID    int           `json:"worker_id"`
	Output      string        `json:"output,omitempty"`
	QueueWait   time.Duration `json:"-"`
	ExecuteTime time.Duration `json:"-"`
	Err         error         `json:"-"`
}

// Handler must respect ctx cancellation for timely shutdown.
type Handler func(ctx context.Context, task Task) (string, error)

type request struct {
	ctx        context.Context
	task       Task
	enqueuedAt time.Time
	reply      chan Result // one result; buffered to avoid blocking a worker
}

// Stats is an instantaneous snapshot. Counts may change during a running task.
type Stats struct {
	Workers          int   `json:"workers"`
	QueueCapacity    int   `json:"queue_capacity"`
	Queued           int   `json:"queued"`
	Active           int64 `json:"active"`
	Submitted        int64 `json:"submitted"`
	Completed        int64 `json:"completed"`
	Failed           int64 `json:"failed"`
	Canceled         int64 `json:"canceled"`
	Rejected         int64 `json:"rejected"`
	MaxQueued        int64 `json:"max_queued"`
	TotalQueueWaitMS int64 `json:"total_queue_wait_ms"`
}

// Pool bounds concurrent work and pending queue size.
// Call Close to gracefully finish accepted tasks; handlers must cooperate with contexts.
type Pool struct {
	jobs        chan request
	workers     int
	taskTimeout time.Duration
	handler     Handler

	mu         sync.Mutex // guards closed and registration into submitting
	closed     bool
	submitting sync.WaitGroup
	workersWG  sync.WaitGroup
	closeOnce  sync.Once
	stopped    chan struct{}

	completed atomic.Int64
	failed    atomic.Int64
	canceled  atomic.Int64
	rejected  atomic.Int64
	submitted atomic.Int64
	active    atomic.Int64
	maxQueued atomic.Int64
	queueWait atomic.Int64 // nanoseconds
}

// New starts workerCount workers; queueSize may be zero (unbuffered queue).
// taskTimeout == 0 means no pool-level per-task deadline.
func New(workerCount, queueSize int, taskTimeout time.Duration, handler Handler) (*Pool, error) {
	if workerCount <= 0 || queueSize < 0 || taskTimeout < 0 || handler == nil {
		return nil, fmt.Errorf("workerpool: invalid configuration")
	}
	p := &Pool{
		jobs:        make(chan request, queueSize),
		workers:     workerCount,
		taskTimeout: taskTimeout,
		handler:     handler,
		stopped:     make(chan struct{}),
	}
	p.workersWG.Add(workerCount)
	for i := 1; i <= workerCount; i++ {
		go p.runWorker(i)
	}
	return p, nil
}

func (p *Pool) runWorker(id int) {
	defer p.workersWG.Done()
	for req := range p.jobs {
		startedAt := time.Now()
		p.active.Add(1)
		queueWait := startedAt.Sub(req.enqueuedAt)
		p.queueWait.Add(int64(queueWait))

		res := Result{TaskID: req.task.ID, WorkerID: id}
		res.QueueWait = queueWait
		if err := req.ctx.Err(); err != nil {
			res.Err = err // task canceled while queued
		} else {
			ctx := req.ctx
			cancel := func() {}
			if p.taskTimeout > 0 {
				ctx, cancel = context.WithTimeout(req.ctx, p.taskTimeout)
			}
			res.Output, res.Err = p.handler(ctx, req.task)
			cancel()
		}
		res.ExecuteTime = time.Since(startedAt)
		p.completed.Add(1)
		if res.Err != nil {
			if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
				p.canceled.Add(1)
			} else {
				p.failed.Add(1)
			}
		}
		req.reply <- res
		close(req.reply)
		p.active.Add(-1)
	}
}

// Submit enqueues one task, waiting for queue space or ctx cancellation.
// An accepted task always sends exactly one Result and then closes its reply channel.
// Context cancellation while queued makes the worker skip its handler.
func (p *Pool) Submit(ctx context.Context, task Task) (<-chan Result, error) {
	if ctx == nil {
		p.rejected.Add(1)
		return nil, errors.New("workerpool: nil context")
	}
	if err := ctx.Err(); err != nil {
		p.rejected.Add(1)
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.rejected.Add(1)
		return nil, ErrClosed
	}
	// Register before Close may call Wait. Both are serialized by mu.
	p.submitting.Add(1)
	p.mu.Unlock()
	defer p.submitting.Done()

	reply := make(chan Result, 1)
	enqueuedAt := time.Now()
	select {
	case p.jobs <- request{ctx: ctx, task: task, enqueuedAt: enqueuedAt, reply: reply}:
		p.submitted.Add(1)
		queued := int64(len(p.jobs))
		for {
			max := p.maxQueued.Load()
			if queued <= max || p.maxQueued.CompareAndSwap(max, queued) {
				break
			}
		}
		return reply, nil
	case <-ctx.Done():
		p.rejected.Add(1)
		return nil, ctx.Err()
	}
}

// Snapshot returns thread-safe monitoring counters.
func (p *Pool) Snapshot() Stats {
	return Stats{
		Workers:          p.workers,
		QueueCapacity:    cap(p.jobs),
		Queued:           len(p.jobs),
		Active:           p.active.Load(),
		Submitted:        p.submitted.Load(),
		Completed:        p.completed.Load(),
		Failed:           p.failed.Load(),
		Canceled:         p.canceled.Load(),
		Rejected:         p.rejected.Load(),
		MaxQueued:        p.maxQueued.Load(),
		TotalQueueWaitMS: time.Duration(p.queueWait.Load()).Milliseconds(),
	}
}

// Close gracefully stops admission and waits for already accepted tasks.
// Concurrent Close calls are safe. Close does not forcibly interrupt handlers.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()

		p.submitting.Wait()
		close(p.jobs)
		p.workersWG.Wait()
		close(p.stopped)
	})
	<-p.stopped
}
