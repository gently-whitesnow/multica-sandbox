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
	for _, base := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path/../api", "https://example.com/a%2Fb", "https://example.com//path", "https://example.com?token=secret", "file:///tmp/api"} {
		if _, err := New(base, "token"); err == nil {
			t.Errorf("accepted %s", base)
		}
	}
}
func TestServerBasePathPrefixesAPI(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { paths <- r.URL.Path }))
	defer server.Close()
	for _, base := range []string{server.URL + "/multica", server.URL + "/multica/"} {
		api, err := NewAllowHTTP(base, "token")
		if err != nil {
			t.Fatal(err)
		}
		if err = api.Heartbeat(context.Background(), testID); err != nil {
			t.Fatal(err)
		}
		if got := <-paths; got != "/multica/api/daemon/heartbeat" {
			t.Fatalf("%s: requested %s", base, got)
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
		if r.Header.Get("X-Client-Capabilities") != "coalesced-comments-v1,platform-skill-v1" {
			t.Error("structured coalesced comments not advertised")
		}
		fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true,"new_comment_count":2,"new_comments_since":"2026-10-03T00:00:00Z","new_comments_delta_known":true,"issue_state_delta_known":true,"issue_changed_fields":["title"],"issue_status":"todo","prior_session_resume_unavailable":true,"coalesced_comment_ids":[%[1]q],"coalesced_comments":[{"id":%[1]q,"thread_id":%[1]q,"author_type":"member","author_name":"Member","content":"earlier","created_at":"2026-10-03T01:00:00Z"}]}}`, testID, testID)
	}))
	defer server.Close()
	api, _ := New(server.URL, "token")
	task, err := api.Claim(context.Background(), testID)
	if err != nil || task.NewCommentCount != 2 || !task.NewCommentsDeltaKnown || !task.IssueStateDeltaKnown || task.IssueChangedFields[0] != "title" ||
		task.IssueStatus != "todo" || !task.PriorSessionResumeUnavailable || task.CoalescedComments[0] != (Comment{ID: testID, ThreadID: testID, AuthorType: "member", AuthorName: "Member", Content: "earlier", CreatedAt: "2026-10-03T01:00:00Z"}) {
		t.Fatalf("claim deltas: %+v %v", task, err)
	}
}

func TestClaimCarriesChatFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true,"chat_session_id":%[1]q,"chat_message":"hi","chat_channel_type":"slack","chat_type":"group","chat_intro":true,"chat_message_attachments":[{"id":%[1]q,"filename":"a.png","content_type":"image/png"}]}}`, testID, testID)
	}))
	defer server.Close()
	api, _ := New(server.URL, "token")
	task, err := api.Claim(context.Background(), testID)
	if err != nil || task.ChatSessionID != testID || task.ChatMessage != "hi" || task.ChatChannelType != "slack" || task.ChatType != "group" ||
		!task.ChatIntro || task.ChatMessageAttachments[0] != (Attachment{testID, "a.png", "image/png"}) {
		t.Fatalf("chat claim: %+v %v", task, err)
	}
}

// Only claims, which carry inline skills, may exceed the 2 MiB response limit.
func TestOnlyClaimsCarryLargeResponses(t *testing.T) {
	padding := strings.Repeat("x", 3<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"start_claim_supported":true,"dispatched_at":"2026-10-04T00:00:00Z","prompt":%q}}`, testID, testID, padding)
			return
		}
		fmt.Fprintf(w, `{"status":"running","padding":%q}`, padding)
	}))
	defer server.Close()
	c, err := NewAllowHTTP(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if task, err := c.Claim(context.Background(), testID); err != nil || task == nil {
		t.Fatalf("large claim: %v", err)
	}
	if _, err := c.Status(context.Background(), testID); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("large status: %v", err)
	}
}
