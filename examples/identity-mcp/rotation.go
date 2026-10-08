package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

// opencodeImage is the newest verified official OpenCode image.
var opencodeImage = opencode.Images[len(opencode.Images)-1]

type rotationStatus struct {
	sync.Mutex
	states map[string]string
}

func (s *rotationStatus) Status(_ context.Context, id string) (string, error) {
	s.Lock()
	defer s.Unlock()
	return s.states[id], nil
}
func (s *rotationStatus) set(id, state string) { s.Lock(); defer s.Unlock(); s.states[id] = state }

type recordingIssuer struct {
	*identity.Service
	sync.Mutex
	issued map[string][]identity.AccessToken
}

func (s *recordingIssuer) AcquireForMCP(ctx context.Context, ref identity.Ref, url string) (identity.AccessToken, error) {
	token, err := s.Service.AcquireForMCP(ctx, ref, url)

	if err == nil {
		s.Lock()
		s.issued[ref.WorkspaceID] = append(s.issued[ref.WorkspaceID], token)
		s.Unlock()
	}
	return token, err
}

type fixtureWorkloads struct{ *docker.Projected }

func (w fixtureWorkloads) Start(ctx context.Context, key string) (execution.ProjectedRun, error) {
	run, err := w.Projected.Start(ctx, key)
	if err != nil {
		return nil, err
	}
	return fixtureProjection{run}, nil
}

type fixtureProjection struct{ execution.ProjectedRun }

func (p fixtureProjection) Write(ctx context.Context, path string, data []byte) error {
	if path == "/workspace/opencode.json" {
		var config map[string]any
		if json.Unmarshal(data, &config) != nil {
			return fmt.Errorf("invalid fixture config")
		}
		config["model"] = "fixture/fixture"
		config["enabled_providers"] = []string{"fixture"}
		config["provider"] = map[string]any{"fixture": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "Credential-free deterministic fixture", "options": map[string]any{"baseURL": "http://gateway:8080/v1"}, "models": map[string]any{"fixture": map[string]any{"name": "Fixture", "limit": map[string]int{"context": 64000, "output": 4096}}}}}
		data, _ = json.Marshal(config)
	}
	return p.ProjectedRun.Write(ctx, path, data)
}

