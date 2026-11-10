//go:build upstream

package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func liveSteeringCrash(t *testing.T, api *multica.Client, s *supplements, f serviceFixture, cid, controller string) {
	s, f.t = steeringScope(t, s), t
	before := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE issue_id='%s';", s.task.IssueID))
	id := steeringFollowup(t, s, "live-steering-cancel-check")
	sql(t, fmt.Sprintf("UPDATE agent_task_queue SET max_attempts=1 WHERE id='%s';", id))
	var workload string
	eventually(t, "crash fixture tool active", func() bool {
		workload = liveSteeringMarker(t, controller, "/workspace/.steering-cancel-active")
		return workload != ""
	})
	session := sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') FROM agent_task_queue WHERE id='%s';", id))
	volume := sql(t, fmt.Sprintf("SELECT coalesce(work_dir,'') FROM agent_task_queue WHERE id='%s';", id))
	if !strings.HasPrefix(session, "ses_") || !strings.HasPrefix(volume, "multica-sandbox-session-") {
		t.Fatal("crash fixture lacks pinned native session/workdir")
	}
	quarantine := steeringQuarantines(t, volume)
	crashed := *s
	crashed.task.ID, crashed.path = id, "/api/issues/"+s.task.IssueID+"/tasks/"+id+"/supplements"
	comment := crashed.create(4, "live-steering-guidance-never", 201)
	eventually(t, "claim before controller SIGKILL", func() bool {
		return sql(t, fmt.Sprintf("SELECT status FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", id, comment)) == "delivering"
	})
	status(t, api, id, "running")
	steeringCrashController(t, f, cid)
	waitTask(t, api, id, "failed")
	if reason := sql(t, fmt.Sprintf("SELECT failure_reason FROM agent_task_queue WHERE id='%s';", id)); reason != "runtime_recovery" {
		t.Fatalf("controller crash did not use native recovery: %s", reason)
	}
	crashed.receipt(comment, "failed|turn_ended|1")
	eventually(t, "orphaned workload removed", func() bool {
		return !strings.Contains(dockerTest(t, "ps", "-a", "-q", "--no-trunc", "--filter", "label=io.multica-sandbox.owner="+controller), workload)
	})
	fresh := steeringFollowup(t, s, "live-steering-resume-check")
	waitTask(t, api, fresh, "completed")
	steeringCrashResult(t, s, id, fresh, session, volume, quarantine)
	if got := sql(t, fmt.Sprintf("SELECT count(*)::int-2 FROM agent_task_queue WHERE issue_id='%s';", s.task.IssueID)); got != before {
		t.Fatalf("controller recovery spawned phantom/retry tasks: before=%s after-minus-two=%s", before, got)
	}
	crashed.receipt(comment, "failed|turn_ended|1")
}

func steeringCrashController(t *testing.T, f serviceFixture, cid string) {
	t.Helper()
	if f.compose("ps", "-q", "controller") != cid {
		t.Fatal("controller container no longer belongs to crash fixture")
	}
	started := dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid)
	ready := strings.Count(dockerTest(t, "logs", cid), readyEvent)
	dockerTest(t, "kill", "--signal", "KILL", cid)
	// A manual Docker kill may suppress restart policy; preserve this container
	// and its controller state volume while explicitly starting it again.
	f.compose("up", "-d", "--no-build", "controller")
	if f.compose("ps", "-q", "controller") != cid {
		t.Fatal("crash recovery unexpectedly replaced controller container/state")
	}
	eventually(t, "controller restarted and ready after SIGKILL", func() bool {
		return dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid) != started &&
			strings.Count(dockerTest(t, "logs", cid), readyEvent) > ready
	})
}

func steeringQuarantines(t *testing.T, volume string) string {
	t.Helper()
	return dockerTest(t, "run", "--rm", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user="+docker.AttemptUser,
		"--mount", "type=volume,source="+volume+",target=/fixture,readonly,volume-nocopy", "--entrypoint", "/bin/sh", officialImage,
		"-c", "set -eu; test -s /fixture/.multica-sandbox/opencode.db; find /fixture/.multica-sandbox -mindepth 2 -maxdepth 2 -name opencode.db | sort")
}

func cleanupSteeringSessions(t *testing.T, controller string) {
	t.Helper()
	volumes := dockerTest(t, "volume", "ls", "-q", "--filter", "label=io.multica-sandbox.owner="+controller, "--filter", "label=io.multica-sandbox.session")
	for _, volume := range strings.Fields(volumes) {
		dockerTest(t, "volume", "rm", volume)
	}
}

func steeringCrashResult(t *testing.T, s *supplements, crashed, fresh, session, volume, quarantine string) {
	t.Helper()
	next := sql(t, fmt.Sprintf("SELECT coalesce(session_id,'')||'|'||coalesce(work_dir,'') FROM agent_task_queue WHERE id='%s';", fresh))
	if !strings.HasPrefix(next, "ses_") || strings.HasPrefix(next, session+"|") || !strings.HasSuffix(next, "|"+volume) {
		t.Fatalf("restart did not retain workdir and replace orphaned native session: %s", next)
	}
	if got := steeringQuarantines(t, volume); got == quarantine || got == "" {
		t.Fatal("controller restart did not quarantine the interrupted native database")
	}
	result := sql(t, fmt.Sprintf("SELECT result::text FROM agent_task_queue WHERE id='%s';", fresh))
	if strings.Contains(result, "STALE-GUIDANCE") || !strings.Contains(result, "Fresh after interrupted owner steering guidance one two") {
		t.Fatalf("controller restart lost continuity notice or leaked old guidance: %s", result)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM task_supplement WHERE task_id='%s';", fresh)); got != "0" {
		t.Fatalf("new attempt inherited old native receipts: %s", got)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE parent_task_id='%s';", crashed)); got != "0" {
		t.Fatalf("max_attempts=1 recovery still queued retry tasks: %s", got)
	}
}
