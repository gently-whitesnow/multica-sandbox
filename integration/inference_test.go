//go:build upstream

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

const gatewayOrigin = "http://litellm:4000"

// workspaceInference is the controller side of ADR 0012: LiteLLM virtual keys in
// controller-only files, reached only through the embedded relay.
type workspaceInference struct {
	project, dir string
	keys         map[string]string
}

func inferenceServices(services map[string]any) {
	// The mock provider checks LiteLLM's provider key and stays off attempt networks.
	services["gateway"] = map[string]any{"networks": map[string]any{"fixture": map[string]any{"aliases": []string{"fixture-provider"}}}, "environment": map[string]string{"INFERENCE_FIXTURE": "1"}}
	services["keycloak"] = map[string]any{"mem_limit": "512m", "environment": map[string]string{"JAVA_OPTS_KC_HEAP": "-Xms64m -Xmx256m"}}
}

func (w *workspaceInference) litellm(t *testing.T, script string, args ...string) string {
	t.Helper()
	prelude := "import json,sys,urllib.request\nauth={'Authorization':'Bearer sk-'+open('/secrets/admin').read(),'Content-Type':'application/json'}\n"
	return dockerTest(t, append([]string{"exec", w.project + "-litellm-1", "python", "-c", prelude + script}, args...)...)
}

// key creates a workspace virtual key; LiteLLM owns its model grants and budget.
func (w *workspaceInference) key(t *testing.T, name string, body map[string]any) {
	data, _ := json.Marshal(body)
	key := w.litellm(t, "r=urllib.request.Request('http://localhost:4000/key/generate',data=sys.argv[1].encode(),headers=auth)\nprint(json.load(urllib.request.urlopen(r))['key'])", string(data))
	if !strings.HasPrefix(key, "sk-") {
		t.Fatal("LiteLLM key generation failed")
	}
	w.keys[name] = key
}

// bind writes the workspace key file in place; the read-only mount sees the change.
func (w *workspaceInference) bind(t *testing.T, name string) {
	if err := os.WriteFile(filepath.Join(w.dir, "keys", workspace), []byte(w.keys[name]), 0644); err != nil {
		t.Fatal(err)
	}
}

func configureInference(t *testing.T, dir, project string, c *service.Config) *workspaceInference {
	w := &workspaceInference{project: project, dir: dir, keys: map[string]string{}}
	if err := os.Mkdir(filepath.Join(dir, "keys"), 0755); err != nil {
		t.Fatal(err)
	}
	w.key(t, "workspace", map[string]any{"models": []string{"fixture", "fixture-new"}, "metadata": map[string]string{"workspace": workspace}})
	w.key(t, "exhausted", map[string]any{"models": []string{"fixture"}, "max_budget": 0})
	w.bind(t, "workspace")
	catalog := inference.Catalog{DefaultModel: "fixture", Models: map[string]inference.Model{"fixture": {Label: "Fixture", Context: 64000, Output: 4096, Thinking: &inference.Thinking{DefaultLevel: "medium", SupportedLevels: []inference.ThinkingLevel{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}}}}}
	writeJSON(t, filepath.Join(dir, "inference.json"), inference.Config{Version: 1, AllowHTTP: true, Gateways: []string{gatewayOrigin}, Bindings: []inference.Binding{{WorkspaceID: workspace, Gateway: gatewayOrigin, KeyFile: "/etc/multica-sandbox/inference-keys/" + workspace}}, Catalogs: []inference.CatalogBinding{{WorkspaceID: workspace, Catalog: catalog}}})
	c.Command = nil
	c.OpenCode.InferenceFile = "/etc/multica-sandbox/inference.json"
	c.OpenCode.InferenceRelay = &service.RelayConfig{Listen: ":8092", URL: "http://inference-relay:8092"}
	return w
}

