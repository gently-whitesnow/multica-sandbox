//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

type serviceFixture struct {
	project, override string
	t                 *testing.T
}

func (f serviceFixture) compose(args ...string) string {
	f.t.Helper()
	base := []string{"compose", "-p", f.project, "-f", "../compose.yaml", "-f", f.override}
	return dockerTest(f.t, append(base, args...)...)
}
func dockerTest(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}
func prepareService(t *testing.T) serviceFixture {
	t.Helper()
	c := service.Config{Server: "http://127.0.0.1:8080", Daemon: daemon, Image: image, Command: []string{"/bin/sh", "-c", "sleep 3"}, Timeout: "30s"}
	return prepareServiceConfig(t, c)
}
func prepareServiceConfig(t *testing.T, c service.Config) serviceFixture {
	t.Helper()
	dir := t.TempDir()
	f := serviceFixture{fmt.Sprintf("sandbox-service-%d", time.Now().UnixNano()), filepath.Join(dir, "compose.json"), t}
	writeJSON(t, filepath.Join(dir, "config.json"), c)
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token(t)), 0600); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, f.override, map[string]any{
		"services": map[string]any{"controller": map[string]any{
			"network_mode": "container:" + os.Getenv("MULTICA_TEST_SERVER_CONTAINER"),
			"volumes":      []map[string]any{{"type": "bind", "source": filepath.Join(dir, "config.json"), "target": "/etc/multica-sandbox/controller.json", "read_only": true}},
		}}, "secrets": map[string]any{"multica_token": map[string]string{"file": filepath.Join(dir, "token")}},
	})
	t.Cleanup(func() {
		f.compose("down", "-v")
		b := docker.Backend{Owner: daemon}
		if err := b.Reconcile(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.compose("up", "-d", "--no-build")
	return f
}
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout: " + description)
}
func waitTask(t *testing.T, api *multica.Client, id, want string) {
	t.Helper()
	eventually(t, "task "+want, func() bool { got, err := api.Status(context.Background(), id); return err == nil && got == want })
}
func containerService(t *testing.T, api *multica.Client, rt multica.Runtime) {
	if os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("set VERIFY_SERVICE=1 for controller image/Compose tests")
	}
	f := prepareService(t)
	cid := f.compose("ps", "-q", "controller")
	eventually(t, "controller ready", func() bool { return strings.Contains(dockerTest(t, "logs", cid), "ready workspaces=") })
	started := dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid)
	first, second := enqueue(t, rt, 30), enqueue(t, rt, 31)
	firstContainer := waitServiceExecution(t, cid, first)
	waitTask(t, api, first, "completed")
	secondContainer := waitServiceExecution(t, cid, second)
	if firstContainer == secondContainer {
		t.Fatal("environment reused between tasks")
	}
	waitTask(t, api, second, "completed")
	if dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid) != started {
		t.Fatal("controller restarted between tasks")
	}
	if ownedContainers(t) != "" {
		t.Fatal("completed attempts leaked")
	}
	t.Run("exclusive-state", func(t *testing.T) { rejectConcurrent(t, f) })
	t.Run("automatic-restart", func(t *testing.T) { restartService(t, f, cid, api, rt) })
	t.Run("graceful-stop-and-recreate", func(t *testing.T) { recreateService(t, f, cid, api, rt) })
}

func waitServiceExecution(t *testing.T, cid, task string) string {
	t.Helper()
	eventually(t, "execution started", func() bool { return strings.Contains(dockerTest(t, "logs", cid), "started task="+task) })
	got := ownedContainers(t)
	if got == "" {
		t.Fatal("no environment for started attempt")
	}
	return got
}
