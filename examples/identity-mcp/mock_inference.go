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
var parentPattern = regexp.MustCompile(`--parent ([0-9a-f-]{36})`)
var warmHints = []*regexp.Regexp{
	regexp.MustCompile(`\d+ new comment\(s\) on this issue since your last run`),
	regexp.MustCompile(`the issue changed: [a-z]+(, [a-z]+)*`),
	regexp.MustCompile(`\d+ DISTINCT threads`),
}

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
	// count and result describe the turn since the latest prompt; a resumed session
	// carries earlier prompts, and a dropped one the upstream continuity notice.
	count, prompts, latest := 0, 0, ""
	ws := "10000000-0000-4000-8000-000000000001"
	issue, result := "", ""
	for _, message := range body.Messages {
		text, ok := message.Content.(string)
		if !ok {
			data, _ := json.Marshal(message.Content)
			text = string(data)
		}
		if message.Role == "user" {
			prompts++
			count, result, latest = 0, "", text
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
	chat := strings.Contains(latest, "You are running as a chat assistant for a Multica workspace.")
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
	case bash && chat && count == 0:
		// Chat runs read the stored transcript and leave a note; a resumed chat reads the note.
		command := "multica chat history --output json | grep -q 'code word violet' && printf violet > chat-note.txt && echo 'history read'"
		if prompts > 1 {
			command = "cat chat-note.txt"
		}
		call("bash", map[string]string{"command": command, "description": "Read the chat"})
	case bash && chat:
		state := map[bool]string{true: "Chat resumed", false: "Chat started"}[prompts > 1]
		for _, word := range []string{"history read", "violet"} {
			if strings.Contains(result, word) {
				state += "; " + word
			}
		}
		delta = map[string]any{"content": state}
	case count < 24 && name != "":
		call(name, readArgs{Workspace: ws, Resource: "document"})
	case bash && issue != "" && count == 0 && (prompts > 1 || strings.Contains(latest, "## Session Continuity Notice")):
		// Follow-up runs report the retained workdir, the conversation and the prompt's warm hints.
		state := map[bool]string{true: "Resumed", false: "Fresh after lost session"}[prompts > 1]
		for _, hint := range warmHints {
			if match := hint.FindString(latest); match != "" {
				state += "; " + match
			}
		}
		parent := ""
		if match := parentPattern.FindStringSubmatch(latest); match != nil {
			parent = " --parent " + match[1]
		}
		call("bash", map[string]string{"command": "printf '%s: %s\\n' '" + state + "' \"$(cat notes.txt)\" > reply.md && multica issue comment add " + issue + parent + " --content-file ./reply.md --output table && rm reply.md", "description": "Post the follow-up"})
	case bash && issue != "" && count == 0:
		// The upstream-style prompt names the issue; the agent reads it through the Multica relay.
		call("bash", map[string]string{"command": "multica issue get " + issue + " --output json", "description": "Read the assigned issue"})
	case bash && issue != "" && count == 1:
		if title := titlePattern.FindStringSubmatch(result); title != nil {
			// Relay fixture tasks hold up to 40 s until the test probe marks the live attempt.
			hold, checkout := "", ""
			if title[1] == "Relay fixture issue" || title[1] == "Repository fixture issue" {
				hold = "i=0; while [ ! -e /workspace/.probed ] && [ $i -lt 200 ]; do sleep 0.2; i=$((i+1)); done; "
			}
			// Repository tasks check out with the unchanged CLI and commit; after the hold, in which
			// the test turns the co-author setting off, they commit again, push with plain git and open a pull request with gh.
			if title[1] == "Repository fixture issue" {
				checkout = `repo=$(multica repo checkout https://git.fixture.test/sandbox/fixture.git) && git -C "$repo" commit -q --allow-empty -m 'Fixture agent work' && `
				hold += `printf 'Checkout: %s %s\n' "$(git -C "$repo" branch --show-current)" "$(cat "$repo/README")" >> reply.md && ` +
					`git -C "$repo" commit -q --allow-empty -m 'Fixture co-author off' && git -C "$repo" push -q origin HEAD && printf 'first run' > notes.txt && ` +
					`{ ! command -v gh >/dev/null || { pr=$(cd "$repo" && gh pr create --head "$(git branch --show-current)" --base main --title 'Fixture agent work' --body 'Fixture agent work') && printf 'Pull request: %s\n' "$pr" >> reply.md; }; } && `
			}
			// A configured tool bundle reports itself; images without jq post only the read.
			call("bash", map[string]string{"command": checkout + hold + "printf '%s\\n' 'Relay fixture read: " + title[1] + "' >> reply.md && { ! command -v jq >/dev/null || jq --version >> reply.md; } && multica issue comment add " + issue + " --content-file ./reply.md --output table && rm reply.md", "description": "Post the result"})
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
