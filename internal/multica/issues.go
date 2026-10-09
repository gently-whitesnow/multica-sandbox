package multica

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// issueGCBatch is upstream's batch cap for the issue GC check.
const issueGCBatch = 500

// ClosedIssues mirrors upstream GetIssueGCChecks for one workspace: it returns the
// last update of each requested issue that is done or cancelled. Unknown, missing
// and unanswered issues are absent; failed batches are joined into the error.
func (c *Client) ClosedIssues(ctx context.Context, workspace string, issues []string) (map[string]time.Time, error) {
	if !validID(workspace) {
		return nil, fmt.Errorf("invalid workspace ID")
	}
	requested := map[string]bool{}
	ids := []string{}
	for _, id := range issues {
		if !validID(id) {
			return nil, fmt.Errorf("invalid issue ID")
		}
		if !requested[id] {
			requested[id] = true
			ids = append(ids, id)
		}
	}
	closed := map[string]time.Time{}
	var result error
	for start := 0; start < len(ids); start += issueGCBatch {
		var out struct {
			Issues []struct {
				ID        string    `json:"id"`
				Found     bool      `json:"found"`
				Status    string    `json:"status"`
				Category  string    `json:"category"`
				UpdatedAt time.Time `json:"updated_at"`
			} `json:"issues"`
		}
		batch := ids[start:min(start+issueGCBatch, len(ids))]
		if err := c.call(ctx, http.MethodPost, "/api/daemon/workspaces/"+workspace+"/issues/gc-check", map[string]any{"issue_ids": batch}, &out); err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, issue := range out.Issues {
			if requested[issue.ID] && issue.Found && !issue.UpdatedAt.IsZero() && terminal(issue.Category, issue.Status) {
				closed[issue.ID] = issue.UpdatedAt
			}
		}
	}
	return closed, result
}

// terminal follows upstream issueGCLifecycle: a lifecycle category decides, else
// the legacy status, which fails closed on custom keys.
func terminal(category, status string) bool {
	switch category {
	case "done", "closed":
		return true
	case "unstarted", "started":
		return false
	}
	return status == "done" || status == "cancelled"
}
