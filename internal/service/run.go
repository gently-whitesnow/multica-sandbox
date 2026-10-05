package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	agentidentity "github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/instance"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
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
	if err = bindState(stateDir, identity{strings.TrimRight(c.Server, "/"), c.Daemon, engine}); err != nil {
		return err
	}
	command := c.Command
	if c.OpenCode != nil {
		c.Command = []string{"/bin/sh"}
	}
	backend := &docker.Backend{Image: c.Image, Owner: c.Daemon, Command: c.Command}
	duration, err := time.ParseDuration(c.Timeout)
	if err != nil || duration <= 0 {
		return fmt.Errorf("positive timeout required")
	}
	p := controller.Probe{API: api, Backend: backend, Duration: duration, Interval: time.Second,
		Observe: func(event, id string) { fmt.Fprintf(out, "%s task=%s\n", event, id) }}
	// Reconcile before validation so a missing image cannot strand old executions.
	if err = backend.Reconcile(ctx); err != nil {
		return err
	}
	if err = backend.Validate(ctx); err != nil {
		return err
	}
	if c.OpenCode != nil {
		c.Command = command
		launch, err := openCodeAdapter(ctx, c, api, backend)
		if err != nil {
			return err
		}
		p.Launch = launch.Start
		p.API = agentFleetAPI{api}
		return serveFleet(ctx, c, stateDir, agentFleetAPI{api}, &p, out)
	}
	return serveFleet(ctx, c, stateDir, api, &p, out)
}

type agentFleetAPI struct{ *multica.Client }

func (a agentFleetAPI) Register(ctx context.Context, ws, daemon string) (multica.Runtime, error) {
	return a.RegisterProvider(ctx, ws, daemon, "opencode")
}

func openCodeAdapter(ctx context.Context, c Config, api *multica.Client, backend *docker.Backend) (*opencode.Adapter, error) {
	config, err := agentidentity.ReadConfig(c.OpenCode.IdentityFile)
	if err != nil || config.Server != strings.TrimRight(c.Server, "/") {
		return nil, agentidentity.ErrDenied
	}
	issuer, err := agentidentity.New(config)
	if err != nil {
		return nil, err
	}
	authority, err := attempt.New(c.OpenCode.Authority)
	if err != nil {
		return nil, err
	}
	if err = authority.Apply(ctx, attempt.Grant{Controller: c.Daemon, Action: "recover"}); err != nil {
		return nil, err
	}
	workloads := &docker.Projected{Backend: *backend, Network: c.OpenCode.Network, Peers: c.OpenCode.Peers}
	if err = workloads.ValidateNetwork(ctx); err != nil {
		return nil, err
	}
	return &opencode.Adapter{Server: config.Server, Controller: c.Daemon, Issuer: issuer, Authority: authority, Status: api, Workloads: workloads, Command: c.Command}, nil
}

func (a agentFleetAPI) Message(ctx context.Context, id string) error { return a.AgentMessage(ctx, id) }
func (a agentFleetAPI) Complete(ctx context.Context, id string) error {
	return a.AgentComplete(ctx, id)
}

func (a agentFleetAPI) Fail(ctx context.Context, id string) error { return a.AgentFail(ctx, id) }
