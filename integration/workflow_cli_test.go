//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// A compatible CLI can advance independently while the real server stays pinned.
func workflowCLI(t *testing.T, api *multica.Client, rt multica.Runtime) {
	binary := os.Getenv("MULTICA_TEST_WORKFLOW_CLI")
	if binary == "" {
		t.Skip("set MULTICA_TEST_WORKFLOW_CLI to a compatible native CLI binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("workflow CLI must be an absolute path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	id := enqueue(t, rt, 910)
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("workflow CLI claim: %v", err)
	}
	if err = api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	raw, err := task.TaskToken()
	if err != nil {
		t.Fatal(err)
	}
	grants := relay.NewGrants(multica.RelayPrefix)
	opaque, err := grants.Issue(task.AttemptKey(), relay.Upstream{Origin: os.Getenv("MULTICA_TEST_URL"), Credential: raw})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(grants, relay.Policy{Allow: multica.RelayPath, Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	home := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "MULTICA_SERVER_URL=" + server.URL,
			"MULTICA_TOKEN=" + opaque, "MULTICA_WORKSPACE_ID=" + workspace,
			"MULTICA_AGENT_ID=" + task.AgentID, "MULTICA_TASK_ID=" + task.ID}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("multica %v: %v: %s", args, err, output)
		}
		return string(output)
	}
	verifyWorkflowCLI(t, home, run)
	grants.Revoke(task.AttemptKey())
	if err = api.Complete(ctx, id, execution.Result{}); err != nil {
		t.Fatal(err)
	}
	t.Log("real CLI created/read/updated workflow, set handoff and switched project on pinned v0.7.2 through task relay")
}

func verifyWorkflowCLI(t *testing.T, home string, run func(...string) string) {
	t.Helper()
	steps := filepath.Join(home, "steps.json")
	if err := os.WriteFile(steps, []byte(`[{"status_key":"backlog"},{"status_key":"in_progress"},{"status_key":"blocked"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	flow := cliResourceID(t, run("workflow", "create", "--name", "CLI workflow fixture", "--steps-file", steps))
	if !strings.Contains(run("workflow", "list", "--output", "json"), flow) {
		t.Fatal("workflow missing from list")
	}
	run("workflow", "get", flow)
	run("workflow", "update", flow, "--description", "CLI workflow update")
	if !strings.Contains(run("workflow", "get", flow), "CLI workflow update") {
		t.Fatal("workflow update not persisted")
	}
	run("workflow", "handoff", "set", flow, "in_progress", "--handler", "project_lead", "--instructions", "CLI handoff fixture", "--next", "done")
	if !strings.Contains(run("workflow", "get", flow), "CLI handoff fixture") {
		t.Fatal("workflow handoff not persisted")
	}
	project := cliResourceID(t, run("project", "create", "--title", "CLI workflow project", "--output", "json"))
	run("project", "workflow", "get", project)
	run("project", "workflow", "set", project, flow, "--dry-run")
	run("project", "workflow", "set", project, flow)
	if !strings.Contains(run("project", "workflow", "get", project), flow) {
		t.Fatal("project workflow not persisted")
	}
	run("project", "workflow", "set", project, "default")
	run("workflow", "delete", flow)
}

func cliResourceID(t *testing.T, output string) string {
	t.Helper()
	var resource struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(output), &resource) != nil || !multica.ValidID(resource.ID) {
		t.Fatal("CLI did not return a resource ID")
	}
	return resource.ID
}
