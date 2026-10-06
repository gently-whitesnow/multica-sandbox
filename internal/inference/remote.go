package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

func (s *Service) remote(ctx context.Context, scope Scope) (Target, error) {
	var out struct {
		Version   int    `json:"version"`
		Workspace Scope  `json:"workspace"`
		Target    Target `json:"target"`
	}
	request := struct {
		Version   int   `json:"version"`
		Workspace Scope `json:"workspace"`
	}{1, scope}
	if s.remoteJSON(ctx, request, s.config.External, &out) != nil || out.Version != 1 || out.Workspace != scope {
		return Target{}, ErrDenied
	}
	return out.Target, nil
}
func (s *Service) remoteJSON(ctx context.Context, request any, c *identity.ExternalConfig, out any) error {
	b, err := readFile(c.BearerFile, 4096)
	bearer := strings.TrimSpace(string(b))
	if err != nil || bearer == "" || strings.ContainsAny(bearer, "\r\n\x00") {
		return ErrDenied
	}
	data, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(data))
	if err != nil {
		return ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := s.client.Do(req)
	if err != nil {
		return ErrDenied
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || resp.StatusCode != 200 || decode(data, out) != nil {
		return ErrDenied
	}
	return nil
}
