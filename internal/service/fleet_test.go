package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
	return newFleetState(a, "test", t.TempDir(), slog.New(slog.DiscardHandler), map[string]string{})
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
	if err := f.claim(context.Background(), execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 5 || f.perRuntime["a"] != 5 {
		t.Fatalf("one workspace's queue was not drained in parallel in one poll: %d active", len(f.active))
	}
}

func TestBusyWorkspaceSharesAdmissionFairly(t *testing.T) {
	a := &fleetFake{}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b", "c": "c"}
	f.admit = func() (bool, string, string) { return len(f.active) < 3, "pressure", "test pool" }
	execute, started, release := track()
	defer close(release)
	if err := f.claim(context.Background(), execute); err != nil {
		t.Fatal(err)
	}
	if len(f.perRuntime) != 3 {
		t.Fatalf("admission not shared one task per workspace per round: %v", f.perRuntime)
	}
	eventuallyLen(t, started, 3)
}

// The controller has no concurrency limit of its own (Multica enforces each agent's
// max_concurrent_tasks at claim): it claims past the removed cap of 64 while the pool
// admits, stops at a refusal and otherwise when Multica hands out no task.
func TestClaimsUntilAdmissionOrMulticaStops(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 50, "b": 50}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b"}
	now := time.Now()
	f.clock = func() time.Time { return now }
	admitted := 80
	f.admit = func() (bool, string, string) { return len(f.active) < admitted, "pressure", "test pool" }
	execute, _, release := track()
	defer close(release)
	if err := f.claim(context.Background(), execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 80 || f.perRuntime["a"] != 40 || f.perRuntime["b"] != 40 || a.claims != 80 {
		t.Fatalf("claims did not stop fairly at the admission refusal: %v, %d claim calls", f.perRuntime, a.claims)
	}
	admitted = 1000
	if err := f.claim(context.Background(), execute); err != nil || len(f.active) != 80 {
		t.Fatalf("claimed within the cooldown: %v %d", err, len(f.active))
	}
	now = now.Add(admissionCooldown)
	// The refusal followed a full round: the first poll only resets the served set.
	for range 2 {
		if err := f.claim(context.Background(), execute); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.active) != 100 || a.queued["a"] != 0 || a.queued["b"] != 0 {
		t.Fatalf("queue not drained while admitted: %d active", len(f.active))
	}
}

