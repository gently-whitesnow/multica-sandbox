package service

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const relayBodyLimit = 32 << 20

// serveRelay forwards attempt requests to their grant's trusted upstream until ctx ends.
// Routes serve exact paths on the same listener instead of the relay. The body limit
// defaults to relayBodyLimit. With tlsConfig the listener terminates TLS.
func serveRelay(ctx context.Context, name string, auth relay.Authorizer, c RelayConfig, p relay.Policy, routes map[string]http.Handler, tlsConfig *tls.Config) error {
	if _, err := relayAddress(name, c); err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("%s relay listen address required", name)
	}
	if p.Limit == 0 {
		p.Limit = relayBodyLimit
	}
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
