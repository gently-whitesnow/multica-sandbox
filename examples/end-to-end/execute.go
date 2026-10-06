package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func docker(ctx context.Context, args ...string) error {
	if exec.CommandContext(ctx, "docker", args...).Run() != nil {
		return fmt.Errorf("fixture Docker operation failed")
	}
	return nil
}
func reap(ctx context.Context) error {
	if os.Getenv("COMPOSE_PROJECT_NAME") != "sandbox-e2e" {
		return fmt.Errorf("disposable project sandbox-e2e required")
	}
	data, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=multica-sandbox.example=sandbox-e2e").Output()
	if err != nil {
		return fmt.Errorf("fixture discovery failed")
	}
	for _, id := range strings.Fields(string(data)) {
		if err := docker(ctx, "rm", "-f", id); err != nil {
			return err
		}
	}
	return nil
}

type model struct {
	ID string `json:"id"`
}

func availableModels(ctx context.Context, c configuration) ([]model, error) {
	var response struct {
		Data []model `json:"data"`
	}
	status, err := exchange(ctx, "GET", "http://cli-proxy:8317/v1/models", c.Proxy, nil, &response)
	if err != nil || status != 200 || len(response.Data) == 0 {
		return nil, fmt.Errorf("CLIProxyAPI needs subscription login before executing a task")
	}
	return response.Data, nil
}
func listModels(ctx context.Context) error {
	c, err := load()
	if err != nil {
		return err
	}
	models, err := availableModels(ctx, c)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(models)
}
func preflight(ctx context.Context, c configuration) error {
	models, err := availableModels(ctx, c)
	if err != nil {
		return err
	}
	selected := os.Getenv("UPSTREAM_MODEL")
	for _, m := range models {
		if m.ID == selected {
			return nil
		}
	}
	return fmt.Errorf("configured model unavailable; inspect ./scripts/e2e.sh models and set UPSTREAM_MODEL")
}
func execute(ctx context.Context) error {
	c, err := load()
	if err != nil {
		return err
	}
	if err = reap(ctx); err != nil {
		return err
	}
	if err = preflight(ctx, c); err != nil {
		return err
	}
	var rt runtime
	data, err := os.ReadFile("/config/runtime.json")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &rt); err != nil {
		return err
	}
	if err = control(ctx, c, "POST", "/api/daemon/runtimes/"+rt.ID+"/recover-orphans", map[string]any{}, nil); err != nil {
		return err
	}
	if err = control(ctx, c, "POST", "/api/daemon/heartbeat", map[string]string{"runtime_id": rt.ID}, nil); err != nil {
		return err
	}
	var claimed struct {
		Task *task `json:"task"`
	}
	if err = control(ctx, c, "POST", "/api/daemon/runtimes/"+rt.ID+"/tasks/claim", map[string]any{}, &claimed); err != nil {
		return err
	}
	if claimed.Task == nil {
		return fmt.Errorf("no queued fixture task; reset stand to rerun")
	}
	t := *claimed.Task
	if t.ID != taskID || t.AgentID != agentID || t.Workspace != workspace || t.IssueID != issueID || !t.StartSupported {
		return fmt.Errorf("unsupported fixture claim")
	}
	if _, err = time.Parse(time.RFC3339Nano, t.DispatchedAt); err != nil {
		return fmt.Errorf("invalid dispatch timestamp")
	}
	inference, err := identity(ctx, c, "inference")
	if err != nil {
		return err
	}
	mcp, err := identity(ctx, c, "mcp")
	if err != nil {
		return err
	}
	if err = control(ctx, c, "POST", "/api/daemon/tasks/"+t.ID+"/start", map[string]any{"runtime_id": rt.ID, "dispatched_at": t.DispatchedAt, "capabilities": []string{}}, nil); err != nil {
		return err
	}
	fmt.Println("START real Multica task; short-lived identity issued outside sandbox")
	var outcome result
	runErr := checkActive(ctx, inference, mcp)
	if runErr == nil {
		outcome, runErr = sandbox(ctx, input{t, inference, mcp}, c, rt)
	}
	var state struct {
		Status string `json:"status"`
	}
	if err = control(ctx, c, "GET", "/api/daemon/tasks/"+t.ID+"/status", nil, &state); err != nil {
		return err
	}
	if state.Status == "cancelled" || state.Status == "cancelling" {
		return control(ctx, c, "POST", "/api/daemon/tasks/"+t.ID+"/cancel-ack", map[string]any{}, nil)
	}
	action := "complete"
	body := map[string]string{"output": "OpenCode read real task context through MCP and produced the verified reference word."}
	if runErr != nil {
		action = "fail"
		body = map[string]string{"error": "E2E fixture execution failed"}
	}
	if err = control(ctx, c, "POST", "/api/daemon/tasks/"+t.ID+"/"+action, body, nil); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	if err = checkEnded(ctx, inference, mcp); err != nil {
		return err
	}
	fmt.Printf("PASS real Multica task completed; %d agent tool events; result verified\n", outcome.Events)
	return nil
}
func sandbox(ctx context.Context, request input, c configuration, rt runtime) (result, error) {
	var out result
	name := "sandbox-e2e-attempt"
	args := []string{"create", "-i", "--name", name, "--label", "multica-sandbox.example=sandbox-e2e", "--network", "sandbox-e2e_sandbox", "--user", "65532:65532", "--workdir", "/workspace", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true", "--memory", "1g", "--memory-swap", "1g", "--cpus", "1", "--pids-limit", "128", "--log-driver", "none", "--tmpfs", "/workspace:rw,nosuid,nodev,size=268435456,mode=1777", "--tmpfs", "/home/agent:rw,nosuid,nodev,size=268435456,mode=1777", "--tmpfs", "/tmp:rw,nosuid,nodev,size=67108864,mode=1777", "sandbox-e2e-agent:local"}
	if err := docker(ctx, args...); err != nil {
		return out, err
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = docker(clean, "rm", "-f", name)
	}()
	runCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	data, _ := json.Marshal(request)
	command := exec.CommandContext(runCtx, "docker", "start", "-ai", name)
	command.Stdin = bytes.NewReader(data)
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	done := make(chan error, 1)
	go func() { done <- command.Run() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return out, fmt.Errorf("sandbox execution failed; raw output withheld")
			}
			if json.Unmarshal(output.Bytes(), &out) != nil || out.Type != "e2e_result" || out.Word != c.Reference || out.Events < 2 {
				return out, fmt.Errorf("agent did not satisfy fixture acceptance")
			}
			return out, nil
		case <-ticker.C:
			var state struct {
				Status string `json:"status"`
			}
			if err := control(ctx, c, "GET", "/api/daemon/tasks/"+taskID+"/status", nil, &state); err != nil {
				cancel()
				<-done
				return out, err
			}
			if state.Status == "cancelled" || state.Status == "cancelling" {
				cancel()
				<-done
				return out, fmt.Errorf("task cancelled")
			}
			if err := control(ctx, c, "POST", "/api/daemon/heartbeat", map[string]string{"runtime_id": rt.ID}, nil); err != nil {
				cancel()
				<-done
				return out, err
			}
		}
	}
}
