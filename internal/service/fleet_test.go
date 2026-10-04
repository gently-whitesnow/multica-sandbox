package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"io"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type fleetFake struct {
	workspaces []multica.Workspace
	claims     int
}

func (a *fleetFake) Workspaces(context.Context) ([]multica.Workspace, error) {
	return a.workspaces, nil
}
func (a *fleetFake) Register(_ context.Context, ws, _ string) (multica.Runtime, error) {
	return multica.Runtime{ID: ws}, nil
}
func (a *fleetFake) Recover(context.Context, string) (multica.Recovery, error) {
	return multica.Recovery{}, nil
}
func (a *fleetFake) Heartbeat(context.Context, string) error { return nil }
func (a *fleetFake) ClaimBatch(_ context.Context, _ string, scopes map[string]string, _ int) ([]multica.Task, error) {
	a.claims++
	for rt, ws := range scopes {
		return []multica.Task{{ID: rt, RuntimeID: rt, WorkspaceID: ws}}, nil
	}
	return nil, nil
}
func newFleet(t *testing.T, a *fleetFake) *fleet {
	return &fleet{api: a, daemon: "test", dir: t.TempDir(), out: io.Discard, known: map[string]string{}, ready: map[string]string{}, served: map[string]bool{}, active: map[string]context.CancelFunc{}, done: make(chan completion, 4)}
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
		f.active[done.runtime]()
		delete(f.active, done.runtime)
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
	f.active[ws1] = cancelOne
	f.active[ws2] = cancelTwo
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

func TestRevokedAttemptCannotHideCleanupFailure(t *testing.T) {
	f := newFleet(t, &fleetFake{})
	f.ready["ws"] = "rt"
	denied := &multica.HTTPError{Status: 403}
	if err := f.completed(completion{"rt", denied}); err != nil || len(f.ready) != 0 {
		t.Fatal("revocation was not scoped")
	}
	err := errors.Join(denied, &controller.CleanupError{Err: fmt.Errorf("remove failed")})
	if f.completed(completion{"rt", err}) == nil {
		t.Fatal("ignored failed cleanup")
	}
}
