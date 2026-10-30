package multica

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/baseurl"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
)

// UpstreamRevision is Multica v0.5.2, the release self-hosted deployments run (ADR 0005).
const UpstreamRevision = "d45aba1cd7582bef9210b921bbb7dc198b48e1ee"
const ProbeProvider = "sandbox-probe"

type Client struct {
	base   string
	token  string
	http   *http.Client
	outbox *Outbox
	// metrics and log, when set, record each request (Instrument).
	metrics *metrics.Metrics
	log     *slog.Logger
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
	// A base path (Multica behind a shared ingress prefix) is kept and API paths are joined below it.
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || !baseurl.Path(u) {
		return nil, fmt.Errorf("server must be an HTTP(S) URL without credentials, query or unclean path")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (allowHTTP || ip != nil && ip.IsLoopback())) {
		return nil, fmt.Errorf("HTTPS required except for literal loopback IPs")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("controller token required")
	}
	return &Client{base: baseurl.Join(u), token: token, http: &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	return c.callLimit(ctx, method, path, body, out, responseLimit)
}

// callLimit is call with a response limit for claims, which carry inline skills.
func (c *Client) callLimit(ctx context.Context, method, path string, body, out any, limit int) error {
	read := 0
	if out != nil {
		read = limit
	}
	data, err := c.send(ctx, method, path, body, read)
	if err != nil || out == nil {
		return err
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid Multica response")
	}
	return nil
}

// send returns a response body of at most read bytes; 0 discards it.
func (c *Client) send(ctx context.Context, method, path string, body any, read int) (data []byte, err error) {
	start, route := time.Now(), Route(path)
	if ctx.Value(resentKey{}) != nil {
		c.metrics.Retry(Target, method, route)
	}
	status, cause := 0, error(nil)
	defer func() {
		errorType := ""
		if status == 0 {
			if cause == nil {
				return
			}
			errorType = metrics.ErrorType(ctx, cause)
		}
		c.metrics.Client(Target, method, route, status, errorType, time.Since(start))
		if c.log != nil && (errorType != "" || status >= 400) {
			c.log.Debug("multica request failed", "method", method, "path", route, "status", status, "error_type", errorType, "seconds", time.Since(start).Seconds())
		}
	}()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Platform", "multica-sandbox-probe")
	// Claims then carry structured coalesced comments instead of a folded trigger body, and
	// inline skills without the redirect stubs for briefs older than multica-platform.
	req.Header.Set("X-Client-Capabilities", "coalesced-comments-v1,platform-skill-v1")
	resp, err := c.http.Do(req)
	if err != nil {
		// The cause names the failure for metrics; callers see only errTransport.
		cause = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errTransport
	}
	status = resp.StatusCode
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{resp.StatusCode}
	}
	if read == 0 {
		return nil, nil
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, int64(read)+1))
	if err != nil {
		return nil, fmt.Errorf("read Multica response")
	}
	if len(data) > read {
		return nil, fmt.Errorf("Multica response exceeds limit")
	}
	return data, nil
}

// Response limits: claims carry inline skills of up to 8 MiB (ADR 0018).
const (
	responseLimit = 2 << 20
	claimLimit    = 16 << 20
)

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

// retry hands call a context that marks resent requests after the first.
func retry(ctx context.Context, schedule []time.Duration, call func(context.Context) error) error {
	err := call(ctx)
	for _, delay := range schedule {
		if !Transient(err) {
			return err
		}
		if waitErr := retrySleep(ctx, delay); waitErr != nil {
			return waitErr
		}
		err = call(resent(ctx))
	}
	return err
}

type resentKey struct{}

func resent(ctx context.Context) context.Context { return context.WithValue(ctx, resentKey{}, true) }

// Target names Multica in client metrics.
const Target = "multica"

// Instrument records each request in m and logs failed ones at debug level.
func (c *Client) Instrument(m *metrics.Metrics, log *slog.Logger) { c.metrics, c.log = m, log }

// routeWords are the literal segments of controller requests; any other segment is an
// identifier and becomes {id}, so the path label stays bounded.
var routeWords = map[string]bool{"api": true, "daemon": true, "workspaces": true, "repos": true, "tasks": true, "claim": true,
	"issues": true, "gc-check": true, "runtimes": true, "status": true, "register": true, "heartbeat": true, "env": true,
	"recover-orphans": true, "prepare-lease": true, "start": true, "messages": true, "cancel-ack": true, "complete": true,
	"fail": true, "usage": true, "session": true, "models": true, "update": true, "local-skills": true, "import": true, "result": true}

// Route is the request path template: no query and identifiers as {id}.
func Route(path string) string {
	path, _, _ = strings.Cut(path, "?")
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if s != "" && !routeWords[s] {
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}
