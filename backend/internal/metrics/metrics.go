// Package metrics contains the small, dependency-free metrics registry used by
// the gateway. It exposes Prometheus text format so the project can be scraped
// without adding another HTTP framework or a global registry.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Registry contains process-local gateway metrics. Counters are intentionally
// monotonic; Prometheus calculates rates between scrapes.
type Registry struct {
	requestsTotal      atomic.Uint64
	requestErrorsTotal atomic.Uint64
	activeRequests     atomic.Int64
	requestDurationNS  atomic.Int64
	streamsTotal       atomic.Uint64
	activeStreams      atomic.Int64
	streamDurationNS   atomic.Int64
	streamErrorsTotal  atomic.Uint64
}

func New() *Registry { return &Registry{} }

func (r *Registry) RequestStarted() { r.activeRequests.Add(1) }

func (r *Registry) RequestFinished(status int, duration time.Duration) {
	r.activeRequests.Add(-1)
	r.requestsTotal.Add(1)
	r.requestDurationNS.Add(duration.Nanoseconds())
	if status >= http.StatusInternalServerError {
		r.requestErrorsTotal.Add(1)
	}
}

func (r *Registry) StreamStarted() {
	r.activeStreams.Add(1)
	r.streamsTotal.Add(1)
}

func (r *Registry) StreamFinished(duration time.Duration, failed bool) {
	r.activeStreams.Add(-1)
	r.streamDurationNS.Add(duration.Nanoseconds())
	if failed {
		r.streamErrorsTotal.Add(1)
	}
}

// ServeHTTP writes metrics in Prometheus' text exposition format. poolText is
// optional and is used by the API package to append worker-pool gauges.
func (r *Registry) ServeHTTP(w http.ResponseWriter, poolText func(io.Writer) error) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fprintf := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	fprintf("# HELP gateway_requests_total Total HTTP requests handled by the gateway.\n# TYPE gateway_requests_total counter\ngateway_requests_total %d\n", r.requestsTotal.Load())
	fprintf("# HELP gateway_request_errors_total HTTP requests ending with a 5xx status.\n# TYPE gateway_request_errors_total counter\ngateway_request_errors_total %d\n", r.requestErrorsTotal.Load())
	fprintf("# HELP gateway_requests_active HTTP requests currently being handled.\n# TYPE gateway_requests_active gauge\ngateway_requests_active %d\n", r.activeRequests.Load())
	fprintf("# HELP gateway_request_duration_seconds_sum Sum of HTTP request durations.\n# TYPE gateway_request_duration_seconds_sum counter\ngateway_request_duration_seconds_sum %.9f\n", float64(r.requestDurationNS.Load())/float64(time.Second))
	fprintf("# HELP gateway_streams_total Total streaming requests started.\n# TYPE gateway_streams_total counter\ngateway_streams_total %d\n", r.streamsTotal.Load())
	fprintf("# HELP gateway_streams_active Streaming requests currently being handled.\n# TYPE gateway_streams_active gauge\ngateway_streams_active %d\n", r.activeStreams.Load())
	fprintf("# HELP gateway_stream_errors_total Streaming requests that ended with an error.\n# TYPE gateway_stream_errors_total counter\ngateway_stream_errors_total %d\n", r.streamErrorsTotal.Load())
	fprintf("# HELP gateway_stream_duration_seconds_sum Sum of streaming request durations.\n# TYPE gateway_stream_duration_seconds_sum counter\ngateway_stream_duration_seconds_sum %.9f\n", float64(r.streamDurationNS.Load())/float64(time.Second))
	if poolText != nil {
		_ = poolText(w)
	}
}
