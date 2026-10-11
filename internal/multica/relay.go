package multica

import (
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
