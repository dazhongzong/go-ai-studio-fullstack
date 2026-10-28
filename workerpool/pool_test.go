package workerpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSubmitAndClose(t *testing.T) {
	p, err := New(3, 2, time.Second, func(ctx context.Context, task Task) (string, error) {
		return task.Prompt + "!", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	replies := make([]<-chan Result, n)
	for i := range replies {
		replies[i], err = p.Submit(context.Background(), Task{ID: uint64(i + 1), Prompt: "ok"})
		if err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	for i, reply := range replies {
		res := <-reply
		if res.Err != nil || res.Output != "ok!" || res.TaskID != uint64(i+1) {
			t.Fatalf("unexpected result: %+v", res)
		}
	}
	stats := p.Snapshot()
	if stats.Completed != n || stats.Failed != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	p.Close() // idempotent
	if _, err := p.Submit(context.Background(), Task{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestTaskDeadline(t *testing.T) {
	p, err := New(1, 0, 15*time.Millisecond, func(ctx context.Context, task Task) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ch, err := p.Submit(context.Background(), Task{ID: 9})
	if err != nil {
		t.Fatal(err)
	}
	if res := <-ch; !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %+v", res)
	}
}

func TestSubmitCancelWhileQueueFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p, err := New(1, 0, 0, func(ctx context.Context, task Task) (string, error) {
		close(started)
		<-release
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := p.Submit(context.Background(), Task{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Submit(ctx, Task{ID: 2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected blocked enqueue deadline, got %v", err)
	}
	close(release)
	if res := <-ch; res.Err != nil {
		t.Fatal(res.Err)
	}
	p.Close()
}

func TestConcurrentSubmitAndClose(t *testing.T) {
	p, err := New(4, 4, time.Second, func(ctx context.Context, task Task) (string, error) {
		time.Sleep(time.Millisecond)
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ch, err := p.Submit(ctx, Task{ID: uint64(i)})
			if err != nil {
				if !errors.Is(err, ErrClosed) {
					t.Errorf("submit failed: %v", err)
				}
				return
			}
			if result := <-ch; result.Err != nil {
				t.Errorf("task failed: %v", result.Err)
			}
		}(i)
	}
	go p.Close()
	wg.Wait()
	p.Close()
}

func TestInvalidConfig(t *testing.T) {
	_, err := New(0, 2, 0, func(context.Context, Task) (string, error) { return "", nil })
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStatsIncludeQueueMetrics(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p, err := New(1, 2, 0, func(ctx context.Context, task Task) (string, error) {
		if task.ID == 1 {
			close(started)
		}
		select {
		case <-release:
			return "ok", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := p.Submit(context.Background(), Task{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := p.Submit(context.Background(), Task{ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	stats := p.Snapshot()
	if stats.Active != 1 || stats.Queued != 1 || stats.Submitted != 2 || stats.MaxQueued < 1 {
		t.Fatalf("unexpected live stats: %+v", stats)
	}

	close(release)
	if firstResult := <-first; firstResult.Err != nil {
		t.Fatal(firstResult.Err)
	}
	if secondResult := <-second; secondResult.Err != nil {
		t.Fatal(secondResult.Err)
	}
	p.Close()
	stats = p.Snapshot()
	if stats.Active != 0 || stats.Completed != 2 || stats.TotalQueueWaitMS < 0 {
		t.Fatalf("unexpected final stats: %+v", stats)
	}
}
