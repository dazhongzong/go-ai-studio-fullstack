package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/go-workerpool-http/workerpool"
)

// Example: go test ./internal/api -run '^$' -bench '^BenchmarkAsk$' -benchtime=2s -cpu=4 -benchmem
func BenchmarkAsk(b *testing.B) {
	for _, workers := range []int{1, 3, 6} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			p, err := workerpool.New(workers, 64, time.Second, func(ctx context.Context, t workerpool.Task) (string, error) {
				timer := time.NewTimer(5 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					return "ok", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
			if err != nil {
				b.Fatal(err)
			}
			defer p.Close()
			h := NewHandlerWithOptions(p, Options{MaxInFlight: 64, AskTimeout: 5 * time.Second})
			durations := make([]int64, b.N)
			var index atomic.Int64
			b.SetParallelism(4) // with -cpu=4: ~16 request generators
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					started := time.Now()
					w := httptest.NewRecorder()
					r := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"prompt":"hello","delay_ms":1}`))
					h.ServeHTTP(w, r)
					elapsed := time.Since(started)
					pos := int(index.Add(1) - 1)
					durations[pos] = int64(elapsed)
					if w.Code != http.StatusOK {
						b.Errorf("status=%d body=%s", w.Code, w.Body.String())
					}
				}
			})
			b.StopTimer()
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			var sum int64
			for _, d := range durations {
				sum += d
			}
			if b.N > 0 {
				p95 := durations[(95*(b.N-1))/100]
				b.ReportMetric(float64(sum)/float64(b.N)/1e6, "avg-ms")
				b.ReportMetric(float64(p95)/1e6, "p95-ms")
			}
		})
	}
}
