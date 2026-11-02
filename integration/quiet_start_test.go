//go:build upstream

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// The service/ACP tests own startup suppression. This fixture checks the real
// server contract: lifecycle/session reports need no transcript placeholder,
// seq 1 is available to agent output, and replay does not duplicate the answer.
func quietStart(t *testing.T, api *multica.Client, rt multica.Runtime) {
	for index, surface := range []string{"issue", "chat"} {
		t.Run(surface, func(t *testing.T) {
			var conversation, agent string
			for turn := range 2 {
				n := 61 + index*2 + turn
				id := enqueue(t, rt, n)
				if turn == 0 {
					conversation = fmt.Sprintf("40000000-0000-4000-8000-%012d", n)
					agent = fmt.Sprintf("30000000-0000-4000-8000-%012d", n)
					if surface == "chat" {
						sql(t, fmt.Sprintf(`INSERT INTO chat_session(id,workspace_id,agent_id,creator_id,title) VALUES('%s','%s','%s','%s','Quiet startup fixture');`, conversation, workspace, agent, user))
					}
				}
				if surface == "chat" {
					sql(t, fmt.Sprintf(`INSERT INTO chat_message(chat_session_id,role,content,task_id) VALUES('%s','user','Fixture turn','%s');
 UPDATE agent_task_queue SET issue_id=NULL,chat_session_id='%s',chat_input_task_id='%s',agent_id='%s' WHERE id='%s';`, conversation, id, conversation, id, agent, id))
				} else {
					sql(t, fmt.Sprintf(`UPDATE agent_task_queue SET issue_id='%s',agent_id='%s' WHERE id='%s';`, conversation, agent, id))
				}
				quietTurn(t, api, rt, id, turn)
				query := fmt.Sprintf("SELECT count(*) FROM comment WHERE issue_id='%s' AND type='comment';", conversation)
				if surface == "chat" {
					query = fmt.Sprintf("SELECT count(*) FROM chat_message WHERE chat_session_id='%s' AND role='assistant';", conversation)
				}
				if count := sql(t, query); count != fmt.Sprint(turn+1) {
					t.Fatalf("%s answer count=%s", surface, count)
				}
			}
		})
	}
	t.Run("native-retry", func(t *testing.T) { quietRetry(t, api, rt) })
}

func quietRetry(t *testing.T, api *multica.Client, rt multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id := enqueue(t, rt, 65)
	sql(t, fmt.Sprintf("UPDATE agent_task_queue SET max_attempts=2 WHERE id='%s';", id))
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("claim: %+v %v", task, err)
	}
	if err := api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	if recovered, err := api.Recover(ctx, rt.ID); err != nil || recovered.Retried != 1 {
		t.Fatalf("retry: %+v %v", recovered, err)
	}
	retryID := sql(t, fmt.Sprintf("SELECT id FROM agent_task_queue WHERE parent_task_id='%s';", id))
	quietTurn(t, api, rt, retryID, 0)
	if count := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message WHERE task_id='%s';", id)); count != "0" {
		t.Fatalf("retried parent created startup rows: %s", count)
	}
}

func quietTurn(t *testing.T, api *multica.Client, rt multica.Runtime, id string, turn int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("claim: %+v %v", task, err)
	}
	if turn == 1 && task.PriorSessionID != "ses_quiet" {
		t.Fatalf("continuation lost session: %q", task.PriorSessionID)
	}
	for range 2 {
		if err := api.Start(ctx, *task); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.ReportSession(ctx, id, "ses_quiet", "fixture-quiet-volume"); err != nil {
		t.Fatal(err)
	}
	if count := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message WHERE task_id='%s';", id)); count != "0" {
		t.Fatalf("lifecycle created transcript rows: %s", count)
	}
	answer := fmt.Sprintf("Substantive fixture answer %d", turn)
	rows := []multica.Message{{Seq: 1, Type: "text", Content: answer, CreatedAt: time.Now().UTC(), IdempotencyKey: "quiet:1"}}
	for range 2 {
		if err := api.ReportMessages(ctx, id, rows); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.AgentComplete(ctx, id, execution.Result{Output: answer, SessionID: "ses_quiet", WorkDir: "fixture-quiet-volume"}); err != nil {
		t.Fatal(err)
	}
	status(t, api, id, "completed")
	if got := sql(t, fmt.Sprintf("SELECT count(*) || ':' || min(seq) || ':' || min(content) FROM task_message WHERE task_id='%s';", id)); got != "1:1:"+answer {
		t.Fatalf("transcript duplicated or changed: %s", got)
	}
}
