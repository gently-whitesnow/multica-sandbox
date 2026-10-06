//go:build upstream

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func modelRequest(t *testing.T, method, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, os.Getenv("MULTICA_TEST_URL")+path, bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.Header.Set("X-Workspace-ID", workspace)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("fixture API HTTP %d", response.StatusCode)
	}
	var out map[string]any
	if json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&out) != nil {
		t.Fatal("invalid fixture API response")
	}
	return out
}
func runtimeDiscovery(t *testing.T, runtime string) string {
	result := modelRequest(t, "POST", "/api/runtimes/"+runtime+"/models?force=true")
	id, ok := result["id"].(string)
	if !ok {
		t.Fatal("discovery request missing")
	}
	eventually(t, "runtime model discovery", func() bool {
		result = modelRequest(t, "GET", "/api/runtimes/"+runtime+"/models/"+id)
		return result["status"] != "pending" && result["status"] != "running"
	})
	var models []multica.ModelEntry
	data, _ := json.Marshal(result["models"])
	if result["status"] != "completed" || result["supported"] != true || json.Unmarshal(data, &models) != nil || len(models) != 1 {
		t.Fatalf("runtime discovery did not complete: %v %v", result["status"], result["error"])
	}
	m := models[0]
	if m.ID != "managed-inference/fixture" || m.Provider != "managed-inference" || m.Label != "Fixture" || !m.Default || m.Thinking == nil || m.Thinking.DefaultLevel != "medium" || len(m.Thinking.SupportedLevels) != 2 || m.Thinking.SupportedLevels[1].Value != "high" {
		t.Fatalf("catalog metadata not published to Multica: %+v", m)
	}
	// The picker's normal path is served from Multica's runtime-scoped cache.
	if cached := modelRequest(t, "POST", "/api/runtimes/"+runtime+"/models"); cached["status"] != "completed" || cached["cached"] != true {
		t.Fatal("completed catalog not cached by Multica")
	}
	t.Log("unmodified upstream discovery completes with the workspace catalog for the runtime and caches it")
	return m.ID
}
