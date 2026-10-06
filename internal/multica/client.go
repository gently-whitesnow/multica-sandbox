package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const UpstreamRevision = "b4ca5b4a23e68b26292a680dca7689a952bb1cd5"
const ProbeProvider = "sandbox-probe"

type Client struct {
	base   string
	token  string
	http   *http.Client
	outbox *Outbox
}

type HTTPError struct{ Status int }

var errTransport = errors.New("Multica transport failed")

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Multica HTTP status %d (response body withheld)", e.Status)
}

func New(base, token string) (*Client, error) { return newClient(base, token, false) }

// NewAllowHTTP is for isolated fixtures whose Multica origin is a private container name.
func NewAllowHTTP(base, token string) (*Client, error) { return newClient(base, token, true) }

func newClient(base, token string, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("server must be an HTTP(S) origin without credentials, query or path")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (allowHTTP || ip != nil && ip.IsLoopback())) {
		return nil, fmt.Errorf("HTTPS required except for literal loopback IPs")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("controller token required")
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Platform", "multica-sandbox-probe")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil {
		return fmt.Errorf("read Multica response")
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("Multica response exceeds limit")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid Multica response")
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// Transient mirrors the upstream daemon retry predicate: transport, 5xx, 408 and 429.
func Transient(err error) bool {
	var status *HTTPError
	if errors.As(err, &status) {
		return status.Status >= 500 || status.Status == http.StatusRequestTimeout || status.Status == http.StatusTooManyRequests
	}
	return errors.Is(err, errTransport)
}

func statusIs(err error, code int) bool {
	var status *HTTPError
	return errors.As(err, &status) && status.Status == code
}

// Conflict reports Multica's rejection of a start for a claim it no longer dispatches to us.
func Conflict(err error) bool { return statusIs(err, http.StatusConflict) }

// Missing reports a task Multica no longer has; the attempt is interrupted without a callback.
func Missing(err error) bool { return statusIs(err, http.StatusNotFound) }

// Upstream daemon schedules; N delays give N+1 attempts.
var (
	terminalRetry = []time.Duration{4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second}
	startRetry    = []time.Duration{500 * time.Millisecond, 2 * time.Second}
	retrySleep    = func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

func retry(ctx context.Context, schedule []time.Duration, call func() error) error {
	err := call()
	for _, delay := range schedule {
		if !Transient(err) {
			return err
		}
		if waitErr := retrySleep(ctx, delay); waitErr != nil {
			return waitErr
		}
		err = call()
	}
	return err
}
