package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func doRequest(ctx context.Context, method, address string, body any, bearer string) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("fixture request failed")
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 65536))
	return resp.StatusCode, data, err
}
func token(ctx context.Context) (string, error) {
	secret, err := os.ReadFile("/secrets/client")
	if err != nil {
		return "", err
	}
	values := url.Values{"grant_type": {"client_credentials"}, "client_id": {"example-agent"}, "client_secret": {string(secret)}}
	req, _ := http.NewRequestWithContext(ctx, "POST", issuer+"/protocol/openid-connect/token", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("issuer unavailable")
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"access_token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token == "" {
		return "", fmt.Errorf("token request failed")
	}
	return out.Token, nil
}
func enroll(ctx context.Context, run grant) error {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, _, err := doRequest(ctx, "POST", "http://gateway:8080/grants", run, string(secret))
	if err != nil || status != 204 {
		return fmt.Errorf("grant registration failed")
	}
	return nil
}
func call(ctx context.Context, bearer, workspace, resource string, want bool) error {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "read_fixture", "arguments": readArgs{workspace, resource}}}
	status, data, err := doRequest(ctx, "POST", "http://gateway:8080/mcp", body, bearer)
	if err != nil {
		return err
	}
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content any  `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if json.Unmarshal(data, &out) != nil && status == 200 {
		return fmt.Errorf("invalid MCP response")
	}
	success := status == 200 && out.Error == nil && !out.Result.IsError && out.Result.Content != nil
	denied := status == 401 || status == 403 || (status == 200 && out.Error == nil && out.Result.IsError)
	if (want && !success) || (!want && !denied) {
		return fmt.Errorf("unexpected MCP decision: status=%d expected-success=%t", status, want)
	}
	return nil
}
func scenario() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := token(ctx)
	if err != nil {
		return err
	}
	b, err := token(ctx)
	if err != nil {
		return err
	}
	unregistered, err := token(ctx)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("attempt tokens are identical")
	}
	runA := grant{"grant-a", "workspace-a", "agent-1", "task-a", "attempt-a", "document-a", fingerprint(a), true}
	runB := grant{"grant-b", "workspace-a", "agent-1", "task-b", "attempt-b", "document-b", fingerprint(b), true}
	for _, run := range []grant{runA, runB} {
		if err := enroll(ctx, run); err != nil {
			return err
		}
	}
	status, _, err := doRequest(ctx, "POST", "http://gateway:8080/grants", runA, a)
	if err != nil || status != 403 {
		return fmt.Errorf("run token reached admin API")
	}
	fmt.Println("PASS run token cannot administer grants")
	checks := []struct {
		name, token, ws, resource string
		allow                     bool
	}{
		{"attempt A allowed", a, "workspace-a", "document-a", true},
		{"attempt B allowed", b, "workspace-a", "document-b", true},
		{"cross-attempt denied", a, "workspace-a", "document-b", false},
		{"cross-workspace denied", a, "workspace-b", "document-a", false},
		{"unregistered signed token denied", unregistered, "workspace-a", "document-a", false},
		{"forged token denied", "invalid", "workspace-a", "document-a", false},
	}
	for _, check := range checks {
		if err := call(ctx, check.token, check.ws, check.resource, check.allow); err != nil {
			return fmt.Errorf("%s: %w", check.name, err)
		}
		fmt.Println("PASS", check.name)
	}
	runA.Active = false
	if err := enroll(ctx, runA); err != nil {
		return err
	}
	if err := call(ctx, a, "workspace-a", "document-a", false); err != nil {
		return err
	}
	if err := call(ctx, b, "workspace-a", "document-b", true); err != nil {
		return err
	}
	fmt.Println("PASS revoked A denied while B remains allowed")
	return nil
}
