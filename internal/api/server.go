package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"example.com/go-workerpool-http/internal/llm"
	"example.com/go-workerpool-http/internal/store"

	"example.com/go-workerpool-http/workerpool"
)

type askRequest struct {
	Prompt         string        `json:"prompt"`
	ConversationID string        `json:"conversation_id"`
	Messages       []chatMessage `json:"messages"` // backward-compatible client context
	DelayMS        int           `json:"delay_ms"` // simulated inference duration
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type askResponse struct {
	RequestID  string `json:"request_id"`
	TaskID     uint64 `json:"task_id"`
	WorkerID   int    `json:"worker_id"`
	Reply      string `json:"reply"`
	Model      string `json:"model"`
	QueueMS    int64  `json:"queue_ms"`
	DurationMS int64  `json:"duration_ms"`
}

type errorResponse struct {
	Error      string `json:"error"`
	RequestID  string `json:"request_id,omitempty"`
	QueueMS    int64  `json:"queue_ms,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type requestIDContextKey struct{}

const (
	DefaultMaxPromptLength    = 8192
	DefaultMaxContextMessages = 24
	DefaultMaxContextLength   = 32768
)

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func logRequests() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		log.Printf("request_id=%s %s %s status=%d duration=%s", c.Writer.Header().Get("X-Request-ID"), c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(started))
	}
}

func withRequestID(ids *atomic.Uint64) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := fmt.Sprintf("req-%08d", ids.Add(1))
		c.Header("X-Request-ID", id)
		ctx := context.WithValue(c.Request.Context(), requestIDContextKey{}, id)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func requestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDContextKey{}).(string); ok {
		return id
	}
	return ""
}

// NewHandler provides HTTP API routes using Gin.
// Streamer is the minimal interface required by the SSE API.
type Streamer interface {
	Stream(context.Context, string, func(llm.Chunk) error) error
}

// Options configures HTTP admission and backend request timeouts.
type Options struct {
	MaxInFlight        int
	AskTimeout         time.Duration
	StreamTimeout      time.Duration
	Streamer           Streamer
	AccessLog          bool
	Model              string
	MaxPromptLength    int
	MaxContextMessages int
	MaxContextLength   int
	Store              store.ConversationStore
}

func NewHandler(p *workerpool.Pool) http.Handler {
	return NewHandlerWithOptions(p, Options{
		MaxInFlight: 8, AskTimeout: 1500 * time.Millisecond,
		AccessLog: true, Model: "mock", MaxPromptLength: DefaultMaxPromptLength,
	})
}

func NewHandlerWithOptions(p *workerpool.Pool, opts Options) http.Handler {
	if opts.MaxInFlight <= 0 {
		opts.MaxInFlight = 8
	}
	if opts.AskTimeout <= 0 {
		opts.AskTimeout = 1500 * time.Millisecond
	}
	if opts.StreamTimeout <= 0 {
		opts.StreamTimeout = 2 * time.Minute
	}
	if opts.Model == "" {
		opts.Model = "mock"
	}
	if opts.MaxPromptLength <= 0 {
		opts.MaxPromptLength = DefaultMaxPromptLength
	}
	if opts.MaxContextMessages <= 0 {
		opts.MaxContextMessages = DefaultMaxContextMessages
	}
	if opts.MaxContextLength <= 0 {
		opts.MaxContextLength = DefaultMaxContextLength
	}
	var requestIDs atomic.Uint64
	var taskIDs atomic.Uint64
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.Use(gin.Recovery(), withRequestID(&requestIDs))
	if opts.AccessLog {
		router.Use(logRequests())
	}

	router.NoMethod(func(c *gin.Context) {
		c.Header("Allow", "GET, POST")
		writeRequestError(c.Writer, http.StatusMethodNotAllowed, c.Request, time.Now(), 0, errors.New("method not allowed"))
	})
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	router.GET("/stats", func(c *gin.Context) {
		c.JSON(http.StatusOK, p.Snapshot())
	})
	router.POST("/ask", limitAsk(opts.MaxInFlight), func(c *gin.Context) {
		started := time.Now()
		input, err := decodeAskInput(c.Writer, c.Request)
		if err != nil {
			writeRequestError(c.Writer, http.StatusBadRequest, c.Request, started, 0, err)
			return
		}
		prepared, err := prepareRequestPrompt(c.Request.Context(), input, opts)
		if err != nil {
			status := http.StatusBadRequest
			var persistenceErr *persistenceError
			if errors.As(err, &persistenceErr) {
				status = http.StatusInternalServerError
			}
			writeRequestError(c.Writer, status, c.Request, started, 0, err)
			return
		}
		if input.DelayMS < 0 || input.DelayMS > 3000 {
			writeRequestError(c.Writer, http.StatusBadRequest, c.Request, started, 0, errors.New("delay_ms must be 0..3000"))
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), opts.AskTimeout)
		defer cancel()
		delay := time.Duration(input.DelayMS) * time.Millisecond
		if delay == 0 {
			delay = 100 * time.Millisecond
		}
		task := workerpool.Task{ID: taskIDs.Add(1), Prompt: prepared.prompt, Delay: delay}
		reply, err := p.Submit(ctx, task)
		if err != nil {
			writeTaskError(c.Writer, c.Request, started, 0, err)
			return
		}
		if err := saveUserMessage(c.Request.Context(), opts.Store, prepared.conversationID, prepared.userPrompt, requestID(c.Request), opts.Model); err != nil {
			writeRequestError(c.Writer, http.StatusInternalServerError, c.Request, started, 0, err)
			return
		}
		select {
		case result := <-reply:
			if result.Err != nil {
				writeTaskError(c.Writer, c.Request, started, result.QueueWait, result.Err)
				return
			}
			if err := saveAssistantMessage(c.Request.Context(), opts.Store, prepared.conversationID, result.Output, requestID(c.Request), opts.Model); err != nil {
				writeRequestError(c.Writer, http.StatusInternalServerError, c.Request, started, result.QueueWait, err)
				return
			}
			c.JSON(http.StatusOK, askResponse{
				RequestID: requestID(c.Request), TaskID: result.TaskID, WorkerID: result.WorkerID,
				Reply: result.Output, Model: opts.Model,
				QueueMS: result.QueueWait.Milliseconds(), DurationMS: time.Since(started).Milliseconds(),
			})
		case <-ctx.Done():
			writeTaskError(c.Writer, c.Request, started, 0, ctx.Err())
		}
	})
	router.POST("/v1/chat/completions", limitAsk(opts.MaxInFlight), openAIChatCompletionsHandler(p, opts, &taskIDs))
	return router
}

// Limit only inference endpoints; health checks and stats remain accessible.
func limitAsk(maxConcurrent int) gin.HandlerFunc {
	slots := make(chan struct{}, maxConcurrent)
	return func(c *gin.Context) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			c.Next()
		default:
			started := time.Now()
			c.Header("Retry-After", "1")
			writeRequestError(c.Writer, http.StatusTooManyRequests, c.Request, started, 0, errors.New("too many concurrent requests"))
			c.Abort()
		}
	}
}

func decodeAskInput(w http.ResponseWriter, r *http.Request) (askRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var input askRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return input, errors.New("expected a single JSON object")
	}
	return input, nil
}

func validatePrompt(prompt string, maxLength int) error {
	if prompt == "" {
		return errors.New("prompt is required")
	}
	if utf8.RuneCountInString(prompt) > maxLength {
		return fmt.Errorf("prompt exceeds maximum length of %d characters", maxLength)
	}
	return nil
}

// preparePrompt accepts both the original single prompt and a complete
// conversation. The Dynamo completions endpoint receives one prompt, so the
// conversation is serialized with explicit speaker markers before inference.
func preparePrompt(input askRequest, maxPromptLength, maxContextMessages, maxContextLength int) (string, error) {
	if len(input.Messages) == 0 {
		prompt := strings.TrimSpace(input.Prompt)
		if err := validatePrompt(prompt, maxPromptLength); err != nil {
			return "", err
		}
		return prompt, nil
	}
	if len(input.Messages) > maxContextMessages {
		return "", fmt.Errorf("too many context messages: maximum is %d", maxContextMessages)
	}

	var builder strings.Builder
	contentLength := 0
	for i, message := range input.Messages {
		message.Role = strings.ToLower(strings.TrimSpace(message.Role))
		message.Content = strings.TrimSpace(message.Content)
		if message.Role != "user" && message.Role != "assistant" && message.Role != "system" {
			return "", fmt.Errorf("messages[%d].role must be user, assistant, or system", i)
		}
		if err := validatePrompt(message.Content, maxPromptLength); err != nil {
			return "", fmt.Errorf("messages[%d].content: %w", i, err)
		}
		contentLength += utf8.RuneCountInString(message.Content)
		if contentLength > maxContextLength {
			return "", fmt.Errorf("conversation exceeds maximum context length of %d characters", maxContextLength)
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		switch message.Role {
		case "user":
			builder.WriteString("User: ")
		case "assistant":
			builder.WriteString("Assistant: ")
		case "system":
			builder.WriteString("System: ")
		}
		builder.WriteString(message.Content)
	}
	lastRole := strings.ToLower(strings.TrimSpace(input.Messages[len(input.Messages)-1].Role))
	if lastRole != "user" {
		return "", errors.New("the last context message must have role user")
	}
	builder.WriteString("\n\nAssistant:")
	return builder.String(), nil
}

type preparedRequest struct {
	prompt         string
	conversationID string
	userPrompt     string
}

type persistenceError struct{ err error }

func (e *persistenceError) Error() string { return e.err.Error() }
func (e *persistenceError) Unwrap() error { return e.err }

func prepareRequestPrompt(ctx context.Context, input askRequest, opts Options) (preparedRequest, error) {
	conversationID := strings.TrimSpace(input.ConversationID)
	if opts.Store == nil || conversationID == "" {
		prompt, err := preparePrompt(input, opts.MaxPromptLength, opts.MaxContextMessages, opts.MaxContextLength)
		return preparedRequest{prompt: prompt}, err
	}
	if len(conversationID) > 64 {
		return preparedRequest{}, errors.New("conversation_id is too long")
	}
	userPrompt := strings.TrimSpace(input.Prompt)
	if userPrompt == "" && len(input.Messages) > 0 {
		lastMessage := input.Messages[len(input.Messages)-1]
		if strings.EqualFold(strings.TrimSpace(lastMessage.Role), "user") {
			userPrompt = strings.TrimSpace(lastMessage.Content)
		}
	}
	if err := validatePrompt(userPrompt, opts.MaxPromptLength); err != nil {
		return preparedRequest{}, err
	}
	history, err := opts.Store.LoadMessages(ctx, conversationID, opts.MaxContextMessages-1)
	if err != nil {
		return preparedRequest{}, &persistenceError{err: err}
	}
	messages := make([]chatMessage, 0, len(history)+1)
	for _, message := range history {
		messages = append(messages, chatMessage{Role: message.Role, Content: message.Content})
	}
	messages = append(messages, chatMessage{Role: "user", Content: userPrompt})
	prompt, err := preparePrompt(askRequest{Messages: messages}, opts.MaxPromptLength, opts.MaxContextMessages, opts.MaxContextLength)
	if err != nil {
		return preparedRequest{}, err
	}
	return preparedRequest{prompt: prompt, conversationID: conversationID, userPrompt: userPrompt}, nil
}

func saveUserMessage(ctx context.Context, conversationStore store.ConversationStore, conversationID, content, requestID, model string) error {
	if conversationStore == nil || conversationID == "" || content == "" {
		return nil
	}
	if err := conversationStore.EnsureConversation(ctx, conversationID, content); err != nil {
		return err
	}
	return conversationStore.SaveMessage(ctx, conversationID, store.Message{Role: "user", Content: content}, requestID, model)
}

func saveAssistantMessage(ctx context.Context, conversationStore store.ConversationStore, conversationID, content, requestID, model string) error {
	if conversationStore == nil || conversationID == "" || content == "" {
		return nil
	}
	return conversationStore.SaveMessage(ctx, conversationID, store.Message{Role: "assistant", Content: content}, requestID, model)
}

func writeRequestError(w http.ResponseWriter, status int, r *http.Request, started time.Time, queueWait time.Duration, err error) {
	jsonReply(w, status, errorResponse{
		Error: err.Error(), RequestID: requestID(r),
		QueueMS: queueWait.Milliseconds(), DurationMS: time.Since(started).Milliseconds(),
	})
}

func writeTaskError(w http.ResponseWriter, r *http.Request, started time.Time, queueWait time.Duration, err error) {
	status := taskErrorStatus(err)
	var remoteErr *llm.HTTPError
	if errors.As(err, &remoteErr) {
		status = http.StatusBadGateway
	}
	writeRequestError(w, status, r, started, queueWait, err)
}

func taskErrorStatus(err error) int {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
	case errors.Is(err, llm.ErrTimeout):
		status = http.StatusGatewayTimeout
	case errors.Is(err, llm.ErrUnavailable), errors.Is(err, llm.ErrProtocol):
		status = http.StatusBadGateway
	case errors.Is(err, workerpool.ErrClosed):
		status = http.StatusServiceUnavailable
	}
	return status
}

// SimulatedInference demonstrates a cancelable LLM backend. Replace later with an HTTP client.
func SimulatedInference(ctx context.Context, task workerpool.Task) (string, error) {
	t := time.NewTimer(task.Delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-t.C:
		return fmt.Sprintf("[mock-llm] %s", task.Prompt), nil
	}
}
