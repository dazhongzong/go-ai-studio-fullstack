// Package llm contains protocol-neutral types shared by inference clients and
// the HTTP API.
package llm

import (
	"errors"
	"fmt"
)

var (
	ErrUnavailable = errors.New("inference backend unavailable")
	ErrTimeout     = errors.New("inference backend timeout")
	ErrProtocol    = errors.New("inference backend protocol error")
)

// HTTPError represents a non-success response from an inference backend.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("inference backend status %d", e.StatusCode)
	}
	return fmt.Sprintf("inference backend status %d: %s", e.StatusCode, e.Body)
}

// Chunk is the normalized token format used by the gateway streamer interface.
type Chunk struct {
	Response  string `json:"response"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
	EvalCount int    `json:"eval_count,omitempty"`
}
