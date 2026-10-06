package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const claimToken = "mat_0123456789abcdef0123456789abcdef01234567"

func relayTask(t *testing.T) multica.Task {
	t.Helper()
	task := safeTask()
	task.IssueID = "60000000-0000-4000-8000-000000000001"
	task.TriggerCommentID = "70000000-0000-4000-8000-000000000001"
	task.TriggerCommentContent = "Please summarize."
	task.Agent.Name = "Fixture agent"
	if err := json.Unmarshal([]byte(`"`+claimToken+`"`), &task.AuthToken); err != nil {
		t.Fatal(err)
	}
	return task
}

func authorized(grants *relay.Grants, bearer string) bool {
	req := httptest.NewRequest("GET", "/api/issues/x", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	lease, ok := grants.Authorize(req)
	if ok {
		lease.Release()
	}
	return ok
}

func TestRelayAttemptDeliversOnlyOpaqueCredential(t *testing.T) {
	grants := relay.NewGrants()
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1)}
	a := Adapter{Issuer: &stubIssuer{}, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: grants, RelayURL: "http://multica-relay:8091"}
	run, err := a.Start(context.Background(), relayTask(t))
	if err != nil {
		t.Fatal(err)
	}
	env := <-workload.envs
	opaque := env["MULTICA_TOKEN"]
	if !strings.HasPrefix(opaque, relay.Prefix) || env["MULTICA_SERVER_URL"] != "http://multica-relay:8091" || env["MULTICA_TASK_ID"] != safeTask().ID || env["MULTICA_AGENT_NAME"] != "Fixture agent" || !authorized(grants, opaque) {
		t.Fatalf("unexpected agent environment: %v", env)
	}
	for name, value := range env {
		if strings.Contains(value, claimToken) {
			t.Fatalf("claim token in %s", name)
		}
	}
	for path, data := range workload.files {
		if strings.Contains(string(data), claimToken) || strings.Contains(string(data), opaque) {
			t.Fatalf("credential projected into %s", path)
		}
	}
	prompt, brief := string(workload.files["/workspace/prompt.txt"]), string(workload.files[BriefPath])
	if !strings.Contains(prompt, "multica issue comment add 60000000-0000-4000-8000-000000000001 --parent 70000000-0000-4000-8000-000000000001 --content-file ./reply.md") || !strings.Contains(brief, "Always Use the `multica` CLI") {
		t.Fatal("upstream instructions missing")
	}
	var config struct{ Permission map[string]string }
	if json.Unmarshal(workload.files["/workspace/opencode.json"], &config) != nil || config.Permission["bash"] != "allow" || config.Permission["webfetch"] != "" || config.Permission["*"] != "deny" {
		t.Fatal("CLI access requires local bash only")
	}
	if err := run.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authorized(grants, opaque) {
		t.Fatal("ended attempt credential still authorized")
	}
}

func TestRelayRequiresTaskToken(t *testing.T) {
	for _, token := range []string{``, `"mul_personal"`} {
		task := relayTask(t)
		task.AuthToken = multica.Secret{}
		if token != "" {
			_ = json.Unmarshal([]byte(token), &task.AuthToken)
		}
		workload := &stubWorkload{}
		a := Adapter{Issuer: &stubIssuer{}, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: relay.NewGrants(), RelayURL: "http://multica-relay:8091"}
		_, err := a.Start(context.Background(), task)
		var rejected *execution.RejectedError
		if !errors.As(err, &rejected) || workload.writes != 0 || workload.runs != 0 {
			t.Fatalf("missing task token not rejected before workload start: %v", err)
		}
	}
}
