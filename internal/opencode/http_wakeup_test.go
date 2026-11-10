package opencode

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestHTTPSupplementHintWakesOwningAttemptBeforePollingFallback(t *testing.T) {
	status := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			_, _ = io.WriteString(w, `{"supplement_capability":"task-supplement-v1"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/supplements") {
			status <- struct{}{}
			_, _ = io.WriteString(w, `{"task_status":"running"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/claim") {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	r, h, _ := nativeHTTPFixture()
	r.task.IssueID = "60000000-0000-4000-8000-000000000001"
	client, err := multica.New(server.URL, "fixture-controller-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UseSupplements(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background(), r.task); err != nil {
		t.Fatal(err)
	}
	if !client.SupplementsNegotiated(r.task) {
		t.Fatal("fixture did not negotiate supplements")
	}
	// Start probes the receipt endpoint before offering the capability.
	<-status
	r.adapter.Steering = client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.supplements(ctx) }()
	httpWaitSupplementStatus(t, status)
	client.WakeSupplement(r.task.RuntimeID, r.task.IssueID)
	select {
	case <-status:
		t.Fatal("foreign task woke the supplement worker")
	case <-time.After(30 * time.Millisecond):
	}
	client.WakeSupplement(r.task.RuntimeID, r.task.ID)
	httpWaitSupplementStatus(t, status)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supplement worker did not stop")
	}
}

func httpWaitSupplementStatus(t *testing.T, status <-chan struct{}) {
	t.Helper()
	select {
	case <-status:
	case <-time.After(time.Second):
		t.Fatal("owning worker did not check status promptly")
	}
}
