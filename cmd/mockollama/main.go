// mockollama is a local protocol simulator for learning without downloading a model.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		var input struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if !input.Stream {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"response": "mock: " + input.Prompt, "done": true})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unavailable", 500)
			return
		}
		for _, part := range []string{"Mock ", "Ollama ", "stream: ", input.Prompt} {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(120 * time.Millisecond):
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"response": part, "done": false}); err != nil {
				return
			}
			flusher.Flush()
		}
		json.NewEncoder(w).Encode(map[string]any{"done": true, "eval_count": 4})
		flusher.Flush()
	})
	log.Println("mockollama listening on http://localhost:11435")
	log.Fatal(http.ListenAndServe(":11435", mux))
}
