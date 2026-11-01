// Package attempt integrates a deployment-owned active-attempt authority.
package attempt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrDenied = errors.New("attempt authorization unavailable or denied")

type Config struct {
	URL        string `json:"url"`
	BearerFile string `json:"bearer_file"`
	AllowHTTP  bool   `json:"allow_http,omitempty"`
}

type Grant struct {
	Version    int    `json:"version"`
	Controller string `json:"controller"`
	Attempt    string `json:"attempt,omitempty"`
	Server     string `json:"server,omitempty"`
	Workspace  string `json:"workspace_id,omitempty"`
	Agent      string `json:"agent_id,omitempty"`
	Task       string `json:"task_id,omitempty"`
	URL        string `json:"mcp_url,omitempty"`
	TokenHash  string `json:"token_hash,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Action     string `json:"action"`
}

type Authority struct {
	config Config
	client *http.Client
	// mu guards the bearer last read and the file state it was read from.
	mu     sync.Mutex
	read   os.FileInfo
	bearer string
}

func New(c Config) (*Authority, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http")) || !filepath.IsAbs(c.BearerFile) {
		return nil, ErrDenied
	}
	return &Authority{config: c, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func Fingerprint(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// secret re-reads the bearer file only when it was replaced, rewritten or re-permissioned,
// so a rotation applies to the next request without a read per renewal.
func (a *Authority) secret() (string, error) {
	current, err := os.Stat(a.config.BearerFile)
	if err != nil {
		return "", ErrDenied
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if read := a.read; read != nil && os.SameFile(read, current) && read.ModTime().Equal(current.ModTime()) && read.Size() == current.Size() && read.Mode() == current.Mode() {
		return a.bearer, nil
	}
	file, err := os.Open(a.config.BearerFile)
	if err != nil {
		return "", ErrDenied
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "", ErrDenied
	}
	secret, err := io.ReadAll(io.LimitReader(file, 4097))
	bearer := strings.TrimSpace(string(secret))
	if err != nil || len(secret) > 4096 || bearer == "" || strings.ContainsAny(bearer, "\r\n") {
		return "", ErrDenied
	}
	a.read, a.bearer = info, bearer
	return bearer, nil
}

func (a *Authority) Apply(ctx context.Context, grant Grant) error {
	grant.Version = 1
	data, err := json.Marshal(grant)
	if err != nil {
		return ErrDenied
	}
	bearer, err := a.secret()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.config.URL, bytes.NewReader(data))
	if err != nil {
		return ErrDenied
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("authority request unavailable: %w", ErrDenied)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("authority HTTP %d: %w", resp.StatusCode, ErrDenied)
	}
	return nil
}
