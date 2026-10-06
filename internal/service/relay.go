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

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const relayBodyLimit = 32 << 20

// serveRelay forwards attempt requests to their grant's trusted upstream until ctx ends.
func serveRelay(ctx context.Context, name, prefix string, c RelayConfig, p relay.Policy) (*relay.Grants, error) {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s relay URL must be an HTTP(S) origin", name)
	}
	if _, _, err = net.SplitHostPort(c.Listen); err != nil {
		return nil, fmt.Errorf("%s relay listen address required", name)
	}
	grants := relay.NewGrants(prefix)
	p.Limit = relayBodyLimit
	handler, err := relay.New(grants, p)
	if err != nil {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("%s relay listener: %w", name, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.Serve(listener) }()
	context.AfterFunc(ctx, func() { _ = srv.Close() })
	return grants, nil
}
