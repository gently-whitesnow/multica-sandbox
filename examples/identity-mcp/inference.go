package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const gatewayOrigin = "http://litellm:4000"
const localRelay = "http://127.0.0.1:8092"

var inferenceWorkspaces = [2]string{"10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002"}

// noMCP is the identity issuer for inference-only attempts; no MCP is selected.
type noMCP struct{}

func (noMCP) AcquireForMCP(context.Context, identity.Ref, string) (identity.AccessToken, error) {
	return identity.AccessToken{}, identity.ErrDenied
}

type inferenceFixture struct {
	adapter *opencode.Adapter
	states  *rotationStatus
	master  string
	keys    map[string]string
	owner   string
	next    int
}

// gatewayKey creates a LiteLLM virtual key: model access and budget stay in the gateway.
func (f *inferenceFixture) gatewayKey(ctx context.Context, name string, body map[string]any) error {
	status, data, err := doRequest(ctx, "POST", gatewayOrigin+"/key/generate", body, f.master)
	var out struct {
		Key string `json:"key"`
	}
	if err != nil || status != 200 || json.Unmarshal(data, &out) != nil || out.Key == "" {
		return fmt.Errorf("LiteLLM key generation failed: %d", status)
	}
	f.keys[name] = out.Key
	return nil
}
func (f *inferenceFixture) bind(workspace, key string) error {
	return os.WriteFile(filepath.Join("/tmp/keys", workspace), []byte(f.keys[key]), 0600)
}
func (f *inferenceFixture) task(workspace, model, effort, instructions string) multica.Task {
	f.next++
	task := multica.Task{StartClaimSupported: true, WorkspaceID: workspace, AgentID: "20000000-0000-4000-8000-000000000001", ID: fmt.Sprintf("40000000-0000-4000-8000-%012d", 100+f.next), RuntimeID: "50000000-0000-4000-8000-000000000001", DispatchedAt: time.Now().UTC().Format(time.RFC3339Nano), Agent: &multica.Agent{ID: "20000000-0000-4000-8000-000000000001", Instructions: instructions, MCPConfig: json.RawMessage(`{}`), Model: model, ThinkingLevel: effort}}
	f.states.set(task.ID, "running")
	return task
}
func (f *inferenceFixture) run(ctx context.Context, task multica.Task) (execution.Result, error) {
	run, err := f.adapter.Start(ctx, task)
	if err != nil {
		return execution.Result{}, fmt.Errorf("attempt rejected: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err = run.Wait(waitCtx)
	return run.Result(), errors.Join(err, run.Remove(context.Background()))
}
func attribution(task multica.Task) string {
	return task.WorkspaceID + "/" + task.AgentID + "/" + task.ID
}
func gatewayStatus(err error) int {
	var failure *execution.AgentFailure
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}

// usage waits until LiteLLM's own end-user spend records show each end user with the
// wanted model group and workspace key ({model, key name}). LiteLLM writes them asynchronously.
func (f *inferenceFixture) usage(ctx context.Context, want map[string][2]string) ([][]string, error) {
	deadline := time.Now().Add(90 * time.Second)
	var rows [][]string
	for {
		out, err := exec.CommandContext(ctx, "docker", "exec", os.Getenv("LITELLM_DB"), "psql", "-U", "litellm", "-d", "litellm", "-At", "-F", "|", "-c", `SELECT end_user_id, model_group, api_key FROM "LiteLLM_DailyEndUserSpend"`).Output()
		if err == nil {
			rows = rows[:0]
			found := map[string]bool{}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				row := strings.Split(line, "|")
				if len(row) != 3 {
					continue
				}
				rows = append(rows, row)
				if w, ok := want[row[0]]; ok && w[0] == row[1] && row[2] == hashKey(f.keys[w[1]]) {
					found[row[0]] = true
				}
			}
			if len(found) == len(want) {
				return rows, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("LiteLLM lacks trusted attribution %v: %v", want, rows)
		}
		time.Sleep(2 * time.Second)
	}
}
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// relayChat calls the embedded relay as a sandboxed caller would, with optional spoofing headers.
func relayChat(ctx context.Context, bearer, model string, header map[string]string) (int, []byte, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "messages": []any{map[string]string{"role": "user", "content": "Fixture reply"}}, "user": "spoofed-body", "metadata": map[string]string{"user_id": "spoofed-metadata"}})
	req, err := http.NewRequestWithContext(ctx, "POST", localRelay+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	return resp.StatusCode, data, err
}

// probe inspects a live attempt as its own user: no key anywhere, no gateway route, only the opaque credential.
func (f *inferenceFixture) probe(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+f.owner).Output()
	if err != nil || len(strings.Fields(string(out))) == 0 {
		return "", fmt.Errorf("live attempt not found")
	}
	container := strings.Fields(string(out))[0]
	script := `set -u; cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n'; find /workspace /tmp -type f -size -1M -exec cat {} + 2>/dev/null; wget -T 2 -q -O /dev/null http://litellm:4000/health/liveliness 2>/dev/null && echo GATEWAY-REACHABLE; true`
	data, err := exec.CommandContext(ctx, "docker", "exec", container, "/bin/sh", "-c", script).Output()
	if err != nil {
		return "", fmt.Errorf("attempt probe failed")
	}
	upstream, _ := os.ReadFile("/secrets/upstream")
	for name, key := range f.keys {
		if bytes.Contains(data, []byte(key)) {
			return "", fmt.Errorf("workspace key %s visible inside the attempt", name)
		}
	}
	if bytes.Contains(data, []byte(f.master)) || bytes.Contains(data, upstream) || bytes.Contains(data, []byte("GATEWAY-REACHABLE")) {
		return "", fmt.Errorf("gateway credential or route reachable from the attempt")
	}
	var config struct {
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	raw, err := exec.CommandContext(ctx, "docker", "exec", container, "cat", "/workspace/opencode.json").Output()
	if err != nil || json.Unmarshal(raw, &config) != nil {
		return "", fmt.Errorf("attempt configuration unreadable")
	}
	opaque := config.Provider["managed-inference"].Options["apiKey"]
	if !strings.HasPrefix(opaque, inference.RelayPrefix) {
		return "", fmt.Errorf("attempt lacks the opaque relay credential")
	}
	return opaque, nil
}

func inferenceScenario() error {
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()
	admin, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return err
	}
	if err = os.MkdirAll("/tmp/keys", 0700); err != nil {
		return err
	}
	owner := sha256.Sum256([]byte(os.Getenv("EXECUTION_NETWORK")))
	f := &inferenceFixture{states: &rotationStatus{states: map[string]string{}}, master: "sk-" + string(admin), keys: map[string]string{}, owner: fmt.Sprintf("90000000-0000-4000-8000-%x", owner[:6])}
	for name, body := range map[string]map[string]any{
		"a":    {"models": []string{"fixture", "fixture-new"}, "metadata": map[string]string{"workspace": inferenceWorkspaces[0]}},
		"b":    {"models": []string{"fixture"}, "metadata": map[string]string{"workspace": inferenceWorkspaces[1]}},
		"zero": {"models": []string{"fixture"}, "max_budget": 0},
	} {
		if err = f.gatewayKey(ctx, name, body); err != nil {
			return err
		}
	}
	if err = errors.Join(f.bind(inferenceWorkspaces[0], "a"), f.bind(inferenceWorkspaces[1], "b")); err != nil {
		return err
	}
	catalog := inference.Catalog{DefaultModel: "fixture", Models: map[string]inference.Model{"fixture": {Label: "Fixture", Context: 64000, Output: 4096, Thinking: &inference.Thinking{DefaultLevel: "medium", SupportedLevels: []inference.ThinkingLevel{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}}}}}
	config := inference.Config{Version: 1, AllowHTTP: true, Gateways: []string{gatewayOrigin}}
	for _, ws := range inferenceWorkspaces {
		config.Bindings = append(config.Bindings, inference.Binding{WorkspaceID: ws, Gateway: gatewayOrigin, KeyFile: filepath.Join("/tmp/keys", ws)})
		config.Catalogs = append(config.Catalogs, inference.CatalogBinding{WorkspaceID: ws, Catalog: catalog})
	}
	source, err := inference.New(config, fixtureServer)
	if err != nil {
		return err
	}
	grants := relay.NewGrants(inference.RelayPrefix)
	handler, err := relay.New(grants, relay.Policy{Allow: inference.RelayPath, Limit: 32 << 20, Reserved: inference.ReservedHeaders, Withhold: true, HeaderTimeout: inference.HeaderTimeout})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8092", Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.ListenAndServe() }()
	defer server.Close()
	authority, err := attempt.New(attempt.Config{URL: "http://gateway:8080/attempts", BearerFile: "/secrets/admin", AllowHTTP: true})
	if err != nil {
		return err
	}
	backend := &docker.Projected{Backend: docker.Backend{Image: opencodeImage, Owner: f.owner, Command: []string{"/bin/sh"}}, Network: os.Getenv("EXECUTION_NETWORK"), Peers: []string{os.Getenv("RELAY_PEER")}}
	if err = backend.Reconcile(ctx); err != nil {
		return err
	}
	defer func() { cancel(); _ = backend.Reconcile(context.Background()) }()
	if err = authority.Apply(ctx, attempt.Grant{Controller: f.owner, Action: "recover"}); err != nil {
		return err
	}
	f.adapter = &opencode.Adapter{Server: fixtureServer, Controller: f.owner, Issuer: noMCP{}, Authority: authority, Status: f.states, Workloads: backend, Inference: source, InferenceRelay: grants, InferenceRelayURL: "http://inference-relay:8092"}
	if err = f.workspaceTasks(ctx); err != nil {
		return err
	}
	if err = f.liveAttempt(ctx); err != nil {
		return err
	}
	return f.refusals(ctx)
}

