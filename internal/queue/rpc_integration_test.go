//go:build integration

package queue

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRabbitMQRoundTrip(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("RABBITMQ_URL"))
	if url == "" {
		t.Skip("set RABBITMQ_URL to run RabbitMQ integration tests")
	}

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer clientCancel()
	client, err := NewClientWithRetry(clientCtx, url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerErr := make(chan error, 1)
	go func() {
		workerErr <- RunWorker(ctx, url, "integration-worker", func(_ context.Context, prompt string) (string, error) {
			return "echo:" + prompt, nil
		})
	}()

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	got, err := client.Call(callCtx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo:hello" {
		t.Fatalf("response=%q", got)
	}

	cancel()
	select {
	case err := <-workerErr:
		if err != nil && !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("worker stopped with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}
