//go:build upstream

package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func fleetMembershipChange(t *testing.T, api *multica.Client, logs *fleetLog, first, second string) {
	sql(t, fmt.Sprintf("DELETE FROM member WHERE workspace_id='%s' AND user_id='%s'; INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner');", fleetWorkspace(1), user, fleetWorkspace(100), user))
	eventually(t, "membership removal observed", func() bool { return logs.contains("removed workspace=" + fleetWorkspace(1)) })
	eventually(t, "new workspace registered", func() bool { return logs.contains("registered workspace=" + fleetWorkspace(100)) })
	waitTask(t, api, second, "completed")
	if strings.Contains(sql(t, fmt.Sprintf("SELECT status FROM agent_task_queue WHERE id='%s';", first)), "completed") {
		t.Fatal("removed workspace attempt continued")
	}
	if len(strings.Fields(ownedContainers(t))) > 1 {
		t.Fatal("removed workspace execution leaked")
	}
	if sql(t, fmt.Sprintf("SELECT visibility FROM agent_runtime WHERE workspace_id='%s' AND daemon_id='%s';", fleetWorkspace(100), daemon)) != "private" {
		t.Fatal("visibility changed automatically")
	}
	// Restoring membership must recover the interrupted attempt without stopping peers.
	sql(t, fmt.Sprintf("INSERT INTO member(workspace_id,user_id,role) VALUES('%s','%s','owner');", fleetWorkspace(1), user))
	waitTask(t, api, first, "failed")
}
