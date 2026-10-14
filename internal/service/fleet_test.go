package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type fleetFake struct {
	workspaces []multica.Workspace
	claims     int
	recoverErr error
	calls      []string
	// queued, when set, bounds the tasks each runtime holds; otherwise every runtime has work.
	queued map[string]int
	// beats holds heartbeat errors by runtime.
	beats map[string]error
}

func (a *fleetFake) Workspaces(context.Context) ([]multica.Workspace, error) {
	return a.workspaces, nil
}
func (a *fleetFake) Register(_ context.Context, ws, _ string) (multica.Runtime, error) {
	return multica.Runtime{ID: ws}, nil
}
func (a *fleetFake) Recover(context.Context, string) (multica.Recovery, error) {
	a.calls = append(a.calls, "recover")
	return multica.Recovery{}, a.recoverErr
}
func (a *fleetFake) Heartbeat(_ context.Context, rt string) error { return a.beats[rt] }
func (a *fleetFake) Replay(context.Context) (int, error) {
	a.calls = append(a.calls, "replay")
	return 0, nil
}
func (a *fleetFake) ClaimBatch(_ context.Context, _ string, scopes map[string]string, _ int) ([]multica.Task, error) {
	a.claims++
	for _, rt := range slices.Sorted(maps.Keys(scopes)) {
		if a.queued != nil && a.queued[rt] == 0 {
			continue
		}
		if a.queued != nil {
			a.queued[rt]--
		}
		return []multica.Task{{ID: fmt.Sprintf("%s-%d", rt, a.claims), RuntimeID: rt, WorkspaceID: scopes[rt]}}, nil
	}
	return nil, nil
}
func newFleet(t *testing.T, a *fleetFake) *fleet {
	return newFleetState(a, "test", t.TempDir(), io.Discard, map[string]string{}, 16, 0)
}

// track starts attempts that block until released and reports what is running.
func track() (func(context.Context, multica.Task) error, func() []string, chan struct{}) {
	var mu sync.Mutex
	running := []string{}
	release := make(chan struct{})
	execute := func(ctx context.Context, task multica.Task) error {
		mu.Lock()
		running = append(running, task.RuntimeID)
		mu.Unlock()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return execute, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(running)
	}, release
}

func TestWorkspaceTasksRunInParallel(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 5, "b": 0}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b"}
	execute, _, release := track()
	defer close(release)
	if err := f.claim(context.Background(), 10, execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 5 || f.perRuntime["a"] != 5 {
		t.Fatalf("one workspace's queue was not drained in parallel in one poll: %d active", len(f.active))
	}
}

func TestWorkspaceConcurrencyCapsOnlyItsWorkspace(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 5, "b": 5}}
	f := newFleet(t, a)
	f.workspaceLimit = 2
	f.ready = map[string]string{"a": "a", "b": "b"}
	execute, _, release := track()
	if err := f.claim(context.Background(), 10, execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 4 || f.perRuntime["a"] != 2 || f.perRuntime["b"] != 2 {
		t.Fatalf("workspace limit not applied per workspace: %v", f.perRuntime)
	}
	release <- struct{}{}
	f.finish(<-f.done)
	if err := f.claim(context.Background(), 10, execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 4 {
		t.Fatalf("freed workspace slot not refilled: %v", f.perRuntime)
	}
	close(release)
}

func TestBusyWorkspaceSharesCapacityFairly(t *testing.T) {
	a := &fleetFake{}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b", "c": "c"}
	execute, started, release := track()
	defer close(release)
	if err := f.claim(context.Background(), 3, execute); err != nil {
		t.Fatal(err)
	}
	if len(f.perRuntime) != 3 {
		t.Fatalf("capacity not shared one task per workspace per round: %v", f.perRuntime)
	}
	eventuallyLen(t, started, 3)
}

func TestRemovedWorkspaceCancelsAllItsAttempts(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 3, "b": 1}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b"}
	execute, _, release := track()
	defer close(release)
	if err := f.claim(context.Background(), 10, execute); err != nil {
		t.Fatal(err)
	}
	f.remove("a", "a")
	for range 3 {
		done := <-f.done
		if done.runtime != "a" || done.err != context.Canceled {
			t.Fatalf("unexpected completion %v", done)
		}
		f.finish(done)
	}
	if len(f.active) != 1 || f.perRuntime["a"] != 0 || f.perRuntime["b"] != 1 {
		t.Fatalf("revocation crossed workspaces or leaked slots: %v", f.perRuntime)
	}
}

func eventuallyLen(t *testing.T, list func() []string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(list()) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("started %v, want %d", list(), n)
		}
	}
}
func TestFleetCapacityAndFairness(t *testing.T) {
	a := &fleetFake{}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b", "c": "c"}
	seen := map[string]bool{}
	execute := func(context.Context, multica.Task) error { return nil }
	for range 3 {
		if err := f.claim(context.Background(), 1, execute); err != nil {
			t.Fatal(err)
		}
		if len(f.active) != 1 {
			t.Fatal("capacity not reserved")
		}
		if err := f.claim(context.Background(), 1, execute); err != nil {
			t.Fatal(err)
		}
		done := <-f.done
		if seen[done.runtime] {
			t.Fatal("busy workspace starved a peer")
		}
		seen[done.runtime] = true
		f.finish(done)
	}
	if a.claims != 3 {
		t.Fatal("claimed without capacity")
	}
}
func TestDiscoveryCancelsRemovedWorkspaceOnly(t *testing.T) {
	ws1 := "10000000-0000-4000-8000-000000000001"
	ws2 := "10000000-0000-4000-8000-000000000002"
	a := &fleetFake{workspaces: []multica.Workspace{{ID: ws1}, {ID: ws2}}}
	f := newFleet(t, a)
	if err := f.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	one, cancelOne := context.WithCancel(context.Background())
	defer cancelOne()
	two, cancelTwo := context.WithCancel(context.Background())
	defer cancelTwo()
	f.active["one"], f.active["two"] = running{ws1, cancelOne}, running{ws2, cancelTwo}
	a.workspaces = []multica.Workspace{{ID: ws2}}
	if err := f.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if one.Err() == nil || two.Err() != nil || len(f.ready) != 1 {
		t.Fatal("revocation crossed workspace boundary")
	}
	known, err := readRegistry(f.dir)
	if err != nil || len(known) != 2 {
		t.Fatal("lost removed workspace ownership")
	}
}

