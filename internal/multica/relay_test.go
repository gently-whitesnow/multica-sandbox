package multica

import (
	"net/http/httptest"
	"testing"
)

func TestTaskRelayCapabilities(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	for _, tc := range []struct {
		method, path string
		allow        bool
	}{
		{"GET", "/api/issues/" + id, true}, {"GET", "/api/issues/ECO-42", true},
		{"PUT", "/api/issues/" + id, true}, {"POST", "/api/issues", true},
		{"POST", "/api/issues/" + id + "/comments", true}, {"GET", "/api/projects/" + id + "/resources", true},
		{"GET", "/api/chat/history", true}, {"GET", "/api/chat/sessions/" + id + "/messages", true},
		{"DELETE", "/api/issues/" + id, false}, {"POST", "/api/issues/batch-delete", false},
		{"PUT", "/api/workspaces/" + id, false}, {"PATCH", "/api/workspaces/" + id + "/members/" + id, false},
		{"GET", "/api/workspaces/" + id + "/env", false}, {"PUT", "/api/agents/" + id, false},
		{"GET", "/api/agents/" + id + "/env", false}, {"GET", "/api/tasks/" + id, false},
		{"POST", "/api/tokens", false}, {"POST", "/api/daemon/register", false},
		{"POST", "/api/projects", false}, {"POST", "/api/autopilots", false},
		{"GET", "/api/autopilots/" + id, false},
		{"POST", "/api/issues/" + id + "/wakeups", true},
		{"PUT", "/api/issues/" + id + "/wakeups/" + id, true},
		{"POST", "/api/issues/" + id + "/wakeups/" + id + "/disable", true},
		{"PUT", "/api/issues/" + id + "/system-wakeups/child_done", false},
		{"POST", "/api/issues/" + id + "/new-admin-route", false}, {"GET", "/api/new-surface", false},
		{"POST", "/api/issues/quick-create", false}, {"PUT", "/api/issues/search", false},
		{"GET", "/api/issues/foo", false}, {"PATCH", "/api/issues/" + id, false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			if got := RelayRequest(httptest.NewRequest(tc.method, tc.path, nil)); got != tc.allow {
				t.Fatalf("allowed=%v, want %v", got, tc.allow)
			}
		})
	}
}
