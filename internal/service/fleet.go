package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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
	attempt, runtime string
	err              error
}

// running is an active attempt and the runtime it was claimed for.
type running struct {
	runtime string
	cancel  context.CancelFunc
}
type fleet struct {
	api          fleetAPI
	daemon, dir  string
	out          io.Writer
	known, ready map[string]string
	served       map[string]bool
	// active holds attempts by key; perRuntime counts them per workspace runtime.
	active     map[string]running
	perRuntime map[string]int
	// workspaceLimit caps attempts per workspace; 0 leaves only the global capacity.
	workspaceLimit int
	done           chan completion
	// admit, when set, defers claims while the attempt pool is under memory pressure;
	// paused records the state so changes are logged once, refused the last refusal.
	admit   func() (bool, string)
	paused  bool
	refused time.Time
	clock   func() time.Time
	// wake carries daemon WebSocket hints and connection changes from listen; runtimes
	// hands it the ready set, published the last set handed over.
	wake      chan wakeEvent
	runtimes  chan []string
	published []string
	// wsUp and wsErr keep wakeup connection logs to state changes.
	wsUp  bool
	wsErr string
}

// wakeupAPI is the upstream daemon WebSocket (multica.Client.Wakeups).
type wakeupAPI interface {
	Wakeups(context.Context, []string, func(), func(multica.Wakeup)) error
}

// wakeEvent is a wakeup hint, or with connected/err a connection change.
type wakeEvent struct {
	multica.Wakeup
	connected bool
	err       error
}

// admissionCooldown keeps claims paused until the pool has been calm this long: under
// load the working set hovers at memory.high and single readings flap.
const admissionCooldown = 15 * time.Second

func newFleetState(api fleetAPI, daemon, dir string, out io.Writer, known map[string]string, capacity, workspaceLimit int) *fleet {
	return &fleet{api: api, daemon: daemon, dir: dir, out: out, known: known, ready: map[string]string{}, served: map[string]bool{},
		active: map[string]running{}, perRuntime: map[string]int{}, workspaceLimit: workspaceLimit, done: make(chan completion, capacity), clock: time.Now,
		wake: make(chan wakeEvent, 64), runtimes: make(chan []string, 1)}
}

func serveFleet(ctx context.Context, c Config, dir string, api fleetAPI, p *controller.Probe, out io.Writer, admit func() (bool, string)) error {
	known, err := readRegistry(dir)
	if err != nil {
		return err
	}
	f := newFleetState(api, c.Daemon, dir, out, known, c.Concurrency, c.WorkspaceConcurrency)
	f.admit = admit
	return f.serve(ctx, c.Concurrency, p.Execute)
}

