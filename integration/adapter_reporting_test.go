//go:build upstream

package integration

import (
	"fmt"
	"testing"
)

func assertNativeReports(t *testing.T, id string, usage bool) {
	t.Helper()
	session := sql(t, fmt.Sprintf("SELECT coalesce(session_id,'') || ':' || session_rollout_missing::text || ':' || coalesce(work_dir,'') FROM agent_task_queue WHERE id='%s';", id))
	if session != ":true:" {
		t.Fatalf("disposable native session not reported: %s", session)
	}
	messages := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message WHERE task_id='%s' AND type='tool_use' AND call_id IS NOT NULL;", id))
	if messages != "24" {
		t.Fatalf("native tool calls missing: %s", messages)
	}
	pairs := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message c JOIN task_message r ON r.task_id=c.task_id AND r.call_id=c.call_id AND r.seq=c.seq+1 WHERE c.task_id='%s' AND c.type='tool_use' AND r.type='tool_result';", id))
	if pairs != "24" {
		t.Fatalf("native call correlation/order lost: %s", pairs)
	}
	ordered := sql(t, fmt.Sprintf("SELECT count(*)=count(DISTINCT seq) AND min(seq)=1 AND max(seq)=count(*) FROM task_message WHERE task_id='%s';", id))
	if ordered != "t" {
		t.Fatal("native message sequence has gaps/duplicates")
	}
	leaks := sql(t, fmt.Sprintf("SELECT count(*) FROM task_message WHERE task_id='%s' AND (coalesce(content,'') || coalesce(input::text,'') || coalesce(output,'')) LIKE '%%claim-secret-sentinel%%';", id))
	if leaks != "0" {
		t.Fatal("claim environment leaked into reporting")
	}
	if usage {
		counters := sql(t, fmt.Sprintf("SELECT provider || ':' || model || ':' || input_tokens || ':' || output_tokens FROM task_usage WHERE task_id='%s';", id))
		if counters != "managed-inference:fixture:275:175" {
			t.Fatalf("native cumulative usage mismatch: %s", counters)
		}
	}
	t.Log("native text/tool pairs/missing-session marker and cumulative fixture usage persisted through pinned Multica APIs")
}
