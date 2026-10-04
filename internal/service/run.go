package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/instance"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func Run(ctx context.Context, c Config, stateDir, tokenPath string, out io.Writer) error {
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.Concurrency < 1 || c.Concurrency > 32 {
		return fmt.Errorf("concurrency must be in [1,32]")
	}
	if !filepath.IsAbs(stateDir) {
		return fmt.Errorf("absolute state directory required")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	release, err := instance.Lock(filepath.Join(stateDir, "controller.lock"))
	if err != nil {
		return err
	}
	defer release()
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("read controller token file: %w", err)
	}
	api, err := multica.New(c.Server, strings.TrimSpace(string(token)))
	if err != nil {
		return err
	}
	engine, err := docker.EngineID(ctx)
	if err != nil {
		return err
	}
	scope := c.Workspace
	if c.Workspaces == "all-accessible" {
		scope = "*"
	}
	if err = bindState(stateDir, identity{strings.TrimRight(c.Server, "/"), scope, c.Daemon, engine}); err != nil {
		return err
	}
	backend := &docker.Backend{Image: c.Image, Owner: c.Daemon, Command: c.Command}
	duration, err := time.ParseDuration(c.Timeout)
	if err != nil || duration <= 0 {
		return fmt.Errorf("positive timeout required")
	}
	p := controller.Probe{API: api, Backend: backend, Duration: duration, Interval: time.Second,
		Observe: func(event, id string) { fmt.Fprintf(out, "%s task=%s\n", event, id) }}
	if c.Workspaces == "all-accessible" {
		if err = backend.Reconcile(ctx); err != nil {
			return err
		}
		if err = backend.Validate(ctx); err != nil {
			return err
		}
		return serveFleet(ctx, c, stateDir, api, &p, out)
	}
	rt, recovered, err := p.Connect(ctx, c.Workspace, c.Daemon)
	if err != nil {
		return err
	}
	// Validate after reconciliation so a missing image cannot strand old executions.
	if err = backend.Validate(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "ready runtime=%s orphaned=%d retried=%d\n", rt.ID, recovered.Orphaned, recovered.Retried)
	return p.Serve(ctx, rt.ID)
}