// workspaceTasks runs one native task per workspace concurrently through the relay.
func (f *inferenceFixture) workspaceTasks(ctx context.Context) error {
	tasks := []multica.Task{f.task(inferenceWorkspaces[0], "managed-inference/fixture", "high", "Reply briefly."), f.task(inferenceWorkspaces[1], "", "", "Reply briefly.")}
	before := selectionEvidence(ctx)
	errs := make(chan error, len(tasks))
	for _, task := range tasks {
		go func() {
			result, err := f.run(ctx, task)
			if err == nil && (result.Output != "Fixture task completed" || !strings.HasPrefix(result.SessionID, "ses_")) {
				err = fmt.Errorf("native task did not complete through the relay")
			}
			for _, key := range f.keys {
				if strings.Contains(result.Output, key) {
					err = fmt.Errorf("workspace key in task result")
				}
			}
			errs <- err
		}()
	}
	for range tasks {
		if err := <-errs; err != nil {
			return err
		}
	}
	after := selectionEvidence(ctx)
	if after["selection:fixture|high"][0] <= before["selection:fixture|high"][0] || after["selection:fixture|medium"][0] <= before["selection:fixture|medium"][0] {
		return fmt.Errorf("explicit/default model and reasoning did not reach the provider")
	}
	if _, err := f.usage(ctx, map[string][2]string{attribution(tasks[0]): {"fixture", "a"}, attribution(tasks[1]): {"fixture", "b"}}); err != nil {
		return err
	}
	fmt.Println("PASS concurrent native OpenCode tasks used their workspace keys through the relay; explicit/default reasoning reached the provider; LiteLLM attributes workspace/agent/task")
	return nil
}

