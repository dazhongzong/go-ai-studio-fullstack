package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"example.com/go-workerpool-http/internal/api"
	"example.com/go-workerpool-http/internal/config"
	"example.com/go-workerpool-http/internal/dynamo"
	"example.com/go-workerpool-http/internal/queue"
	"example.com/go-workerpool-http/workerpool"
)

func main() {
	if err := config.LoadDotEnv(); err != nil {
		log.Fatalf("load .env: %v", err)
	}
	queueURL := envString("QUEUE_URL", "amqp://guest:guest@127.0.0.1:5672/")
	backend := envString("BACKEND", "mock")
	model := envString("DYNAMO_MODEL", "Qwen/Qwen2.5-0.5B-Instruct")
	taskTimeout := durationEnv("TASK_TIMEOUT", 90*time.Second)

	var infer func(context.Context, string) (string, error)
	switch backend {
	case "mock":
		infer = func(ctx context.Context, prompt string) (string, error) {
			return api.SimulatedInference(ctx, workerpool.Task{Prompt: prompt, Delay: 100 * time.Millisecond})
		}
	case "dynamo":
		client := dynamo.New(envString("DYNAMO_URL", "http://127.0.0.1:8000"), model)
		infer = func(ctx context.Context, prompt string) (string, error) {
			return client.Generate(ctx, prompt)
		}
	default:
		log.Fatalf("invalid BACKEND=%q: use mock or dynamo", backend)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := queue.RunWorker(ctx, queueURL, func(ctx context.Context, prompt string) (string, error) {
		jobCtx, cancel := context.WithTimeout(ctx, taskTimeout)
		defer cancel()
		return infer(jobCtx, prompt)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		log.Printf("invalid %s=%q, using %s", name, value, fallback)
		return fallback
	}
	return duration
}
