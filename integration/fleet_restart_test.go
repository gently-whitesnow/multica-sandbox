//go:build upstream

package integration

import (
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

func fleetRestart(t *testing.T) {
	if os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("set VERIFY_SERVICE=1")
	}
	c := service.Config{Server: "http://127.0.0.1:8080", Daemon: daemon, Image: image, Command: []string{"/bin/sh", "-c", "sleep 12"}, Timeout: "30s"}
	gate := t.TempDir()
	f := prepareServiceConfig(t, c, func(controller map[string]any) { fleetCleanupGate(t, gate, controller) })
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(gate, "release"), nil, 0600) })
	cid := f.compose("ps", "-q", "controller")
	controllerDiagnostics(t, cid)
	began := time.Now()
	eventually(t, "fleet ready", func() bool { return strings.Contains(dockerTest(t, "logs", cid), readyEvent) })
	t.Logf("fleet ready %s after compose up", time.Since(began).Round(time.Millisecond))
	rt := multica.Runtime{ID: sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE workspace_id='%s' AND daemon_id='%s';", fleetWorkspace(2), daemon))}
	task := enqueueWorkspace(t, rt, 201, fleetWorkspace(2))
	old := waitServiceExecution(t, cid, task)
	sql(t, fmt.Sprintf("DELETE FROM member WHERE workspace_id='%s' AND user_id='%s';", fleetWorkspace(2), user))
	if err := os.WriteFile(filepath.Join(gate, "victim"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	started := dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid)
	dockerTest(t, "exec", cid, "/bin/kill", "-QUIT", "1")
	began = time.Now()
	eventually(t, "Docker restarted fleet process", func() bool {
		return dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid) != started
	})
	fleetReleaseCleanup(t, gate, cid)
	// Readiness includes orphan removal, whose production budget is 3 minutes;
	// the ordinary 30-second assertion incorrectly included that whole phase.
	eventuallyWithin(t, "fleet reconciled and ready", docker.RemoveTimeout+30*time.Second, func() bool { return strings.Count(dockerTest(t, "logs", cid), readyEvent) >= 2 })
	t.Logf("fleet ready again %s after SIGQUIT", time.Since(began).Round(time.Millisecond))
	if strings.Contains(ownedContainers(t), old) {
		t.Fatal("removed workspace orphan survived restart")
	}
	if !strings.Contains(dockerTest(t, "exec", cid, "cat", "/var/lib/multica-sandbox/runtimes.json"), rt.ID) {
		t.Fatal("removed workspace registry lost")
	}
	api, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	peer := multica.Runtime{ID: sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE workspace_id='%s' AND daemon_id='%s';", fleetWorkspace(3), daemon))}
	next := enqueueWorkspace(t, peer, 202, fleetWorkspace(3))
	waitTask(t, api, next, "completed")
	sql(t, fmt.Sprintf("INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner');", fleetWorkspace(2), user))
	waitTask(t, api, task, "failed")
}

// A fixture-only CLI shim holds removal of exactly this test's orphan. It never
// blocks another resource or changes the engine, disk, or production controller.
func fleetCleanupGate(t *testing.T, dir string, controller map[string]any) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = rm ] && [ "$2" = -fv ] && [ -f /fleet-gate/victim ] && [ "$3" = "$(cat /fleet-gate/victim)" ]; then
 : > /tmp/fleet-cleanup-blocked
 while [ ! -f /fleet-gate/release ]; do sleep 0.1; done
fi
exec /usr/local/bin/docker "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	controller["volumes"] = append(controller["volumes"].([]map[string]any), map[string]any{"type": "bind", "source": dir, "target": "/fleet-gate", "read_only": true})
	controller["environment"] = map[string]string{"PATH": "/fleet-gate:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
}

func fleetReleaseCleanup(t *testing.T, dir, cid string) {
	t.Helper()
	eventually(t, "orphan removal held by fixture gate", func() bool {
		err := exec.Command("docker", "exec", cid, "test", "-f", "/tmp/fleet-cleanup-blocked").Run()
		return err == nil
	})
	// Reproduce the old failure deterministically while still inside the backend's
	// supported removal budget. Readiness must stay false until the gate releases.
	until := time.Now().Add(31 * time.Second)
	for time.Now().Before(until) {
		if strings.Count(dockerTest(t, "logs", cid), readyEvent) != 1 {
			t.Fatal("fleet became ready before orphan reconciliation")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("orphan cleanup held beyond the old 30-second assertion, within its 3-minute backend budget")
}
