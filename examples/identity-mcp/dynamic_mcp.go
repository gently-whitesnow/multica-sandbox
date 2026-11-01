package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

type dynamicIssuer struct{ calls atomic.Int32 }

func (*dynamicIssuer) ManagedMCP(u string) bool { return u == "http://managed.fixture.invalid/mcp" }
func (*dynamicIssuer) AnonymousMCP(string) bool { return false }
func (i *dynamicIssuer) AcquireForMCP(context.Context, identity.Ref, identity.Env, string) (identity.AccessToken, error) {
	i.calls.Add(1)
	return identity.AccessToken{}, errors.New("public MCP requested managed identity")
}

type dynamicMCPFixture struct {
	sync.Mutex
	round   int
	grant   string
	problem string
	tools   map[string]int
	first   chan struct{}
	release chan struct{}
}

func dynamicMCP() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := &dynamicMCPFixture{tools: map[string]int{}, first: make(chan struct{}, 1), release: make(chan struct{})}
	mcpRelay := opencode.NewMCPRelay()
	handler, err := relay.New(mcpRelay, relay.Policy{Allow: opencode.MCPRelayPath, Limit: 1 << 20, Reserved: opencode.MCPReservedHeaders})
	if err != nil {
		return err
	}
	proxy, _ := url.Parse("http://127.0.0.1:8096")
	servers, err := f.serve(handler)
	if err != nil {
		return err
	}
	defer func() {
		for _, server := range servers {
			_ = server.Close()
		}
	}()
	sum := sha256.Sum256([]byte(os.Getenv("EXECUTION_NETWORK")))
	owner := fmt.Sprintf("90000000-0000-4000-8000-%x", sum[:6])
	if err := os.MkdirAll("/tmp/dynamic-sessions", 0700); err != nil {
		return err
	}
	backend := &docker.Projected{Backend: docker.Backend{Image: opencodeImage, Owner: owner, Command: []string{"/bin/sh"}}, Network: os.Getenv("EXECUTION_NETWORK"), Peers: []string{os.Getenv("RELAY_PEER")}, Helper: os.Getenv("MCP_HELPER_IMAGE"), Forward: "mcp-relay:8091", Sessions: &docker.Sessions{Dir: "/tmp/dynamic-sessions", TTL: time.Hour, Max: 8}}
	if err := backend.Reconcile(ctx); err != nil {
		return err
	}
	defer func() {
		cancel()
		_ = backend.Reconcile(context.Background())
		backend.Sessions.TTL = time.Nanosecond
		_ = backend.Sessions.Collect(context.Background(), owner)
	}()
	states := &rotationStatus{states: map[string]string{}}
	issuer := &dynamicIssuer{}
	adapter := &opencode.Adapter{Server: fixtureServer, Controller: owner, Issuer: issuer, Status: states, Workloads: fixtureWorkloads{backend}, MCPRelay: mcpRelay, MCPRelayURL: "http://mcp-relay:8095", MCPPublicProxy: proxy, Relay: relay.NewGrants(multica.RelayPrefix), RelayURL: "http://mcp-relay:8091", EnvAllow: []string{"PUBLIC_TOKEN"}}
	var prior execution.Result
	for round := 1; round <= 3; round++ {
		prior, err = f.run(ctx, adapter, states, round, prior)
		if err != nil {
			return fmt.Errorf("dynamic MCP round %d: %w", round, err)
		}
	}
	if issuer.calls.Load() != 0 {
		return errors.New("public MCP requested managed JWT")
	}
	f.Lock()
	defer f.Unlock()
	if f.problem != "" || len(f.tools) != 6 {
		return fmt.Errorf("dynamic MCP evidence failed: %s, calls=%d", f.problem, len(f.tools))
	}
	fmt.Println("PASS native OpenCode: anonymous/static/ENV HTTP MCP via proxy; exact queries; no JWT or secret projection; changed connections resume; empty selection clears tools; old grants revoked")
	return nil
}

func (f *dynamicMCPFixture) serve(relayed http.Handler) ([]*http.Server, error) {
	servers := []*http.Server{}
	for address, handler := range map[string]http.Handler{":8080": http.HandlerFunc(f.model), ":8095": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Lock()
		f.grant = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.Unlock()
		relayed.ServeHTTP(w, r)
	}), "127.0.0.1:8096": f.proxy()} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, server := range servers {
				_ = server.Close()
			}
			return nil, err
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, server)
		go func() { _ = server.Serve(listener) }()
	}
	return servers, nil
}

func dynamicTask(round int, prior execution.Result) multica.Task {
	connections := map[string]any{}
	for _, name := range []string{"anonymous", "static", "environment"} {
		entry := map[string]any{"url": fmt.Sprintf("http://public-mcp.fixture.invalid/%s?route=round%d%%2Ffixed&token=fixture-query-token-round-%d", name, round, round)}
		if name == "static" {
			entry["headers"] = map[string]string{"Authorization": fmt.Sprintf("Bearer fixture-static-token-round-%d", round), "Cookie": fmt.Sprintf("selected=fixture-cookie-token-round-%d", round)}
		}
		if name == "environment" {
			entry["headers"] = map[string]string{"Authorization": "Bearer {env:PUBLIC_TOKEN}"}
		}
		connections[name] = entry
	}
	if round == 3 {
		connections = map[string]any{}
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": connections})
	task := multica.Task{StartClaimSupported: true, WorkspaceID: "10000000-0000-4000-8000-000000000001", AgentID: "20000000-0000-4000-8000-000000000001", ID: fmt.Sprintf("40000000-0000-4000-8000-%012d", round), RuntimeID: "50000000-0000-4000-8000-000000000001", DispatchedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatSessionID: "60000000-0000-4000-8000-000000000001", ChatMessage: "Run the isolated dynamic MCP probes.", PriorSessionID: prior.SessionID, PriorWorkDir: prior.WorkDir, Agent: &multica.Agent{ID: "20000000-0000-4000-8000-000000000001", MCPConfig: config}}
	_ = json.Unmarshal([]byte(`"mat_0123456789abcdef0123456789abcdef01234567"`), &task.AuthToken)
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{"PUBLIC_TOKEN":"fixture-env-token-round-%d"}`, round)), &task.Agent.CustomEnv)
	return task
}
