//go:build upstream

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

func nativeClaims(t *testing.T, api *multica.Client, rt multica.Runtime) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id := enqueue(t, rt, 51)
	agent := "30000000-0000-4000-8000-000000000051"
	ap, run := "b1000000-0000-4000-8000-000000000001", "b2000000-0000-4000-8000-000000000001"
	sql(t, fmt.Sprintf(`INSERT INTO autopilot(id,workspace_id,title,description,assignee_type,assignee_id,execution_mode,created_by_type,created_by_id) VALUES('%s','%s','Fixture run-only','Report the scheduled fixture outcome.','agent','%s','run_only','member','%s');
 INSERT INTO autopilot_run(id,autopilot_id,source,status,trigger_payload) VALUES('%s','%s','schedule','running','{"check":"native fixture"}');
 UPDATE agent_task_queue SET issue_id=NULL,autopilot_run_id='%s' WHERE id='%s';
 UPDATE autopilot_run SET task_id='%s' WHERE id='%s';`, ap, workspace, agent, user, run, ap, run, id, id, run))
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("run-only claim: %v", err)
	}
	prompt, brief, err := opencode.Prompt(*task, true, true, opencode.Fresh)
	if err != nil || task.AutopilotRunID != run || task.IssueID != "" || !strings.Contains(string(prompt), "Report the scheduled fixture outcome") || !strings.Contains(string(prompt), "native fixture") || !strings.Contains(string(brief), "autopilot run result") {
		t.Fatalf("run-only context missing: %v", err)
	}
	if err = api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	if err = api.AgentComplete(ctx, id, execution.Result{Output: "Fixture scheduled outcome"}); err != nil {
		t.Fatal(err)
	}
	if got := sql(t, fmt.Sprintf("SELECT status FROM autopilot_run WHERE id='%s';", run)); got != "completed" {
		t.Fatalf("autopilot run status=%s", got)
	}

	// A credential-free local installation and synthetic task delivery snapshot
	// carry Telegram context. No valid external recipient or bot token exists.
	chat := "b3000000-0000-4000-8000-000000000001"
	sql(t, fmt.Sprintf(`INSERT INTO channel_installation(id,workspace_id,agent_id,channel_type,config,status,installer_user_id) VALUES('b6000000-0000-4000-8000-000000000001','%s','%s','telegram','{}','active','%s');`, workspace, agent, user))
	sql(t, fmt.Sprintf(`INSERT INTO chat_session(id,workspace_id,agent_id,creator_id,title) VALUES('%s','%s','%s','%s','Synthetic Telegram');`, chat, workspace, agent, user))
	sql(t, fmt.Sprintf(`INSERT INTO channel_chat_session_binding(id,chat_session_id,installation_id,channel_type,channel_chat_id,chat_type) VALUES('b5000000-0000-4000-8000-000000000001','%s','b6000000-0000-4000-8000-000000000001','telegram','synthetic-fixture','p2p');`, chat))
	for n := 0; n < 2; n++ {
		id = fmt.Sprintf("b4000000-0000-4000-8000-%012d", n)
		sql(t, fmt.Sprintf(`BEGIN;
 INSERT INTO chat_message(chat_session_id,role,content,task_id) VALUES('%s','user','Synthetic channel message','%s');
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,chat_session_id,chat_input_task_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','%s','queued',1,'%s','%s');
 INSERT INTO channel_task_delivery(task_id,binding_id,installation_id,channel_type,channel_chat_id,chat_type,route_revision) VALUES('%s','b5000000-0000-4000-8000-000000000001','b6000000-0000-4000-8000-000000000001','telegram','synthetic-fixture','p2p',1); COMMIT;`, chat, id, id, agent, rt.ID, chat, id, user, user, id))
		task, err = api.Claim(ctx, rt.ID)
		if err != nil || task == nil || task.ID != id || task.ChatChannelType != "telegram" || task.ChatType != "p2p" {
			t.Fatalf("Telegram claim: %v", err)
		}
		prompt, _, err = opencode.Prompt(*task, true, true, opencode.Fresh)
		if err != nil || !strings.Contains(string(prompt), "operating inside a Telegram conversation") || strings.Contains(string(prompt), "Your assigned issue ID") {
			t.Fatalf("Telegram prompt: %v", err)
		}
		if n == 1 && (task.PriorSessionID != "ses_fixturetelegram" || task.PriorWorkDir != "fixture-telegram-volume") {
			t.Fatalf("Telegram prior session=%q workdir=%q; pointer=%s", task.PriorSessionID, task.PriorWorkDir, sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') || '|' || coalesce(work_dir,'') || '|' || coalesce(runtime_id::text,'') FROM chat_session WHERE id='%s';", chat)))
		}
		if err = api.Start(ctx, *task); err != nil {
			t.Fatal(err)
		}
		if err = api.ReportSession(ctx, id, "ses_fixturetelegram", "fixture-telegram-volume"); err != nil {
			t.Fatal(err)
		}
		// No bot credential exists and the synthetic chat id is not a Telegram
		// recipient. The local fixture cannot perform an outbound API request.
		if err = api.AgentComplete(ctx, id, execution.Result{Output: "Synthetic fixture reply", SessionID: "ses_fixturetelegram", WorkDir: "fixture-telegram-volume"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("native run_only completed; synthetic Telegram claims preserve channel context and follow-up continuity without outbound delivery")
}
