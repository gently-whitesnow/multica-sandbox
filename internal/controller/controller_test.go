package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

const id = "11111111-1111-4111-8111-111111111111"

// Like the upstream daemon, transient errors are retried or tolerated; claim, lease and exhausted retries still stop.
func TestProbeTransientControlPlaneFailures(t *testing.T) {
	tolerated := map[string]bool{"messages": true, "status": true, "heartbeat": true}
	for _, failure := range []string{"claim", "prepare-lease", "start", "messages", "status", "heartbeat", "complete"} {
		t.Run(failure, func(t *testing.T) {
			completed := false
			claimed := false
			started := false
			failedCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				if action == failure && (action != "heartbeat" || started) {
					failedCalls++
					w.WriteHeader(503)
					return
				}
				switch action {
				case "claim":
					claimed = true
					fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"start_claim_supported":true,"dispatched_at":"2026-10-04T00:00:00Z"}}`, id, id)
				case "start":
					started = true
					fmt.Fprint(w, `{}`)
				case "status":
					fmt.Fprint(w, `{"status":"running"}`)
				case "complete":
					completed = true
				}
			}))
			defer server.Close()
			api, _ := multica.New(server.URL, "token")
			p := Probe{API: api, Interval: time.Millisecond, Duration: 20 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := p.Run(ctx, id)
			if (err == nil) != tolerated[failure] || completed != tolerated[failure] {
				t.Fatalf("error=%v completed=%v", err, completed)
			}
			if failedCalls == 0 || (failure == "start" && failedCalls < 2) {
				t.Fatalf("requests=%d", failedCalls)
			}
			if failure != "claim" && !claimed {
				t.Fatal("fixture did not claim")
			}
		})
	}
}
func TestCancellationWinsOverCompletion(t *testing.T) {
	acknowledged, completed := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			fmt.Fprintf(w, `{"task":{"id":%q,"runtime_id":%q,"start_claim_supported":true,"dispatched_at":"2026-10-04T00:00:00Z"}}`, id, id)
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"status":"cancelled"}`)
		case strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			acknowledged = true
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completed = true
		}
	}))
	defer server.Close()
	api, _ := multica.New(server.URL, "token")
	p := Probe{API: api, Interval: time.Millisecond, Duration: time.Millisecond}
	if err := p.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !acknowledged || completed {
		t.Fatalf("ack=%v complete=%v", acknowledged, completed)
	}
}
func TestRecoveryMustSucceedBeforeRun(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/register") {
			fmt.Fprintf(w, `{"runtimes":[{"id":%q,"provider":"sandbox-probe"}]}`, id)
			return
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	api, _ := multica.New(server.URL, "token")
	p := Probe{API: api}
	if _, _, err := p.Connect(context.Background(), id, id); err == nil {
		t.Fatal("failed recovery ignored")
	}
	if len(calls) != 2 || !strings.HasSuffix(calls[1], "/recover-orphans") {
		t.Fatalf("unexpected startup calls: %v", calls)
	}
}
