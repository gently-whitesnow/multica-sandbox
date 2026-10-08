package multica

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type terminalServer struct {
	mu     sync.Mutex
	status []int
	bodies []map[string]any
}

func (s *terminalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.bodies = append(s.bodies, body)
	code := 200
	if len(s.status) > 0 {
		code, s.status = s.status[0], s.status[1:]
	}
	w.WriteHeader(code)
}

func instantRetries(t *testing.T) {
	sleep := retrySleep
	retrySleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { retrySleep = sleep })
}

func outboxClient(t *testing.T, s *terminalServer) (*Client, string) {
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	c, err := New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "reports")
	if err = c.UseOutbox(dir); err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func queued(t *testing.T, dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func TestTerminalReportRetriesTransientErrorsOnly(t *testing.T) {
	instantRetries(t)
	for _, tc := range []struct {
		status   []int
		requests int
		ok       bool
	}{{[]int{503, 429, 408}, 4, true}, {[]int{400}, 1, false}, {[]int{502, 502, 502, 502, 502, 502}, 6, false}} {
		s := &terminalServer{status: tc.status}
		server := httptest.NewServer(s)
		c, _ := New(server.URL, "fixture")
		err := c.Deliver(context.Background(), Terminal{Task: testID, Output: "done"})
		server.Close()
		if (err == nil) != tc.ok || len(s.bodies) != tc.requests {
			t.Fatalf("status=%v error=%v requests=%d", tc.status, err, len(s.bodies))
		}
	}
}

func TestQueuedReportSurvivesOutageAndReplays(t *testing.T) {
	instantRetries(t)
	s := &terminalServer{status: []int{503, 503, 503, 503, 503, 503}}
	c, dir := outboxClient(t, s)
	err := c.Deliver(context.Background(), Terminal{Task: testID, Failed: true, Error: "opencode timed out after 1m0s", Reason: "timeout", Session: "ses_x", WorkDir: "volume", Retired: "ses_old"})
	if !errors.Is(err, ErrDeferred) || queued(t, dir) != 1 {
		t.Fatalf("report not durably deferred: %v", err)
	}
	if n, err := c.Replay(context.Background()); n != 0 || err != nil {
		t.Fatal("replayed before backoff elapsed", n, err)
	}
	c.outbox.now = func() time.Time { return time.Now().Add(time.Hour) }
	if n, err := c.Replay(context.Background()); n != 1 || err != nil || queued(t, dir) != 0 {
		t.Fatal("queued report not replayed", n, err)
	}
	last := s.bodies[len(s.bodies)-1]
	if last["failure_reason"] != "timeout" || last["session_id"] != "ses_x" || last["work_dir"] != "volume" || last["retired_session_id"] != "ses_old" || last["session_rollout_missing"] != nil {
		t.Fatalf("replayed payload changed: %v", last)
	}
}

func TestRepeatedRejectionQuarantinesOnlyAfterAge(t *testing.T) {
	instantRetries(t)
	s := &terminalServer{status: []int{400, 400, 400, 400}}
	c, dir := outboxClient(t, s)
	now := time.Now()
	c.outbox.now = func() time.Time { return now }
	if err := c.Deliver(context.Background(), Terminal{Task: testID}); !errors.Is(err, ErrDeferred) {
		t.Fatal(err)
	}
	for range 2 {
		now = now.Add(time.Minute)
		_, _ = c.Replay(context.Background())
	}
	if queued(t, dir) != 1 {
		t.Fatal("quick rejections quarantined the report")
	}
	now = now.Add(rejectionAge)
	_, _ = c.Replay(context.Background())
	if queued(t, dir) != 0 || queued(t, filepath.Join(dir, "failed")) != 1 {
		t.Fatal("persistent rejection not quarantined")
	}
}

func TestInFlightReportIsNotReplayedTwice(t *testing.T) {
	s := &terminalServer{}
	c, _ := outboxClient(t, s)
	if err := c.outbox.save(pending{Version: 1, Terminal: Terminal{Task: testID}}); err != nil {
		t.Fatal(err)
	}
	c.outbox.begin(testID)
	if n, _ := c.Replay(context.Background()); n != 0 || len(s.bodies) != 0 {
		t.Fatal("concurrent delivery")
	}
}
