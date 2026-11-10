//go:build upstream

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/examples/identity-mcp/faultproxy"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// liveSteeringFaults runs the real controller, the patched native server and the
// owned OpenCode 2.2.5 service behind the fault proxy. Every step is gated on an
// observed state (native receipt, OpenCode inbox, tool marker), never on a sleep.
func liveSteeringFaults(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") != "1" || os.Getenv("VERIFY_OPENCODE") != "1" || os.Getenv("VERIFY_SERVICE") != "1" {
		t.Skip("requires the opt-in native patches and real service/OpenCode fixtures")
	}
	e := startSteering(t, api, true, func(multica.Runtime) {})
	t.Run("ambiguous-claim-and-ack", func(t *testing.T) { steeringAmbiguousOutcomes(t, api, e) })
	t.Run("stable-id-admission-retry", func(t *testing.T) { steeringAdmissionRetry(t, api, e) })
	t.Run("vanished-receipt-large-history", func(t *testing.T) { steeringVanishedReceipt(t, api, e) })
	t.Run("consumed-ack-controller-crash", func(t *testing.T) { steeringConsumedCrash(t, api, e) })
}

// steeringRun is one fixture task held open by the scripted model.
type steeringRun struct {
	*supplements
	e                  *steeringEnv
	container, session string
}

// hold starts a task whose model blocks in a tool until release("hold").
func (e *steeringEnv) hold(t *testing.T, api *multica.Client, n int) *steeringRun {
	t.Helper()
	clearFaults(t, e.proxy)
	id := enqueue(t, e.rt, n)
	issue, trigger := fmt.Sprintf("40000000-0000-4000-8000-%012d", n), fmt.Sprintf("d3000000-0000-4000-8000-%012d", n)
	sql(t, fmt.Sprintf(`UPDATE issue SET title='Steering fault fixture' WHERE id='%[1]s';
INSERT INTO comment(id,issue_id,workspace_id,author_type,author_id,content,type) VALUES('%[2]s','%[1]s','%[3]s','member','%[4]s','Steering fault fixture input','comment');
UPDATE agent_task_queue SET trigger_comment_id='%[2]s' WHERE id='%[5]s';`, issue, trigger, workspace, user, id))
	s := &supplements{t: t, api: api, task: multica.Task{ID: id, IssueID: issue, AgentID: fmt.Sprintf("30000000-0000-4000-8000-%012d", n)}}
	s.path = "/api/issues/" + issue + "/tasks/" + id + "/supplements"
	s.request = func(method, path, body string, want int) string {
		return nativeAuthorityRequest(t, context.Background(), os.Getenv("MULTICA_TEST_URL"), token(t), method, path, body, want)
	}
	run := &steeringRun{supplements: s, e: e}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("task: %s", sql(t, fmt.Sprintf("SELECT status||'|'||coalesce(error,'')||'|'||coalesce(result::text,'') FROM agent_task_queue WHERE id='%s';", id)))
			t.Logf("receipts: %s", sql(t, fmt.Sprintf("SELECT status,count(*) FROM task_supplement WHERE task_id='%s' GROUP BY status ORDER BY status;", id)))
		}
	})
	run.marker("hold-active")
	run.session = sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') FROM agent_task_queue WHERE id='%s';", id))
	if !strings.HasPrefix(run.session, "ses_") {
		t.Fatal("native HTTP session not pinned mid-turn")
	}
	return run
}

// marker waits for a tool of this run to report itself and pins its container.
func (r *steeringRun) marker(name string) {
	r.t.Helper()
	eventually(r.t, "tool marker "+name, func() bool {
		r.container = liveSteeringTaskMarker(r.t, r.e.controller, "/workspace/.steering-"+name, r.task.ID)
		return r.container != ""
	})
}

func (r *steeringRun) release(name string) {
	r.t.Helper()
	dockerTest(r.t, "exec", r.container, "touch", "/workspace/.steering-"+name)
}

func (r *steeringRun) status(comment string) string {
	return sql(r.t, fmt.Sprintf("SELECT coalesce((SELECT status||'|'||attempt_count FROM task_supplement WHERE task_id='%s' AND comment_id='%s'),'none');", r.task.ID, comment))
}

func (r *steeringRun) awaitStatus(comment, want string) {
	r.t.Helper()
	eventually(r.t, "native receipt "+want, func() bool { return r.status(comment) == want })
}

