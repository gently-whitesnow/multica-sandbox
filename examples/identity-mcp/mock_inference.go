package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var issuePattern = regexp.MustCompile(`Your assigned issue ID is: ([0-9a-f-]{36})`)
var titlePattern = regexp.MustCompile(`"title":\s*"([A-Za-z0-9 ]{1,64})"`)

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
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body) != nil {
		w.WriteHeader(400)
		return
	}
	count := 0
	ws := "10000000-0000-4000-8000-000000000001"
	issue, result := "", ""
	for _, message := range body.Messages {
		text, ok := message.Content.(string)
		if !ok {
			data, _ := json.Marshal(message.Content)
			text = string(data)
		}
		if message.Role == "tool" {
			count++
			result = text
		}
		if strings.Contains(text, "workspace-two") {
			ws = "10000000-0000-4000-8000-000000000002"
		}
		if match := issuePattern.FindStringSubmatch(text); match != nil {
			issue = match[1]
		}
	}
	name, bash := "", false
	for _, tool := range body.Tools {
		if strings.HasSuffix(tool.Function.Name, "read_fixture") {
			name = tool.Function.Name
		}
		bash = bash || tool.Function.Name == "bash"
	}
	if name != "" || (bash && issue != "") {
		time.Sleep(2 * time.Second)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := map[string]any{"id": fmt.Sprintf("turn-%d", count), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "fixture"}
	delta := map[string]any{"content": "Fixture task completed"}
	finish := "stop"
	call := func(tool string, args any) {
		data, _ := json.Marshal(args)
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("call-%d", count), "type": "function", "function": map[string]string{"name": tool, "arguments": string(data)}}}}
		finish = "tool_calls"
	}
	switch {
	case count < 24 && name != "":
		call(name, readArgs{Workspace: ws, Resource: "document"})
	case bash && issue != "" && count == 0:
		// The upstream-style prompt names the issue; the agent reads it through the Multica relay.
		call("bash", map[string]string{"command": "multica issue get " + issue + " --output json", "description": "Read the assigned issue"})
	case bash && issue != "" && count == 1:
		if title := titlePattern.FindStringSubmatch(result); title != nil {
			// A configured tool bundle reports itself; images without jq post only the read.
			call("bash", map[string]string{"command": "printf '%s\\n' 'Relay fixture read: " + title[1] + "' > reply.md && { ! command -v jq >/dev/null || jq --version >> reply.md; } && multica issue comment add " + issue + " --content-file ./reply.md --output table && rm reply.md", "description": "Post the result"})
		}
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
