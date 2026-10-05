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

func (s *Service) remote(ctx context.Context, ref identity.Ref) (Target, error) {
	c := s.config.External
	b, err := readFile(c.BearerFile, 4096)
	bearer := strings.TrimSpace(string(b))
	if err != nil || bearer == "" || strings.ContainsAny(bearer, "\r\n\x00") {
		return Target{}, ErrDenied
	}
	data, _ := json.Marshal(struct {
		Version int          `json:"version"`
		Agent   identity.Ref `json:"agent"`
	}{1, ref})
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(data))
	if err != nil {
		return Target{}, ErrDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := s.client.Do(req)
	if err != nil {
		return Target{}, ErrDenied
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 65537))
	var out struct {
		Version int          `json:"version"`
		Agent   identity.Ref `json:"agent"`
		Target  Target       `json:"target"`
	}
	if err != nil || resp.StatusCode != 200 || decode(data, &out) != nil || out.Version != 1 || out.Agent != ref {
		return Target{}, ErrDenied
	}
	return out.Target, nil
}
