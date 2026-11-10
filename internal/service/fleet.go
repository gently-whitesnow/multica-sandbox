package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
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
	log          *slog.Logger
	metrics      *metrics.Metrics
	known, ready map[string]string
	served       map[string]bool
	// active holds attempts by key; perRuntime counts them per workspace runtime.
	active     map[string]running
	perRuntime map[string]int
	// done is unbuffered: each attempt sends once, and serve receives every active
	// attempt, also on shutdown.
	done chan completion
	// admit, when set, defers claims while the attempt pool is under memory pressure;
	// paused records the state so changes are logged once, refused the last refusal
	// and reason its bounded cause.
	admit         func() (bool, string, string)
	paused        bool
	refused       time.Time
	reason        string
	clock         func() time.Time
	autoAdmission *pool.AutoAdmission
	// wake carries daemon WebSocket hints and connection changes from listen; runtimes
	// hands it the ready set, published the last set handed over.
	wake      chan wakeEvent
	runtimes  chan []string
	published []string
	// wsUp and wsErr keep wakeup connection logs to state changes.
	wsUp  bool
	wsErr string
	// drain carries deploy drain requests (true) and their release (false); while
	// draining no task is claimed, running attempts finish and report as usual.
	drain    <-chan bool
	draining bool
	// stopping holds controller-fatal attempt errors: no task is claimed, the other
	// attempts finish and report, then serve returns them for a restart.
	stopping error
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

func newFleetState(api fleetAPI, daemon, dir string, log *slog.Logger, known map[string]string) *fleet {
	return &fleet{api: api, daemon: daemon, dir: dir, log: log, known: known, ready: map[string]string{}, served: map[string]bool{},
		active: map[string]running{}, perRuntime: map[string]int{}, done: make(chan completion), clock: time.Now,
		wake: make(chan wakeEvent, 64), runtimes: make(chan []string, 1)}
}

func serveFleet(ctx context.Context, c Config, dir string, api fleetAPI, p *controller.Probe, admit func() (bool, string, string)) error {
	known, err := readRegistry(dir)
	if err != nil {
		return err
	}
	f := newFleetState(api, c.Daemon, dir, c.logger(), known)
	f.admit, f.metrics, f.drain = admit, c.metrics, c.Drain
	if c.OpenCode != nil && c.OpenCode.Pool != nil && c.OpenCode.Pool.AutoAdmission {
		pc := c.OpenCode.Pool
		workingSet := pc.MaxWorkingSet
		if workingSet == 0 {
			workingSet = pool.DefaultMaxWorkingSet
		}
		f.autoAdmission = &pool.AutoAdmission{Pool: &pool.Pool{Dir: pc.CgroupDir, MaxPressure: pool.DefaultMaxPressure, MaxWorkingSet: workingSet}, DiskPath: pc.DiskPath}
		if f.autoAdmission.AttemptMax, err = attemptCeiling(pc, f.autoAdmission.Pool); err != nil {
			return err
		}
	}
	return f.serve(ctx, p.Execute)
}

func (f *fleet) serve(ctx context.Context, execute func(context.Context, multica.Task) error) (result error) {
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
	// Durable delivery acknowledgements and terminal reports precede recovery.
	f.replay(ctx)
	if err := f.tolerate("sync", f.sync(ctx)); err != nil {
		return err
	}
	f.metrics.Workspaces(len(f.ready))
	f.log.Info("controller ready", "workspaces", len(f.ready), "version", metrics.Version, "revision", metrics.Revision)
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
			return f.stopping
		case completed := <-f.done:
			f.finish(completed)
			if err := f.completed(completed); err != nil {
				f.stop(err)
			}
			if f.stopping != nil && len(f.active) == 0 {
				return f.stopping
			}
		case draining := <-f.drain:
			f.setDraining(draining)
		case <-discovery.C:
			if err := f.tolerate("sync", f.sync(ctx)); err != nil {
				return err
			}
			f.metrics.Workspaces(len(f.ready))
			f.publish()
		case <-heartbeat.C:
			if err := f.tolerate("heartbeat", f.beat(ctx)); err != nil {
				return err
			}
			f.publish()
		case e := <-f.wake:
			if err := f.wakeup(ctx, e, execute); err != nil {
				return err
			}
		case <-poll.C:
			if err := f.tolerate("claim", f.claim(ctx, execute)); err != nil {
				return err
			}
		case <-replay.C:
			f.replay(ctx)
		}
	}
}

