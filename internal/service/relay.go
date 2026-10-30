package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const relayBodyLimit = 32 << 20

// serveRelay forwards attempt requests to their grant's trusted upstream until ctx ends.
// Routes serve exact paths on the same listener instead of the relay. The body limit
// defaults to relayBodyLimit. With tlsConfig the listener terminates TLS. Every request
// is recorded under the lower-case name.
func (config Config) serveRelay(ctx context.Context, name string, auth relay.Authorizer, c RelayConfig, p relay.Policy, routes map[string]http.Handler, tlsConfig *tls.Config) error {
	if _, err := relayAddress(name, c); err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("%s relay listen address required", name)
	}
	if p.Limit == 0 {
		p.Limit = relayBodyLimit
	}
	p.Record = config.relayRecord(strings.ToLower(name))
	proxy, err := relay.New(auth, p)
	if err != nil {
		return err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route := routes[r.URL.Path]; route != nil && r.URL.RawQuery == "" {
			route.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("%s relay listener: %w", name, err)
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	return nil
}

// relayRecord records relay requests and logs refusals and upstream errors at debug
// level: method and path only, never headers, query or bodies.
func (c Config) relayRecord(name string) func(*http.Request, int, error, time.Duration) {
	log := c.logger()
	return func(r *http.Request, status int, err error, elapsed time.Duration) {
		errorType := ""
		if err != nil {
			errorType = relayErrorType(r.Context(), err)
		}
		c.metrics.Relay(name, status, errorType, elapsed)
		if errorType != "" || status >= 400 {
			log.Debug("relay request refused", "relay", name, "method", r.Method, "path", r.URL.Path, "status", status, "error_type", errorType, "seconds", elapsed.Seconds())
		}
	}
}

func relayErrorType(ctx context.Context, err error) string {
	var body *http.MaxBytesError
	switch {
	case errors.Is(err, relay.ErrPolicy):
		return "policy-denied"
	case errors.Is(err, relay.ErrCredential):
		return "invalid-credential"
	case errors.Is(err, relay.ErrRedirect):
		return "redirect-refused"
	case errors.As(err, &body):
		return "body-limit"
	}
	return metrics.ErrorType(ctx, err)
}

// relayAddress validates the agent-facing relay origin and returns its host:port.
func relayAddress(name string, c RelayConfig) (string, error) {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s relay URL must be an HTTP(S) origin", name)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