// liveAttempt checks a streaming attempt: leakage, spoofing, cross-workspace keys, key change and cancellation.
func (f *inferenceFixture) liveAttempt(ctx context.Context) error {
	opened := selectionEvidence(ctx)["slow-stream"][0]
	slowA := f.task(inferenceWorkspaces[0], "managed-inference/fixture", "", "slow-stream")
	runA, err := f.adapter.Start(ctx, slowA)
	if err != nil {
		return err
	}
	defer runA.Remove(context.Background())
	if err = eventuallyFixture(ctx, func() bool { return selectionEvidence(ctx)["slow-stream"][0] > opened }); err != nil {
		return fmt.Errorf("agent stream did not open")
	}
	opaqueA, err := f.probe(ctx)
	if err != nil {
		return err
	}
	if status, _, _ := relayChat(ctx, opaqueA, "fixture-new", map[string]string{"X-Litellm-Customer-Id": "spoofed-customer", "X-Litellm-End-User-Id": "spoofed-end-user", "X-Litellm-Tags": "spoofed"}); status != 200 {
		return fmt.Errorf("live attempt credential refused: %d", status)
	}
	upstream, _ := os.ReadFile("/secrets/upstream")
	for _, bearer := range []string{f.keys["a"], f.master, string(upstream), inference.RelayPrefix + strings.Repeat("0", 64), "mat_relay_" + strings.Repeat("0", 64), ""} {
		if status, _, _ := relayChat(ctx, bearer, "fixture", nil); status != 401 {
			return fmt.Errorf("upstream, forged or foreign credential accepted: %d", status)
		}
	}
	// A changed workspace key applies to new attempts; the running attempt keeps its grant.
	if err = f.bind(inferenceWorkspaces[0], "zero"); err != nil {
		return err
	}
	budget := f.task(inferenceWorkspaces[0], "managed-inference/fixture", "high", "Reply briefly.")
	if _, err = f.run(ctx, budget); gatewayStatus(err) != 422 {
		return fmt.Errorf("budget refusal not surfaced after key change: %v", err)
	}
	if status, _, _ := relayChat(ctx, opaqueA, "fixture", nil); status != 200 {
		return fmt.Errorf("key change affected a running attempt: %d", status)
	}
	if err = f.bind(inferenceWorkspaces[0], "a"); err != nil {
		return err
	}
	// Revocation ends both the agent's and a caller's in-flight streams.
	stream, err := openStream(ctx, opaqueA)
	if err != nil {
		return err
	}
	closed := selectionEvidence(ctx)["slow-stream"][1]
	ended := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stream); close(ended) }()
	if err = runA.Remove(context.Background()); err != nil {
		return err
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("revocation left a relay stream open")
	}
	stream.Close()
	if err = eventuallyFixture(ctx, func() bool { return selectionEvidence(ctx)["slow-stream"][1] >= closed+2 }); err != nil {
		return fmt.Errorf("gateway did not observe stream cancellation")
	}
	if status, _, _ := relayChat(ctx, opaqueA, "fixture", nil); status != 401 {
		return fmt.Errorf("ended attempt credential accepted: %d", status)
	}
	// Workspace B's credential maps to key B, which lacks fixture-new; key A has it.
	slowB := f.task(inferenceWorkspaces[1], "managed-inference/fixture", "", "slow-stream")
	runB, err := f.adapter.Start(ctx, slowB)
	if err != nil {
		return err
	}
	defer runB.Remove(context.Background())
	opaqueB, err := f.probe(ctx)
	if err != nil {
		return err
	}
	status, body, _ := relayChat(ctx, opaqueB, "fixture-new", nil)
	if status != 403 || bytes.Contains(body, []byte("sk-")) || !bytes.Contains(body, []byte(`"code":"403"`)) {
		return fmt.Errorf("cross-workspace key use or gateway error leak: %d %s", status, body)
	}
	if err = runB.Remove(context.Background()); err != nil {
		return err
	}
	rows, err := f.usage(ctx, map[string][2]string{attribution(slowA): {"fixture-new", "a"}, attribution(budget): {"fixture", "zero"}})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if strings.Contains(row[0], "spoofed") {
			return fmt.Errorf("caller-supplied attribution reached LiteLLM")
		}
	}
	fmt.Println("PASS live attempt held only an opaque credential; spoofed attribution, cross-workspace, upstream, forged and ended credentials denied; key change applied to new attempts; revocation cancelled streams")
	return nil
}