// claim takes one task per workspace per round, so a busy workspace cannot starve its
// peers, until draining, pool admission or Multica stops it. Concurrency is each agent's
// max_concurrent_tasks, which Multica enforces at claim; Multica keeps the queue.
func (f *fleet) claim(ctx context.Context, execute func(context.Context, multica.Task) error) error {
	if f.draining || f.stopping != nil {
		return nil
	}
	if f.autoAdmission != nil {
		f.autoAdmission.Observe(len(f.active), f.clock())
	}
	claimed := false
	for f.admitted() {
		scopes := map[string]string{}
		for ws, rt := range f.ready {
			if !f.served[rt] {
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
		f.metrics.Active(1)
		if f.autoAdmission != nil {
			f.autoAdmission.Started(f.clock())
		}
		go func() { f.done <- completion{key, task.RuntimeID, execute(runCtx, task)} }()
	}
	return nil
}

// setDraining holds or releases claims for a controller deploy; Multica keeps the queue.
func (f *fleet) setDraining(draining bool) {
	if draining == f.draining {
		return
	}
	f.draining = draining
	f.metrics.Draining(draining)
	switch {
	case draining:
		f.log.Info("claims draining", "active", len(f.active))
	case f.stopping != nil:
		f.log.Info("drain released; claims stay stopped until the restart", "active", len(f.active))
	default:
		f.log.Info("claims resumed after drain", "active", len(f.active))
	}
}

// stop holds claims after a controller-fatal attempt error, so it does not cancel the
// attempts still running; the restart reconciles what the failed one left (ADR 0006).
func (f *fleet) stop(err error) {
	if f.stopping == nil {
		f.metrics.Paused(true, "restart")
	}
	f.stopping = errors.Join(f.stopping, err)
	f.log.Error("claims stopped: the controller restarts once running attempts finish", "active", len(f.active), "error", err.Error())
}

// admitted asks the pool before each claim; tasks wait in the Multica queue meanwhile.
func (f *fleet) admitted() bool {
	if f.admit == nil {
		return true
	}
	ok, reason, status := f.admit()
	now := f.clock()
	if ok && f.autoAdmission != nil {
		ok, reason, status = f.autoAdmission.Admit(len(f.active), now)
	}
	// Warmup paces starts: it neither pauses claims nor restarts the cooldown.
	if !ok && reason != pool.RefusalWarmup {
		f.refused, f.reason = now, reason
	}
	calm := now.Sub(f.refused) >= admissionCooldown
	switch {
	case calm && f.paused:
		f.log.Info("claims resumed", "pool", status)
	case !calm && !f.paused:
		f.log.Warn("claims paused", "pool", status)
	}
	f.paused = !calm
	f.metrics.Paused(f.paused, f.reason)
	return ok && calm
}

// finish forgets a completed attempt.
func (f *fleet) finish(done completion) {
	if attempt, ok := f.active[done.attempt]; ok {
		attempt.cancel()
		delete(f.active, done.attempt)
		f.metrics.Active(-1)
		if f.perRuntime[attempt.runtime]--; f.perRuntime[attempt.runtime] <= 0 {
			delete(f.perRuntime, attempt.runtime)
		}
	}
}

const replayInterval = 5 * time.Second

func (f *fleet) replay(ctx context.Context) {
	delivered, err := f.api.Replay(ctx)
	if delivered > 0 {
		f.log.Info("durable reports replayed", "reports", delivered)
	}
	if err != nil && ctx.Err() == nil {
		f.log.Warn("terminal report replay failed", "error", err.Error())
	}
}

// tolerate keeps the controller serving through transient Multica errors, as the upstream daemon does.
func (f *fleet) tolerate(stage string, err error, attrs ...any) error {
	if err == nil || !multica.Transient(err) {
		return err
	}
	f.log.Warn("deferred "+stage, append(attrs, "error", err.Error())...)
	return nil
}

// completed returns an error only for an attempt that may leave host state behind: a
// failed cleanup, a backend error (start failures join raw cleanup errors) or a task
// left running in Multica. Restart reconciles resources and recovers orphans (ADR 0006).
// Rejected tasks never arrive here: they are reported as failures.
func (f *fleet) completed(done completion) error {
	if done.err == nil || done.err == context.Canceled {
		return nil
	}
	var cleanup *controller.CleanupError
	if errors.As(done.err, &cleanup) {
		return done.err
	}
	if f.tolerate("attempt", done.err, "runtime", done.runtime) == nil {
		return nil
	}
	// A task refused or settled elsewhere, for example deleted between claim and start,
	// ends only its attempt; a revoked runtime is removed by its heartbeat (beat).
	if inaccessible(done.err) || multica.Conflict(done.err) {
		f.log.Warn("attempt ended", "runtime", done.runtime, "error", done.err.Error())
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
func (f *fleet) wakeup(ctx context.Context, e wakeEvent, execute func(context.Context, multica.Task) error) error {
	switch {
	case e.connected:
		if !f.wsUp {
			f.log.Info("wakeups connected", "runtimes", len(f.published))
		}
		f.wsUp, f.wsErr = true, ""
	case e.err != nil:
		if f.wsUp || e.err.Error() != f.wsErr {
			f.log.Warn("wakeups unavailable, polling only", "error", e.err.Error())
		}
		f.wsUp, f.wsErr = false, e.err.Error()
	case e.Event == multica.WakeupPendingWork:
		for ws, rt := range f.ready {
			if rt == e.Runtime {
				return f.tolerate("heartbeat", f.beatRuntime(ctx, ws, rt))
			}
		}
	case e.Event == multica.WakeupTask:
		return f.tolerate("claim", f.claim(ctx, execute))
	case e.Event == multica.WakeupWorkspaces:
		err := f.tolerate("sync", f.sync(ctx))
		f.publish()
		return err
	}
	return nil
}
