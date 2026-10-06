//go:build upstream

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const image = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

func containerLifecycle(t *testing.T, api *multica.Client, rt multica.Runtime) {
	for n, scenario := range []string{"complete", "fail", "cancel", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			task := enqueue(t, rt, 10+n)
			script := "exit 0"
			want := "completed"
			if scenario == "fail" {
				script = "exit 7"
				want = "failed"
			}
			if scenario == "cancel" || scenario == "timeout" {
				script = "sleep 60"
				want = "failed"
			}
			b := &docker.Backend{Image: image, Owner: daemon, Command: []string{"/bin/sh", "-c", script}}
			t.Cleanup(func() {
				if err := b.Reconcile(context.Background()); err != nil {
					t.Error(err)
				}
			})
			p := controller.Probe{API: api, Backend: b, Interval: 100 * time.Millisecond, Duration: time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if scenario == "cancel" {
				want = "cancelled"
				p.Observe = func(event, id string) {
					if event == "started" {
						cancelTask(t, ctx, id)
					}
				}
			}
			if err := p.Run(ctx, rt.ID); err != nil {
				t.Fatal(err)
			}
			status(t, api, task, want)
			if reason := sql(t, fmt.Sprintf("SELECT coalesce(failure_reason,'') FROM agent_task_queue WHERE id='%s';", task)); scenario == "timeout" && reason != "timeout" || scenario == "fail" && (reason == "" || reason == "execution_failed") {
				t.Fatalf("failure reason not usable by Multica retry policy: %q", reason)
			}
			if got := ownedContainers(t); got != "" {
				t.Fatalf("terminal task retained containers: %s", got)
			}
		})
	}
	t.Run("container-process-crash", func(t *testing.T) { restartProcess(t, api, rt, true) })
}
func cancelTask(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "POST", os.Getenv("MULTICA_TEST_URL")+"/api/tasks/"+id+"/cancel", bytes.NewBufferString("{}"))
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Workspace-ID", workspace)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("cancel: HTTP %d", res.StatusCode)
	}
}
func ownedContainers(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=io.multica-sandbox.owner="+daemon).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
