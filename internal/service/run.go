package service

import (
	"context"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"io"
	"net/http"
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
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
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
	newClient := multica.New
	if c.AllowHTTP {
		newClient = multica.NewAllowHTTP
	}
	api, err := newClient(c.Server, strings.TrimSpace(string(token)))
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
	if err = api.UseOutbox(filepath.Join(stateDir, "terminal-reports")); err != nil {
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
		launch, err := openCodeAdapter(ctx, c, stateDir, api, backend)
		if err != nil {
			return err
		}
		p.Launch = launch.Start
		agentAPI := &agentFleetAPI{Client: api, inference: launch.Inference, server: strings.TrimRight(c.Server, "/")}
		p.API = agentAPI
		return serveFleet(ctx, c, stateDir, agentAPI, &p, out)
	}
	return serveFleet(ctx, c, stateDir, api, &p, out)
}

func openCodeAdapter(ctx context.Context, c Config, stateDir string, api *multica.Client, backend *docker.Backend) (*opencode.Adapter, error) {
	workloads := &docker.Projected{Backend: *backend, Network: c.OpenCode.Network, Peers: c.OpenCode.Peers, Helper: c.OpenCode.Helper}
	if s := c.OpenCode.Sessions; s != nil {
		ttl, err := time.ParseDuration(s.TTL)
		if c.OpenCode.Helper == "" || err != nil || ttl <= 0 || s.Max < 1 || s.Max > 1024 {
			return nil, fmt.Errorf("sessions require the helper, a positive ttl and max in [1,1024]")
		}
		workloads.Sessions = &docker.Sessions{Dir: filepath.Join(stateDir, "sessions"), TTL: ttl, Max: s.Max}
		if err := os.MkdirAll(workloads.Sessions.Dir, 0700); err != nil {
			return nil, err
		}
		// Reconciliation kept session volumes; expired ones go before any attempt starts.
		if err := workloads.Sessions.Collect(ctx, c.Daemon); err != nil {
			return nil, err
		}
	}
	if (c.OpenCode.MulticaRelay == nil) != (c.OpenCode.MulticaCLI == "") {
		return nil, fmt.Errorf("multica_relay and the digest-pinned multica_cli artifact require each other")
	}
	git := c.OpenCode.GitRelay != nil
	if c.OpenCode.ForgeRelay != nil && !git {
		return nil, fmt.Errorf("forge_relay requires git_relay")
	}
	if git != (c.OpenCode.GitFile != "") || (git && c.OpenCode.Helper == "") || (c.OpenCode.Helper != "" && c.OpenCode.MulticaRelay == nil) {
		return nil, fmt.Errorf("git_relay and git_file require each other and the helper; the helper requires multica_relay")
	}
	var err error
	if c.OpenCode.Helper != "" {
		// The forwarder reaches the checkout endpoint on the Multica relay listener.
		if workloads.Forward, err = relayAddress("Multica", *c.OpenCode.MulticaRelay); err != nil {
			return nil, err
		}
	}
	if workloads.Bundles, err = bundles(c.OpenCode, c.Tools); err != nil {
		return nil, err
	}
	// The digest-pinned image cannot change, so one startup inspection covers every attempt.
	report, err := workloads.Output(ctx, opencode.ImageProbe)
	if err != nil {
		return nil, fmt.Errorf("inspect agent image: %w", err)
	}
	if err = opencode.CheckImage(report, c.OpenCode.MulticaRelay != nil, git); err != nil {
		return nil, err
	}
	if err = checkTools(ctx, workloads, c.Tools); err != nil {
		return nil, err
	}
	config, err := agentidentity.ReadConfig(c.OpenCode.IdentityFile)
	if err != nil {
		return nil, agentidentity.ErrDenied
	}
	issuer, err := agentidentity.New(config, strings.TrimRight(c.Server, "/"))
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
	if err = workloads.ValidateNetwork(ctx); err != nil {
		return nil, err
	}
	adapter := &opencode.Adapter{Server: strings.TrimRight(c.Server, "/"), Controller: c.Daemon, Issuer: issuer, Authority: authority, Status: api, Reporter: api, Workloads: workloads, Command: c.Command}
	if c.OpenCode.MulticaRelay != nil {
		grants := relay.NewGrants(multica.RelayPrefix)
		var routes map[string]http.Handler
		if git {
			hosts, err := repo.ReadConfig(c.OpenCode.GitFile)
			if err != nil {
				return nil, err
			}
			adapter.GitRelay = repo.NewRelay(c.OpenCode.GitRelay.URL, hosts)
			if err = serveRelay(ctx, "Git", adapter.GitRelay, *c.OpenCode.GitRelay, relay.Policy{Allow: repo.GitPath, Limit: repo.PushLimit, HeaderTimeout: repo.PushTimeout}, nil, nil); err != nil {
				return nil, err
			}
			if err = serveForge(ctx, c.OpenCode.ForgeRelay, adapter.GitRelay, hosts, workloads); err != nil {
				return nil, err
			}
			// The upstream CLI authenticates checkout with its Multica relay credential.
			adapter.Checkout, adapter.Settings = &repo.Checkout{Auth: grants, Hosts: hosts}, api
			routes = map[string]http.Handler{"/repo/checkout": adapter.Checkout}
		}
		if err = serveRelay(ctx, "Multica", grants, *c.OpenCode.MulticaRelay, relay.Policy{Allow: multica.RelayPath}, routes, nil); err != nil {
			return nil, err
		}
		adapter.Relay = grants
		adapter.RelayURL = strings.TrimRight(c.OpenCode.MulticaRelay.URL, "/")
	}
	source, grants, err := openCodeInference(ctx, c)
	if err != nil {
		return nil, err
	}
	if source != nil {
		adapter.Inference, adapter.InferenceRelay = source, grants
		adapter.InferenceRelayURL = strings.TrimRight(c.OpenCode.InferenceRelay.URL, "/")
	}
	return adapter, nil
}

// serveForge serves gh calls for the API names of Git hosts on TLS (ADR 0015); attempts
// resolve those names to loopback, where the helper forwards to the forge relay.
func serveForge(ctx context.Context, c *RelayConfig, git *repo.Relay, hosts *repo.Hosts, workloads *docker.Projected) error {
	if c == nil {
		return nil
	}
	if !strings.HasPrefix(c.URL, "https://") || len(hosts.APINames()) == 0 {
		return fmt.Errorf("forge_relay requires an https url and a Git host with an api")
	}
	forge, err := repo.NewForge(git)
	if err != nil {
		return err
	}
	if workloads.ForgeForward, err = relayAddress("forge", *c); err != nil {
		return err
	}
	workloads.ForgeHosts = hosts.APINames()
	return serveRelay(ctx, "forge", forge, *c, relay.Policy{Allow: repo.ForgePath}, nil, forge.TLSConfig())
}

func (a *agentFleetAPI) Message(ctx context.Context, id string) error { return a.AgentMessage(ctx, id) }
func (a *agentFleetAPI) Complete(ctx context.Context, id string, result execution.Result) error {
	return a.AgentComplete(ctx, id, result)
}
