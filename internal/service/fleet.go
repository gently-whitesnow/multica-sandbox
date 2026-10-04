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
	if err := f.sync(ctx); err != nil {
		return err
	}
	fmt.Fprintf(f.out, "ready workspaces=%d capacity=%d\n", len(f.ready), capacity)
	poll := time.NewTicker(time.Second)
	discovery := time.NewTicker(10 * time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer discovery.Stop()
	defer heartbeat.Stop()
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
			if err := f.sync(ctx); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := f.beat(ctx); err != nil {
				return err
			}
		case <-poll.C:
			if err := f.claim(ctx, capacity, execute); err != nil {
				return err
			}
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

func (f *fleet) completed(done completion) error {
	if done.err == nil || done.err == context.Canceled {
		return nil
	}
	var cleanup *controller.CleanupError
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
