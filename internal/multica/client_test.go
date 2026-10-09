package multica

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testID = "11111111-1111-4111-8111-111111111111"

func TestRejectUnsafeOrigins(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?token=secret", "file:///tmp/api"} {
		if _, err := New(base, "token"); err == nil {
			t.Errorf("accepted %s", base)
		}
	}
}
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	api, _ := New(source.URL, "private-token")
	err := api.Heartbeat(context.Background(), testID)
	if err == nil || reached {
		t.Fatal("redirect followed or accepted")
	}
}
func TestResponseAndErrorDoNotExposeCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("missing auth")
		}
		if r.URL.Path == "/api/daemon/heartbeat" {
			w.WriteHeader(403)
			fmt.Fprint(w, "private-token")
			return
		}
		fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true,"auth_token":"private-token","custom_env":{"API_KEY":"private-token"}}}`, testID, testID)
	}))
	defer server.Close()
	api, _ := New(server.URL, "private-token")
	task, err := api.Claim(context.Background(), testID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", task), "private-token") {
		t.Fatal("claim credentials retained")
	}
	err = api.Heartbeat(context.Background(), testID)
	var status *HTTPError
	if !errors.As(err, &status) || status.Status != 403 || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}
func TestClaimRequiresFencing(t *testing.T) {
	for _, claim := range []string{
		`{"id":"../escape"}`,
		fmt.Sprintf(`{"id":%q,"runtime_id":%q}`, testID, testID),
		fmt.Sprintf(`{"id":%q,"runtime_id":%q,"start_claim_supported":true,"dispatched_at":"not-a-date"}`, testID, testID),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"task":%s}`, claim) }))
		api, _ := New(server.URL, "token")
		if _, err := api.Claim(context.Background(), testID); err == nil {
			t.Error("unfenced claim accepted")
		}
		server.Close()
	}
}

func TestClaimCarriesResumeDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Capabilities") != "coalesced-comments-v1" {
			t.Error("structured coalesced comments not advertised")
		}
		fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true,"new_comment_count":2,"new_comments_since":"2026-10-03T00:00:00Z","new_comments_delta_known":true,"issue_state_delta_known":true,"issue_changed_fields":["title"],"issue_status":"todo","prior_session_resume_unavailable":true,"coalesced_comment_ids":[%[1]q],"coalesced_comments":[{"id":%[1]q,"thread_id":%[1]q,"author_type":"member","author_name":"Member","content":"earlier","created_at":"2026-10-03T01:00:00Z"}]}}`, testID, testID)
	}))
	defer server.Close()
	api, _ := New(server.URL, "token")
	task, err := api.Claim(context.Background(), testID)
	if err != nil || task.NewCommentCount != 2 || !task.NewCommentsDeltaKnown || !task.IssueStateDeltaKnown || task.IssueChangedFields[0] != "title" ||
		task.IssueStatus != "todo" || !task.PriorSessionResumeUnavailable || task.CoalescedComments[0] != (Comment{testID, testID, "member", "Member", "earlier", "2026-10-03T01:00:00Z"}) {
		t.Fatalf("claim deltas: %+v %v", task, err)
	}
}
