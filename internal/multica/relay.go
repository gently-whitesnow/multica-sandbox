package multica

import (
	"net/http"
	"slices"
	"strings"
)

// RelayPrefix keeps the upstream CLI's task-token check; the suffix is longer than a real mat_ token.
const RelayPrefix = "mat_relay_"

// CLIDir is where the controller mounts the CLI artifact (deploy/multica-cli.Dockerfile);
// its bin directory leads PATH, so image copies cannot shadow it.
const CLIDir = "/opt/multica-sandbox/multica"

// Credential, session, daemon and account surfaces at UpstreamRevision. A task
// token reaching them could mint or exchange longer-lived credentials (for
// example POST /api/tokens) or act as the daemon; the relay never forwards them.
var relayDenied = []string{"/api/daemon", "/api/tokens", "/api/cli-token", "/api/auth", "/api/cloud-billing", "/api/cloud-runtime", "/api/cloud-subscriptions", "/api/integrations", "/api/plugin-bridge", "/api/invitations", "/api/share-links"}

// membershipDenied are self-service workspace membership actions under /api/workspaces/{id}/.
// They are gated by the caller's identity rather than a workspace role, so a task token
// acting as the runtime owner could enrol it in other workspaces (servers that offer
// open workspaces) or remove it from its own.
var membershipDenied = []string{"join", "leave"}

// RelayPath admits the task-token API surface the upstream CLI uses; paths are already cleaned.
func RelayPath(path string) bool {
	if !strings.HasPrefix(path, "/api/") || path == "/api/workspaces/joinable" {
		return false
	}
	if rest, ok := strings.CutPrefix(path, "/api/workspaces/"); ok {
		if _, action, ok := strings.Cut(rest, "/"); ok && slices.Contains(membershipDenied, action) {
			return false
		}
	}
	for _, prefix := range relayDenied {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	return true
}

// RelayRequest is the sandbox task capability surface, independently of the runtime
// owner's workspace role. Everything not listed is denied, including new upstream
// routes. The controller uses its administrative PAT directly, never this relay.
// These routes are member-level in the pinned server router; upstream still checks
// the task's workspace binding, actor identity and resource permissions.
func RelayRequest(r *http.Request) bool {
	if !RelayPath(r.URL.Path) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return false
	}
	get := r.Method == http.MethodGet
	switch parts[0] {
	case "issues":
		return issueRequest(parts, r.Method)
	case "workspaces":
		return get && len(parts) >= 2 && validID(parts[1]) && (len(parts) == 2 || len(parts) == 3 && parts[2] == "members")
	case "agents":
		return get && (len(parts) == 1 || len(parts) == 2 && validID(parts[1]))
	case "comments":
		if len(parts) < 2 || !validID(parts[1]) {
			return false
		}
		if len(parts) == 2 {
			return r.Method == http.MethodPut
		}
		return len(parts) == 3 && parts[2] == "resolve" && (r.Method == http.MethodPost || r.Method == http.MethodDelete)
	case "projects":
		if len(parts) == 1 {
			return get
		}
		if len(parts) == 2 {
			return get && (parts[1] == "search" || validID(parts[1]))
		}
		return get && len(parts) == 3 && validID(parts[1]) && parts[2] == "resources"
	case "chat":
		if len(parts) == 2 && (parts[1] == "history" || parts[1] == "thread") {
			return get
		}
		return get && len(parts) == 4 && parts[1] == "sessions" && validID(parts[2]) && parts[3] == "messages"
	}
	return false
}

func issueRequest(parts []string, method string) bool {
	get := method == http.MethodGet
	if len(parts) == 1 {
		return get || method == http.MethodPost
	}
	if len(parts) == 2 {
		if slices.Contains([]string{"search", "children", "child-progress", "grouped"}, parts[1]) {
			return get
		}
		return taskResourceID(parts[1]) && (get || method == http.MethodPut)
	}
	if !taskResourceID(parts[1]) {
		return false
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "comments", "wakeups":
			return get || method == http.MethodPost
		case "children", "timeline", "task-runs", "attachments", "labels", "active-task", "workflow-handoff":
			return get
		}
	}
	if len(parts) >= 4 && parts[2] == "wakeups" && validID(parts[3]) {
		if len(parts) == 4 {
			return method == http.MethodPut || method == http.MethodDelete
		}
		if len(parts) == 5 {
			switch parts[4] {
			case "disable", "enable", "trigger", "checkin":
				return method == http.MethodPost
			case "instruction":
				return method == http.MethodPatch
			case "runs":
				return get
			}
		}
	}

	return false
}

// Issue identifiers, in addition to UUIDs, are accepted by the pinned issue API.
func taskResourceID(id string) bool {
	if validID(id) {
		return true
	}
	prefix, number, ok := strings.Cut(id, "-")
	if !ok || prefix == "" || number == "" {
		return false
	}
	for _, c := range prefix {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
