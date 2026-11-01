package opencode

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	gitrepo "github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

var ErrDenied = errors.New("OpenCode task projection denied")

// unattended are the permissions native OpenCode would ask a person about; under ACP a
// request is refused, so they are denied up front.
var unattended = []string{"question", "doom_loop"}

// Config projects permissions; selected MCP connections reach OpenCode through session/new,
// not the configuration. With the Multica relay (local) the agent works like native
// OpenCode: the disposable container and its network are the boundary, so every tool is
// allowed, including paths outside the workdir and web research. Without it permissions are
// deny-by-default and only the selected MCP tools are allowed.
func Config(connections map[string]Remote, local bool) ([]byte, error) {
	permissions := map[string]string{"*": "deny"}
	if local {
		permissions["*"] = "allow"
		for _, tool := range unattended {
			permissions[tool] = "deny"
		}
	}
	for name := range connections {
		permissions[name+"_*"] = "allow"
	}
	return json.Marshal(map[string]any{"autoupdate": false, "share": "disabled", "permission": permissions})
}

// Continuity says how a run relates to the claim's prior session.
type Continuity int

const (
	// Fresh runs have no prior session.
	Fresh Continuity = iota
	// Resumed runs continue the prior native session.
	Resumed
	// Lost runs were meant to continue a session that cannot be restored.
	Lost
)

// Prompt returns the per-turn prompt and, for upstream CLI issue and chat tasks, the AGENTS.md brief.
// With checkout, the brief offers `multica repo checkout` for the claim repositories.
func Prompt(t multica.Task, upstream, checkout bool, c Continuity) ([]byte, []byte, error) {
	if t.Agent == nil || !validComments(t) || chatTask(t) && !validChat(t) || autopilotTask(t) && !validAutopilot(t) || !validProject(t) {
		return nil, nil, ErrDenied
	}
	sources, err := repositories(t)
	if err != nil {
		return nil, nil, err
	}
	var prompt, brief string
	notice := sessionContinuityNotice
	if upstream && (chatTask(t) || upstreamTask(t) || autopilotTask(t)) {
		if chatTask(t) {
			prompt, notice = chatPrompt(t), chatNoticeFor(t)
		} else if autopilotTask(t) {
			prompt = autopilotPrompt(t)
		} else {
			prompt = upstreamPrompt(t, c == Resumed && !t.PriorSessionResumeUnavailable)
		}
		switch {
		case checkout:
			sources = checkoutRepositories(t.Repos)
		case sources != "":
			sources = "## Repositories\n\nRepository references (no local checkout; access through selected authorized MCP):\n" + sources + "\n\n"
		}
		brief = upstreamBrief(t, sources, checkout)
	} else {
		sections := []string{t.Agent.Instructions, t.WorkspaceContext, "Assigned issue: " + t.IssueID, t.ProjectTitle, t.ProjectDescription}
		for _, r := range t.ProjectResources {
			sections = append(sections, "Project resource: "+projectResource(r))
		}
		sections = append(sections, t.TriggerCommentContent)
		for _, comment := range t.CoalescedComments {
			sections = append(sections, comment.Content)
		}
		sections = append(sections, t.ChatMessage)
		if sources != "" {
			sections = append(sections, "Repository references (no local checkout; access through selected authorized MCP):\n"+sources)
		}
		prompt = strings.Join(sections, "\n\n")
	}
	if upstream {
		prompt += turnContext(t)
	}
	if c == Lost || t.PriorSessionResumeUnavailable {
		// Upstream appends the notice to the per-turn message, never the brief.
		prompt = strings.TrimRight(prompt, "\n") + "\n\n" + strings.TrimRight(notice, "\n")
	}
	limit := 65536
	if autopilotTask(t) {
		limit = 512 << 10
	}
	if _, err := projectFile(t); err != nil || len(prompt) > limit || len(brief) > 65536 {
		return nil, nil, ErrDenied
	}
	if brief == "" {
		return []byte(prompt), nil, nil
	}
	return []byte(prompt), []byte(brief), nil
}

// maxRepositories bounds claim repositories; upstream sets no limit.
const maxRepositories = 256

func repositories(t multica.Task) (string, error) {
	if len(t.Repos) > maxRepositories {
		return "", ErrDenied
	}
	sources := []string{}
	for _, repo := range t.Repos {
		// Upstream URL forms the Git relay serves, or any credential-free HTTPS reference.
		u, err := url.Parse(repo.URL)
		https := err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
		if !https && !gitrepo.ValidURL(repo.URL) || strings.ContainsAny(repo.URL, "\r\n\x00") {
			return "", ErrDenied
		}
		encoded, err := json.Marshal(repo)
		if err != nil {
			return "", ErrDenied
		}
		sources = append(sources, string(encoded))
	}
	return strings.Join(sources, "\n"), nil
}
