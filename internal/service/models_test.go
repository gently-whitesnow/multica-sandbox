package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	agentidentity "github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type scopedCatalog struct{ ref agentidentity.Ref }

func (s *scopedCatalog) Acquire(context.Context, agentidentity.Ref) (inference.Session, error) {
	return inference.Session{}, fmt.Errorf("discovery must not issue JWT")
}
func (s *scopedCatalog) Catalog(_ context.Context, ref agentidentity.Ref) (inference.Catalog, error) {
	s.ref = ref
	return inference.Catalog{DefaultModel: "demo", Models: map[string]inference.Model{"demo": {Context: 64000, Output: 4096}}}, nil
}
func TestDiscoveryUsesRegisteredWorkspaceAndControllerOrigin(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/heartbeat" {
			fmt.Fprintf(w, `{"pending_model_list":{"id":%q,"agent_id":%q,"server":"https://spoof.invalid","workspace_id":"spoof"}}`, id, id)
			return
		}
		var result struct {
			Models []multica.ModelEntry `json:"models"`
		}
		if json.NewDecoder(r.Body).Decode(&result) != nil || len(result.Models) != 1 || result.Models[0].ID != "managed-inference/demo" || !result.Models[0].Default {
			t.Error("bad scoped discovery result")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := multica.New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	source := &scopedCatalog{}
	api := &agentFleetAPI{Client: client, inference: source, server: "https://trusted.example.invalid"}
	api.scopes.Store(id, "registered-workspace")
	if err = api.Heartbeat(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if source.ref.Server != api.server || source.ref.WorkspaceID != "registered-workspace" || source.ref.AgentID != id {
		t.Fatal("untrusted discovery selectors accepted")
	}
}

func TestFailureReasonsMatchUpstreamDaemon(t *testing.T) {
	for _, tc := range []struct {
		cause           error
		message, reason string
	}{
		{&execution.TimeoutError{After: time.Minute}, "opencode timed out after 1m0s", "timeout"},
		{&execution.IdleError{After: time.Minute}, "agent produced no new messages for 1m0s; force-stopped by idle watchdog", "idle_watchdog"},
		{&execution.AgentFailure{Message: "opencode stream ended without a terminal signal (step still open at EOF)"}, "opencode stream ended without a terminal signal (step still open at EOF)", ""},
		{&execution.RejectedError{Err: errors.New("private detail")}, "Sandbox rejected the task before OpenCode started", "environment_prepare_failed"},
		{fmt.Errorf("wrapped: %w", &execution.AgentFailure{Status: 429}), "Agent inference request failed (HTTP 429)", ""},
		{errors.New("private detail"), "OpenCode execution or identity delivery failed", ""},
	} {
		if message, reason := failure(tc.cause); message != tc.message || reason != tc.reason {
			t.Errorf("%v -> %q %q", tc.cause, message, reason)
		}
	}
}
