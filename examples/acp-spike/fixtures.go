package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Scenario markers in the prompt select deterministic model turns; no provider is contacted.
const (
	markTool   = "SPIKE-TOOL"
	markRotate = "SPIKE-ROTATE"
	markResume = "SPIKE-RESUME"
	markCancel = "SPIKE-CANCEL"
	markDenied = "SPIKE-DENIED"
)

// deniedTool is offered by 2.x although the projected permissions do not allow it.
const deniedTool = "opencode_worktree_remove"

// buildSeconds keeps a tool silent long enough to tell in-flight visibility from completion.
const buildSeconds = 6

var debug = false

func inference(w http.ResponseWriter, r *http.Request) {
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
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	// count is the number of tool results since the latest user prompt.
	count, prompts, latest := 0, 0, ""
	for _, message := range body.Messages {
		text, ok := message.Content.(string)
		if !ok {
			data, _ := json.Marshal(message.Content)
			text = string(data)
		}
		switch message.Role {
		case "user":
			prompts++
			count, latest = 0, text
		case "tool":
			count++
		}
	}
	// OpenCode 2.x names the shell tool "shell"; 1.x calls it "bash".
	probe, shell, names := "", "bash", []string{}
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
		if tool.Function.Name == "shell" {
			shell = "shell"
		}
		if strings.HasSuffix(tool.Function.Name, "probe") {
			probe = tool.Function.Name
		}
	}
	if debug {
		fmt.Printf("  inference: prompts=%d count=%d tools=%v\n", prompts, count, names)
	}
	delta, finish := map[string]any{"content": "ok"}, "stop"
	call := func(tool string, args any) {
		data, _ := json.Marshal(args)
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", count), "type": "function", "function": map[string]string{"name": tool, "arguments": string(data)}}}}
		finish = "tool_calls"
	}
	switch {
	case strings.Contains(latest, markTool) && count == 0 && probe != "":
		call(probe, map[string]string{})
	case strings.Contains(latest, markTool) && count <= 1:
		call(shell, map[string]string{"command": fmt.Sprintf("sleep %d && echo built", buildSeconds), "description": "Build"})
	case strings.Contains(latest, markTool):
		delta = map[string]any{"content": "tool turn done"}
	case strings.Contains(latest, markRotate) && count == 0:
		// The controller rewrites the auth store while this tool runs.
		call(shell, map[string]string{"command": "sleep 4", "description": "Wait"})
	case strings.Contains(latest, markRotate) && count == 1 && probe != "":
		call(probe, map[string]string{})
	case strings.Contains(latest, markResume):
		delta = map[string]any{"content": fmt.Sprintf("prompts seen: %d", prompts)}
	case strings.Contains(latest, markDenied) && count == 0:
		call(deniedTool, map[string]string{})
	case strings.Contains(latest, markCancel) && count == 0:
		call(shell, map[string]string{"command": "sleep 120", "description": "Hang"})
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := map[string]any{"id": fmt.Sprintf("turn-%d", count), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture"}
	chunk["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	chunk["usage"] = map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	data, _ = json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

// bearers records the Authorization header of every MCP request, in order.
type bearers struct {
	mu   sync.Mutex
	seen []string
}

func (b *bearers) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if debug {
			fmt.Printf("  mcp: %s %s\n", r.Method, r.Header.Get("Mcp-Method"))
		}
		b.mu.Lock()
		b.seen = append(b.seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		b.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (b *bearers) list() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

func mcpFixture(seen *bearers) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "spike", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "probe", Description: "Return a fixed value"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "probed"}}}, nil, nil
	})
	// The fixture listens on loopback and the container calls it as host.docker.internal.
	options := &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}
	return seen.record(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, options))
}