func (f *fleet) serve(ctx context.Context, capacity int, execute func(context.Context, multica.Task) error) (result error) {
	defer func() {
		if ctx.Err() != nil && result == ctx.Err() {
			result = nil
		}
		for _, attempt := range f.active {
			attempt.cancel()
		}
		for len(f.active) > 0 {
			completed := <-f.done
			f.finish(completed)
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
	fmt.Fprintf(f.out, "ready workspaces=%d capacity=%d workspace_capacity=%d\n", len(f.ready), capacity, f.workspaceLimit)
	if api, ok := f.api.(wakeupAPI); ok {
		listenCtx, stop := context.WithCancel(ctx)
		defer stop()
		go f.listen(listenCtx, api)
		f.publish()
	}
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
			f.finish(completed)
			if err := f.completed(completed); err != nil {
				return err
			}
		case <-discovery.C:
			if err := f.tolerate("sync", f.sync(ctx)); err != nil {
				return err
			}
			f.publish()
		case <-heartbeat.C:
			if err := f.tolerate("heartbeat", f.beat(ctx)); err != nil {
				return err
			}
			f.publish()
		case e := <-f.wake:
			if err := f.wakeup(ctx, e, capacity, execute); err != nil {
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

// claim fills free capacity one task per workspace per round, so a busy workspace cannot
// starve its peers; unused capacity is never pre-claimed and Multica keeps the queue.
func (f *fleet) claim(ctx context.Context, capacity int, execute func(context.Context, multica.Task) error) error {
	claimed := false
	for len(f.active) < capacity && f.admitted() {
		scopes := map[string]string{}
		for ws, rt := range f.ready {
			if !f.served[rt] && (f.workspaceLimit == 0 || f.perRuntime[rt] < f.workspaceLimit) {
				scopes[rt] = ws
			}
		}
		var tasks []multica.Task
		if len(scopes) > 0 {
			var err error
			if tasks, err = f.api.ClaimBatch(ctx, f.daemon, scopes, 1); err != nil {
				return err
			}
		}
		if len(tasks) == 0 {
			clear(f.served)
			if !claimed {
				return nil
			}
			// Served workspaces may hold more work: start the next round.
			claimed = false
			continue
		}
		task, key := tasks[0], tasks[0].AttemptKey()
		if _, duplicate := f.active[key]; duplicate {
			return fmt.Errorf("claimed an attempt that is already active")
		}
		runCtx, cancel := context.WithCancel(ctx)
		f.active[key] = running{task.RuntimeID, cancel}
		f.perRuntime[task.RuntimeID]++
		f.served[task.RuntimeID], claimed = true, true
		go func() { f.done <- completion{key, task.RuntimeID, execute(runCtx, task)} }()
	}
	return nil
}

// admitted asks the pool before each claim; tasks wait in the Multica queue meanwhile.
func (f *fleet) admitted() bool {
	if f.admit == nil {
		return true
	}
	ok, status := f.admit()
	now := f.clock()
	if !ok {
		f.refused = now
	}
	calm := ok && now.Sub(f.refused) >= admissionCooldown
	switch {
	case calm && f.paused:
		fmt.Fprintf(f.out, "claims resumed: %s\n", status)
	case !ok && !f.paused:
		fmt.Fprintf(f.out, "claims paused: %s\n", status)
	}
	f.paused = !calm
	return calm
}

// finish releases a completed attempt's slot.
func (f *fleet) finish(done completion) {
	if attempt, ok := f.active[done.attempt]; ok {
		attempt.cancel()
		delete(f.active, done.attempt)
		if f.perRuntime[attempt.runtime]--; f.perRuntime[attempt.runtime] <= 0 {
			delete(f.perRuntime, attempt.runtime)
		}
	}
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
	if errors.As(done.err, &cleanup) {
		return done.err
	}
	if f.tolerate("attempt runtime="+done.runtime, done.err) == nil {
		return nil
	}
	// A refused task, for example one deleted between claim and start, ends only its
	// attempt; a revoked runtime is removed by its heartbeat (beat).
	if inaccessible(done.err) {
		fmt.Fprintf(f.out, "attempt ended runtime=%s: %v\n", done.runtime, done.err)
		return nil
	}
	return done.err
}

// publish hands listen the ready runtimes when they change.
func (f *fleet) publish() {
	ids := slices.Sorted(maps.Values(f.ready))
	if slices.Equal(ids, f.published) {
		return
	}
	f.published = ids
	select {
	case <-f.runtimes:
	default:
	}
	f.runtimes <- ids
}

// listen keeps the daemon WebSocket open for the ready runtimes, reconnecting with
// backoff and on every change of the set. Scheduled heartbeats and claim polling stay
// the correctness path; hints only make them happen now.
func (f *fleet) listen(ctx context.Context, api wakeupAPI) {
	var runtimes []string
	backoff := time.Second
	for {
		for len(runtimes) == 0 {
			select {
			case <-ctx.Done():
				return
			case runtimes = <-f.runtimes:
			}
		}
		conn, cancel := context.WithCancel(ctx)
		result := make(chan error, 1)
		started := time.Now()
		go func(ids []string) {
			result <- api.Wakeups(conn, ids, func() { f.notify(wakeEvent{connected: true}) }, func(w multica.Wakeup) { f.notify(wakeEvent{Wakeup: w}) })
		}(runtimes)
		select {
		case <-ctx.Done():
			cancel()
			return
		case runtimes = <-f.runtimes:
			cancel()
			<-result
			continue
		case err := <-result:
			cancel()
			f.notify(wakeEvent{err: err})
		}
		if time.Since(started) > 10*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case runtimes = <-f.runtimes:
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// notify drops events when the loop is behind: hints are advisory.
func (f *fleet) notify(e wakeEvent) {
	select {
	case f.wake <- e:
	default:
	}
}

// wakeup acts on a hint as the upstream daemon does: a heartbeat for pending runtime
// work (model lists), a claim round for tasks, a sync for membership changes.
func (f *fleet) wakeup(ctx context.Context, e wakeEvent, capacity int, execute func(context.Context, multica.Task) error) error {
	switch {
	case e.connected:
		if !f.wsUp {
			fmt.Fprintf(f.out, "wakeups connected runtimes=%d\n", len(f.published))
		}
		f.wsUp, f.wsErr = true, ""
	case e.err != nil:
		if f.wsUp || e.err.Error() != f.wsErr {
			fmt.Fprintf(f.out, "wakeups unavailable, polling only: %v\n", e.err)
		}
		f.wsUp, f.wsErr = false, e.err.Error()
	case e.Event == multica.WakeupPendingWork:
		for ws, rt := range f.ready {
			if rt == e.Runtime {
				return f.tolerate("heartbeat", f.beatRuntime(ctx, ws, rt))
			}
		}
	case e.Event == multica.WakeupTask:
		return f.tolerate("claim", f.claim(ctx, capacity, execute))
	case e.Event == multica.WakeupWorkspaces:
		err := f.tolerate("sync", f.sync(ctx))
		f.publish()
		return err
	}
	return nil
}
