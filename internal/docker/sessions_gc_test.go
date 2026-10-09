package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionPlanByIssueState(t *testing.T) {
	s := &Sessions{Dir: t.TempDir(), TTL: time.Hour, Grace: time.Minute, Max: 8}
	checked := time.Now()
	labels := map[string]string{}
	used := func(name, issue string, at time.Time) {
		labels[name] = "w1/agent/" + issue
		if err := os.WriteFile(filepath.Join(s.Dir, name), nil, 0600); err != nil || os.Chtimes(filepath.Join(s.Dir, name), at, at) != nil {
			t.Fatal(err)
		}
	}
	idle := checked.Add(-30 * time.Minute)
	used("done", "i-done", idle)
	used("cancelled", "i-cancelled", idle)
	used("recent", "i-recent", idle)
	used("open", "i-open", idle)
	used("busy", "i-busy", idle)
	used("reused", "i-reused", checked.Add(time.Second))
	used("expired", "i-open", checked.Add(-2*time.Hour))
	labels["unrecorded"] = "w1/agent/i-done2"
	labels["foreign"] = "w2/agent/i-done"
	s.busy = map[string]bool{"busy": true}
	old := checked.Add(-time.Hour)
	closed := map[string]time.Time{"w1/i-done": old, "w1/i-cancelled": old, "w1/i-busy": old, "w1/i-reused": old, "w1/i-done2": old, "w1/i-recent": checked}
	doomed, err := s.plan(labels, closed, checked)
	slices.Sort(doomed)
	// Closed past grace and idle since the check goes; unknown, recent, busy, reused and other workspaces stay.
	if err != nil || !slices.Equal(doomed, []string{"cancelled", "done", "expired"}) {
		t.Fatalf("doomed %v %v", doomed, err)
	}
	if !s.busy["done"] || !s.busy["busy"] || s.busy["open"] {
		t.Fatal("removal not reserved")
	}
	s.busy = nil
	s.Max = 3
	delete(labels, "expired")
	doomed, _ = s.plan(labels, nil, checked)
	// Beyond Max the least recently used go; volumes without a record count as used now.
	if len(doomed) != len(labels)-3 || slices.Contains(doomed, "unrecorded") || slices.Contains(doomed, "reused") {
		t.Fatalf("cap doomed %v", doomed)
	}
}

func TestSessionIssueChecksAreScopedPerWorkspace(t *testing.T) {
	asked := map[string][]string{}
	s := &Sessions{Closed: func(_ context.Context, workspace string, issues []string) map[string]time.Time {
		asked[workspace] = append(asked[workspace], issues...)
		if workspace == "w2" {
			return nil // a failed check keeps every volume
		}
		return map[string]time.Time{"i1": time.Unix(1, 0)}
	}}
	closed := s.closed(context.Background(), map[string]string{"a": "w1/agent/i1", "b": "w2/agent/i1", "c": "w2/agent/i2", "d": "malformed"})
	slices.Sort(asked["w2"])
	if len(asked) != 2 || !slices.Equal(asked["w1"], []string{"i1"}) || !slices.Equal(asked["w2"], []string{"i1", "i2"}) {
		t.Fatalf("asked %v", asked)
	}
	if len(closed) != 1 || closed["w1/i1"].IsZero() {
		t.Fatalf("closed %v", closed)
	}
}

func TestSweepRunsAtStartAndEveryInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls, reports atomic.Int32
	done := make(chan struct{})
	go func() {
		sweep(ctx, 10*time.Millisecond, func(context.Context) error {
			if calls.Add(1) >= 3 {
				cancel()
			}
			return errors.New("docker unavailable")
		}, func(error) { reports.Add(1) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not repeat")
	}
	// Failures are reported and the sweep keeps going; cancellation is not a failure.
	if calls.Load() < 3 || reports.Load() != 2 {
		t.Fatalf("calls=%d reports=%d", calls.Load(), reports.Load())
	}
}
