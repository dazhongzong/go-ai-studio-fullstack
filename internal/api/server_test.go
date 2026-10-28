package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/go-workerpool-http/internal/store"
	"example.com/go-workerpool-http/workerpool"
)

type memoryConversationStore struct {
	history []store.Message
	saved   []store.Message
}

func (s *memoryConversationStore) EnsureConversation(context.Context, string, string) error {
	return nil
}

func (s *memoryConversationStore) LoadMessages(context.Context, string, int) ([]store.Message, error) {
	return append([]store.Message(nil), s.history...), nil
}

func (s *memoryConversationStore) SaveMessage(_ context.Context, _ string, message store.Message, _, _ string) error {
	s.saved = append(s.saved, message)
	return nil
}

func newTestHandler(t *testing.T, timeout time.Duration) http.Handler {
	t.Helper()
	p, err := workerpool.New(2, 2, timeout, SimulatedInference)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return NewHandler(p)
}

func TestAskSuccess(t *testing.T) {
	h := newTestHandler(t, time.Second)
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"prompt":"hello","delay_ms":1}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "[mock-llm] hello") {
		t.Fatalf("status=%d response=%s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"request_id":"req-`, `"queue_ms":`, `"duration_ms":`, `"model":"mock"`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("response missing %q: %s", field, w.Body.String())
		}
	}
	if requestID := w.Header().Get("X-Request-ID"); !strings.HasPrefix(requestID, "req-") {
		t.Errorf("request id header=%q", requestID)
	}
}

func TestPromptLengthLimit(t *testing.T) {
	p, err := workerpool.New(1, 1, time.Second, SimulatedInference)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h := NewHandlerWithOptions(p, Options{MaxPromptLength: 3, AskTimeout: time.Second})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"prompt":"abcd"}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "maximum length") {
		t.Fatalf("status=%d response=%s", w.Code, w.Body.String())
	}
}

func TestAskUsesConversationContext(t *testing.T) {
	gotPrompt := make(chan string, 1)
	p, err := workerpool.New(1, 1, time.Second, func(ctx context.Context, task workerpool.Task) (string, error) {
		gotPrompt <- task.Prompt
		return "context-aware reply", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h := NewHandlerWithOptions(p, Options{AskTimeout: time.Second})
	body := `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"continue"}]}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d response=%s", w.Code, w.Body.String())
	}
	got := <-gotPrompt
	for _, want := range []string{"User: hello", "Assistant: hi", "User: continue", "Assistant:"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q: %q", want, got)
		}
	}
}

func TestAskLoadsAndPersistsMySQLConversation(t *testing.T) {
	conversationStore := &memoryConversationStore{
		history: []store.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}
	p, err := workerpool.New(1, 1, time.Second, func(ctx context.Context, task workerpool.Task) (string, error) {
		return task.Prompt, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h := NewHandlerWithOptions(p, Options{AskTimeout: time.Second, Store: conversationStore})
	w := httptest.NewRecorder()
	body := `{"conversation_id":"chat-1","prompt":"continue"}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d response=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"User: hello", "Assistant: hi", "User: continue", "Assistant:"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("response missing %q: %s", want, w.Body.String())
		}
	}
	if len(conversationStore.saved) != 2 || conversationStore.saved[0].Role != "user" || conversationStore.saved[1].Role != "assistant" {
		t.Fatalf("saved messages=%+v", conversationStore.saved)
	}
}

func TestAskRejectsInvalidConversation(t *testing.T) {
	h := newTestHandler(t, time.Second)
	cases := []string{
		`{"messages":[{"role":"assistant","content":"not a user turn"}]}`,
		`{"messages":[{"role":"system","content":"unsupported"},{"role":"user","content":"hello"}]}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d response=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestAskBadJSONAndMethod(t *testing.T) {
	h := newTestHandler(t, time.Second)
	cases := []struct {
		method, body string
		status       int
	}{
		{http.MethodPost, `{"prompt":`, http.StatusBadRequest},
		{http.MethodPost, `{"prompt":" "}`, http.StatusBadRequest},
		{http.MethodPost, `{"prompt":"hello","unknown":1}`, http.StatusBadRequest},
		{http.MethodGet, "", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, "/ask", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("want %d got %d", tc.status, w.Code)
		}
	}
}

func TestAskTimeout(t *testing.T) {
	h := newTestHandler(t, 10*time.Millisecond)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"prompt":"slow","delay_ms":300}`))
	h.ServeHTTP(w, req)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("want 504, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequestCanceledBeforeHandler(t *testing.T) {
	h := newTestHandler(t, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(`{"prompt":"hello"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("want 408 got %d", w.Code)
	}
}
