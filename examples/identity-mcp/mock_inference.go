package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// No provider or account is contacted: deterministic tool turns test the MCP adapter.
func mockInference(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		}
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	count := 0
	ws := "10000000-0000-4000-8000-000000000001"
	for _, message := range body.Messages {
		if message.Role == "tool" {
			count++
		}
		data, _ := json.Marshal(message.Content)
		if strings.Contains(string(data), "workspace-two") {
			ws = "10000000-0000-4000-8000-000000000002"
		}
	}
	name := ""
	for _, tool := range body.Tools {
		if strings.HasSuffix(tool.Function.Name, "read_fixture") {
			name = tool.Function.Name
		}
	}
	if name != "" {
		time.Sleep(2 * time.Second)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := map[string]any{"id": fmt.Sprintf("turn-%d", count), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture"}
	delta := map[string]any{"content": "Fixture task completed"}
	finish := "stop"
	if count < 24 && name != "" {
		args, _ := json.Marshal(readArgs{Workspace: ws, Resource: "document"})
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", count), "type": "function", "function": map[string]string{"name": name, "arguments": string(args)}}}}
		finish = "tool_calls"
	}
	chunk["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	// Test-only provider usage checks native-to-Multica accumulation, not billing.
	chunk["usage"] = map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	data, _ = json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}
