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

func TestWorkspaceRepositoriesListURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/workspaces/"+testID+"/repos" {
			http.NotFound(w, r)
		}
		fmt.Fprint(w, `{"workspace_id":"`+testID+`","repos":[{"url":"https://git.example.invalid/team/a.git","ref":"main"},{"url":""},{"url":"git@git.example.invalid:team/b.git"}],"settings":{}}`)
	}))
	defer server.Close()
	api, _ := NewAllowHTTP(server.URL, "token")
	if got, err := api.WorkspaceRepositories(context.Background(), testID); err != nil || len(got) != 2 || got[0] != "https://git.example.invalid/team/a.git" || got[1] != "git@git.example.invalid:team/b.git" {
		t.Fatalf("workspace repositories: %v %v", got, err)
	}
	if _, err := api.WorkspaceRepositories(context.Background(), "../x"); err == nil {
		t.Fatal("invalid workspace accepted")
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
