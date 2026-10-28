package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"example.com/go-workerpool-http/internal/llm"
	"example.com/go-workerpool-http/workerpool"
)

// openAIChatRequest is intentionally compatible with the common subset of
// OpenAI Chat Completions requests. Unknown optional OpenAI fields are ignored
// so existing SDKs can call this gateway without schema failures.
type openAIChatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Stream         bool          `json:"stream"`
	ConversationID string        `json:"conversation_id,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Message      *openAIMessage `json:"message,omitempty"`
	Delta        *openAIDelta   `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type openAICompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
}

type openAIErrorBody struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

func openAIChatCompletionsHandler(p *workerpool.Pool, opts Options, taskIDs *atomic.Uint64) gin.HandlerFunc {
	return func(c *gin.Context) {
		input, err := decodeOpenAIChatInput(c.Writer, c.Request)
		if err != nil {
			writeOpenAIError(c.Writer, http.StatusBadRequest, err)
			return
		}

		requestInput := askRequest{
			ConversationID: input.ConversationID,
			Messages:       input.Messages,
		}
		requestInput.ClientID, err = optionalClientID(c.Request, opts.Store != nil)
		if err != nil {
			writeOpenAIError(c.Writer, http.StatusBadRequest, err)
			return
		}
		prepared, err := prepareRequestPrompt(c.Request.Context(), requestInput, opts)
		if err != nil {
			status := http.StatusBadRequest
			var persistenceErr *persistenceError
			if errors.As(err, &persistenceErr) {
				status = http.StatusInternalServerError
			}
			writeOpenAIError(c.Writer, status, err)
			return
		}

		if input.Stream {
			handleOpenAIStream(c, prepared, opts)
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), opts.AskTimeout)
		defer cancel()
		task := workerpool.Task{
			ID:     taskIDs.Add(1),
			Prompt: prepared.prompt,
			Delay:  100 * time.Millisecond,
		}
		reply, err := p.Submit(ctx, task)
		if err != nil {
			writeOpenAIError(c.Writer, taskErrorStatus(err), err)
			return
		}
		if err := saveUserMessage(c.Request.Context(), opts.Store, requestInput.ClientID, prepared.conversationID, prepared.userPrompt, requestID(c.Request), opts.Model); err != nil {
			writeOpenAIError(c.Writer, http.StatusInternalServerError, err)
			return
		}

		select {
		case result := <-reply:
			if result.Err != nil {
				writeOpenAIError(c.Writer, taskErrorStatus(result.Err), result.Err)
				return
			}
			if err := saveAssistantMessage(c.Request.Context(), opts.Store, requestInput.ClientID, prepared.conversationID, result.Output, requestID(c.Request), opts.Model); err != nil {
				writeOpenAIError(c.Writer, http.StatusInternalServerError, err)
				return
			}
			finishReason := "stop"
			c.JSON(http.StatusOK, openAICompletionResponse{
				ID:      openAICompletionID(c.Request),
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   opts.Model,
				Choices: []openAIChoice{{
					Index:        0,
					Message:      &openAIMessage{Role: "assistant", Content: result.Output},
					FinishReason: &finishReason,
				}},
			})
		case <-ctx.Done():
			writeOpenAIError(c.Writer, taskErrorStatus(ctx.Err()), ctx.Err())
		}
	}
}

func handleOpenAIStream(c *gin.Context, prepared preparedRequest, opts Options) {
	if opts.Streamer == nil {
		writeOpenAIError(c.Writer, http.StatusServiceUnavailable, errors.New("streaming is not enabled"))
		return
	}
	streamStarted := time.Now()
	streamFailed := false
	if opts.Metrics != nil {
		opts.Metrics.StreamStarted()
		defer func() {
			opts.Metrics.StreamFinished(time.Since(streamStarted), streamFailed)
		}()
	}
	if err := saveUserMessage(c.Request.Context(), opts.Store, prepared.clientID, prepared.conversationID, prepared.userPrompt, requestID(c.Request), opts.Model); err != nil {
		streamFailed = true
		writeOpenAIError(c.Writer, http.StatusInternalServerError, err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), opts.StreamTimeout)
	defer cancel()
	completionID := openAICompletionID(c.Request)
	created := time.Now().Unix()
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	writeData := func(value any) error {
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", body); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	writeChunk := func(delta *openAIDelta, finishReason *string) error {
		return writeData(openAICompletionResponse{
			ID: completionID, Object: "chat.completion.chunk", Created: created,
			Model:   opts.Model,
			Choices: []openAIChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
		})
	}
	writeDone := func() error {
		if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}

	if err := writeChunk(&openAIDelta{Role: "assistant"}, nil); err != nil {
		streamFailed = true
		return
	}
	var assistant strings.Builder
	finished := false
	err := opts.Streamer.Stream(ctx, prepared.prompt, func(chunk llm.Chunk) error {
		if chunk.Response != "" {
			assistant.WriteString(chunk.Response)
			if err := writeChunk(&openAIDelta{Content: chunk.Response}, nil); err != nil {
				return err
			}
		}
		if chunk.Done {
			finished = true
			if err := saveAssistantMessage(c.Request.Context(), opts.Store, prepared.clientID, prepared.conversationID, assistant.String(), requestID(c.Request), opts.Model); err != nil {
				return err
			}
			finishReason := "stop"
			if err := writeChunk(&openAIDelta{}, &finishReason); err != nil {
				return err
			}
			return writeDone()
		}
		return nil
	})
	if err == nil && !finished {
		if saveErr := saveAssistantMessage(c.Request.Context(), opts.Store, prepared.clientID, prepared.conversationID, assistant.String(), requestID(c.Request), opts.Model); saveErr != nil {
			err = saveErr
		} else {
			finishReason := "stop"
			if finishErr := writeChunk(&openAIDelta{}, &finishReason); finishErr != nil {
				err = finishErr
			} else {
				err = writeDone()
			}
		}
	}
	if err != nil {
		streamFailed = true
		_ = writeData(openAIErrorBody{Error: openAIError{
			Message: err.Error(), Type: openAIErrorType(err), Param: nil, Code: nil,
		}})
	}
}

func decodeOpenAIChatInput(w http.ResponseWriter, r *http.Request) (openAIChatRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var input openAIChatRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return input, errors.New("expected a single JSON object")
	}
	return input, nil
}

func openAICompletionID(r *http.Request) string {
	return "chatcmpl-" + requestID(r)
}

func writeOpenAIError(w http.ResponseWriter, status int, err error) {
	jsonReply(w, status, openAIErrorBody{Error: openAIError{
		Message: err.Error(), Type: openAIErrorType(err), Param: nil, Code: nil,
	}})
}

func openAIErrorType(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "server_error"
	}
	return "invalid_request_error"
}
