package multica

import "strings"

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
