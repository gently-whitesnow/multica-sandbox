package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

type staticResolver struct {
	bindings map[Ref]Binding
}

func (s *staticResolver) Resolve(ctx context.Context, r Ref) (Credentials, error) {
	if ctx.Err() != nil {
		return Credentials{}, ErrDenied
	}
	b, ok := s.bindings[r]
	if !ok {
		return Credentials{}, ErrDenied
	}
	secret, err := readSecret(b.SecretFile)
	if err != nil {
		return Credentials{}, ErrDenied
	}
	return Credentials{b.Principal, secret}, nil
}

type remoteResolver struct {
	config ExternalConfig
	server string
	client *http.Client
}

func (s *remoteResolver) Resolve(ctx context.Context, r Ref) (Credentials, error) {
	if !validRef(r, s.server) {
		return Credentials{}, ErrDenied
	}
	bearer, err := readSecret(s.config.BearerFile)
	if err != nil {
		return Credentials{}, ErrDenied
	}
	data, _ := json.Marshal(resolveRequest{1, r})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(data))
	if err != nil {
		return Credentials{}, ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	response, err := s.client.Do(req)
	if err != nil {
		return Credentials{}, ErrDenied
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	var out resolveResponse
	if err != nil || response.StatusCode != 200 || strictJSON(body, &out) != nil || out.Version != 1 || out.Agent != r || !text(out.ClientSecret, 8192) {
		return Credentials{}, ErrDenied
	}
	return Credentials{out.Principal, out.ClientSecret}, nil
}
