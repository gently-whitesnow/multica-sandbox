package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type startupRun struct{ failure error }

func (r startupRun) Wait(context.Context) error   { return r.failure }
func (r startupRun) Remove(context.Context) error { return nil }
func (r startupRun) Result() execution.Result {
	return execution.Result{Output: "answer", SessionID: "ses_fresh"}
}

func TestAgentLifecycleDoesNotPostStartupMessages(t *testing.T) {
	for _, surface := range []string{"issue", "chat"} {
		for _, outcome := range []string{"complete", "failed", "rejected", "cancel-ack"} {
			t.Run(surface+"/"+outcome, func(t *testing.T) { agentLifecycle(t, surface, outcome) })
		}
	}
}

func agentLifecycle(t *testing.T, surface, outcome string) {
	t.Helper()
	const id = "10000000-0000-4000-8000-000000000001"
	posts := map[string][]map[string]any{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			posts[action] = append(posts[action], body)
		}
		if action == "status" {
			state := "running"
			if outcome == "cancel-ack" {
				state = "cancelled"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": state})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := multica.New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	p := controller.Probe{API: &agentFleetAPI{Client: client}, Interval: time.Millisecond, Duration: time.Second,
		Launch: func(context.Context, multica.Task) (execution.Run, error) {
			mu.Lock()
			if len(posts["messages"]) != 0 || len(posts["start"]) == 0 {
				t.Error("startup created transcript or omitted lifecycle start")
			}
			mu.Unlock()
			if outcome == "rejected" {
				return nil, &execution.RejectedError{Err: errors.New("private detail")}
			}
			run := startupRun{}
			if outcome == "failed" {
				run.failure = &execution.AgentFailure{Message: "opencode reported APIError"}
			}
			return run, nil
		}}
	task := multica.Task{ID: id, RuntimeID: id, DispatchedAt: time.Now().UTC().Format(time.RFC3339Nano), IssueID: id}
	if surface == "chat" {
		task.IssueID, task.ChatSessionID = "", id
	}
	// A retry (or resumed conversation) must be just as quiet as its first start.
	for range 2 {
		if err := p.Execute(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	checkStartupPosts(t, posts, outcome)
}

func checkStartupPosts(t *testing.T, posts map[string][]map[string]any, outcome string) {
	t.Helper()
	terminal := outcome
	if outcome == "rejected" || outcome == "failed" {
		terminal = "fail"
	}
	if len(posts["messages"]) != 0 || len(posts["start"]) != 2 || len(posts[terminal]) != 2 {
		t.Fatalf("lifecycle posts: %+v", posts)
	}
	for _, body := range posts[terminal] {
		if outcome == "complete" && body["output"] != "answer" {
			t.Fatalf("answer lost: %+v", body)
		}
		if terminal == "fail" && (body["error"] == "" || body["error"] == nil) {
			t.Fatalf("failure lost: %+v", body)
		}
	}
	for _, action := range []string{"complete", "fail", "cancel-ack"} {
		if action != terminal && len(posts[action]) != 0 {
			t.Fatalf("duplicate terminal: %+v", posts)
		}
	}
}
