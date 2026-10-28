// Package ollama connects to the Ollama /api/generate endpoint.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	ErrUnavailable = errors.New("ollama unavailable")
	ErrTimeout     = errors.New("ollama timeout")
	ErrProtocol    = errors.New("ollama protocol error")
)

// HTTPError represents a non-200 response returned by Ollama.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("ollama status %d", e.StatusCode)
	}
	return fmt.Sprintf("ollama status %d: %s", e.StatusCode, e.Body)
}

type Chunk struct {
	Response  string `json:"response"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
	EvalCount int    `json:"eval_count,omitempty"`
}

type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

func New(baseURL, model string) *Client {
	return NewWithOptions(baseURL, model, 120*time.Second)
}

// NewWithOptions creates an Ollama client. The response header timeout only
// covers waiting for Ollama to start responding; the request Context controls
// the total lifetime so streaming responses can run for a long time.
func NewWithOptions(baseURL, model string, responseHeaderTimeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	if model == "" {
		model = "qwen2.5:0.5b"
	}
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 120 * time.Second
	}
	// Reuse this client (and its Transport) across concurrent requests.
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		baseTransport = &http.Transport{}
	}
	transport := baseTransport.Clone()
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Model: model, HTTP: &http.Client{Transport: transport}}
}

func (c *Client) request(ctx context.Context, prompt string, stream bool) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{
		"model":  c.Model,
		"prompt": prompt,
		"stream": stream,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

func protocolError(err error) error {
	return fmt.Errorf("%w: %w", ErrProtocol, err)
}

// Generate reads a non-streamed JSON response.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	resp, err := c.request(ctx, prompt, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var output Chunk
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&output); err != nil {
		return "", fmt.Errorf("%w: decode JSON: %v", ErrProtocol, err)
	}
	if output.Error != "" {
		return "", fmt.Errorf("%w: %s", ErrProtocol, output.Error)
	}
	if !output.Done {
		return "", protocolError(io.ErrUnexpectedEOF)
	}
	return output.Response, nil
}

// Stream reads newline-delimited JSON. Decoder handles successive JSON objects.
// The callback runs synchronously: slow consumers exert backpressure upstream.
func (c *Client) Stream(ctx context.Context, prompt string, onChunk func(Chunk) error) error {
	resp, err := c.request(ctx, prompt, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var chunk Chunk
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				return protocolError(io.ErrUnexpectedEOF)
			}
			return fmt.Errorf("%w: decode stream: %v", ErrProtocol, err)
		}
		if chunk.Error != "" {
			return fmt.Errorf("%w: %s", ErrProtocol, chunk.Error)
		}
		if err := onChunk(chunk); err != nil {
			return err
		}
		if chunk.Done {
			return nil
		}
	}
}
