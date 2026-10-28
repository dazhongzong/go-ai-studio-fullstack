// Package dynamo connects to the NVIDIA Dynamo OpenAI-compatible frontend.
package dynamo

import (
	"bufio"
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

	"example.com/go-workerpool-http/internal/llm"
)

type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

type completionChoice struct {
	Text         string   `json:"text,omitempty"` // legacy completions compatibility
	Message      *message `json:"message,omitempty"`
	Delta        *message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type completionResponse struct {
	Choices []completionChoice `json:"choices"`
	Error   string             `json:"error,omitempty"`
}

func New(baseURL, model string) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8000"
	}
	if model == "" {
		model = "Qwen/Qwen2.5-0.5B-Instruct"
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	} else {
		transport = transport.Clone()
	}
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Transport: transport},
	}
}

func (c *Client) request(ctx context.Context, prompt string, stream bool) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.Model,
		"messages": []message{{
			Role:    "user",
			Content: prompt,
		}},
		"stream": stream,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("%w: %v", llm.ErrTimeout, err)
		}
		return nil, fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &llm.HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	resp, err := c.request(ctx, prompt, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var output completionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&output); err != nil {
		return "", fmt.Errorf("%w: decode response: %v", llm.ErrProtocol, err)
	}
	if output.Error != "" {
		return "", fmt.Errorf("%w: %s", llm.ErrProtocol, output.Error)
	}
	if len(output.Choices) == 0 {
		return "", fmt.Errorf("%w: response contains no choices", llm.ErrProtocol)
	}
	choice := output.Choices[0]
	if choice.Message != nil {
		return choice.Message.Content, nil
	}
	return choice.Text, nil
}

// Stream consumes the OpenAI-compatible SSE response and adapts it to the
// existing gateway streamer interface.
func (c *Client) Stream(ctx context.Context, prompt string, onChunk func(llm.Chunk) error) error {
	resp, err := c.request(ctx, prompt, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	done := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			if !done {
				if err := onChunk(llm.Chunk{Done: true}); err != nil {
					return err
				}
			}
			return nil
		}
		var output completionResponse
		if err := json.Unmarshal([]byte(data), &output); err != nil {
			return fmt.Errorf("%w: decode SSE response: %v", llm.ErrProtocol, err)
		}
		if output.Error != "" {
			return fmt.Errorf("%w: %s", llm.ErrProtocol, output.Error)
		}
		if len(output.Choices) == 0 {
			continue
		}
		choice := output.Choices[0]
		text := choice.Text
		if choice.Delta != nil {
			text = choice.Delta.Content
		}
		chunk := llm.Chunk{Response: text}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			chunk.Done = true
			done = true
		}
		if err := onChunk(chunk); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: read SSE response: %v", llm.ErrProtocol, err)
	}
	if !done {
		return fmt.Errorf("%w: stream ended before completion", llm.ErrProtocol)
	}
	return nil
}
