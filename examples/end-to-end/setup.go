package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type runtime struct {
	ID string `json:"id"`
}

func sql(ctx context.Context, query string) error {
	project := os.Getenv("COMPOSE_PROJECT_NAME")
	if project != "sandbox-e2e" {
		return fmt.Errorf("disposable project sandbox-e2e required")
	}
	command := exec.CommandContext(ctx, "docker", "exec", "-i", project+"-postgres-1", "psql", "-U", "fixture", "-d", "fixture", "-v", "ON_ERROR_STOP=1")
	command.Stdin = strings.NewReader(query)
	if command.Run() != nil {
		return fmt.Errorf("fixture SQL failed")
	}
	return nil
}
func setup(ctx context.Context) error {
	c, err := load()
	if err != nil {
		return err
	}
	if err = sql(ctx, fmt.Sprintf(`INSERT INTO "user"(id,name,email) VALUES('%s','E2E fixture','fixture@example.invalid') ON CONFLICT DO NOTHING;
 INSERT INTO workspace(id,name,slug) VALUES('%s','E2E fixture','sandbox-e2e') ON CONFLICT DO NOTHING;
 INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner') ON CONFLICT DO NOTHING;`, user, workspace, workspace, user)); err != nil {
		return err
	}
	var registered struct {
		Runtimes []runtime `json:"runtimes"`
	}
	err = control(ctx, c, "POST", "/api/daemon/register", map[string]any{"workspace_id": workspace, "daemon_id": daemonID, "device_name": "E2E fixture", "runtimes": []map[string]string{{"name": "OpenCode E2E example", "type": "opencode", "version": "1.18.34", "status": "online"}}}, &registered)
	if err != nil {
		return err
	}
	if len(registered.Runtimes) != 1 {
		return fmt.Errorf("unexpected runtime response")
	}
	rt := registered.Runtimes[0]
	instructions := "Use the remote fixture MCP tools to read the assigned issue and its reference word. Create /workspace/result.txt containing exactly that word. Summarize completion. Do not use the network directly."
	err = sql(ctx, fmt.Sprintf(`INSERT INTO agent(id,workspace_id,name,instructions,model,runtime_mode,runtime_id,owner_id,status) VALUES('%s','%s','E2E agent','%s','demo','local','%s','%s','idle') ON CONFLICT DO NOTHING;
 INSERT INTO issue(id,workspace_id,title,description,status,creator_type,creator_id,number,assignee_type,assignee_id) VALUES('%s','%s','Read the reference word through MCP','Use read_reference and write its word to result.txt.','in_progress','member','%s',1,'agent','%s') ON CONFLICT DO NOTHING;
 INSERT INTO agent_task_queue(id,agent_id,runtime_id,issue_id,status,max_attempts,originator_user_id,accountable_user_id) VALUES('%s','%s','%s','%s','queued',1,'%s','%s') ON CONFLICT DO NOTHING;`, agentID, workspace, instructions, rt.ID, user, issueID, workspace, user, agentID, taskID, agentID, rt.ID, issueID, user, user))
	if err != nil {
		return err
	}
	if err = writeJSON("/config/runtime.json", rt); err != nil {
		return err
	}
	// The workspace's LiteLLM virtual key grants the demo model and limits; only the controller reads it.
	var key struct {
		Key string `json:"key"`
	}
	status, err := exchange(ctx, "POST", "http://litellm:4000/key/generate", "sk-"+c.Master, map[string]any{"models": []string{"demo"}, "rpm_limit": 20, "tpm_limit": 30000, "metadata": map[string]string{"workspace": workspace}}, &key)
	if err != nil || status != 200 || key.Key == "" {
		return fmt.Errorf("LiteLLM workspace key unavailable")
	}
	if err = os.WriteFile("/config/workspace-key", []byte(key.Key), 0600); err != nil {
		return err
	}
	fmt.Println("READY real Multica issue and queued task")
	return nil
}
