package multica

import "testing"

func TestRelayPathDefersAPIAuthorization(t *testing.T) {
	for _, path := range []string{
		"/api/issues/ECO-42", "/api/projects", "/api/projects/x/resources",
		"/api/issue-workflows", "/api/autopilots", "/api/skills", "/api/agents/x/env",
		"/api/workspaces/x/members", "/api/workspaces/joinable", "/api/workspaces/x/join",
		"/api/tokens", "/api/cli-token", "/api/auth/login", "/api/daemon/tasks/claim",
		"/api/integrations/composio/connect", "/api/future-native-route",
	} {
		if !RelayPath(path) {
			t.Errorf("Multica API authorization intercepted: %s", path)
		}
	}
	for _, path := range []string{"/", "/api", "/apix/projects", "/ws", "/health", "/uploads/x"} {
		if RelayPath(path) {
			t.Errorf("non-API path admitted: %s", path)
		}
	}
}
