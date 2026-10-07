package multica

import "strings"

// RelayPrefix keeps the upstream CLI's task-token check; the suffix is longer than a real mat_ token.
const RelayPrefix = "mat_relay_"

// CLIDir is where the controller mounts the CLI artifact (deploy/multica-cli.Dockerfile);
// its bin directory leads PATH, so image copies cannot shadow it.
const CLIDir = "/opt/multica-sandbox/multica"

// Credential, session, daemon and account surfaces at UpstreamRevision. A task
// token reaching them could mint or exchange longer-lived credentials (for
// example POST /api/tokens) or act as the daemon; the relay never forwards them.
var relayDenied = []string{"/api/daemon", "/api/tokens", "/api/cli-token", "/api/auth", "/api/cloud-billing", "/api/cloud-runtime", "/api/cloud-subscriptions", "/api/integrations", "/api/plugin-bridge", "/api/invitations", "/api/share-links"}

// RelayPath admits the task-token API surface the upstream CLI uses; paths are already cleaned.
func RelayPath(path string) bool {
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	for _, prefix := range relayDenied {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return false
		}
	}
	return true
}
