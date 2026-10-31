package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

type baoSource struct {
	config TokenSourceConfig
	client *http.Client
}

func (s baoSource) request(ctx context.Context, address, method, token string, input, out any) error {
	var body bytes.Buffer
	if input != nil && json.NewEncoder(&body).Encode(input) != nil {
		return ErrDenied
	}
	req, err := http.NewRequestWithContext(ctx, method, address, &body)
	if err != nil {
		return ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return ErrDenied
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrDenied
	}
	if out != nil && json.NewDecoder(response.Body).Decode(out) != nil {
		return ErrDenied
	}
	return nil
}

func (s baoSource) Token(ctx context.Context, c Credentials) (bearer string, err error) {
	if len(c.request.Scopes) != 0 || c.request.Resource != "" {
		return "", ErrDenied
	}
	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if s.request(ctx, s.config.LoginURL, http.MethodPost, "", map[string]string{"role_id": c.ClientID, "secret_id": c.secret}, &login) != nil || !text(login.Auth.ClientToken, 8192) {
		return "", ErrDenied
	}
	// Revoke the short-lived login token even if issuance was cancelled. It never
	// reaches an attempt, disk, logs, or the MCP recipient.
	defer func() {
		if s.request(context.WithoutCancel(ctx), s.config.RevokeURL, http.MethodPost, login.Auth.ClientToken, nil, nil) != nil {
			bearer, err = "", ErrDenied
		}
	}()
	var result struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if s.request(ctx, s.config.TokenURL, http.MethodGet, login.Auth.ClientToken, nil, &result) != nil || !text(result.Data.Token, 16384) {
		return "", ErrDenied
	}
	return result.Data.Token, nil
}
