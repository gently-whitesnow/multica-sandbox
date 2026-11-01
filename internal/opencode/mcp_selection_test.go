package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSelectDynamicHTTPMCPConfigFormats(t *testing.T) {
	for _, raw := range []string{
		`{"mcpServers":{"docs":{"url":"https://any-mcp.example.invalid/mcp","headers":{"authorization":"Bearer private-static","X-Api-Key":"private-key","Cookie":"session=private-cookie"}}}}`,
		`{"mcp":{"docs":{"type":"remote","url":"https://any-mcp.example.invalid/mcp","oauth":false,"enabled":true,"headers":{"authorization":"Bearer private-static","X-Api-Key":"private-key","Cookie":"session=private-cookie"}}}}`,
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(raw)
		selected, err := Select(task)
		if err != nil || len(selected) != 1 || selected["docs"].URL != "https://any-mcp.example.invalid/mcp" || selected["docs"].headers["Authorization"] != "Bearer private-static" || selected["docs"].headers["Cookie"] != "session=private-cookie" {
			t.Fatalf("HTTP MCP selection failed: %v", err)
		}
		connection, _ := json.Marshal(selected["docs"])
		config, _ := Config(selected, false)
		formatted := fmt.Sprintf("%v %+v %#v %s %q", selected, selected, selected, selected["docs"], selected["docs"])
		for _, value := range []string{"private-static", "private-key", "private-cookie"} {
			if strings.Contains(string(connection)+string(config)+formatted, value) {
				t.Fatal("MCP credentials leaked through projection or formatting")
			}
		}
		if strings.Contains(string(connection)+string(config), "headers") {
			t.Fatal("MCP credential fields entered runtime configuration")
		}
	}
}

func TestSelectSkipsDisabledMCPServers(t *testing.T) {
	for _, raw := range []string{
		`{"mcp":{"enabled":{"type":"remote","url":"https://tools.example.invalid/mcp"},"disabled":{"enabled":false},"unsupported":{"type":"local","command":["secret-command"],"enabled":false}}}`,
		`{"mcpServers":{"enabled":{"url":"https://tools.example.invalid/mcp"},"disabled":{"disabled":true}}}`,
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(raw)
		selected, err := Select(task)
		if err != nil || len(selected) != 1 || selected["enabled"].URL == "" {
			t.Fatalf("disabled MCP selection failed: %v", err)
		}
	}
}

func TestSelectUnsupportedMCPReportsFeatureWithoutCredentials(t *testing.T) {
	for _, tc := range []struct {
		entry, feature string
	}{
		{`{"type":"local","command":["private-command"]}`, "stdio"},
		{`{"type":"remote","url":"https://tools.example.invalid/mcp","oauth":{"clientSecret":"private-oauth"}}`, "OAuth"},
		{`{"type":"remote","url":"https://tools.example.invalid/mcp","timeout":30000}`, "timeout"},
		{`{"type":"remote","url":"https://tools.example.invalid/mcp","environment":{"TOKEN":"private-env"}}`, "ENV"},
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(`{"mcp":{"docs":` + tc.entry + `}}`)
		_, err := Select(task)
		if !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), tc.feature) || strings.Contains(err.Error(), "private-") {
			t.Fatalf("unsupported MCP feature not reported safely: %v", err)
		}
	}
}

func TestSelectMCPRejectsAmbiguousOrMalformedConfig(t *testing.T) {
	for _, raw := range []string{
		`{"mcpServers":{},"mcp":{}}`, `{"mcp":null}`, `{"mcp":{"x":null}}`,
		`{"mcp":{"x":{"type":"remote","url":"https://tools/mcp","extraAuth":"private-value"}}}`,
		`{"mcp":{"x":{"type":"remote","url":"https://tools/mcp","enabled":"false"}}}`,
	} {
		task := safeTask()
		task.Agent.MCPConfig = json.RawMessage(raw)
		if _, err := Select(task); !errors.Is(err, ErrDenied) || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("malformed MCP config did not fail safely: %v", err)
		}
	}
}

func TestMCPQueryAndHeadersStayControllerOnly(t *testing.T) {
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{"mcp":{"docs":{"type":"remote","url":"https://tools.example.invalid/private-path-token?project_ref=private-project&api_key=private-query-token","headers":{"Authorization":"Bearer private-header-token"}}}}`)
	selected, err := Select(task)
	if err != nil {
		t.Fatal(err)
	}
	if selected["docs"].URL != "https://tools.example.invalid/private-path-token?project_ref=private-project&api_key=private-query-token" {
		t.Fatal("selected MCP target was rewritten")
	}
	encoded, _ := json.Marshal(selected)
	config, _ := Config(selected, false)
	formatted := fmt.Sprintf("%v %+v %#v %s %q", selected, selected, selected, selected["docs"], selected["docs"])
	if strings.Contains(string(encoded)+string(config)+formatted, "private-") || strings.Contains(string(encoded)+formatted, "tools.example.invalid") {
		t.Fatal("MCP endpoint or credentials leaked through formatting/JSON/config")
	}
	for _, target := range []string{"https://tools.example.invalid/mcp?token={env:SECRET_TOKEN}", "https://tools.example.invalid/mcp?token=%7Benv%3ASECRET_TOKEN%7D"} {
		task.Agent.MCPConfig = json.RawMessage(`{"mcpServers":{"docs":{"url":"` + target + `"}}}`)
		if _, err := Select(task); err == nil || !strings.Contains(err.Error(), "URL cannot contain credential templates") || strings.Contains(err.Error(), "SECRET_TOKEN") {
			t.Fatalf("URL ENV substitution not rejected safely: %v", err)
		}
	}
}