func openStream(ctx context.Context, bearer string) (io.ReadCloser, error) {
	body, _ := json.Marshal(map[string]any{"model": "fixture", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "slow-stream"}}})
	req, _ := http.NewRequestWithContext(ctx, "POST", localRelay+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("relay stream refused")
	}
	if _, err = bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("relay stream not flushed")
	}
	return resp.Body, nil
}

// refusals checks that model selection is forwarded verbatim and gateway refusals are not substituted.
func (f *inferenceFixture) refusals(ctx context.Context) error {
	before := selectionEvidence(ctx)
	fresh := f.task(inferenceWorkspaces[0], "managed-inference/fixture-new", "high", "Reply briefly.")
	if result, err := f.run(ctx, fresh); err != nil || result.Output != "Fixture task completed" {
		return fmt.Errorf("model absent from the catalog was not forwarded: %v", err)
	}
	if selectionEvidence(ctx)["selection:fixture-new|high"][0] <= before["selection:fixture-new|high"][0] {
		return fmt.Errorf("explicit model/effort did not reach the provider")
	}
	ungranted := f.task(inferenceWorkspaces[0], "managed-inference/ungranted", "high", "Reply briefly.")
	if _, err := f.run(ctx, ungranted); gatewayStatus(err) != 403 {
		return fmt.Errorf("gateway model refusal not surfaced: %v", err)
	}
	rows, err := f.usage(ctx, map[string][2]string{attribution(fresh): {"fixture-new", "a"}})
	if err != nil {
		return err
	}
	// LiteLLM records no usage for an auth-stage refusal; any other model would be a substitution.
	for _, row := range rows {
		if row[0] == attribution(ungranted) && row[1] != "ungranted" {
			return fmt.Errorf("refused model substituted by %s", row[1])
		}
	}
	fmt.Println("PASS explicit models absent from the catalog are forwarded; gateway model/budget refusals reach the task as HTTP 403/422 without substitution")
	return nil
}

func eventuallyFixture(ctx context.Context, condition func() bool) error {
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("condition not reached")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func selectionEvidence(ctx context.Context) map[string][2]int {
	secret, err := os.ReadFile("/secrets/admin")
	if err != nil {
		return nil
	}
	status, data, err := doRequest(ctx, "GET", "http://gateway:8080/evidence", nil, string(secret))
	var evidence map[string][2]int
	if err != nil || status != 200 || json.Unmarshal(data, &evidence) != nil {
		return nil
	}
	return evidence
}
