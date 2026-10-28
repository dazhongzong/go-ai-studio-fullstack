package dynamo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/go-workerpool-http/internal/llm"
)

func TestGenerateUsesOpenAIChatCompletions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
			Stream   bool      `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "test-model" || len(body.Messages) != 1 ||
			body.Messages[0].Role != "user" || body.Messages[0].Content != "hello" || body.Stream {
			t.Fatalf("body=%+v", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	answer, err := New(srv.URL, "test-model").Generate(context.Background(), "hello")
	if err != nil || answer != "world" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
}

func TestStreamSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"text\":\"a\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"text\":\"b\",\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	defer srv.Close()

	var pieces []string
	err := New(srv.URL, "test-model").Stream(context.Background(), "hello", func(chunk llm.Chunk) error {
		pieces = append(pieces, chunk.Response)
		return nil
	})
	if err != nil || strings.Join(pieces, "") != "ab" {
		t.Fatalf("pieces=%v err=%v", pieces, err)
	}
}
