//go:build upstream

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

// Real task tokens are mat_ plus 40 hex characters; relay credentials are longer.
var taskToken = regexp.MustCompile(`mat_[0-9a-f]{40}([^0-9a-f]|$)`)

// The agent-side probe runs inside the live attempt as its own user.
const attemptProbe = `set -u
token=$(cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | sed -n 's/^MULTICA_TOKEN=//p' | head -1)
[ -n "$token" ] || exit 3
printf '%s' "$token"
cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | grep -qE 'mat_[0-9a-f]{40}([^0-9a-f]|$)' && exit 10
grep -rqE 'mat_[0-9a-f]{40}([^0-9a-f]|$)' /workspace /tmp 2>/dev/null && exit 11
wget -q -O /dev/null --header "Authorization: Bearer $token" --post-data '{"name":"exfiltrate"}' http://multica-relay:8091/api/tokens 2>&1 | grep -q ' 403 ' || exit 12
wget -T 2 -q -O /dev/null "http://$1:8080/health" 2>/dev/null && exit 13
exit 0`

func multicaRelayService(t *testing.T, api *multica.Client) {
	agentImage := os.Getenv("MULTICA_TEST_AGENT_IMAGE")
	if os.Getenv("VERIFY_OPENCODE") != "1" || os.Getenv("VERIFY_SERVICE") != "1" || agentImage == "" {
		t.Skip("set VERIFY_SERVICE=1 VERIFY_OPENCODE=1 for the Multica relay agent image")
	}
	dir := t.TempDir()
	stamp := time.Now().UnixNano()
	project := fmt.Sprintf("sandbox-relay-%d", stamp)
	server := os.Getenv("MULTICA_TEST_SERVER_CONTAINER")
	network := strings.TrimSuffix(server, "-server")
	fixture := filepath.Join(dir, "fixture.json")
	writeJSON(t, fixture, map[string]any{"services": map[string]any{"seed": map[string]any{"environment": map[string]string{"ROTATION_FIXTURE": "1"}}, "gateway": map[string]any{"networks": []string{"fixture", "execution"}}}, "networks": map[string]any{"fixture": map[string]any{"external": true, "name": network, "internal": nil}, "execution": map[string]any{"internal": true}}})
	compose := func(args ...string) string {
		return dockerTest(t, append([]string{"compose", "-p", project, "-f", "../examples/identity-mcp/compose.yaml", "-f", fixture}, args...)...)
	}
	t.Cleanup(func() { compose("down", "-v", "--remove-orphans") })
	compose("build", "seed", "gateway")
	compose("up", "-d", "--wait", "--wait-timeout", "180", "gateway")

	controller := "90000000-0000-4000-8000-000000000014"
	agent := "a1000000-0000-4000-8000-000000000014"
	issue := "a2000000-0000-4000-8000-000000000014"
	id := "a3000000-0000-4000-8000-000000000014"
	issuer := "http://keycloak:8080/realms/sandbox-example"
	writeJSON(t, filepath.Join(dir, "identity.json"), identity.Config{Version: 1, AllowHTTP: true, Issuers: []identity.IssuerConfig{{Name: "fixture", URL: issuer, TokenURL: issuer + "/protocol/openid-connect/token", JWKSURL: issuer + "/protocol/openid-connect/certs", MaxTTLSeconds: 30}}, Bindings: []identity.Binding{{WorkspaceID: workspace, AgentID: agent, Principal: identity.Principal{Issuer: "fixture", ClientID: "example-agent", Subject: "30000000-0000-4000-8000-000000000001"}, SecretFile: "/identity-secrets/client"}}, MCP: []identity.MCPRule{{URL: "http://gateway:8080/mcp", Issuer: "fixture"}}})
	command := `OPENCODE_CONFIG_CONTENT='{"model":"fixture/fixture","enabled_providers":["fixture"],"provider":{"fixture":{"npm":"@ai-sdk/openai-compatible","name":"Fixture","options":{"baseURL":"http://gateway:8080/v1"},"models":{"fixture":{"name":"Fixture","limit":{"context":64000,"output":4096}}}}}}' exec opencode run --format json "$(cat /workspace/prompt.txt)"`
	f := serviceFixture{fmt.Sprintf("sandbox-relay-controller-%d", stamp), filepath.Join(dir, "controller.json"), t}
	controllerName := f.project + "-controller-1"
	c := service.Config{Server: "http://" + server + ":8080", AllowHTTP: true, Daemon: controller, Image: agentImage, Command: []string{"/bin/sh", "-c", command}, Timeout: "180s", OpenCode: &service.OpenCodeConfig{IdentityFile: "/etc/multica-sandbox/identity.json", Authority: attempt.Config{URL: "http://gateway:8080/attempts", BearerFile: "/identity-secrets/admin", AllowHTTP: true}, Network: project + "_execution", Peers: []string{project + "-gateway-1", controllerName}, MulticaRelay: &service.RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}}}
	writeJSON(t, filepath.Join(dir, "config.json"), c)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token(t)), 0600); err != nil {
		t.Fatal(err)
	}
	// The controller has its own namespace: Multica on the control network, relay alias on the template.
	writeJSON(t, f.override, map[string]any{"services": map[string]any{"controller": map[string]any{"networks": map[string]any{"control": map[string]any{}, "execution": map[string]any{"aliases": []string{"multica-relay"}}}, "volumes": managedVolumes(dir, c)}}, "networks": map[string]any{"control": map[string]any{"external": true, "name": network}, "execution": map[string]any{"external": true, "name": project + "_execution"}}, "volumes": map[string]any{"identity-secrets": map[string]any{"external": true, "name": project + "_credentials"}}, "secrets": map[string]any{"multica_token": map[string]string{"file": filepath.Join(dir, "token")}}})
	t.Cleanup(func() {
		f.compose("down", "-v")
		if err := (&docker.Backend{Owner: controller}).Reconcile(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.compose("up", "-d", "--no-build")
	eventually(t, "relay controller registration", func() bool { return strings.Contains(dockerTest(t, "logs", controllerName), "ready workspaces=") })
	runtime := sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE daemon_id='%s' AND workspace_id='%s' AND provider='opencode';", controller, workspace))
	if runtime == "" {
		t.Fatal("relay runtime absent")
	}
	sql(t, fmt.Sprintf(`INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id,owner_id,status,instructions,mcp_config) VALUES('%s','%s','Relay fixture agent','local','%s','%s','idle','Answer through the Multica CLI.','{"mcpServers":{}}');
 INSERT INTO issue(id,workspace_id,title,description,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Relay fixture issue','Read me through the relay.','todo','member','%s',99914,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, agent, workspace, runtime, user, issue, workspace, user, agent, id, agent, runtime, issue, user, user))

	opaque := ""
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		state, err := api.Status(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if state == "running" && opaque == "" {
			if container := dockerTest(t, "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+controller); container != "" {
				out, err := exec.Command("docker", "exec", container, "/bin/sh", "-c", attemptProbe, "probe", server).Output()
				var exit *exec.ExitError
				switch {
				case err == nil:
					opaque = string(out)
				case errors.As(err, &exit) && exit.ExitCode() == 3:
				default:
					t.Fatalf("attempt boundary probe failed: %v", err)
				}
			}
		}
		if state == "completed" {
			break
		}
		if state == "failed" {
			t.Fatal("relay task failed: " + sql(t, fmt.Sprintf("SELECT coalesce(error,'') FROM agent_task_queue WHERE id='%s';", id)))
		}
		time.Sleep(250 * time.Millisecond)
	}
	status(t, api, id, "completed")
	if !strings.HasPrefix(opaque, "mat_relay_") {
		t.Fatal("live attempt credential was not observed")
	}
	comments := sql(t, fmt.Sprintf("SELECT count(*) || ':' || min(author_type) || ':' || min(author_id::text) FROM comment WHERE issue_id='%s' AND type='comment';", issue))
	content := sql(t, fmt.Sprintf("SELECT content FROM comment WHERE issue_id='%s' AND type='comment';", issue))
	if comments != "1:agent:"+agent || !strings.Contains(content, "Relay fixture read: Relay fixture issue") {
		t.Fatalf("agent did not read the issue and comment through the CLI: %s %q", comments, content)
	}
	// The ended attempt's credential is denied by the relay itself, not by network absence.
	denied := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "/bin/sh", "-e", "T="+opaque, agentImage, "-c", `wget -q -O /dev/null --header "Authorization: Bearer $T" http://`+controllerName+`:8091/api/issues/`+issue+` 2>&1 | grep -q ' 401 '`)
	if out, err := denied.CombinedOutput(); err != nil {
		t.Fatalf("ended attempt credential not denied: %v %s", err, out)
	}
	logs := dockerTest(t, "logs", controllerName)
	transcript := sql(t, fmt.Sprintf("SELECT coalesce(string_agg(coalesce(content,'') || coalesce(input::text,'') || coalesce(output,''), ' '),'') FROM task_message WHERE task_id='%s';", id))
	if taskToken.MatchString(logs+transcript+content) || strings.Contains(logs, opaque) {
		t.Fatal("task credential leaked into controller logs, transcript or comments")
	}
	t.Log("pinned Multica claim -> controller relay -> upstream multica CLI read issue and posted one agent comment; mat_ token stayed outside the attempt")
}
