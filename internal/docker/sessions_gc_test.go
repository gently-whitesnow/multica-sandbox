package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
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
	closed := s.closed(context.Background(), map[string]string{"a": "w1/agent/i1", "b": "w2/agent/i1", "c": "w2/agent/i2", "d": "malformed", "e": "w1/agent/chat:s1"})
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
		sweep(ctx, 10*time.Millisecond, time.Hour, func(context.Context) error {
			if calls.Add(1) >= 3 {
				cancel()
			}
			return errors.New("docker unavailable")
		}, nil, func(error) { reports.Add(1) })
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

// fakeEngine serves the docker calls of session collection over sized volumes on a fake disk.
type fakeEngine struct {
	mu                 sync.Mutex
	labels             map[string]string
	base, artifacts    map[string]int64
	available          uint64
	helper, unexpected []string
}

func (e *fakeEngine) run(_ context.Context, _ time.Duration, args ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case args[0] == "volume" && args[1] == "ls":
		out := ""
		for name, label := range e.labels {
			out += name + " " + label + "\n"
		}
		return []byte(out), nil
	case args[0] == "volume" && args[1] == "rm":
		name := args[3]
		e.available += uint64(e.base[name] + e.artifacts[name])
		delete(e.labels, name)
		return nil, nil
	case args[0] == "run":
		for _, flag := range []string{"--network=none", "--read-only", "--cap-drop=ALL", "--user=" + AttemptUser, "--runtime=runc"} {
			if !slices.Contains(args, flag) {
				return nil, fmt.Errorf("helper without %s", flag)
			}
		}
		mode := args[len(args)-2]
		name := strings.TrimSuffix(args[slices.Index(args, "--name")+1], "-gc")
		e.helper = append(e.helper, mode+" "+name)
		freed := int64(0)
		if mode == "prune" {
			freed, e.artifacts[name] = e.artifacts[name], 0
			e.available += uint64(freed)
		}
		return fmt.Appendf(nil, `{"freed":%d,"size":%d}`, freed, e.base[name]+e.artifacts[name]), nil
	}
	e.unexpected = append(e.unexpected, strings.Join(args, " "))
	return nil, nil
}

func (e *fakeEngine) disk() (pool.Disk, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return pool.Disk{Total: 100 << 30, Available: e.available}, nil
}

