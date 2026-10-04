//go:build upstream

package integration

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func restartProcess(t *testing.T, api *multica.Client, rt multica.Runtime) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "probe")
	build := exec.Command("go", "build", "-o", binary, "../cmd/sandbox-probe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	id := enqueue(t, rt, 7)
	args := []string{"-server", os.Getenv("MULTICA_TEST_URL"), "-workspace", workspace, "-daemon", daemon, "-lock", filepath.Join(dir, "controller.lock"), "-duration", "1h", "-interval", "100ms"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "MULTICA_PROBE_TOKEN="+token(t))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	started := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "started task=") {
				started <- true
				return
			}
		}
		started <- false
	}()
	select {
	case ok := <-started:
		if !ok {
			t.Fatal("probe exited before start")
		}
	case <-ctx.Done():
		t.Fatal("probe did not start")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	status(t, api, id, "running")
	restart := exec.CommandContext(ctx, binary, append(args, "-recover-only")...)
	restart.Env = cmd.Env
	out, err := restart.CombinedOutput()
	if err != nil {
		t.Fatalf("restart: %v %s", err, out)
	}
	if !strings.Contains(string(out), "orphaned=1") {
		t.Fatalf("restart failed to reconcile: %s", out)
	}
	status(t, api, id, "failed")
	t.Log("SIGKILL released the instance lock; a fresh process recovered the orphan under the same daemon identity")
}
