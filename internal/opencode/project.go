package opencode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// Project context mirrors Multica v0.5.2 execenv writeProjectContext, formatProjectResource
// and writeProjectResources. Re-check on upgrades.

// ProjectPath is upstream's .multica/project/resources.json in the agent working directory.
const ProjectPath = repo.WorkDir + "/.multica/project/resources.json"

// maxProjectResources bounds resources like repositories; the server sets no limit.
const maxProjectResources = maxRepositories

var resourceType = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// githubRepo is the github_repo resource_ref the server validates and lifts into repos.
type githubRepo struct {
	URL               string `json:"url"`
	DefaultBranchHint string `json:"default_branch_hint,omitempty"`
	Ref               string `json:"ref,omitempty"`
}

// validProject rejects only what the v0.5.2 server never sends: resources without their
// project, non-UUID IDs, unknown type names, invalid JSON, a local_directory without its
// daemon (upstream fails it too), and github_repo resources outside the claim
// repositories, into which the server lifts them (ADR 0015). Other text is rendered inert.
func validProject(t multica.Task) bool {
	if t.ProjectID != "" && !multica.ValidID(t.ProjectID) || t.ProjectID == "" && len(t.ProjectResources) > 0 || len(t.ProjectResources) > maxProjectResources {
		return false
	}
	repos := map[string]bool{}
	for _, r := range t.Repos {
		repos[strings.TrimSpace(r.URL)] = true
	}
	for _, r := range t.ProjectResources {
		if !multica.ValidID(r.ID) || !resourceType.MatchString(r.Type) || strings.ContainsRune(r.Label, 0) || len(r.Ref) > 0 && !json.Valid(r.Ref) {
			return false
		}
		switch r.Type {
		case "github_repo":
			var ref githubRepo
			if json.Unmarshal(r.Ref, &ref) != nil || !repos[ref.URL] {
				return false
			}
		case "local_directory":
			var ref struct {
				DaemonID string `json:"daemon_id"`
			}
			if json.Unmarshal(r.Ref, &ref) != nil || strings.TrimSpace(ref.DaemonID) == "" {
				return false
			}
		}
	}
	return true
}

// code is an inline code span that holds any text on one line; plain text keeps
// upstream's single backticks.
func code(text string) string {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	longest := 0
	for _, part := range strings.FieldsFunc(text, func(c rune) bool { return c != '`' }) {
		longest = max(longest, len(part))
	}
	if longest == 0 {
		return "`" + text + "`"
	}
	fence := strings.Repeat("`", longest+1)
	return fence + " " + text + " " + fence
}

// PinnedHere reports a local_directory resource pinned to this controller's daemon ID. The
// upstream daemon would run the agent in that host path; attempts never mount host paths.
func PinnedHere(t multica.Task, daemon string) bool {
	for _, r := range t.ProjectResources {
		var ref struct {
			DaemonID string `json:"daemon_id"`
		}
		if r.Type == "local_directory" && json.Unmarshal(r.Ref, &ref) == nil && strings.TrimSpace(ref.DaemonID) == daemon {
			return true
		}
	}
	return false
}

// projectContext is upstream writeProjectContext; without checkout the CLI hints are omitted.
func projectContext(t multica.Task, checkout bool) string {
	if t.ProjectID == "" && len(t.ProjectResources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Project Context\n\n")
	if t.ProjectTitle != "" {
		fmt.Fprintf(&b, "The active project for this task is **%s**.\n\n", strings.NewReplacer("\r", " ", "\n", " ", "*", "").Replace(t.ProjectTitle))
	}
	if description := strings.TrimSpace(t.ProjectDescription); description != "" {
		b.WriteString("Project description — durable context the project owner set for work in this project:\n\n" + description + "\n\n")
	}
	if len(t.ProjectResources) == 0 {
		b.WriteString("This project has no resources attached yet.\n\n")
		return b.String()
	}
	b.WriteString("Project resources (also written to `.multica/project/resources.json`):\n\n")
	for _, r := range t.ProjectResources {
		b.WriteString("- " + projectResource(r) + "\n")
	}
	b.WriteString("\nResources are pointers — open them only when relevant to the task.")
	if checkout {
		b.WriteString(" For `github_repo` resources, use `multica repo checkout <url>` to fetch the code. ")
		b.WriteString("A resource listing a starting point is checked out there automatically — pass `--ref <branch-or-sha>` only to override it, when a task or handoff names a different revision.")
	}
	b.WriteString("\n\n")
	return b.String()
}

// projectResource is upstream formatProjectResource; labels stay on their bullet.
func projectResource(r multica.ProjectResource) string {
	var out string
	if r.Type == "github_repo" {
		var ref githubRepo
		_ = json.Unmarshal(r.Ref, &ref)
		out = "**GitHub repo**: " + strings.NewReplacer("\r", " ", "\n", " ").Replace(ref.URL)
		var details []string
		if ref.Ref != "" {
			details = append(details, "checkout ref: "+code(ref.Ref))
		}
		if ref.DefaultBranchHint != "" {
			details = append(details, "default branch hint: "+code(ref.DefaultBranchHint))
		}
		if len(details) > 0 {
			out += " (" + strings.Join(details, ", ") + ")"
		}
	} else {
		ref := string(r.Ref)
		if ref == "" {
			ref = "{}"
		}
		out = fmt.Sprintf("**%s**: %s", r.Type, code(ref))
	}
	if r.Label != "" {
		out += " — " + strings.NewReplacer("\r", " ", "\n", " ").Replace(r.Label)
	}
	return out
}

// projectFile is upstream's resources.json, written whenever the task has a project.
func projectFile(t multica.Task) ([]byte, error) {
	if t.ProjectID == "" && len(t.ProjectResources) == 0 {
		return nil, nil
	}
	resources := make([]multica.ProjectResource, len(t.ProjectResources))
	for i, r := range t.ProjectResources {
		if len(r.Ref) == 0 {
			r.Ref = json.RawMessage("{}")
		}
		resources[i] = r
	}
	data, err := json.MarshalIndent(struct {
		ProjectID          string                    `json:"project_id,omitempty"`
		ProjectTitle       string                    `json:"project_title,omitempty"`
		ProjectDescription string                    `json:"project_description,omitempty"`
		Resources          []multica.ProjectResource `json:"resources"`
	}{t.ProjectID, t.ProjectTitle, t.ProjectDescription, resources}, "", "  ")
	if err != nil || len(data) > 65536 {
		return nil, ErrDenied
	}
	return data, nil
}
