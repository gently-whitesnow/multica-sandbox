//go:build upstream

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// issueGCCheck verifies the controller's issue-state query against upstream's batch
// gc-check: done and cancelled issues are reported, open ones and issues of other
// workspaces are not, and a workspace without access fails.
func issueGCCheck(t *testing.T, api *multica.Client) {
	foreign := "10000000-0000-4000-8000-000000000301"
	stranger := "41000000-0000-4000-8000-000000000399"
	issues := map[string]string{}
	ids := []string{stranger}
	for n, status := range []string{"done", "cancelled", "in_review", "todo"} {
		issues[status] = fmt.Sprintf("41000000-0000-4000-8000-%012d", 301+n)
		ids = append(ids, issues[status])
		sql(t, fmt.Sprintf(`INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number) VALUES('%s','%s','GC fixture','%s','member','%s',%d);`, issues[status], workspace, status, user, 301+n))
	}
	sql(t, fmt.Sprintf(`INSERT INTO workspace (id,name,slug) VALUES ('%s','GC foreign fixture','sandbox-gc-foreign');
 INSERT INTO issue(id,workspace_id,title,status,creator_type,creator_id,number) VALUES('%s','%s','GC fixture','done','member','%s',1);`, foreign, stranger, foreign, user))
	closed, err := api.ClosedIssues(context.Background(), workspace, ids)
	if err != nil || len(closed) != 2 || closed[issues["done"]].IsZero() || closed[issues["cancelled"]].IsZero() {
		t.Fatalf("closed issues %v: %v", closed, err)
	}
	if closed, err := api.ClosedIssues(context.Background(), foreign, []string{stranger}); err == nil || len(closed) != 0 {
		t.Fatalf("foreign workspace answered: %v", closed)
	}
}
