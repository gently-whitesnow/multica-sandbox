package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const gatewayOrigin = "https://gateway.example.invalid"

// workspaceInference resolves a key per workspace; key changes model operator rotation.
type workspaceInference struct {
	keys   map[string]string
	scopes []inference.Scope
	denied bool
}

func (s *workspaceInference) Acquire(_ context.Context, scope inference.Scope) (inference.Target, error) {
	s.scopes = append(s.scopes, scope)
	key, ok := s.keys[scope.WorkspaceID]
	if s.denied || !ok {
		return inference.Target{}, inference.ErrDenied
	}
	var target inference.Target
	data, _ := json.Marshal(map[string]string{"gateway": gatewayOrigin, "key": key})
	if json.Unmarshal(data, &target) != nil {
		return inference.Target{}, inference.ErrDenied
	}
	return target, nil
}
func (s *workspaceInference) Catalog(context.Context, inference.Scope) (inference.Catalog, error) {
	return inference.Catalog{DefaultModel: "demo", Models: map[string]inference.Model{"demo": {Context: 64000, Output: 4096}}}, nil
}

func inferenceLease(t *testing.T, grants *relay.Grants, bearer string) (relay.Lease, bool) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	lease, ok := grants.Authorize(req)
	if ok {
		lease.Release()
	}
	return lease, ok
}

func inferenceAdapter(source Inference, grants Grants, workload *stubWorkload) *Adapter {
	return &Adapter{Server: "https://multica.example.invalid", Issuer: &stubIssuer{}, Inference: source, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, InferenceRelay: grants, InferenceRelayURL: "http://inference-relay:8092"}
}

func TestInferenceDenialBeforeWorkload(t *testing.T) {
	workload := &stubWorkload{}
	grants := relay.NewGrants(inference.RelayPrefix)
	a := inferenceAdapter(&workspaceInference{denied: true}, grants, workload)
	_, err := a.Start(context.Background(), safeTask())
	var rejected *execution.RejectedError
	if !errors.As(err, &rejected) || workload.writes != 0 || workload.runs != 0 {
		t.Fatal("binding denial admitted workload", err)
	}
}

func TestInferenceAttemptDeliversOnlyOpaqueCredential(t *testing.T) {
	const keyA, keyB, rotated = "sk-workspace-a-secret", "sk-workspace-b-secret", "sk-workspace-a-rotated"
	grants := relay.NewGrants(inference.RelayPrefix)
	task := safeTask()
	other := safeTask()
	other.ID = "40000000-0000-4000-8000-000000000009"
	other.WorkspaceID = "10000000-0000-4000-8000-000000000002"
	source := &workspaceInference{keys: map[string]string{task.WorkspaceID: keyA, other.WorkspaceID: keyB}}
	workload := &stubWorkload{files: map[string][]byte{}}
	run, err := inferenceAdapter(source, grants, workload).Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Plugin   []string `json:"plugin"`
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(workload.files["/workspace/opencode.json"], &config) != nil || config.Plugin != nil {
		t.Fatal("invalid native configuration")
	}
	options := config.Provider[inferenceProvider].Options
	opaque := options["apiKey"]
	if options["baseURL"] != "http://inference-relay:8092/v1" || !strings.HasPrefix(opaque, inference.RelayPrefix) {
		t.Fatalf("provider not pointed at the relay: %v", options)
	}
	for path, data := range workload.files {
		if strings.Contains(string(data), keyA) || strings.Contains(string(data), gatewayOrigin) {
			t.Fatalf("workspace key or gateway projected into %s", path)
		}
	}
	lease, ok := inferenceLease(t, grants, opaque)
	if !ok || lease.Credential != keyA || lease.Origin.String() != gatewayOrigin || lease.Header.Get(inference.AttributionHeader) != task.WorkspaceID+"/"+task.AgentID+"/"+task.ID {
		t.Fatal("grant does not carry the workspace key, gateway and claim attribution")
	}
	if source.scopes[0] != (inference.Scope{Server: "https://multica.example.invalid", WorkspaceID: task.WorkspaceID}) {
		t.Fatal("binding not selected from trusted controller and claim")
	}
	// Another workspace's attempt gets its own key; a changed key applies to new attempts only.
	source.keys[task.WorkspaceID] = rotated
	otherWorkload := &stubWorkload{files: map[string][]byte{}}
	otherRun, err := inferenceAdapter(source, grants, otherWorkload).Start(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(otherWorkload.files["/workspace/opencode.json"], &config)
	if lease, _ := inferenceLease(t, grants, config.Provider[inferenceProvider].Options["apiKey"]); lease.Credential != keyB {
		t.Fatal("workspace key crossed workspaces")
	}
	if lease, _ := inferenceLease(t, grants, opaque); lease.Credential != keyA {
		t.Fatal("running attempt changed key")
	}
	if err = run.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := inferenceLease(t, grants, opaque); ok {
		t.Fatal("ended attempt credential still authorized")
	}
	next := safeTask()
	next.ID = "40000000-0000-4000-8000-000000000010"
	nextWorkload := &stubWorkload{files: map[string][]byte{}}
	nextRun, err := inferenceAdapter(source, grants, nextWorkload).Start(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(nextWorkload.files["/workspace/opencode.json"], &config)
	if lease, _ := inferenceLease(t, grants, config.Provider[inferenceProvider].Options["apiKey"]); lease.Credential != rotated {
		t.Fatal("new attempt did not use the changed key")
	}
	if err = errors.Join(otherRun.Remove(context.Background()), nextRun.Remove(context.Background())); err != nil {
		t.Fatal(err)
	}
}

func TestInferenceGrantFailureRejectsAttempt(t *testing.T) {
	grants := relay.NewGrants(inference.RelayPrefix)
	task := safeTask()
	// An attempt can hold only one grant; a duplicate claim fails before any workload.
	if _, err := grants.Issue(task.AttemptKey(), relay.Upstream{Origin: gatewayOrigin, Credential: "sk-held"}); err != nil {
		t.Fatal(err)
	}
	workload := &stubWorkload{}
	_, err := inferenceAdapter(&workspaceInference{keys: map[string]string{task.WorkspaceID: "sk-key"}}, grants, workload).Start(context.Background(), task)
	var rejected *execution.RejectedError
	if !errors.As(err, &rejected) || workload.writes != 0 {
		t.Fatal("grant failure admitted workload", err)
	}
}
