//go:build upstream

package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const user = "10000000-0000-4000-8000-000000000001"
const workspace = "10000000-0000-4000-8000-000000000002"
const daemon = "10000000-0000-4000-8000-000000000003"

func sql(t *testing.T, query string) string {
	t.Helper()
	container := os.Getenv("MULTICA_TEST_DB_CONTAINER")
	if !strings.HasPrefix(container, "multica-sandbox-contract-") {
		t.Fatal("disposable fixture container required; run scripts/test-upstream.sh")
	}
	cmd := exec.Command("docker", "exec", "-i", container, "psql", "-U", "contract", "-d", "contract", "-At", "-v", "ON_ERROR_STOP=1")
	cmd.Stdin = strings.NewReader(query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture SQL: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}
func token(t *testing.T) string {
	t.Helper()
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		t.Fatal("fixture JWT secret required")
	}
	encode := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{"sub": user, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
	data := encode([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + encode(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return data + "." + encode(mac.Sum(nil))
}
func fixture(t *testing.T) (*multica.Client, multica.Runtime) {
	t.Helper()
	sql(t, fmt.Sprintf(`INSERT INTO "user" (id,name,email) VALUES ('%s','Contract fixture','contract@example.invalid');
 INSERT INTO workspace (id,name,slug) VALUES ('%s','Contract fixture','sandbox-contract');
 INSERT INTO member (workspace_id,user_id,role) VALUES ('%s','%s','owner');`, user, workspace, workspace, user))
	api, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
	if err != nil {
		t.Fatal(err)
	}
	p := controller.Probe{API: api}
	rt, _, err := p.Connect(context.Background(), workspace, daemon)
	if err != nil {
		t.Fatal(err)
	}
	return api, rt
}
func enqueue(t *testing.T, rt multica.Runtime, n int) string {
	t.Helper()
	id := fmt.Sprintf("20000000-0000-4000-8000-%012d", n)
	agent := fmt.Sprintf("30000000-0000-4000-8000-%012d", n)
	issue := fmt.Sprintf("40000000-0000-4000-8000-%012d", n)
	sql(t, fmt.Sprintf(`INSERT INTO agent(id,workspace_id,name,runtime_mode,runtime_id,owner_id,status) VALUES('%s','%s','Probe fixture %d','local','%s','%s','idle');
 INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Probe fixture','in_progress','member','%s',%d,'agent','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s');`, agent, workspace, n, rt.ID, user, issue, workspace, user, n, agent, id, agent, rt.ID, issue, user, user))
	return id
}
func status(t *testing.T, api *multica.Client, id, want string) {
	t.Helper()
	got, err := api.Status(context.Background(), id)
	if err != nil || got != want {
		t.Fatalf("status = %q, %v; want %s", got, err, want)
	}
}

func TestUpstreamLifecycle(t *testing.T) {
	api, rt := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if got, err := api.Claim(ctx, rt.ID); err != nil || got != nil {
		t.Fatalf("empty claim: %v %v", got, err)
	}
	for n, fail := range []bool{false, true} {
		id := enqueue(t, rt, n+1)
		p := controller.Probe{API: api, Interval: 20 * time.Millisecond, Duration: 60 * time.Millisecond, Fail: fail}
		if err := p.Run(ctx, rt.ID); err != nil {
			t.Fatal(err)
		}
		want := "completed"
		if fail {
			want = "failed"
		}
		status(t, api, id, want)
		if got := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message WHERE task_id='%s';", id)); got != "1" {
			t.Fatalf("message count: %s", got)
		}
		t.Logf("%s: claimed, lease renewed, started, message stored, %s", id, want)
	}
	t.Run("cancellation", func(t *testing.T) {
		id := enqueue(t, rt, 3)
		p := controller.Probe{API: api, Interval: 20 * time.Millisecond, Duration: time.Second, Observe: func(event, task string) {
			if event != "started" {
				return
			}
			req, _ := http.NewRequestWithContext(ctx, "POST", os.Getenv("MULTICA_TEST_URL")+"/api/tasks/"+task+"/cancel", bytes.NewBufferString("{}"))
			req.Header.Set("Authorization", "Bearer "+token(t))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Workspace-ID", workspace)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("cancel HTTP %d", res.StatusCode)
			}
		}}
		if err := p.Run(ctx, rt.ID); err != nil {
			t.Fatal(err)
		}
		status(t, api, id, "cancelled")
	})
	t.Run("rejected-start", func(t *testing.T) {
		id := enqueue(t, rt, 4)
		task, err := api.Claim(ctx, rt.ID)
		if err != nil || task == nil {
			t.Fatalf("claim: %v", err)
		}
		stale := *task
		stale.DispatchedAt = "2000-01-01T00:00:00Z"
		if err := api.Start(ctx, stale); err == nil {
			t.Fatal("stale claim accepted")
		}
		status(t, api, id, "dispatched")
		if err := api.Start(ctx, *task); err != nil {
			t.Fatal(err)
		}
		if err := api.Start(ctx, *task); err != nil {
			t.Fatalf("safe start replay: %v", err)
		}
		if err := api.Complete(ctx, id); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("restart-reconciliation", func(t *testing.T) {
		for n, want := range []string{"dispatched", "running"} {
			id := enqueue(t, rt, 5+n)
			task, err := api.Claim(ctx, rt.ID)
			if err != nil || task == nil {
				t.Fatalf("claim: %v", err)
			}
			if want == "running" {
				if err := api.Start(ctx, *task); err != nil {
					t.Fatal(err)
				}
			}
			status(t, api, id, want)
			fresh, err := multica.New(os.Getenv("MULTICA_TEST_URL"), token(t))
			if err != nil {
				t.Fatal(err)
			}
			p := controller.Probe{API: fresh}
			next, recovered, err := p.Connect(ctx, workspace, daemon)
			if err != nil || next.ID != rt.ID || recovered.Orphaned != 1 {
				t.Fatalf("recovery: runtime=%s count=%d err=%v", next.ID, recovered.Orphaned, err)
			}
			status(t, api, id, "failed")
			again, err := fresh.Recover(ctx, rt.ID)
			if err != nil || again.Orphaned != 0 {
				t.Fatalf("recovery replay: %+v %v", again, err)
			}
		}
	})
	t.Run("process-crash", func(t *testing.T) { restartProcess(t, api, rt, false) })
	t.Run("containers", func(t *testing.T) { containerLifecycle(t, api, rt) })
	t.Run("server-owned-retry", func(t *testing.T) { serverRetry(t, api, rt) })
	t.Run("controller-service", func(t *testing.T) { containerService(t, api, rt) })
}
