package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const relayBodyLimit = 32 << 20

// serveRelay forwards agent CLI calls to the configured Multica origin until ctx ends.
func serveRelay(ctx context.Context, server string, c RelayConfig) (*relay.Grants, error) {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("Multica relay URL must be an HTTP(S) origin")
	}
	if _, _, err = net.SplitHostPort(c.Listen); err != nil {
		return nil, fmt.Errorf("Multica relay listen address required")
	}
	grants := relay.NewGrants()
	handler, err := relay.New(server, grants, multica.RelayPath, relayBodyLimit)
	if err != nil {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("Multica relay listener: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	return grants, nil
}