// usage waits until LiteLLM's end-user spend records show each end user with the wanted
// {model group, key name}; it returns every {end user, model group, key hash} row.
func (w *workspaceInference) usage(t *testing.T, want map[string][2]string) [][]string {
	t.Helper()
	var rows [][]string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out := dockerTest(t, "exec", w.project+"-litellm-db-1", "psql", "-U", "litellm", "-d", "litellm", "-At", "-F", "|", "-c", `SELECT end_user_id, model_group, api_key FROM "LiteLLM_DailyEndUserSpend"`)
		rows = rows[:0]
		found := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			row := strings.Split(line, "|")
			if len(row) != 3 {
				continue
			}
			rows = append(rows, row)
			if v, ok := want[row[0]]; ok && v[0] == row[1] && row[2] == hashKey(w.keys[v[1]]) {
				found[row[0]] = true
			}
		}
		if len(found) == len(want) {
			return rows
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("LiteLLM lacks trusted attribution %v", want)
	return nil
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (w *workspaceInference) leaked(text string) bool {
	for _, key := range w.keys {
		if strings.Contains(text, key) {
			return true
		}
	}
	return false
}

func managedVolumes(dir string, c service.Config) []map[string]any {
	volumes := []map[string]any{{"type": "bind", "source": filepath.Join(dir, "config.json"), "target": "/etc/multica-sandbox/controller.json", "read_only": true}, {"type": "bind", "source": filepath.Join(dir, "identity.json"), "target": "/etc/multica-sandbox/identity.json", "read_only": true}, {"type": "volume", "source": "identity-secrets", "target": "/identity-secrets", "read_only": true}}
	if c.OpenCode.InferenceFile != "" {
		volumes = append(volumes, map[string]any{"type": "bind", "source": filepath.Join(dir, "inference.json"), "target": "/etc/multica-sandbox/inference.json", "read_only": true}, map[string]any{"type": "bind", "source": filepath.Join(dir, "keys"), "target": "/etc/multica-sandbox/inference-keys", "read_only": true})
	}
	return volumes
}

func selectionCount(t *testing.T, project, model, effort string) int {
	t.Helper()
	var evidence map[string][2]int
	if json.Unmarshal([]byte(dockerTest(t, "exec", project+"-gateway-1", "/identity-example", "evidence")), &evidence) != nil {
		t.Fatal("invalid selection evidence")
	}
	return evidence["selection:"+model+"|"+effort][0]
}

// controllerModelSelections checks discovery, explicit selections and gateway refusals through the relay.
func controllerModelSelections(t *testing.T, api *multica.Client, w *workspaceInference, runtime, agent string) {
	discovered := strings.TrimPrefix(runtimeDiscovery(t, runtime), "managed-inference/")
	run := func(i int, model, want string) string {
		id := fmt.Sprintf("70000000-0000-4000-8000-%012d", i+70)
		issue := fmt.Sprintf("80000000-0000-4000-8000-%012d", i+70)
		sql(t, fmt.Sprintf(`UPDATE agent SET model='managed-inference/%s',thinking_level='high' WHERE id='%s';
 INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Model fixture','todo','member','%s',%d,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, model, agent, issue, workspace, user, 100070+i, agent, id, agent, runtime, issue, user, user))
		waitTask(t, api, id, want)
		return id
	}
	before := selectionCount(t, w.project, discovered, "high")
	run(0, discovered, "completed")
	beforeNew := selectionCount(t, w.project, "fixture-new", "high")
	fresh := run(1, "fixture-new", "completed")
	if selectionCount(t, w.project, discovered, "high") <= before || selectionCount(t, w.project, "fixture-new", "high") <= beforeNew {
		t.Fatal("selected model/reasoning did not reach the provider")
	}
	ungranted := run(2, "ungranted", "failed")
	if !strings.Contains(sql(t, fmt.Sprintf("SELECT error FROM agent_task_queue WHERE id='%s';", ungranted)), "HTTP 403") {
		t.Fatal("gateway model refusal not surfaced in Multica")
	}
	// A changed workspace key applies to the next attempt: the new key's budget is exhausted.
	w.bind(t, "exhausted")
	budget := run(3, discovered, "failed")
	if !strings.Contains(sql(t, fmt.Sprintf("SELECT error FROM agent_task_queue WHERE id='%s';", budget)), "HTTP 422") {
		t.Fatal("gateway budget refusal not surfaced in Multica")
	}
	w.bind(t, "workspace")
	rows := w.usage(t, map[string][2]string{attribution(agent, fresh): {"fixture-new", "workspace"}, attribution(agent, budget): {discovered, "exhausted"}})
	for _, row := range rows {
		if row[0] == attribution(agent, ungranted) && row[1] != "ungranted" {
			t.Fatal("refused model substituted")
		}
	}
	t.Log("real Multica discovered/selected model and reasoning reach LiteLLM through the relay; refusals and a changed workspace key surface as HTTP 403/422 without substitution")
}

func attribution(agent, task string) string {
	return workspace + "/" + agent + "/" + task
}

// restartFailsClosed restarts the controller under a live streaming attempt and checks its grant died.
func restartFailsClosed(t *testing.T, w *workspaceInference, network, controllerName, agentImage, runtime, agent string) {
	id := "a3000000-0000-4000-8000-000000000036"
	issue := "a2000000-0000-4000-8000-000000000036"
	sql(t, fmt.Sprintf(`UPDATE agent SET model='',thinking_level='' WHERE id='%s';
 INSERT INTO issue(id,workspace_id,title,description,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Restart fixture','slow-stream','todo','member','%s',99936,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, agent, issue, workspace, user, agent, id, agent, runtime, issue, user, user))
	owner := sql(t, fmt.Sprintf("SELECT daemon_id FROM agent_runtime WHERE id='%s';", runtime))
	opaque := ""
	eventually(t, "streaming attempt", func() bool {
		container := dockerTest(t, "ps", "-q", "--filter", "label=io.multica-sandbox.owner="+owner)
		if container == "" {
			return false
		}
		opaque = inferenceCredential(t, w, container)
		return opaque != ""
	})
	relay := func(model string, header ...string) string {
		args := []string{"run", "--rm", "--network", network, "--entrypoint", "/bin/sh", "-e", "T=" + opaque, agentImage, "-c", `wget -S -q -O /dev/null --header "Authorization: Bearer $T" --header "Content-Type: application/json" ` + strings.Join(header, " ") + ` --post-data '{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"Fixture reply"}],"user":"spoofed-body","metadata":{"user_id":"spoofed-metadata"}}' http://` + controllerName + `:8092/v1/chat/completions 2>&1 | grep -o 'HTTP/[0-9.]* [0-9][0-9][0-9]' | head -1 | cut -d' ' -f2`}
		return dockerTest(t, args...)
	}
	if status := relay("fixture-new", `--header "X-Litellm-Customer-Id: spoofed-customer"`, `--header "X-Litellm-End-User-Id: spoofed-end-user"`); status != "200" {
		t.Fatalf("live attempt credential refused: %s", status)
	}
	dockerTest(t, "restart", "--time", "5", controllerName)
	eventually(t, "controller ready after restart", func() bool {
		return strings.Count(dockerTest(t, "logs", controllerName), "ready workspaces=") >= 2
	})
	// The restarted relay answers, but the in-memory grant is gone: fail closed.
	if status := relay("fixture"); status != "401" {
		t.Fatalf("relay credential survived controller restart: %q", status)
	}
	eventually(t, "restart recovery", func() bool {
		state := sql(t, fmt.Sprintf("SELECT status FROM agent_task_queue WHERE id='%s';", id))
		return state != "running" && state != "dispatched" && state != "queued"
	})
	rows := w.usage(t, map[string][2]string{attribution(agent, id): {"fixture-new", "workspace"}})
	for _, row := range rows {
		if strings.Contains(row[0], "spoofed") {
			t.Fatal("caller-supplied attribution reached LiteLLM")
		}
	}
	t.Log("caller attribution headers/body cannot spoof LiteLLM end_user; a controller restart revoked the live inference grant")
}

// inferenceCredential checks a live attempt as its own user and returns its opaque provider key.
func inferenceCredential(t *testing.T, w *workspaceInference, container string) string {
	t.Helper()
	ctx := `cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n'; find /workspace /tmp -type f -size -1M -exec cat {} + 2>/dev/null; wget -T 2 -q -O /dev/null http://litellm:4000/health/liveliness 2>/dev/null && echo GATEWAY-REACHABLE; true`
	out, err := execOutput(container, ctx)
	if err != nil {
		return ""
	}
	if w.leaked(out) || strings.Contains(out, "GATEWAY-REACHABLE") {
		t.Fatal("workspace key or gateway route visible inside the attempt")
	}
	var config struct {
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	raw, err := execOutput(container, "cat /workspace/opencode.json")
	if err != nil || json.Unmarshal([]byte(raw), &config) != nil {
		return ""
	}
	opaque := config.Provider["managed-inference"].Options["apiKey"]
	if !strings.HasPrefix(opaque, inference.RelayPrefix) {
		t.Fatal("attempt lacks the opaque inference credential")
	}
	return opaque
}

func execOutput(container, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "/bin/sh", "-c", script).Output()
	return string(out), err
}