func fakeSessions(t *testing.T, e *fakeEngine, used map[string]time.Duration) *Sessions {
	old := commandWithin
	commandWithin = e.run
	t.Cleanup(func() { commandWithin = old })
	s := &Sessions{Dir: t.TempDir(), TTL: 72 * time.Hour, Max: 8, Helper: "helper"}
	e.labels = map[string]string{}
	for name, ago := range used {
		e.labels[name] = "w1/agent/i-" + name
		at := time.Now().Add(-ago)
		if err := os.WriteFile(filepath.Join(s.Dir, name), nil, 0600); err != nil || os.Chtimes(filepath.Join(s.Dir, name), at, at) != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestPressurePrunesThenRemovesIdleVolumes(t *testing.T) {
	const gib = 1 << 30
	e := &fakeEngine{available: 5 * gib,
		base:      map[string]int64{"a": 4 * gib, "b": 2 * gib, "c": 6 * gib, "busy": 20 * gib},
		artifacts: map[string]int64{"a": 3 * gib, "b": 2 * gib, "c": 1 * gib, "busy": 9 * gib}}
	s := fakeSessions(t, e, map[string]time.Duration{"a": 3 * time.Hour, "b": 2 * time.Hour, "c": time.Minute, "busy": 4 * time.Hour})
	var reclaims []Reclaim
	s.Disk, s.Reclaimed, s.busy = e.disk, func(r Reclaim) { reclaims = append(reclaims, r) }, map[string]bool{"busy": true}
	if err := s.relieve(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	// Artifacts of every idle volume go first, then the least recently used volume until
	// 15% is free; the busy volume is never touched.
	if !slices.Equal(e.helper, []string{"prune a", "prune b", "prune c"}) || e.labels["a"] != "" || len(e.labels) != 3 || len(e.unexpected) != 0 {
		t.Fatalf("helper %v volumes %v other %v", e.helper, e.labels, e.unexpected)
	}
	want := Reclaim{Trigger: "pressure", Removed: 1, Pruned: 3, Artifacts: 6 * gib, Freed: 10 * gib}
	if len(reclaims) != 1 || reclaims[0] != want || s.busy["b"] {
		t.Fatalf("reclaims %+v", reclaims)
	}
	// Above the pause watermark nothing is collected.
	e.helper, e.available = nil, 12*gib
	if err := s.relieve(context.Background(), "owner"); err != nil || len(e.helper) != 0 || len(e.labels) != 3 {
		t.Fatalf("no pressure %v %v", err, e.labels)
	}
	// Pruned volumes are not rescanned: under renewed pressure the rest go whole.
	e.available = 9 * gib
	if err := s.relieve(context.Background(), "owner"); err != nil || len(e.helper) != 0 || len(e.labels) != 1 || e.labels["busy"] == "" {
		t.Fatalf("second pass %v %v %v", err, e.helper, e.labels)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "b.gc")); !os.IsNotExist(err) {
		t.Fatal("record of a removed volume kept")
	}
}

func TestSweepPrunesAgedArtifactsAndBoundsBytes(t *testing.T) {
	const gib = 1 << 30
	e := &fakeEngine{
		base:      map[string]int64{"old": 3 * gib, "mid": 4 * gib, "new": 5 * gib, "busy": 2 * gib},
		artifacts: map[string]int64{"old": 2 * gib, "mid": gib, "new": gib, "busy": 0}}
	s := fakeSessions(t, e, map[string]time.Duration{"old": 20 * time.Hour, "mid": 13 * time.Hour, "new": time.Hour, "busy": 30 * time.Hour})
	var reclaims []Reclaim
	s.ArtifactAge, s.Budget, s.Reclaimed = 12*time.Hour, 10*gib, func(r Reclaim) { reclaims = append(reclaims, r) }
	if err := os.WriteFile(filepath.Join(s.Dir, "busy.gc"), []byte(`{"size":2147483648}`), 0600); err != nil {
		t.Fatal(err)
	}
	s.busy = map[string]bool{"busy": true}
	if err := s.collect(context.Background(), "owner", true); err != nil {
		t.Fatal(err)
	}
	// Volumes idle past the age are pruned; unmeasured ones are measured; the least recently
	// used idle volumes go until 3+4+6+2 GiB fits 10 GiB, counting the busy one.
	if !slices.Equal(e.helper, []string{"prune old", "prune mid", "du new"}) || len(e.unexpected) != 0 {
		t.Fatalf("helper %v other %v", e.helper, e.unexpected)
	}
	if e.labels["old"] != "" || e.labels["mid"] != "" || e.labels["new"] == "" || e.labels["busy"] == "" {
		t.Fatalf("volumes %v", e.labels)
	}
	want := []Reclaim{{Trigger: "age", Pruned: 2, Artifacts: 3 * gib}, {Trigger: "budget", Removed: 2}}
	if !slices.Equal(reclaims, want) {
		t.Fatalf("reclaims %+v", reclaims)
	}
	// Current records spare the next sweep any helper run; a later use makes them stale.
	e.helper = nil
	if err := s.collect(context.Background(), "owner", true); err != nil || len(e.helper) != 0 {
		t.Fatalf("rescanned %v %v", e.helper, err)
	}
	if err := s.release("new"); err != nil {
		t.Fatal(err)
	}
	if _, current := s.record("new"); current {
		t.Fatal("record survived a use")
	}
}

func TestSweepRelievesBetweenCollections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var relieved atomic.Int32
	go sweep(ctx, time.Hour, 5*time.Millisecond, func(context.Context) error { return nil }, func(context.Context) error {
		if relieved.Add(1) == 2 {
			cancel()
		}
		return nil
	}, func(error) {})
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("pressure not checked")
	}
}
