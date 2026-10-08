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
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
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
grep -rqE 'mat_[0-9a-f]{40}([^0-9a-f]|$)' /proc/1/root/workspace /proc/1/root/tmp 2>/dev/null && exit 11
wget -q -O /dev/null --header "Authorization: Bearer $token" --post-data '{"name":"exfiltrate"}' http://multica-relay:8091/api/tokens 2>&1 | grep -q ' 403 ' || exit 12
wget -T 2 -q -O /dev/null "http://$1:8080/health" 2>/dev/null && exit 13
tr '\0' '\n' < /proc/1/environ | grep -qx 'PATH=/opt/multica-sandbox/multica/bin:.*' || exit 14
test -x /proc/1/root/opt/multica-sandbox/multica/bin/multica || exit 15
exit 0`

// repositoryProbe checks the live checkout boundary as the attempt user; $1 is the Git host password.
const repositoryProbe = `set -u
test -f /proc/1/root/workspace/work/fixture/README || exit 3
env=$(cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n')
value() { printf '%s\n' "$env" | sed -n "s/^$1=//p" | head -1; }
git=$(value GIT_CONFIG_VALUE_0 | sed -n 's/^Authorization: Bearer //p')
token=$(value MULTICA_TOKEN)
case "$git" in msg_*) ;; *) exit 20 ;; esac
printf '%s\n' "$env" | grep -qF "$1" && exit 21
grep -rqF "$1" /proc/1/root/workspace /proc/1/root/tmp 2>/dev/null && exit 22
grep -q 'url = https://git.fixture.test/sandbox/fixture.git' /proc/1/root/workspace/work/fixture/.git/config || exit 23
r=http://git-relay:8093/git.fixture.test/sandbox
wget -q -O /dev/null --header "Authorization: Bearer $git" "$r/fixture.git/info/refs?service=git-upload-pack" || exit 24
wget -q -O /dev/null --header "Authorization: Bearer $git" "$r/other.git/info/refs?service=git-upload-pack" && exit 25
wget -q -O /dev/null --header "Authorization: Bearer $git" "$r/fixture.git/info/refs?service=git-receive-pack" && exit 26
wget -q -O /dev/null --header "Authorization: Bearer $token" "$r/fixture.git/info/refs?service=git-upload-pack" && exit 27
body="{\"url\":\"https://git.fixture.test/sandbox/fixture.git\",\"workspace_id\":\"$(value MULTICA_WORKSPACE_ID)\",\"task_id\":\"$(value MULTICA_TASK_ID)\",\"workdir\":\"/workspace\"}"
wget -q -O /dev/null --header "Authorization: Bearer $token" --post-data "$body" http://127.0.0.1:` + repo.DaemonPort + `/repo/checkout 2>&1 | grep -q ' 403 ' || exit 28
printf '%s' "$git"`

// multicaRelayService runs one relay task on an image without multica; full adds repository
// checkout through the Git relay (the image needs git), inference and restart.
func multicaRelayService(t *testing.T, api *multica.Client, agentImage string, n int, full bool) {
	cli := os.Getenv("MULTICA_TEST_CLI_IMAGE")
	if os.Getenv("VERIFY_OPENCODE") != "1" || os.Getenv("VERIFY_SERVICE") != "1" || agentImage == "" || cli == "" {
		t.Skip("set VERIFY_SERVICE=1 VERIFY_OPENCODE=1 for the Multica relay agent and CLI images")
	}
	dir := t.TempDir()
	stamp := time.Now().UnixNano()
	project := fmt.Sprintf("sandbox-relay-%d", stamp)
	server := os.Getenv("MULTICA_TEST_SERVER_CONTAINER")
	network := strings.TrimSuffix(server, "-server")
	fixture := filepath.Join(dir, "fixture.json")
	withInference := full && os.Getenv("VERIFY_INFERENCE") == "1"
	services := map[string]any{"seed": map[string]any{"environment": map[string]string{"ROTATION_FIXTURE": "1"}}, "gateway": map[string]any{"networks": []string{"fixture", "execution"}}}
	target := "gateway"
	if withInference {
		// Agents reach inference only through the controller relay; the mock provider stays behind LiteLLM.
		inferenceServices(services)
		target = "litellm"
		dockerTest(t, "network", "create", "--internal", project+"_execution")
		t.Cleanup(func() { dockerTest(t, "network", "rm", project+"_execution") })
	}
	writeJSON(t, fixture, map[string]any{"services": services, "networks": map[string]any{"fixture": map[string]any{"external": true, "name": network, "internal": nil}, "execution": map[string]any{"internal": true}}})
	targets := []string{target}
	if full {
		targets = append(targets, "git")
	}
	startFixture(t, project, fixture, targets)

	controller := fmt.Sprintf("90000000-0000-4000-8000-%012d", n)
	agent := fmt.Sprintf("a1000000-0000-4000-8000-%012d", n)
	issue := fmt.Sprintf("a2000000-0000-4000-8000-%012d", n)
	id := fmt.Sprintf("a3000000-0000-4000-8000-%012d", n)
	issuer := "http://keycloak:8080/realms/sandbox-example"
	writeJSON(t, filepath.Join(dir, "identity.json"), identity.Config{Version: 1, AllowHTTP: true, Issuers: []identity.IssuerConfig{{Name: "fixture", URL: issuer, TokenURL: issuer + "/protocol/openid-connect/token", JWKSURL: issuer + "/protocol/openid-connect/certs", MaxTTLSeconds: 30}}, Bindings: []identity.Binding{{WorkspaceID: workspace, AgentID: agent, Principal: identity.Principal{Issuer: "fixture", ClientID: "example-agent", Subject: "30000000-0000-4000-8000-000000000001"}, SecretFile: "/identity-secrets/client"}}, MCP: []identity.MCPRule{{URL: "http://gateway:8080/mcp", Issuer: "fixture"}}})
	command := `OPENCODE_CONFIG_CONTENT='{"model":"fixture/fixture","enabled_providers":["fixture"],"provider":{"fixture":{"npm":"@ai-sdk/openai-compatible","name":"Fixture","options":{"baseURL":"http://gateway:8080/v1"},"models":{"fixture":{"name":"Fixture","limit":{"context":64000,"output":4096}}}}}}' exec opencode run --format json "$(cat /workspace/prompt.txt)"`
	f := serviceFixture{fmt.Sprintf("sandbox-relay-controller-%d", stamp), filepath.Join(dir, "controller.json"), t}
	controllerName := f.project + "-controller-1"
	c := service.Config{Server: "http://" + server + ":8080", AllowHTTP: true, Daemon: controller, Image: agentImage, Command: []string{"/bin/sh", "-c", command}, Timeout: "180s", OpenCode: &service.OpenCodeConfig{IdentityFile: "/etc/multica-sandbox/identity.json", Authority: attempt.Config{URL: "http://gateway:8080/attempts", BearerFile: "/identity-secrets/admin", AllowHTTP: true}, Network: project + "_execution", Peers: []string{project + "-gateway-1", controllerName}, MulticaRelay: &service.RelayConfig{Listen: ":8091", URL: "http://multica-relay:8091"}, MulticaCLI: cli}}
	tool := os.Getenv("MULTICA_TEST_TOOL_IMAGE")
	if full && tool != "" {
		c.Tools = []service.Tool{{Name: "jq", Image: tool, Check: []string{"bin/jq", "--version"}}}
	}
	title := "Relay fixture issue"
	if full {
		title = "Repository fixture issue"
		configureRepositories(t, dir, project, &c)
	}
	var w *workspaceInference
	if withInference {
		w = configureInference(t, dir, project, &c)
		c.OpenCode.Peers = []string{controllerName}
	}
	writeJSON(t, filepath.Join(dir, "config.json"), c)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token(t)), 0600); err != nil {
		t.Fatal(err)
	}
	// The controller has its own namespace: Multica on the control network, relay aliases on the template.
	writeJSON(t, f.override, map[string]any{"services": map[string]any{"controller": map[string]any{"networks": map[string]any{"control": map[string]any{}, "execution": map[string]any{"aliases": []string{"multica-relay", "inference-relay", "git-relay"}}}, "volumes": managedVolumes(dir, c)}}, "networks": map[string]any{"control": map[string]any{"external": true, "name": network}, "execution": map[string]any{"external": true, "name": project + "_execution"}}, "volumes": map[string]any{"identity-secrets": map[string]any{"external": true, "name": project + "_credentials"}}, "secrets": map[string]any{"multica_token": map[string]string{"file": filepath.Join(dir, "token")}}})
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
	sql(t, fmt.Sprintf(`INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id,owner_id,status,instructions,mcp_config) VALUES('%s','%s','Relay fixture agent %d','local','%s','%s','idle','Answer through the Multica CLI.','{"mcpServers":{}}');
 INSERT INTO issue(id,workspace_id,title,description,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','%s','Read me through the relay.','todo','member','%s',%d,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, agent, workspace, n, runtime, user, issue, workspace, title, user, 99900+n, agent, id, agent, runtime, issue, user, user))

	opaque, inferenceOpaque, gitOpaque, secret := "", "", "", ""
	if full {
		secret = strings.TrimSpace(dockerTest(t, "exec", project+"-git-1", "cat", "/secrets/git"))
	}
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		state, err := api.Status(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		// The fixture agent holds its final step until the probe marks the attempt (mock_inference.go).
		if state == "running" && (opaque == "" || (w != nil && inferenceOpaque == "") || (full && gitOpaque == "")) {
			if container := dockerTest(t, "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+controller); container != "" {
				if opaque == "" {
					opaque = liveProbe(t, container, "attempt boundary", attemptProbe, server)
				}
				if opaque != "" && w != nil && inferenceOpaque == "" {
					inferenceOpaque = inferenceCredential(t, w, container)
				}
				if opaque != "" && full && gitOpaque == "" {
					gitOpaque = liveProbe(t, container, "repository boundary", repositoryProbe, secret)
				}
				if opaque != "" && (w == nil || inferenceOpaque != "") && (!full || gitOpaque != "") {
					if _, err := execOutput(container, "touch /proc/1/root/workspace/.probed"); err != nil {
						t.Fatalf("mark probed attempt: %v", err)
					}
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
	if !strings.HasPrefix(opaque, "mat_relay_") || (w != nil && inferenceOpaque == "") {
		t.Fatalf("live attempt credential was not observed (multica %t, inference %t)", opaque != "", inferenceOpaque != "")
	}
	content := agentComment(t, issue, agent, title, c.Tools != nil, full, n)
	// The ended attempt's credential is denied by the relay itself, not by network absence.
	denied := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "/bin/sh", "-e", "T="+opaque, image, "-c", `wget -q -O /dev/null --header "Authorization: Bearer $T" http://`+controllerName+`:8091/api/issues/`+issue+` 2>&1 | grep -q ' 401 '`)
	if out, err := denied.CombinedOutput(); err != nil {
		t.Fatalf("ended attempt credential not denied: %v %s", err, out)
	}
	if full {
		repositoryEnded(t, network, controllerName, gitOpaque, secret, content)
	}
	if w != nil {
		denied := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "/bin/sh", "-e", "T="+inferenceOpaque, image, "-c", `wget -q -O /dev/null --header "Authorization: Bearer $T" --post-data '{}' http://`+controllerName+`:8092/v1/chat/completions 2>&1 | grep -q ' 401 '`)
		if out, err := denied.CombinedOutput(); err != nil {
			t.Fatalf("ended inference credential not denied: %v %s", err, out)
		}
	}
	logs := dockerTest(t, "logs", controllerName)
	transcript := sql(t, fmt.Sprintf("SELECT coalesce(string_agg(coalesce(content,'') || coalesce(input::text,'') || coalesce(output,''), ' '),'') FROM task_message WHERE task_id='%s';", id))
	if taskToken.MatchString(logs+transcript+content) || strings.Contains(logs, opaque) {
		t.Fatal("task credential leaked into controller logs, transcript or comments")
	}
	t.Log("pinned Multica claim -> controller relay -> upstream multica CLI read issue and posted one agent comment; mat_ token stayed outside the attempt")
	if w == nil || !full {
		return
	}
	result := sql(t, fmt.Sprintf("SELECT coalesce(result::text,'') || coalesce(error,'') FROM agent_task_queue WHERE id='%s';", id))
	if w.leaked(logs+transcript+content+result) || (inferenceOpaque != "" && strings.Contains(logs, inferenceOpaque)) {
		t.Fatal("workspace key leaked into controller logs, transcript, comments or results")
	}
	if usage := sql(t, fmt.Sprintf("SELECT provider || ':' || model FROM task_usage WHERE task_id='%s';", id)); usage != "opencode:managed-inference/fixture" {
		t.Fatalf("native usage not attributed to the managed model: %s", usage)
	}
	w.usage(t, map[string][2]string{attribution(agent, id): {"fixture", "workspace"}})
	t.Log("the same task used the workspace LiteLLM key through the inference relay; LiteLLM attributes workspace/agent/task")
	restartFailsClosed(t, w, network, controllerName, runtime, agent)
	controllerModelSelections(t, api, w, runtime, agent)
}

// configureRepositories adds the helper, the Git relay and a claim repository on the fixture Git host.
func configureRepositories(t *testing.T, dir, project string, c *service.Config) {
	helper := os.Getenv("MULTICA_TEST_HELPER_IMAGE")
	if helper == "" {
		t.Fatal("MULTICA_TEST_HELPER_IMAGE is required for repository checkout")
	}
	c.OpenCode.Helper, c.OpenCode.GitFile = helper, "/etc/multica-sandbox/git.json"
	c.OpenCode.GitRelay = &service.RelayConfig{Listen: ":8093", URL: "http://git-relay:8093"}
	writeJSON(t, filepath.Join(dir, "git.json"), repo.Config{Version: 1, AllowHTTP: true, Hosts: []repo.Host{{WorkspaceID: workspace, Host: "git.fixture.test", Upstream: "http://" + project + "-git-1:8080", Username: "fixture", PasswordFile: "/identity-secrets/git"}}})
	sql(t, fmt.Sprintf(`UPDATE workspace SET repos='[{"url":"https://git.fixture.test/sandbox/fixture.git","description":"Fixture repository"}]' WHERE id='%s';`, workspace))
	t.Cleanup(func() { sql(t, fmt.Sprintf(`UPDATE workspace SET repos='[]' WHERE id='%s';`, workspace)) })
}

// repositoryEnded checks that the ended Git grant is denied and the host password never left the controller.
func repositoryEnded(t *testing.T, network, controller, opaque, secret, content string) {
	denied := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "/bin/sh", "-e", "T="+opaque, image, "-c", `wget -q -O /dev/null --header "Authorization: Bearer $T" "http://`+controller+`:8093/git.fixture.test/sandbox/fixture.git/info/refs?service=git-upload-pack" 2>&1 | grep -q ' 401 '`)
	if out, err := denied.CombinedOutput(); err != nil {
		t.Fatalf("ended Git credential not denied: %v %s", err, out)
	}
	if strings.Contains(dockerTest(t, "logs", controller)+content, secret) {
		t.Fatal("Git host password leaked into controller logs or comments")
	}
	t.Log("unchanged multica repo checkout cloned the claim repository through the Git relay on the task branch; host password stayed in the controller")
}

// startFixture builds and starts the identity-mcp services; cleanup removes them with their images.
func startFixture(t *testing.T, project, override string, targets []string) {
	compose := func(args ...string) string {
		return dockerTest(t, append([]string{"compose", "-p", project, "--profile", "inference", "--profile", "repositories", "-f", "../examples/identity-mcp/compose.yaml", "-f", override}, args...)...)
	}
	t.Cleanup(func() { compose("down", "-v", "--remove-orphans", "--rmi", "local") })
	compose(append([]string{"build", "seed", "gateway"}, targets[1:]...)...)
	compose(append([]string{"up", "-d", "--wait", "--wait-timeout", "240"}, targets...)...)
}

// liveProbe runs a boundary probe in the live attempt; exit 3 means not ready yet and yields "".
func liveProbe(t *testing.T, container, name, script string, args ...string) string {
	out, err := execOutput(container, script, args...)
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 3) {
		t.Fatalf("%s probe failed: %v", name, err)
	}
	return out
}

// agentComment returns the single agent comment after checking what the fixture agent reported.
func agentComment(t *testing.T, issue, agent, title string, tool, checkout bool, n int) string {
	comments := sql(t, fmt.Sprintf("SELECT count(*) || ':' || min(author_type) || ':' || min(author_id::text) FROM comment WHERE issue_id='%s' AND type='comment';", issue))
	content := sql(t, fmt.Sprintf("SELECT content FROM comment WHERE issue_id='%s' AND type='comment';", issue))
	if comments != "1:agent:"+agent || !strings.Contains(content, "Relay fixture read: "+title) {
		t.Fatalf("agent did not read the issue and comment through the CLI: %s %q", comments, content)
	}
	if tool && !strings.Contains(content, "jq-1.") {
		t.Fatalf("agent did not run the bundled tool: %q", content)
	}
	if checkout && !strings.Contains(content, fmt.Sprintf("Checkout: agent/relay-fixture-agent-%d/%012d Repository fixture fixture", n, n)) {
		t.Fatalf("agent did not check out its task branch through the Git relay: %q", content)
	}
	return content
}
