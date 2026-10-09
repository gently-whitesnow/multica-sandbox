package multica

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchRejectsCrossWorkspaceAndOverclaim(t *testing.T) {
	other := "22222222-2222-4222-8222-222222222222"
	valid := fmt.Sprintf(`{"id":%q,"runtime_id":%q,"workspace_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true}`, testID, testID, testID)
	cross := fmt.Sprintf(`{"id":%q,"runtime_id":%q,"workspace_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true}`, testID, testID, other)
	for _, body := range []string{cross, valid + "," + valid} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"tasks":[%s]}`, body) }))
		api, _ := New(server.URL, "token")
		if _, err := api.ClaimBatch(context.Background(), testID, map[string]string{testID: testID}, 1); err == nil {
			t.Fatal("unsafe batch accepted")
		}
		server.Close()
	}
}

func TestCoAuthoredByFollowsWorkspaceSettings(t *testing.T) {
	for body, want := range map[string]bool{
		`{"settings":{}}`: true,
		`{"settings":{"co_authored_by_enabled":false}}`:                      false,
		`{"settings":{"github_enabled":false}}`:                              false,
		`{"settings":{"github_enabled":true,"co_authored_by_enabled":true}}`: true,
		`not json`: true,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/daemon/workspaces/"+testID+"/repos" {
				http.NotFound(w, r)
			}
			fmt.Fprint(w, body)
		}))
		api, _ := NewAllowHTTP(server.URL, "token")
		if got, err := api.CoAuthoredBy(context.Background(), testID); got != want || (err != nil) != (body == "not json") {
			t.Errorf("%s: %t %v", body, got, err)
		}
		server.Close()
	}
}