// ownerScript locates the attempt's OpenCode owner and its loopback credential.
const ownerScript = `pid=$(for d in /proc/[0-9]*; do tr '\0' ' ' < "$d/cmdline" 2>/dev/null | grep -q '^opencode serve' && echo "${d#/proc/}"; done | head -1)
[ -n "$pid" ] || exit 4
auth=$(tr '\0' '\n' < "/proc/$pid/environ" | sed -n 's/^SANDBOX_HTTP_AUTH=//p')
`

// inbox counts the inputs OpenCode admitted but has not promoted yet.
func (r *steeringRun) inbox() int {
	r.t.Helper()
	out, err := execOutput(r.container, ownerScript+`wget -q -O - --header "Authorization: Basic $auth" "http://127.0.0.1:4096/api/session/$1/inbox"`, r.session)
	if err != nil {
		return -1
	}
	return strings.Count(out, `"id":"msg_`)
}

func (r *steeringRun) awaitInbox(want int) {
	r.t.Helper()
	eventually(r.t, fmt.Sprintf("OpenCode inbox holds %d input(s)", want), func() bool { return r.inbox() == want })
}

// finish checks the run ended in its original task and session with each input
// consumed once, in order, and with a consistent transcript and usage.
func (r *steeringRun) finish(order string, inferences int) {
	r.t.Helper()
	waitTask(r.t, r.api, r.task.ID, "completed")
	result := sql(r.t, fmt.Sprintf("SELECT coalesce(session_id,'')||'|'||result::text FROM agent_task_queue WHERE id='%s';", r.task.ID))
	if !strings.HasPrefix(result, r.session+"|") || !strings.Contains(result, "ECHO-ORDER "+order) || strings.Contains(result, "DUPLICATE-INPUT") {
		r.t.Fatalf("steering result/session: %s", result)
	}
	if got := sql(r.t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE issue_id='%s';", r.task.IssueID)); got != "1" {
		r.t.Fatalf("steering spawned another native task: %s", got)
	}
	if got := sql(r.t, fmt.Sprintf("SELECT count(*)=count(DISTINCT seq) AND min(seq)=1 AND max(seq)=count(*) FROM task_message WHERE task_id='%s';", r.task.ID)); got != "t" {
		r.t.Fatal("steering transcript sequence has gaps or duplicates")
	}
	if got := sql(r.t, fmt.Sprintf("SELECT input_tokens||'|'||output_tokens FROM task_usage WHERE task_id='%s';", r.task.ID)); got != fmt.Sprintf("%d|%d", 11*inferences, 7*inferences) {
		r.t.Fatalf("steering usage not accumulated once per inference: %s", got)
	}
}

// steeringAmbiguousOutcomes loses the HTTP response of a committed claim and of a
// committed acknowledgement. Neither is repeated natively and order is kept.
func steeringAmbiguousOutcomes(t *testing.T, api *multica.Client, e *steeringEnv) {
	r := e.hold(t, api, 792)
	arm(t, e.proxy, faultproxy.Lose, claimPath, 1)
	arm(t, e.proxy, faultproxy.Lose, ackPath, 1)
	a, b := r.create(1, "live-steering-echo-a", 201), r.create(2, "live-steering-echo-b", 201)
	r.awaitStatus(a, "delivering|1")
	if hits, _ := faults(t, e.proxy, faultproxy.Lose, claimPath); hits != 1 {
		t.Fatalf("claim response was not lost: hits=%d", hits)
	}
	// The controller reads the lost claim back after its backoff and admits it.
	r.awaitInbox(1)
	if got := r.status(b); got != "pending|0" {
		t.Fatalf("second input claimed while the first outcome was ambiguous: %s", got)
	}
	r.release("hold-release")
	r.marker("echo-a")
	r.awaitStatus(a, "delivered|1")
	if hits, _ := faults(t, e.proxy, faultproxy.Lose, ackPath); hits != 1 {
		t.Fatalf("acknowledgement response was not lost: hits=%d", hits)
	}
	status(t, api, r.task.ID, "running")
	// The lost acknowledgement is resent idempotently before the next claim.
	r.awaitStatus(b, "delivering|1")
	r.awaitInbox(1)
	r.release("echo-release-a")
	r.marker("echo-b")
	r.awaitStatus(b, "delivered|1")
	r.release("echo-release-b")
	r.finish("a b", 5)
	if a, b := r.status(a), r.status(b); a != "delivered|1" || b != "delivered|1" {
		t.Fatalf("ambiguous outcomes claimed twice or lost a receipt: %s %s", a, b)
	}
	t.Log("lost claim and acknowledgement responses: read back, acknowledged idempotently, consumed once in order in the original task/session")
}

