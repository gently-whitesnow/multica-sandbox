package opencode

import (
	"context"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func sessionAdapter(workload *stubWorkload) Adapter {
	return Adapter{Server: "https://multica.example.invalid", Issuer: &stubIssuer{}, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Relay: relay.NewGrants(multica.RelayPrefix), RelayURL: "http://multica-relay:8091"}
}

func TestRetainedWorkdirResumesOnlyItsSession(t *testing.T) {
	for name, c := range map[string]struct {
		retained, prior string
		reused, resume  bool
	}{
		"resumed":       {"volume", "volume", true, true},
		"forged prior":  {"volume", "other", true, false},
		"created now":   {"volume", "volume", false, false},
		"not retained":  {"", "volume", false, false},
		"invalid prior": {"volume", "volume", true, false},
	} {
		t.Run(name, func(t *testing.T) {
			workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 1), retained: c.retained, reused: c.reused}
			a := sessionAdapter(workload)
			task := relayTask(t)
			task.PriorSessionID, task.PriorWorkDir = "ses_prior", c.prior
			if name == "invalid prior" {
				task.PriorSessionID = "--help"
			}
			run, err := a.Start(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			env := <-workload.envs
			if workload.workdir != (execution.Workdir{Workspace: task.WorkspaceID, Agent: task.AgentID, Issue: task.IssueID, Prior: c.prior}) {
				t.Fatalf("requested workdir: %+v", workload.workdir)
			}
			prompt := string(workload.files["/workspace/prompt.txt"])
			if (env["MULTICA_SANDBOX_RESUME"] == "ses_prior") != c.resume || strings.Contains(prompt, "You're resuming the prior session") != c.resume ||
				strings.Contains(prompt, "## Session Continuity Notice") == c.resume || (env["OPENCODE_DB"] == SessionDB) != (c.retained != "") {
				t.Fatalf("resume %t: env %v prompt %q", c.resume, env, prompt)
			}
			if err := run.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if result := run.Result(); result.WorkDir != c.retained || result.Disposable != (c.retained == "") || result.RetiredSessionID != "" {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}

func TestRefusedResumeRetriesFreshOnce(t *testing.T) {
	workload := &stubWorkload{files: map[string][]byte{}, envs: make(chan map[string]string, 2), retained: "volume", reused: true, failFirst: true}
	a := sessionAdapter(workload)
	task := relayTask(t)
	task.PriorSessionID, task.PriorWorkDir = "ses_prior", "volume"
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	first, second := <-workload.envs, <-workload.envs
	if first["MULTICA_SANDBOX_RESUME"] != "ses_prior" || second["MULTICA_SANDBOX_RESUME"] != "" || second["OPENCODE_DB"] != SessionDB {
		t.Fatalf("retry environment: %v then %v", first, second)
	}
	if prompt := string(workload.files["/workspace/prompt.txt"]); !strings.Contains(prompt, "## Session Continuity Notice") || strings.Contains(prompt, "You're resuming") {
		t.Fatalf("fresh retry prompt: %q", prompt)
	}
	if err := run.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := run.Result(); result.RetiredSessionID != "ses_prior" || result.WorkDir != "volume" {
		t.Fatalf("result: %+v", result)
	}
}
