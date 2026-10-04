package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func exchange(ctx context.Context, method, address, bearer string, payload any, out any) (int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Workspace-ID", workspace)
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fixture transport failed")
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
	}
	return resp.StatusCode, err
}
func control(ctx context.Context, c configuration, method, path string, body, out any) error {
	status, err := exchange(ctx, method, "http://multica:8080"+path, session(c), body, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("Multica %s: HTTP %d", path, status)
	}
	return nil
}
func identity(ctx context.Context, c configuration, scope string) (string, error) {
	body := url.Values{"grant_type": {"client_credentials"}, "client_id": {"agent-demo"}, "client_secret": {c.Client}, "scope": {scope}}
	req, err := http.NewRequestWithContext(ctx, "POST", issuer+"/protocol/openid-connect/token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("identity issuer unavailable")
	}
	defer resp.Body.Close()
	var result struct {
		Token string `json:"access_token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&result) != nil || result.Token == "" {
		return "", fmt.Errorf("identity issuance failed")
	}
	return result.Token, nil
}
