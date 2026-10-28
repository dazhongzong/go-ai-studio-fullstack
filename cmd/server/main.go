package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"example.com/go-workerpool-http/internal/api"
	"example.com/go-workerpool-http/internal/config"
	"example.com/go-workerpool-http/internal/dynamo"
	"example.com/go-workerpool-http/internal/logging"
	"example.com/go-workerpool-http/internal/queue"
	"example.com/go-workerpool-http/internal/store"
	"example.com/go-workerpool-http/workerpool"
)

func main() {
	if err := config.LoadDotEnv(); err != nil {
		log.Fatalf("load .env: %v", err)
	}
	closeLog, err := logging.Setup("api")
	if err != nil {
		log.Fatalf("setup api log: %v", err)
	}
	defer closeLog()
	if strings.TrimSpace(os.Getenv("GIN_MODE")) == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	backend := os.Getenv("BACKEND")
	if backend == "" {
		backend = "mock"
	}
	model := envString("DYNAMO_MODEL", "Qwen/Qwen2.5-0.5B-Instruct")
	log.Printf("api configuration backend=%s model=%s dynamo_url=%s", backend, model, envString("DYNAMO_URL", "http://127.0.0.1:8000"))
	maxPromptLength := envInt("MAX_PROMPT_LENGTH", api.DefaultMaxPromptLength)
	maxContextMessages := envInt("MAX_CONTEXT_MESSAGES", api.DefaultMaxContextMessages)
	maxContextLength := envInt("MAX_CONTEXT_LENGTH", api.DefaultMaxContextLength)
	maxInFlight := envInt("MAX_IN_FLIGHT", 8)
	poolWorkers := envInt("POOL_WORKERS", 3)
	poolQueueSize := envInt("POOL_QUEUE_SIZE", 5)
	handler := workerpool.Handler(api.SimulatedInference)
	opts := api.Options{
		MaxInFlight: maxInFlight, AskTimeout: 1500 * time.Millisecond,
		StreamTimeout: 2 * time.Minute, AccessLog: true,
		Model: model, MaxPromptLength: maxPromptLength,
		MaxContextMessages: maxContextMessages, MaxContextLength: maxContextLength,
		APIKey: strings.TrimSpace(os.Getenv("API_KEY")),
	}
	var conversationStore *store.MySQLStore
	if dsn := strings.TrimSpace(os.Getenv("MYSQL_DSN")); dsn != "" {
		var err error
		conversationStore, err = store.OpenMySQL(context.Background(), dsn)
		if err != nil {
			log.Fatal(err)
		}
		defer conversationStore.Close()
		opts.Store = conversationStore
		log.Println("mysql conversation store enabled")
	}
	var dynamoClient *dynamo.Client
	switch backend {
	case "mock":
		opts.Model = "mock"
	case "dynamo":
		dynamoClient = dynamo.New(envString("DYNAMO_URL", "http://127.0.0.1:8000"), model)
		handler = func(ctx context.Context, task workerpool.Task) (string, error) {
			return dynamoClient.Generate(ctx, task.Prompt)
		}
		opts.Streamer = dynamoClient
		opts.AskTimeout = durationEnv("ASK_TIMEOUT", 100*time.Second)
		opts.StreamTimeout = durationEnv("STREAM_TIMEOUT", 180*time.Second)
	default:
		log.Fatalf("invalid BACKEND=%q: use mock or dynamo", backend)
	}
	var queueClient *queue.Client
	if queueURL := strings.TrimSpace(os.Getenv("QUEUE_URL")); queueURL != "" {
		var err error
		queueClient, err = queue.NewClient(queueURL)
		if err != nil {
			log.Fatal(err)
		}
		defer queueClient.Close()
		handler = func(ctx context.Context, task workerpool.Task) (string, error) {
			return queueClient.Call(ctx, task.Prompt)
		}
		log.Printf("rabbitmq inference queue enabled: %s", queueURL)
	}
	opts.Readiness = func(ctx context.Context) error {
		if dynamoClient != nil {
			if err := dynamoClient.Health(ctx); err != nil {
				return fmt.Errorf("dynamo is not ready: %w", err)
			}
		}
		if queueClient != nil {
			if err := queueClient.Ready(); err != nil {
				return fmt.Errorf("rabbitmq is not ready: %w", err)
			}
		}
		if conversationStore != nil {
			if err := conversationStore.Ping(ctx); err != nil {
				return fmt.Errorf("mysql is not ready: %w", err)
			}
		}
		return nil
	}
	taskTimeout := durationEnv("TASK_TIMEOUT", 700*time.Millisecond)
	if backend == "dynamo" {
		taskTimeout = durationEnv("TASK_TIMEOUT", 90*time.Second)
	}
	pool, err := workerpool.New(poolWorkers, poolQueueSize, taskTimeout, handler)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	log.Printf("worker pool ready workers=%d queue_capacity=%d task_timeout=%s", poolWorkers, poolQueueSize, taskTimeout)
	if opts.APIKey == "" {
		log.Println("api key authentication disabled")
	} else {
		log.Println("api key authentication enabled")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{
		Addr: ":" + port, Handler: webHandler(api.NewHandlerWithOptions(pool, opts), backend, model, webDist()),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       60 * time.Second,
		// No server-wide WriteTimeout: streams may run for minutes.
		// StreamTimeout and request Context bound application work.
		WriteTimeout: 0,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	log.Printf("api server listening address=http://127.0.0.1:%s", port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		log.Println("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown: %v", err)
	}
	log.Println("api server stopped")
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

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		log.Printf("invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}
	return n
}

// webDist locates the built Vite assets relative to the project root.
// The server remains an API-only application when assets are not built yet.
func webDist() string {
	if fromEnv := os.Getenv("WEB_DIST"); fromEnv != "" {
		return fromEnv
	}
	return filepath.Join("frontend", "dist")
}

// webHandler keeps existing API URLs backward compatible and adds a /api
// namespace for both Vite's dev proxy and same-origin production deployment.
func webHandler(apiHandler http.Handler, backend, model, distDir string) http.Handler {
	router := gin.New()
	router.Use(gin.Recovery())
	router.HandleMethodNotAllowed = true
	router.GET("/api/info", func(c *gin.Context) {
		displayModel := model
		if displayModel == "" {
			displayModel = "qwen2.5:0.5b"
		}
		if backend == "mock" {
			displayModel = "mock"
		}
		c.JSON(http.StatusOK, struct {
			Backend   string `json:"backend"`
			Model     string `json:"model"`
			Streaming bool   `json:"streaming"`
		}{backend, displayModel, backend == "dynamo"})
	})
	apiHandlerWithoutPrefix := gin.WrapH(http.StripPrefix("/api", apiHandler))
	for _, path := range []string{"/ask", "/v1/chat/completions", "/healthz", "/livez", "/readyz", "/stats"} {
		router.Any("/api"+path, apiHandlerWithoutPrefix)
		router.Any(path, gin.WrapH(apiHandler))
	}
	// Built assets are optional; during development Vite serves the UI.
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err == nil {
		staticHandler := gin.WrapH(http.FileServer(http.Dir(distDir)))
		router.NoRoute(staticHandler)
	}
	return router
}
