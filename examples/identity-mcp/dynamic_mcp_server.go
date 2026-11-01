package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The fixture proxy owns DNS: the selected public hostname never resolves in
// the controller, and no real Internet service is contacted.
func (f *dynamicMCPFixture) proxy() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "dynamic-mcp-fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "probe_dynamic", Description: "Read isolated fixture evidence"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]bool, error) {
		select {
		case f.first <- struct{}{}:
		default:
		}
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return nil, map[string]bool{"ok": true}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Lock()
		round := f.round
		name := strings.TrimPrefix(r.URL.Path, "/")
		want := map[string]string{"anonymous": "", "static": fmt.Sprintf("Bearer fixture-static-token-round-%d", round), "environment": fmt.Sprintf("Bearer fixture-env-token-round-%d", round)}
		authorization, valid := want[name]
		query := fmt.Sprintf("route=round%d%%2Ffixed&token=fixture-query-token-round-%d", round, round)
		if round == 3 || !valid || r.Host != "public-mcp.fixture.invalid" || r.URL.RawQuery != query || r.Header.Get("Authorization") != authorization || strings.HasPrefix(authorization, "Bearer mcp_relay_") {
			f.problem = "public recipient, exact query or authentication mismatch"
		}
		if name == "static" && r.Header.Get("Cookie") != fmt.Sprintf("selected=fixture-cookie-token-round-%d", round) || name != "static" && r.Header.Get("Cookie") != "" {
			f.problem = "recipient-scoped cookie mismatch"
		}
		if r.Method == http.MethodPost {
			var body struct {
				Method string `json:"method"`
			}
			// The SDK reads the same stream after this evidence-only prefix peek.
			if r.Body != nil {
				var data json.RawMessage
				if json.NewDecoder(r.Body).Decode(&data) == nil {
					_ = json.Unmarshal(data, &body)
					r.Body = io.NopCloser(bytes.NewReader(data))
				}
			}
			if body.Method == "tools/call" {
				f.tools[fmt.Sprintf("%d/%s", round, name)]++
			}
		}
		problem := f.problem
		f.Unlock()
		if problem != "" {
			http.Error(w, "fixture rejected request", http.StatusForbidden)
			return
		}
		// The synthetic proxy maps the asserted public target to this local SDK backend.
		r.Host = "127.0.0.1"
		handler.ServeHTTP(w, r)
	})
}

func (f *dynamicMCPFixture) model(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	count, result := 0, ""
	for _, message := range body.Messages {
		if message.Role == "user" {
			count, result = 0, ""
		}
		if message.Role == "tool" {
			count++
			data, _ := json.Marshal(message.Content)
			result = string(data)
		}
	}
	names, execute := []string{}, ""
	for _, tool := range body.Tools {
		if strings.HasSuffix(tool.Function.Name, "probe_dynamic") {
			names = append(names, tool.Function.Name)
		}
		if tool.Function.Name == "execute" {
			execute = tool.Function.Name
		}
	}
	// OpenCode also asks this provider for a title without any tools.
	if len(body.Tools) == 0 {
		_, _ = io.WriteString(w, `{"id":"title","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"Dynamic MCP fixture"},"finish_reason":"stop"}]}`)
		return
	}
	sort.Strings(names)
	delta, finish := f.turn(count, result, names, execute)
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := map[string]any{"id": fmt.Sprintf("dynamic-%d", count), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	chunk["usage"] = map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	data, _ = json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

// OpenCode 2.2 exposes MCP through native Code Mode; older releases expose
// individual tools. The deterministic fixture follows the real tool interface.
func (f *dynamicMCPFixture) turn(count int, result string, names []string, execute string) (map[string]any, string) {
	f.Lock()
	defer f.Unlock()
	tool, args := "", map[string]string{}
	if execute != "" {
		if count == 0 {
			tool = execute
			args["code"] = `return [await tools.anonymous.probe_dynamic({}), await tools.environment.probe_dynamic({}), await tools.static.probe_dynamic({})]`
			if f.round == 3 {
				args["code"] = `return ["anonymous", "environment", "static"].some(x => Object.keys(tools).includes(x)) ? "MCP_SELECTION_STALE" : "MCP_SELECTION_EMPTY"`
			}
		} else if f.round == 3 && !strings.Contains(result, "MCP_SELECTION_EMPTY") {
			f.problem = "retained native session kept removed MCP aliases"
		} else if f.round < 3 {
			for _, name := range []string{"anonymous", "environment", "static"} {
				if f.tools[fmt.Sprintf("%d/%s", f.round, name)] == 0 {
					f.problem = fmt.Sprintf("native Code Mode omitted selected MCP: %.500s", result)
				}
			}
		}
	} else if f.round != 3 && len(names) == 3 {
		if count < len(names) {
			tool = names[count]
		}
	} else if f.round != 3 || len(names) != 0 {
		f.problem = "native session retained or omitted selected MCP tools"
	}
	if tool != "" {
		data, _ := json.Marshal(args)
		return map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("dynamic-%d", count), "type": "function", "function": map[string]string{"name": tool, "arguments": string(data)}}}}, "tool_calls"
	}
	return map[string]any{"content": "Dynamic MCP fixture completed"}, "stop"
}
