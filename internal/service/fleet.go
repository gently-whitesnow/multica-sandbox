package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type fleetAPI interface {
	Workspaces(context.Context) ([]multica.Workspace, error)
	Register(context.Context, string, string) (multica.Runtime, error)
	Recover(context.Context, string) (multica.Recovery, error)
	Heartbeat(context.Context, string) error
	ClaimBatch(context.Context, string, map[string]string, int) ([]multica.Task, error)
	Replay(context.Context) (int, error)
}
type completion struct {
	runtime string
	err     error
}
type fleet struct {
	api          fleetAPI
	daemon, dir  string
	out          io.Writer
	known, ready map[string]string
	served       map[string]bool
	active       map[string]context.CancelFunc
	done         chan completion
}

func serveFleet(ctx context.Context, c Config, dir string, api fleetAPI, p *controller.Probe, out io.Writer) error {
	known, err := readRegistry(dir)
	if err != nil {
		return err
	}
	f := &fleet{api: api, daemon: c.Daemon, dir: dir, out: out, known: known, ready: map[string]string{}, served: map[string]bool{}, active: map[string]context.CancelFunc{}, done: make(chan completion, c.Concurrency)}
	return f.serve(ctx, c.Concurrency, p.Execute)
}

func (f *fleet) serve(ctx context.Context, capacity int, execute func(context.Context, multica.Task) error) (result error) {
	defer func() {
		if ctx.Err() != nil && result == ctx.Err() {
			result = nil
		}
		for _, cancel := range f.active {
			cancel()
		}
		for len(f.active) > 0 {
			completed := <-f.done
			delete(f.active, completed.runtime)
			if completed.err != context.Canceled {
				result = errors.Join(result, completed.err)
			}
		}
	}()
	// Queued terminal reports go first, so recovery cannot rerun finished attempts.
	f.replay(ctx)
	if err := f.tolerate("sync", f.sync(ctx)); err != nil {
		return err
	}
	fmt.Fprintf(f.out, "ready workspaces=%d capacity=%d\n", len(f.ready), capacity)
	poll := time.NewTicker(time.Second)
	discovery := time.NewTicker(10 * time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	replay := time.NewTicker(replayInterval)
	defer poll.Stop()
	defer discovery.Stop()
	defer heartbeat.Stop()
	defer replay.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case completed := <-f.done:
			f.active[completed.runtime]()
			delete(f.active, completed.runtime)
			if err := f.completed(completed); err != nil {
				return err
			}
		case <-discovery.C:
			if err := f.tolerate("sync", f.sync(ctx)); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := f.tolerate("heartbeat", f.beat(ctx)); err != nil {
				return err
			}
		case <-poll.C:
			if err := f.tolerate("claim", f.claim(ctx, capacity, execute)); err != nil {
				return err
			}
		case <-replay.C:
			f.replay(ctx)
		}
	}
}

func (f *fleet) claim(ctx context.Context, capacity int, execute func(context.Context, multica.Task) error) error {
	for len(f.active) < capacity {
		scopes := map[string]string{}
		for ws, rt := range f.ready {
			if f.active[rt] == nil && !f.served[rt] {
				scopes[rt] = ws
			}
		}
		if len(scopes) == 0 {
			clear(f.served)
			return nil
		}
		// One claim per workspace per round; unused capacity is never pre-claimed.
		tasks, err := f.api.ClaimBatch(ctx, f.daemon, scopes, 1)
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			clear(f.served)
			return nil
		}
		task := tasks[0]
		runCtx, cancel := context.WithCancel(ctx)
		f.active[task.RuntimeID] = cancel
		f.served[task.RuntimeID] = true
		go func() { f.done <- completion{task.RuntimeID, execute(runCtx, task)} }()
	}
	return nil
}

const replayInterval = 5 * time.Second

func (f *fleet) replay(ctx context.Context) {
	delivered, err := f.api.Replay(ctx)
	if delivered > 0 {
		fmt.Fprintf(f.out, "replayed terminal reports=%d\n", delivered)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(f.out, "terminal report replay failed: %v\n", err)
	}
}

// tolerate keeps the controller serving through transient Multica errors, as the upstream daemon does.
func (f *fleet) tolerate(stage string, err error) error {
	if err == nil || !multica.Transient(err) {
		return err
	}
	fmt.Fprintf(f.out, "deferred %s: %v\n", stage, err)
	return nil
}

func (f *fleet) completed(done completion) error {
	if done.err == nil || done.err == context.Canceled {
		return nil
	}
	var cleanup *controller.CleanupError
	if !errors.As(done.err, &cleanup) && f.tolerate("attempt runtime="+done.runtime, done.err) == nil {
		return nil
	}
	if inaccessible(done.err) && !errors.As(done.err, &cleanup) {
		for ws, rt := range f.ready {
			if rt == done.runtime {
				f.remove(ws, rt)
			}
		}
		return nil
	}
	return done.err
}