func TestRemovedWorkspaceCancelsAllItsAttempts(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 3, "b": 1}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b"}
	execute, _, release := track()
	defer close(release)
	if err := f.claim(context.Background(), execute); err != nil {
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

// A round interrupted by an admission refusal resumes with the workspaces it did not serve.
func TestRefusedRoundKeepsFairness(t *testing.T) {
	a := &fleetFake{}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a", "b": "b", "c": "c"}
	now := time.Now()
	f.clock = func() time.Time { return now }
	f.admit = func() (bool, string, string) { return len(f.active) == 0, "pressure", "test pool" }
	seen := map[string]bool{}
	execute := func(context.Context, multica.Task) error { return nil }
	for range 3 {
		now = now.Add(admissionCooldown)
		if err := f.claim(context.Background(), execute); err != nil {
			t.Fatal(err)
		}
		if len(f.active) != 1 {
			t.Fatal("admission refusal not applied")
		}
		done := <-f.done
		if seen[done.runtime] {
			t.Fatal("busy workspace starved a peer")
		}
		seen[done.runtime] = true
		f.finish(done)
	}
	if a.claims != 3 {
		t.Fatal("claimed while admission refused")
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
	for _, status := range []int{403, 404, 409} {
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
	// A backend start error may join a failed removal and leaves the task running in Multica.
	if f.completed(completion{"attempt", "rt", errors.New("docker start failed")}) == nil {
		t.Fatal("backend failure with uncertain host state ignored")
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
	if err := f.serve(ctx, func(context.Context, multica.Task) error { return nil }); err != nil {
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
	f := newFleetState(a, "test", t.TempDir(), slog.New(slog.DiscardHandler), map[string]string{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execute, _, _ := track()
	go func() { _ = f.serve(ctx, execute) }()
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

// TestDrainHoldsClaimsUntilReleased pins the deploy drain: running attempts keep
// running, nothing new is claimed while draining, and release resumes claims.
func TestDrainHoldsClaimsUntilReleased(t *testing.T) {
	a := &fleetFake{queued: map[string]int{"a": 3}}
	f := newFleet(t, a)
	f.ready = map[string]string{"a": "a"}
	execute, _, release := track()
	defer close(release)
	if err := f.claim(context.Background(), execute); err != nil || len(f.active) != 3 {
		t.Fatalf("first claim: %v, %d active", err, len(f.active))
	}
	f.setDraining(true)
	release <- struct{}{}
	f.finish(<-f.done)
	a.queued["a"] = 2
	claims := a.claims
	if err := f.claim(context.Background(), execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 2 || a.claims != claims {
		t.Fatalf("drained controller claimed: %d active, %d claim calls", len(f.active), a.claims-claims)
	}
	f.setDraining(false)
	if err := f.claim(context.Background(), execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 4 {
		t.Fatalf("released drain did not resume claims: %d active", len(f.active))
	}
}

func TestDrainSignalReachesServeLoop(t *testing.T) {
	a := &fleetFake{workspaces: []multica.Workspace{{ID: "ws-a"}}, queued: map[string]int{}}
	f := newFleetState(a, "test", t.TempDir(), slog.New(slog.DiscardHandler), map[string]string{})
	drain := make(chan bool)
	f.drain = drain
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.serve(ctx, func(context.Context, multica.Task) error { return nil }) }()
	drain <- true
	drain <- false
	drain <- true
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if !f.draining {
		t.Fatal("drain request not applied")
	}
}

// Shutdown cancels every attempt and receives each completion from the unbuffered
// done channel, here more attempts than the removed cap of 64.
func TestShutdownCollectsEveryAttempt(t *testing.T) {
	a := &fleetFake{workspaces: []multica.Workspace{{ID: "ws-a"}, {ID: "ws-b"}}, queued: map[string]int{"ws-a": 40, "ws-b": 40}}
	f := newFleet(t, a)
	execute, started, release := track()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.serve(ctx, execute) }()
	eventuallyLen(t, started, 80)
	cancel()
	select {
	case err := <-served:
		if err != nil || len(f.active) != 0 {
			t.Fatalf("shutdown: %v, %d active", err, len(f.active))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not collect every attempt")
	}
}

// lockedFake lets a test add queued work while serve claims.
type lockedFake struct {
	*fleetFake
	mu sync.Mutex
}

func (a *lockedFake) ClaimBatch(ctx context.Context, daemon string, scopes map[string]string, n int) ([]multica.Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fleetFake.ClaimBatch(ctx, daemon, scopes, n)
}

// syncLog is a log destination the test reads while serve writes.
type syncLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A failed cleanup stops claims but not the neighbours: they finish and report, then
// serve returns the error for a restart.
func TestFatalAttemptErrorLetsNeighboursFinish(t *testing.T) {
	a := &lockedFake{fleetFake: &fleetFake{workspaces: []multica.Workspace{{ID: "ws-a"}, {ID: "ws-b"}}, queued: map[string]int{"ws-a": 1, "ws-b": 1}}}
	var log syncLog
	f := newFleetState(a, "test", t.TempDir(), slog.New(slog.NewTextHandler(&log, nil)), map[string]string{})
	fatal := &controller.CleanupError{Err: errors.New("remove failed")}
	release, neighbour := make(chan struct{}), make(chan error, 1)
	execute := func(ctx context.Context, task multica.Task) error {
		if task.RuntimeID == "ws-a" {
			return fatal
		}
		select {
		case <-release:
			neighbour <- nil
		case <-ctx.Done():
			neighbour <- ctx.Err()
		}
		return nil
	}
	served := make(chan error, 1)
	go func() { served <- f.serve(context.Background(), execute) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), "claims stopped"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("fatal attempt error not handled: %s", log.String())
		}
	}
	a.mu.Lock()
	a.queued["ws-a"], a.queued["ws-b"] = 5, 5
	claims := a.claims
	a.mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	select {
	case err := <-served:
		t.Fatalf("serve returned before the neighbour finished: %v", err)
	case err := <-neighbour:
		t.Fatalf("neighbour ended early: %v", err)
	default:
	}
	a.mu.Lock()
	if a.claims != claims {
		t.Fatalf("claimed %d times while stopping", a.claims-claims)
	}
	a.mu.Unlock()
	close(release)
	select {
	case err := <-served:
		if !errors.Is(err, fatal) || <-neighbour != nil || len(f.active) != 0 {
			t.Fatalf("serve returned %v with %d active", err, len(f.active))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the neighbour finished")
	}
}
