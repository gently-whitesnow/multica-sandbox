//go:build upstream

package integration

import (
	"path/filepath"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

func inferenceFixture(t *testing.T, services map[string]any) {
	root, err := filepath.Abs("../examples/identity-mcp")
	if err != nil {
		t.Fatal(err)
	}
	services["seed"] = map[string]any{"environment": map[string]string{"ROTATION_FIXTURE": "1", "INFERENCE_FIXTURE": "1"}}
	services["keycloak"] = map[string]any{"mem_limit": "512m", "environment": map[string]string{"JAVA_OPTS_KC_HEAP": "-Xms64m -Xmx256m"}}
	services["gateway"] = map[string]any{"networks": map[string]any{"fixture": map[string]any{"aliases": []string{"fixture-provider"}}, "execution": map[string]any{}}, "environment": map[string]string{"INFERENCE_FIXTURE": "1"}}
	services["litellm"] = map[string]any{
		"mem_limit": "768m", "cpus": 0.5,
		"image":    "ghcr.io/berriai/litellm:v1.104.0@sha256:625981c83410a3ea68eb0697590a57ec1d764d634514d54fa5db0591077ee839",
		"networks": []string{"fixture", "execution"}, "depends_on": map[string]any{"gateway": map[string]string{"condition": "service_healthy"}},
		"entrypoint":  []string{"/bin/sh", "-c", `export LITELLM_MASTER_KEY="$$(cat /secrets/admin)"; export FIXTURE_UPSTREAM_KEY="$$(cat /secrets/upstream)"; exec litellm --config /fixture/litellm.yaml --port 4000`},
		"environment": map[string]string{"PYTHONPATH": "/fixture", "LITELLM_LOCAL_MODEL_COST_MAP": "True"},
		"volumes":     []string{"credentials:/secrets:ro", root + "/litellm.yaml:/fixture/litellm.yaml:ro", root + "/litellm_auth.py:/fixture/litellm_auth.py:ro"},
		"healthcheck": map[string]any{"test": []string{"CMD", "python", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:4000/health/liveliness')"}, "interval": "2s", "timeout": "2s", "retries": 60},
	}
}
func configureServiceInference(t *testing.T, dir, project string, c *service.Config, binding identity.Config) {
	// Use independent inference issuance and recipient; MCP credentials remain unchanged.
	binding.MCP = nil
	binding.Bindings[0].ClientID = "example-inference"
	binding.Bindings[0].Subject = "30000000-0000-4000-8000-000000000002"
	binding.Bindings[0].SecretFile = "/identity-secrets/inference"
	writeJSON(t, filepath.Join(dir, "inference-identity.json"), binding)
	target := inference.Target{Gateway: inference.Gateway{URL: "http://litellm:4000/v1", Issuer: "fixture"}}
	writeJSON(t, filepath.Join(dir, "inference.json"), inference.Config{Version: 1, AllowHTTP: true, IdentityFile: "/etc/multica-sandbox/inference-identity.json", Gateways: []inference.Gateway{target.Gateway}, Bindings: []inference.Binding{{WorkspaceID: workspace, AgentID: binding.Bindings[0].AgentID, Target: target}}, Catalogs: []inference.CatalogBinding{{WorkspaceID: workspace, AgentID: binding.Bindings[0].AgentID, Catalog: inference.Catalog{DefaultModel: "fixture", Models: map[string]inference.Model{"fixture": {Context: 64000, Output: 4096}}}}}})
	c.Command = nil
	c.OpenCode.InferenceFile = "/etc/multica-sandbox/inference.json"
	c.OpenCode.Peers = append(c.OpenCode.Peers, project+"-litellm-1")
}
func managedVolumes(dir string, c service.Config) []map[string]any {
	volumes := []map[string]any{{"type": "bind", "source": filepath.Join(dir, "config.json"), "target": "/etc/multica-sandbox/controller.json", "read_only": true}, {"type": "bind", "source": filepath.Join(dir, "identity.json"), "target": "/etc/multica-sandbox/identity.json", "read_only": true}, {"type": "volume", "source": "identity-secrets", "target": "/identity-secrets", "read_only": true}}
	if c.OpenCode.InferenceFile != "" {
		for _, name := range []string{"inference.json", "inference-identity.json"} {
			volumes = append(volumes, map[string]any{"type": "bind", "source": filepath.Join(dir, name), "target": "/etc/multica-sandbox/" + name, "read_only": true})
		}
	}
	return volumes
}