// attempt starts a cancellable attempt of the runtime under key.
func (f *fleet) attempt(key, rt string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	f.active[key] = running{rt, cancel}
	f.perRuntime[rt]++
	return ctx
}

func TestRefusedTaskEndsOnlyItsAttempt(t *testing.T) {
	f := newFleet(t, &fleetFake{})
	f.ready["ws"] = "rt"
	peer := f.attempt("peer", "rt")
	for _, status := range []int{403, 404} {
		// For example the task was deleted between claim and start.
		denied := &multica.HTTPError{Status: status}
		if err := f.completed(completion{"attempt", "rt", denied}); err != nil || f.ready["ws"] != "rt" || peer.Err() != nil {
			t.Fatalf("%d: a task refusal removed the runtime or cancelled its peers: %v", status, err)
		}
		err := errors.Join(denied, &controller.CleanupError{Err: fmt.Errorf("remove failed")})
		if f.completed(completion{"attempt", "rt", err}) == nil {
			t.Fatal("ignored failed cleanup")
		}
	}
}

func TestHeartbeatRemovesOnlyARevokedRuntime(t *testing.T) {
	a := &fleetFake{beats: map[string]error{"gone": &multica.HTTPError{Status: 404}, "busy": &multica.UnansweredError{Err: &multica.HTTPError{Status: 400}}}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "gone", "b": "busy", "c": "ok"}
	gone, busy := f.attempt("1", "gone"), f.attempt("2", "busy")
	if err := f.beat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gone.Err() == nil || busy.Err() != nil || len(f.ready) != 2 || f.ready["a"] != "" {
		t.Fatalf("heartbeat removal: ready=%v", f.ready)
	}
}

func TestTransientMulticaErrorsKeepServing(t *testing.T) {
	unavailable := &multica.HTTPError{Status: 503}
	a := &fleetFake{workspaces: []multica.Workspace{{ID: "10000000-0000-4000-8000-000000000001"}}, recoverErr: unavailable}
	f := newFleet(t, a)
	if err := f.tolerate("sync", f.sync(context.Background())); err != nil || len(f.ready) != 0 {
		t.Fatal("transient recovery failure stopped the controller or readied the workspace", err)
	}
	a.recoverErr = nil
	if err := f.sync(context.Background()); err != nil || len(f.ready) != 1 {
		t.Fatal("workspace not recovered on the next sync", err)
	}
	if err := f.completed(completion{"attempt", "rt", unavailable}); err != nil {
		t.Fatal("transient attempt error stopped the controller")
	}
	if f.completed(completion{"attempt", "rt", errors.Join(unavailable, &controller.CleanupError{Err: fmt.Errorf("remove failed")})}) == nil {
		t.Fatal("cleanup uncertainty hidden by a transient error")
	}
	if f.completed(completion{"attempt", "rt", &multica.HTTPError{Status: 400}}) == nil {
		t.Fatal("protocol rejection ignored")
	}
}

func TestReplayPrecedesRecovery(t *testing.T) {
	a := &fleetFake{workspaces: []multica.Workspace{{ID: "10000000-0000-4000-8000-000000000001"}}}
	f := newFleet(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.serve(ctx, 1, func(context.Context, multica.Task) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(a.calls) < 2 || a.calls[0] != "replay" || a.calls[1] != "recover" {
		t.Fatalf("calls=%v", a.calls)
	}
}

// wakeFake is a fleet API with the daemon WebSocket: hints go out as sent.
type wakeFake struct {
	*fleetFake
	hints chan multica.Wakeup
	beat  chan string
}

func (a *wakeFake) Heartbeat(_ context.Context, rt string) error {
	a.beat <- rt
	return nil
}
func (a *wakeFake) Wakeups(ctx context.Context, _ []string, connected func(), deliver func(multica.Wakeup)) error {
	connected()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case w := <-a.hints:
			deliver(w)
		}
	}
}

func TestPendingWorkHintHeartbeatsItsRuntimeNow(t *testing.T) {
	a := &wakeFake{fleetFake: &fleetFake{workspaces: []multica.Workspace{{ID: "ws-a"}, {ID: "ws-b"}}, queued: map[string]int{}},
		hints: make(chan multica.Wakeup, 1), beat: make(chan string, 16)}
	f := newFleetState(a, "test", t.TempDir(), io.Discard, map[string]string{}, 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execute, _, _ := track()
	go func() { _ = f.serve(ctx, 1, execute) }()
	a.hints <- multica.Wakeup{Event: multica.WakeupPendingWork, Runtime: "ws-b"}
	select {
	case rt := <-a.beat:
		if rt != "ws-b" {
			t.Fatalf("heartbeat for %s, want ws-b", rt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending work waited for the scheduled heartbeat")
	}
}
