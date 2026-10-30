//go:build upstream

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

// projectClaim checks the v0.5.2 claim of an issue in a project with resources: github_repo
// resources arrive lifted into repos and the projection renders upstream's project context.
func projectClaim(t *testing.T, ctx context.Context, api *multica.Client, rt multica.Runtime) {
	const project = "80000000-0000-4000-8000-000000000040"
	id := enqueue(t, rt, 40)
	// Project repositories replace workspace repositories in the claim.
	sql(t, fmt.Sprintf(`UPDATE workspace SET repos='[{"url":"https://git.example.invalid/team/services.git"}]' WHERE id='%s';`, workspace))
	t.Cleanup(func() { sql(t, fmt.Sprintf(`UPDATE workspace SET repos='[]' WHERE id='%s';`, workspace)) })
	sql(t, fmt.Sprintf(`INSERT INTO project(id,workspace_id,title,description) VALUES('%[1]s','%[2]s','Fixture project','Ship the fixture.');
 INSERT INTO project_resource(project_id,workspace_id,resource_type,resource_ref,label,position) VALUES
  ('%[1]s','%[2]s','github_repo','{"url":"https://git.example.invalid/team/fixture.git","ref":"release"}','Primary source',0),
  ('%[1]s','%[2]s','local_directory','{"daemon_id":"other-daemon","local_path":"/srv/fixture"}',NULL,1),
  ('%[1]s','%[2]s','github_repo','{"url":"git@git.example.invalid:team/docs.git"}','Docs, main line',2);
 UPDATE issue SET project_id='%[1]s' WHERE id='40000000-0000-4000-8000-000000000040';`, project, workspace))
	task, err := api.Claim(ctx, rt.ID)
	if err != nil || task == nil || task.ID != id {
		t.Fatalf("claim: %v", err)
	}
	if task.ProjectID != project || len(task.ProjectResources) != 3 || task.ProjectResources[0].Type != "github_repo" || task.ProjectResources[0].Label != "Primary source" ||
		len(task.Repos) != 2 || task.Repos[0].URL != "https://git.example.invalid/team/fixture.git" || task.Repos[0].Ref != "release" || task.Repos[1].URL != "git@git.example.invalid:team/docs.git" {
		t.Fatalf("project context not claimed: %+v %+v", task.ProjectResources, task.Repos)
	}
	// Checkout admits them after a refresh, as upstream ensureRepoReady does.
	if repos, err := api.WorkspaceRepositories(ctx, workspace); err != nil || len(repos) != 1 || repos[0] != "https://git.example.invalid/team/services.git" {
		t.Fatalf("workspace repositories: %v %v", repos, err)
	}
	_, brief, err := opencode.Prompt(*task, true, true, opencode.Fresh)
	if err != nil || opencode.PinnedHere(*task, daemon) ||
		!strings.Contains(string(brief), "The active project for this task is **Fixture project**.") ||
		!strings.Contains(string(brief), "- **GitHub repo**: https://git.example.invalid/team/fixture.git (checkout ref: `release`) — Primary source\n") ||
		!strings.Contains(string(brief), "- https://git.example.invalid/team/fixture.git (starts from `release`)") ||
		!strings.Contains(string(brief), "- **GitHub repo**: git@git.example.invalid:team/docs.git — Docs, main line\n") {
		t.Fatalf("project brief: %v\n%s", err, brief)
	}
	if err := api.Start(ctx, *task); err != nil {
		t.Fatal(err)
	}
	if err := api.Complete(ctx, id, execution.Result{}); err != nil {
		t.Fatal(err)
	}
}