// steeringAdmissionRetry loses the response of an admission OpenCode committed:
// the owner is stopped while the controller posts, the request is cut, and the
// resumed owner admits it. The controller's retry reuses the stable ID.
func steeringAdmissionRetry(t *testing.T, api *multica.Client, e *steeringEnv) {
	r := e.hold(t, api, 793)
	signal := func(name string) {
		t.Helper()
		if out, err := execOutput(r.container, ownerScript+`kill -`+name+` "$pid"`); err != nil {
			t.Fatalf("signal owner %s: %v %s", name, err, out)
		}
	}
	signal("STOP")
	resumed := false
	defer func() {
		if !resumed {
			signal("CONT")
		}
	}()
	c := r.create(1, "live-steering-echo-c", 201)
	r.awaitStatus(c, "delivering|1")
	// The stopped owner leaves the admission unanswered. One second lets the small
	// loopback request reach its socket; then the client side is cut.
	eventually(t, "admission request in flight", func() bool {
		_, err := execOutput(r.container, `for d in /proc/[0-9]*; do tr '\0' ' ' < "$d/cmdline" 2>/dev/null | grep -q '^wget .*/prompt' && sleep 1 && kill -KILL "${d#/proc/}" && exit 0; done; exit 1`)
		return err == nil
	})
	resent := func() int { return strings.Count(dockerTest(t, "logs", e.container), "resending under the same id") }
	retries := resent()
	signal("CONT")
	resumed = true
	// OpenCode admitted the cut request although the controller saw a failure:
	// the input is in its inbox before the controller has resent anything.
	r.awaitInbox(1)
	if resent() != retries {
		t.Fatal("inconclusive: the controller resent before the cut admission was observed")
	}
	eventually(t, "controller resent the admission", func() bool { return resent() > retries })
	if got := r.inbox(); got != 1 {
		t.Fatalf("stable ID did not deduplicate the resent admission: inbox=%d", got)
	}
	r.release("hold-release")
	r.marker("echo-c")
	r.awaitStatus(c, "delivered|1")
	r.release("echo-release-c")
	r.finish("c", 4)
	t.Log("real OpenCode 2.2.5 admitted a request whose response was lost; the controller's resend under the same stable ID was deduplicated and consumed once")
}

// steeringVanishedReceipt consumes an input whose receipt disappears before the
// acknowledgement, on an issue beyond the native timeline window. The evidence
// is quarantined and the next input is delivered in the same run.
func steeringVanishedReceipt(t *testing.T, api *multica.Client, e *steeringEnv) {
	r := e.hold(t, api, 794)
	sql(t, fmt.Sprintf(`WITH history AS (
  INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type,created_at,updated_at)
  SELECT '%[1]s','%[2]s','member','%[3]s',repeat('history ',150),'comment',now()-interval '1 hour'+n*interval '1 millisecond',now()
  FROM generate_series(1,5000) n RETURNING id)
 INSERT INTO task_supplement(task_id,workspace_id,issue_id,comment_id,author_id,client_request_id,status,attempt_count,delivered_at)
 SELECT '%[4]s','%[2]s','%[1]s',id,'%[3]s',gen_random_uuid(),'delivered',1,now() FROM history;`, r.task.IssueID, workspace, user, r.task.ID))
	arm(t, e.proxy, faultproxy.Hold, ackPath, 1)
	d := r.create(1, "live-steering-echo-d", 201)
	r.awaitInbox(1)
	r.release("hold-release")
	r.marker("echo-d")
	eventually(t, "acknowledgement held before Multica", func() bool { _, held := faults(t, e.proxy, faultproxy.Hold, ackPath); return held == 1 })
	// Multica refuses this through its comment API while the receipt is in flight.
	r.request("DELETE", "/api/comments/"+d, "", 409)
	sql(t, fmt.Sprintf("DELETE FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", r.task.ID, d))
	clearFaults(t, e.proxy)
	f := r.create(2, "live-steering-echo-f", 201)
	r.awaitStatus(f, "delivering|1")
	r.awaitInbox(1)
	status(t, api, r.task.ID, "running")
	r.release("echo-release-d")
	r.marker("echo-f")
	r.awaitStatus(f, "delivered|1")
	r.release("echo-release-f")
	r.finish("d f", 5)
	if got := r.status(d); got != "none" {
		t.Fatalf("vanished receipt was recreated or acknowledged: %s", got)
	}
	evidence := dockerTest(t, "exec", e.container, "/bin/sh", "-c", "ls /var/lib/multica-sandbox/supplements/quarantine | wc -l; grep -l receipt_none /var/lib/multica-sandbox/supplements/quarantine/* | wc -l")
	if strings.Join(strings.Fields(evidence), " ") != "1 1" || !strings.Contains(dockerTest(t, "logs", e.container), "supplement consumption quarantined") {
		t.Fatalf("consumption evidence not quarantined exactly once: %s", evidence)
	}
	t.Log("receipt vanished after consumption on a 5000-comment issue: evidence quarantined, never called delivered, next input consumed in the same run")
}