func rotation() error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	config := resolverConfig()
	second := config.Bindings[0]
	second.WorkspaceID = "10000000-0000-4000-8000-000000000002"
	config.Bindings = append(config.Bindings, second)
	service, err := identity.New(config, fixtureServer)
	if err != nil {
		return err
	}
	issuer := &recordingIssuer{Service: service, issued: map[string][]identity.AccessToken{}}
	authority, err := attempt.New(attempt.Config{URL: "http://gateway:8080/attempts", BearerFile: "/secrets/admin", AllowHTTP: true})
	if err != nil {
		return err
	}
	owner := sha256.Sum256([]byte(os.Getenv("EXECUTION_NETWORK")))
	controller := fmt.Sprintf("90000000-0000-4000-8000-%x", owner[:6])
	peers := []string{os.Getenv("MCP_PEER")}
	backend := &docker.Projected{Backend: docker.Backend{Image: opencodeImage, Owner: controller, Command: []string{"/bin/sh"}}, Network: os.Getenv("EXECUTION_NETWORK"), Peers: peers}
	if err := backend.Reconcile(ctx); err != nil {
		return err
	}
	defer func() { cancel(); _ = backend.Reconcile(context.Background()) }()
	if err := authority.Apply(ctx, attempt.Grant{Controller: controller, Action: "recover"}); err != nil {
		return err
	}
	states := &rotationStatus{states: map[string]string{}}
	adapter := &opencode.Adapter{Server: fixtureServer, Controller: controller, Issuer: issuer, Authority: authority, Status: states, Workloads: fixtureWorkloads{backend}}
	tasks := []multica.Task{}
	for i, binding := range config.Bindings {
		task := multica.Task{StartClaimSupported: true, WorkspaceID: binding.WorkspaceID, AgentID: binding.AgentID, ID: fmt.Sprintf("40000000-0000-4000-8000-%012d", i+1), RuntimeID: fmt.Sprintf("50000000-0000-4000-8000-%012d", i+1), DispatchedAt: time.Now().UTC().Format(time.RFC3339Nano), Agent: &multica.Agent{ID: binding.AgentID, Instructions: "Read document repeatedly using fixture MCP.", MCPConfig: json.RawMessage(`{"mcpServers":{"fixture":{"url":"http://gateway:8080/mcp"}}}`)}}
		if i == 1 {
			task.Agent.Instructions += " workspace-two"
		}
		tasks = append(tasks, task)
		states.set(task.ID, "running")
	}
	results := make(chan execution.Result, 2)
	done := make(chan error, 2)
	ready := make(chan error, 2)
	for _, task := range tasks {
		go func() {
			run, err := adapter.Start(ctx, task)
			ready <- err
			if err == nil {
				err = run.Wait(ctx)
				err = errorsJoin(err, run.Remove(context.Background()))
				results <- run.Result()
			}
			done <- err
		}()
	}
	for range tasks {
		if err := <-ready; err != nil {
			cancel()
		}
	}
	if err := checkCrossWorkspace(ctx, issuer, tasks); err != nil {
		cancel()
	}
	var executionErr error
	for range tasks {
		if err := <-done; err != nil {
			executionErr = errorsJoin(executionErr, err)
			cancel()
		}
	}
	if executionErr != nil {
		return executionErr
	}
	firstResult, secondResult := <-results, <-results
	if !strings.HasPrefix(firstResult.SessionID, "ses_") || firstResult.SessionID == secondResult.SessionID || !firstResult.Disposable || !secondResult.Disposable || firstResult.Output != "Fixture task completed" || secondResult.Output != "Fixture task completed" {
		return fmt.Errorf("concurrent native attempts did not produce independent disposable sessions/results")
	}
	if err := checkEvidence(ctx, tasks); err != nil {
		return err
	}
	for _, task := range tasks {
		issuer.Lock()
		tokens := append([]identity.AccessToken(nil), issuer.issued[task.WorkspaceID]...)
		issuer.Unlock()
		if len(tokens) < 3 || time.Now().Before(tokens[1].ExpiresAt) {
			return fmt.Errorf("task did not cross two JWT expiries")
		}
		latest := tokens[len(tokens)-1]
		if err := call(ctx, latest.Bearer(), task.WorkspaceID, "document", false); err != nil {
			return fmt.Errorf("ended attempt retained access: %w", err)
		}
	}
	if err := rotationFailures(ctx, adapter, states, tasks); err != nil {
		return err
	}
	if err := rotationLease(ctx, adapter, tasks[0]); err != nil {
		return err
	}
	fmt.Println("PASS two same-process OpenCode tasks crossed multiple real JWT expiries; native OAuth rotation, workspace isolation, revocation and cleanup")
	return nil
}
func errorsJoin(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func checkEvidence(ctx context.Context, tasks []multica.Task) error {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, data, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	if err != nil || status != 200 {
		return fmt.Errorf("evidence unavailable")
	}
	var evidence map[string][2]int
	if json.Unmarshal(data, &evidence) != nil {
		return fmt.Errorf("invalid evidence")
	}
	for _, task := range tasks {
		stats := evidence[task.AttemptKey()]
		if stats[0] < 24 || stats[1] < 3 {
			return fmt.Errorf("insufficient successful MCP calls/rotations: calls=%d versions=%d", stats[0], stats[1])
		}
	}
	return nil
}

func printEvidence() error {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	status, data, err := doRequest(context.Background(), "GET", "http://gateway:8080/evidence", nil, string(secret))
	var evidence map[string][2]int
	if err != nil || status != 200 || json.Unmarshal(data, &evidence) != nil {
		return fmt.Errorf("evidence unavailable")
	}
	return json.NewEncoder(os.Stdout).Encode(evidence)
}

func checkCrossWorkspace(ctx context.Context, issuer *recordingIssuer, tasks []multica.Task) error {
	issuer.Lock()
	initialA, initialB := issuer.issued[tasks[0].WorkspaceID], issuer.issued[tasks[1].WorkspaceID]
	issuer.Unlock()
	if len(initialA) != 0 && len(initialB) != 0 {
		if err := call(ctx, initialA[0].Bearer(), tasks[1].WorkspaceID, "document", false); err != nil {
			return err
		}
		if err := call(ctx, initialB[0].Bearer(), tasks[1].WorkspaceID, "document", true); err != nil {
			return err
		}
	}

	return nil
}
