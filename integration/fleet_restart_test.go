//go:build upstream

package integration

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

func fleetRestart(t *testing.T) {
	if os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("set VERIFY_SERVICE=1")
	}
	c := service.Config{Server: "http://127.0.0.1:8080", Workspaces: "all-accessible", Concurrency: 2, Daemon: daemon, Image: image, Command: []string{"/bin/sh", "-c", "sleep 12"}, Timeout: "30s"}
	f := prepareServiceConfig(t, c)
	cid := f.compose("ps", "-q", "controller")
	eventually(t, "fleet ready", func() bool { return strings.Contains(dockerTest(t, "logs", cid), "ready workspaces=") })
	rt := multica.Runtime{ID: sql(t, fmt.Sprintf("SELECT id FROM agent_runtime WHERE workspace_id='%s' AND daemon_id='%s';", fleetWorkspace(2), daemon))}
	task := enqueueWorkspace(t, rt, 201, fleetWorkspace(2))
	old := waitServiceExecution(t, cid, task)
	sql(t, fmt.Sprintf("DELETE FROM member WHERE workspace_id='%s' AND user_id='%s';", fleetWorkspace(2), user))
	dockerTest(t, "exec", cid, "/bin/kill", "-QUIT", "1")
	eventually(t, "fleet restarted", func() bool { return strings.Count(dockerTest(t, "logs", cid), "ready workspaces=") >= 2 })
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