// steeringConsumedCrash kills the controller after the model consumed an input
// and the journal recorded it, while the acknowledgement has not reached Multica.
func steeringConsumedCrash(t *testing.T, api *multica.Client, e *steeringEnv) {
	r := e.hold(t, api, 795)
	sql(t, fmt.Sprintf("UPDATE agent_task_queue SET max_attempts=1 WHERE id='%s';", r.task.ID))
	arm(t, e.proxy, faultproxy.Hold, ackPath, 1)
	g := r.create(1, "live-steering-echo-g", 201)
	r.awaitInbox(1)
	h := r.create(2, "live-steering-echo-h", 201)
	r.release("hold-release")
	r.marker("echo-g")
	eventually(t, "acknowledgement held before Multica", func() bool { _, held := faults(t, e.proxy, faultproxy.Hold, ackPath); return held == 1 })
	// Confirmed consumption, no native acknowledgement, second input untouched.
	if g, h := r.status(g), r.status(h); g != "delivering|1" || h != "pending|0" {
		t.Fatalf("receipts before the crash: %s %s", g, h)
	}
	status(t, api, r.task.ID, "running")
	data := dockerTest(t, "exec", e.container, "/bin/sh", "-c", `for path in /var/lib/multica-sandbox/supplements/*.json; do
 grep -q "$1" "$path" && cat "$path"
done; true`, "journal", r.task.ID)
	var journal struct {
		Phase   string
		Task    string `json:"task_id"`
		Session string `json:"session_id"`
		Current struct {
			Comment string `json:"comment_id"`
		}
	}
	if json.Unmarshal([]byte(data), &journal) != nil || journal.Phase != "consumed" || journal.Task != r.task.ID || journal.Session != r.session || journal.Current.Comment != g {
		t.Fatal("durable consumption evidence is not bound to the original task/session/input")
	}
	steeringCrashController(t, e.f, e.container, func() { clearFaults(t, e.proxy) })
	waitTask(t, api, r.task.ID, "failed")
	if reason := sql(t, fmt.Sprintf("SELECT failure_reason FROM agent_task_queue WHERE id='%s';", r.task.ID)); reason != "runtime_recovery" {
		t.Fatalf("controller crash did not use native recovery: %s", reason)
	}
	// Replay acknowledges exactly the proven consumption and nothing else.
	r.awaitStatus(g, "delivered|1")
	if got := sql(t, fmt.Sprintf("SELECT status||'|'||coalesce(failure_reason,'')||'|'||attempt_count FROM task_supplement WHERE task_id='%s' AND comment_id='%s';", r.task.ID, h)); got != "failed|turn_ended|0" {
		t.Fatalf("unclaimed input was confirmed or claimed after the crash: %s", got)
	}
	eventually(t, "terminal journal collected", func() bool {
		return dockerTest(t, "exec", e.container, "/bin/sh", "-c", "ls /var/lib/multica-sandbox/supplements | grep -c '\\.json$' || true") == "0"
	})
	fresh := steeringFollowup(t, steeringScope(t, r.supplements), "live-steering-resume-check")
	waitTask(t, api, fresh, "completed")
	replayed := sql(t, fmt.Sprintf("SELECT (SELECT count(*) FROM task_supplement WHERE task_id='%[1]s')||'|'||(SELECT count(*) FROM task_message WHERE task_id='%[1]s' AND (coalesce(content,'')||coalesce(input::text,'')) LIKE '%%live-steering-echo-%%')||'|'||coalesce(session_id,'') FROM agent_task_queue WHERE id='%[1]s';", fresh))
	if !strings.HasPrefix(replayed, "0|0|ses_") || strings.HasSuffix(replayed, "|"+r.session) {
		t.Fatalf("restart reinjected guidance or resumed the interrupted owner: %s", replayed)
	}
	if got := sql(t, fmt.Sprintf("SELECT count(*) FROM agent_task_queue WHERE parent_task_id='%s';", r.task.ID)); got != "0" {
		t.Fatalf("recovery queued a retry task: %s", got)
	}
	if got := r.status(g); got != "delivered|1" {
		t.Fatalf("replayed receipt changed: %s", got)
	}
	t.Log("controller SIGKILL between proven consumption and native acknowledgement: receipt replayed once after restart, no reinjection, unclaimed input settled turn_ended")
}
