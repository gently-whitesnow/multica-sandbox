//go:build upstream

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func rejectConcurrent(t *testing.T, f serviceFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "compose", "-p", f.project, "-f", "../compose.yaml", "-f", f.override, "run", "--rm", "--no-deps", "controller").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "controller identity already in use") {
		t.Fatalf("duplicate controller: %v %s", err, out)
	}
}
func restartService(t *testing.T, f serviceFixture, cid string, api *multica.Client, rt multica.Runtime) {
	task := enqueue(t, rt, 32)
	waitTask(t, api, task, "running")
	old := waitServiceExecution(t, cid, task)
	dockerTest(t, "exec", cid, "/bin/kill", "-QUIT", "1")
	eventually(t, "Docker automatic restart", func() bool { return dockerTest(t, "inspect", "--format", "{{.RestartCount}}", cid) != "0" })
	waitTask(t, api, task, "failed")
	if strings.Contains(ownedContainers(t), old) {
		t.Fatal("old attempt survived recovery")
	}
	next := enqueue(t, rt, 33)
	waitTask(t, api, next, "completed")
	if ownedContainers(t) != "" {
		t.Fatal("next attempt leaked")
	}
}
func recreateService(t *testing.T, f serviceFixture, cid string, api *multica.Client, rt multica.Runtime) {
	task := enqueue(t, rt, 34)
	waitTask(t, api, task, "running")
	waitServiceExecution(t, cid, task)
	before := dockerTest(t, "exec", cid, "cat", "/var/lib/multica-sandbox/identity.json")
	f.compose("stop")
	if ownedContainers(t) != "" {
		t.Fatal("graceful stop left execution alive")
	}
	if dockerTest(t, "inspect", "--format", "{{.State.ExitCode}}", cid) != "0" {
		t.Fatal("graceful stop failed")
	}
	f.compose("up", "-d", "--no-build", "--force-recreate")
	fresh := f.compose("ps", "-q", "controller")
	if fresh == cid {
		t.Fatal("expected a new controller container")
	}
	eventually(t, "recreated controller ready", func() bool { return strings.Contains(dockerTest(t, "logs", fresh), "ready runtime=") })
	after := dockerTest(t, "exec", fresh, "cat", "/var/lib/multica-sandbox/identity.json")
	if before != after {
		t.Fatal("identity changed during recreation")
	}
	status(t, api, task, "failed")
	next := enqueue(t, rt, 35)
	waitTask(t, api, next, "completed")
}
