package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var inferenceReplies atomic.Int64

// slowStreams counts opened and closed fixture streams for cancellation checks.
var slowStreams [2]atomic.Int64

// provider is the credential-checked mock behind LiteLLM; it records the selection it receives.
func (g *registry) provider(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("INFERENCE_FIXTURE") == "1" {
		key, err := os.ReadFile("/secrets/upstream")
		if err != nil || r.Header.Get("Authorization") != "Bearer "+string(key) {
			w.WriteHeader(403)
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var selection struct {
		Model  string `json:"model"`
		Effort string `json:"reasoning_effort"`
	}
	if err != nil || json.Unmarshal(body, &selection) != nil {
		w.WriteHeader(400)
		return
	}
	g.Lock()
	g.calls["selection:"+selection.Model+"|"+selection.Effort]++
	g.Unlock()
	if bytes.Contains(body, []byte("slow-stream")) {
		slowStream(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	mockInference(w, r)
	inferenceReplies.Add(1)
}

// slowStream keeps an SSE response open until the caller disconnects.
func slowStream(w http.ResponseWriter, r *http.Request) {
	slowStreams[0].Add(1)
	defer slowStreams[1].Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		chunk, _ := json.Marshal(map[string]any{"id": "slow", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": fmt.Sprintf("%d ", i)}, "finish_reason": nil}}})
		if _, err := fmt.Fprintf(w, "data: %s\n\n", chunk); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
