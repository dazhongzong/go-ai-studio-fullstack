package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGenerateStreamFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Model != "unit-test" || input.Prompt != "hello" || input.Stream {
			t.Errorf("bad input %+v", input)
		}
		w.Write([]byte(`{"response":"mock-response","done":true}`))
	}))
	defer srv.Close()
	client := New(srv.URL, "unit-test")
	answer, err := client.Generate(context.Background(), "hello")
	if err != nil || answer != "mock-response" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
}

func TestStreamChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte("{\"response\":\"a\",\"done\":false}\n{\"response\":\"b\",\"done\":false}\n{\"done\":true}\n"))
	}))
	defer srv.Close()
	var pieces []string
	err := New(srv.URL, "fake").Stream(context.Background(), "hello", func(c Chunk) error {
		if c.Response != "" {
			pieces = append(pieces, c.Response)
		}
		return nil
	})
	if err != nil || strings.Join(pieces, "") != "ab" {
		t.Fatalf("pieces=%v, err=%v", pieces, err)
	}
}

func TestGenerateCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately delay response headers; no assumption that the test
		// server immediately observes a disconnected client.
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"response":"late","done":true}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := New(srv.URL, "fake").Generate(ctx, "hello")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestStreamTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":"foo","done":false}` + "\n"))
	}))
	defer srv.Close()
	err := New(srv.URL, "fake").Stream(context.Background(), "hi", func(Chunk) error { return nil })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPErrorIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "missing").Generate(context.Background(), "hello")
	var remoteErr *HTTPError
	if !errors.As(err, &remoteErr) || remoteErr.StatusCode != http.StatusNotFound {
		t.Fatalf("error=%v typed=%+v", err, remoteErr)
	}
}

func TestResponseHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"response":"late","done":true}`))
	}))
	defer srv.Close()

	_, err := NewWithOptions(srv.URL, "slow", 10*time.Millisecond).Generate(context.Background(), "hello")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}
