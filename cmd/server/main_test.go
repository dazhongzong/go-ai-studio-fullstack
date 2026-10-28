package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/go-workerpool-http/internal/api"
	"example.com/go-workerpool-http/workerpool"
)

func TestFrontendAndAPI(t *testing.T) {
	pool, err := workerpool.New(2, 2, time.Second, api.SimulatedInference)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<h1>Go AI Studio</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	app := webHandler(api.NewHandlerWithOptions(pool, api.Options{MaxInFlight: 4, AskTimeout: time.Second}), "mock", "", dist)

	t.Run("frontend", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("Go AI Studio")) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("info", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/info", nil))
		var v struct {
			Backend   string `json:"backend"`
			Model     string `json:"model"`
			Streaming bool   `json:"streaming"`
		}
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.Backend != "mock" || v.Model != "mock" || v.Streaming {
			t.Fatalf("bad info: %+v", v)
		}
	})
	t.Run("api-ask", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/ask", bytes.NewBufferString(`{"prompt":"hello", "delay_ms":1}`))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("hello")) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("api-stats", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"workers":2`)) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("original-urls-still-work", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
	})
}
