package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func (f *dynamicMCPFixture) run(ctx context.Context, adapter *opencode.Adapter, states *rotationStatus, round int, prior execution.Result) (execution.Result, error) {
	f.Lock()
	f.round = round
	f.Unlock()
	task := dynamicTask(round, prior)
	states.set(task.ID, "running")
	run, err := adapter.Start(ctx, task)
	if err != nil {
		return execution.Result{}, err
	}
	defer run.Remove(context.Background())
	done := make(chan error, 1)
	go func() { done <- run.Wait(ctx) }()
	if round == 1 {
		select {
		case <-f.first:
		case err := <-done:
			f.Lock()
			problem := f.problem
			f.Unlock()
			return execution.Result{}, fmt.Errorf("native attempt ended before MCP probe: %v (%s)", err, problem)
		case <-ctx.Done():
			return execution.Result{}, ctx.Err()
		}
		err = dynamicNoSecrets(ctx, adapter.Controller)
		if err == nil {
			f.Lock()
			grant := f.grant
			f.Unlock()
			err = dynamicRelayStatus(ctx, grant, "/anonymous?override=must-not-forward", 405)
			if err == nil {
				err = dynamicRelayStatus(ctx, grant, "/not-selected", 403)
			}
		}
		close(f.release)
		if err != nil {
			return execution.Result{}, err
		}
	}
	if err := <-done; err != nil {
		return execution.Result{}, err
	}
	result := run.Result()
	if result.Output != "Dynamic MCP fixture completed" || result.SessionID == "" || result.WorkDir == "" {
		return result, fmt.Errorf("native task/session result missing")
	}
	if round > 1 && (result.SessionID != prior.SessionID || result.WorkDir != prior.WorkDir) {
		return result, fmt.Errorf("config update did not resume the retained session")
	}
	f.Lock()
	grant := f.grant
	problem := f.problem
	f.Unlock()
	if problem != "" {
		return result, errors.New(problem)
	}
	if err := run.Remove(context.Background()); err != nil {
		return result, err
	}
	states.set(task.ID, "completed")
	if grant != "" {
		err = dynamicRelayStatus(ctx, grant, "/anonymous", 401)
	}
	return result, err
}

func dynamicRelayStatus(ctx context.Context, grant, path string, want int) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8095"+path, nil)
	req.Header.Set("Authorization", "Bearer "+grant)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		return fmt.Errorf("MCP grant/path status=%d want=%d", res.StatusCode, want)
	}
	return nil
}

func dynamicNoSecrets(ctx context.Context, owner string) error {
	data, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+owner).Output()
	containers := strings.Fields(string(data))
	if err != nil || len(containers) != 1 {
		return fmt.Errorf("expected one native attempt")
	}
	command := `cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | grep -qx 'USER_SETTING=native-env-delivered' || exit 3; if cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | grep -qE 'IAM_CLIENT_ID=|IAM_CLIENT_SECRET=|fixture-iam-secret|fixture-iam-token|PUBLIC_TOKEN=|fixture-static-token-round-1|fixture-env-token-round-1|fixture-query-token-round-1|fixture-cookie-token-round-1'; then exit 1; fi; if grep -raqE 'fixture-iam-secret|fixture-iam-token|fixture-static-token-round-1|fixture-env-token-round-1|fixture-query-token-round-1|fixture-cookie-token-round-1|public-mcp.fixture.invalid' /workspace/opencode.json /workspace/work; then exit 2; fi`
	output, err := exec.CommandContext(ctx, "docker", "exec", "--user", "65532:65532", containers[0], "sh", "-c", command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("MCP secrets projected into native attempt: %v %.120s", err, output)
	}
	return nil
}
