//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

// officialImage is the newest verified upstream OpenCode image, used without changes.
var officialImage = opencode.Images[len(opencode.Images)-1]

func managedMCPService(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_OPENCODE") != "1" || os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("set VERIFY_SERVICE=1 VERIFY_OPENCODE=1 for real controller/MCP tasks")
	}
	dir := t.TempDir()
	project := fmt.Sprintf("sandbox-managed-%d", time.Now().UnixNano())
	network := strings.TrimSuffix(os.Getenv("MULTICA_TEST_SERVER_CONTAINER"), "-server")
	fixture := filepath.Join(dir, "fixture.json")
	services := map[string]any{"seed": map[string]any{"environment": map[string]string{"ROTATION_FIXTURE": "1"}}, "provider": providerService()}
	writeJSON(t, fixture, map[string]any{"services": services, "networks": map[string]any{"fixture": map[string]any{"external": true, "name": network, "internal": nil}, "execution": map[string]any{"internal": true}}})
	compose := func(args ...string) string {
		return dockerTest(t, append([]string{"compose", "-p", project, "-f", "../examples/identity-mcp/compose.yaml", "-f", fixture}, args...)...)
	}
	t.Cleanup(func() { compose("down", "-v", "--remove-orphans", "--rmi", "local") })
	compose("build", "seed", "gateway", "provider")
	compose("up", "-d", "--wait", "--wait-timeout", "180", "gateway", "provider")
	controller := "90000000-0000-4000-8000-000000000029"
	agent := "20000000-0000-4000-8000-000000000001"
	id := "70000000-0000-4000-8000-000000000001"
	issue := "80000000-0000-4000-8000-000000000001"
	issuer := "http://keycloak:8080/realms/sandbox-example"
	binding := identity.Config{Version: 1, AllowHTTP: true, Issuer: identity.IssuerConfig{URL: issuer, TokenURL: issuer + "/protocol/openid-connect/token", JWKSURL: issuer + "/protocol/openid-connect/certs", MaxTTLSeconds: 30}, Bindings: []identity.Binding{{WorkspaceID: workspace, AgentID: agent, ClientID: "example-agent", SecretFile: "/identity-secrets/client"}}, MCP: []string{"http://gateway:8080/mcp"}}
	writeJSON(t, filepath.Join(dir, "identity.json"), binding)
	// This credential-free mock model is trusted fixture configuration, outside claim data.
	command := `OPENCODE_CONFIG_CONTENT='{"model":"fixture/fixture","enabled_providers":["fixture"],"provider":{"fixture":{"npm":"@ai-sdk/openai-compatible","name":"Fixture","options":{"baseURL":"http://provider:8080/v1"},"models":{"fixture":{"name":"Fixture","limit":{"context":64000,"output":4096}}}}}}' exec opencode acp`
	f := serviceFixture{fmt.Sprintf("sandbox-agent-service-%d", time.Now().UnixNano()), filepath.Join(dir, "controller.json"), t}
	// Attempts reach the mock model on the provider and MCP only through the controller relay.
	mcpRelay := &service.RelayConfig{Listen: ":8095", URL: "http://mcp-relay:8095"}
	c := service.Config{Server: "http://" + os.Getenv("MULTICA_TEST_SERVER_CONTAINER") + ":8080", AllowHTTP: true, Daemon: controller, Image: officialImage, Command: []string{"/bin/sh", "-c", command}, Timeout: "180s", OpenCode: &service.OpenCodeConfig{IdentityFile: "/etc/multica-sandbox/identity.json", MCPRelay: mcpRelay, Authority: attempt.Config{URL: "http://gateway:8080/attempts", BearerFile: "/identity-secrets/admin", AllowHTTP: true}, Network: project + "_execution", Peers: []string{project + "-provider-1", f.project + "-controller-1"}}}
	prepareManagedService(t, f, dir, c, project, network)
	cid := f.compose("ps", "-q", "controller")
	eventually(t, "OpenCode runtime registration", func() bool { return strings.Contains(dockerTest(t, "logs", cid), readyEvent) })
	runtime := sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE daemon_id='%s' AND workspace_id='%s' AND provider='opencode';", controller, workspace))
	if runtime == "" {
		t.Fatal("OpenCode runtime absent")
	}
	sql(t, fmt.Sprintf(`INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id,owner_id,status,instructions,mcp_config,custom_env) VALUES('%s','%s','MCP fixture','local','%s','%s','idle','Read document repeatedly using fixture MCP. workspace-two','{"mcpServers":{"fixture":{"url":"http://gateway:8080/mcp"}}}','{"DO_NOT_PROJECT":"claim-secret-sentinel"}');
 INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Managed MCP fixture','in_progress','member','%s',99999,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, agent, workspace, runtime, user, issue, workspace, user, agent, id, agent, runtime, issue, user, user))
	started := dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid)
	observedSession := ""
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		state, err := api.Status(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if state == "running" && observedSession == "" {
			observedSession = sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') FROM agent_task_queue WHERE id='%s';", id))
		}
		if state == "completed" {
			break
		}
		if state == "failed" {
			t.Fatal("real controller task failed")
		}
		time.Sleep(250 * time.Millisecond)
	}
	status(t, api, id, "completed")
	if !strings.HasPrefix(observedSession, "ses_") {
		t.Fatal("native session was not pinned mid-flight")
	}
	var evidence map[string][2]int
	if err := json.Unmarshal([]byte(dockerTest(t, "exec", project+"-gateway-1", "/identity-example", "evidence")), &evidence); err != nil {
		t.Fatal(err)
	}
	successful := false
	for key, stats := range evidence {
		if strings.Contains(key, id) && stats[0] >= 24 && stats[1] >= 3 {
			successful = true
		}
	}
	if !successful {
		t.Fatalf("real controller did not complete successful MCP calls across rotations: %v", evidence)
	}
	if dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid) != started {
		t.Fatal("controller restarted during task")
	}
	if output := sql(t, fmt.Sprintf("SELECT result FROM agent_task_queue WHERE id='%s';", id)); !strings.Contains(output, "Fixture task completed") {
		t.Fatal("native result was not reported")
	}

	assertNativeReports(t, id)
	f.compose("stop")
	t.Log("real Multica claim -> persistent Compose controller -> native OpenCode task -> real Keycloak/MCP rotations -> cleanup; inference is a deterministic mock")
}

// providerService serves the mock model alone on the attempt template: the controller
// resolves the gateway (MCP, authority) on the control network, never through a peer alias.
func providerService() map[string]any {
	build := map[string]string{"context": "../..", "dockerfile": "examples/identity-mcp/Dockerfile", "target": "fixture"}
	health := map[string]any{"test": []string{"CMD", "/identity-example", "health"}, "interval": "2s", "timeout": "2s", "retries": 60}
	return map[string]any{"build": build, "command": []string{"provider"}, "networks": []string{"execution"}, "healthcheck": health}
}

// prepareManagedService starts the controller on the control network with the MCP relay
// alias on the fixture project's attempt template.
func prepareManagedService(t *testing.T, f serviceFixture, dir string, c service.Config, project, control string) {
	writeJSON(t, filepath.Join(dir, "config.json"), c)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token(t)), 0600); err != nil {
		t.Fatal(err)
	}
	networks := map[string]any{"control": map[string]any{"external": true, "name": control}, "execution": map[string]any{"external": true, "name": project + "_execution"}}
	controller := map[string]any{"networks": map[string]any{"control": map[string]any{}, "execution": map[string]any{"aliases": []string{"mcp-relay"}}}, "volumes": managedVolumes(dir, c)}
	writeJSON(t, f.override, map[string]any{"services": map[string]any{"controller": controller}, "networks": networks, "volumes": map[string]any{"identity-secrets": map[string]any{"external": true, "name": project + "_credentials"}}, "secrets": map[string]any{"multica_token": map[string]string{"file": filepath.Join(dir, "token")}}})
	t.Cleanup(func() {
		f.compose("down", "-v")
		if err := (&docker.Backend{Owner: c.Daemon}).Reconcile(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.compose("up", "-d", "--no-build")
}
