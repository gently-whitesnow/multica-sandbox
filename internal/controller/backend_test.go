package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type lifecycleAPI struct {
	API
	events   *[]string
	status   string
	failure  string
	reported *error
}

func (a lifecycleAPI) record(s string) error {
	*a.events = append(*a.events, s)
	if a.failure == s {
		return errors.New(s)
	}
	return nil
}
func (a lifecycleAPI) Register(context.Context, string, string) (multica.Runtime, error) {
	return multica.Runtime{ID: id}, a.record("register")
}
func (a lifecycleAPI) Recover(context.Context, string) (multica.Recovery, error) {
	return multica.Recovery{}, a.record("recover")
}
func (a lifecycleAPI) RenewPreparation(context.Context, multica.Task) error { return a.record("lease") }
func (a lifecycleAPI) Start(context.Context, multica.Task) error            { return a.record("start") }
func (a lifecycleAPI) Message(context.Context, string) error                { return a.record("message") }
func (a lifecycleAPI) Status(context.Context, string) (string, error) {
	return a.status, a.record("status")
}
func (a lifecycleAPI) Heartbeat(context.Context, string) error { return a.record("heartbeat") }
func (a lifecycleAPI) Complete(context.Context, string) error  { return a.record("complete") }
func (a lifecycleAPI) Fail(_ context.Context, _ string, cause error) error {
	if a.reported != nil {
		*a.reported = cause
	}
	return a.record("fail")
}
func (a lifecycleAPI) CancelAck(context.Context, string) error { return a.record("ack") }

type testBackend struct {
	events                                                         *[]string
	cleanupError, errorBeforeRecovery, errorStarting, errorWaiting error
	running                                                        bool
}

func (b *testBackend) Reconcile(context.Context) error {
	*b.events = append(*b.events, "reap")
	return b.errorBeforeRecovery
}
func (b *testBackend) Start(context.Context, string) (execution.Run, error) {
	*b.events = append(*b.events, "launch")
	return b, b.errorStarting
}
func (b *testBackend) Wait(ctx context.Context) error {
	if b.running {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.errorWaiting
}
func (b *testBackend) Remove(context.Context) error {
	*b.events = append(*b.events, "remove")
	return b.cleanupError
}

func TestRecoveryRequiresSuccessfulReaping(t *testing.T) {
	for _, fail := range []bool{false, true} {
		events := []string{}
		b := &testBackend{events: &events}
		if fail {
			b.errorBeforeRecovery = errors.New("unavailable")
		}
		p := Probe{API: lifecycleAPI{events: &events}, Backend: b}
		_, _, err := p.Connect(context.Background(), id, id)
		want := []string{"reap", "register", "recover"}
		if fail {
			want = []string{"reap"}
		}
		if (err != nil) != fail || !reflect.DeepEqual(events, want) {
			t.Fatalf("events=%v error=%v", events, err)
		}
	}
}
func TestCleanupPrecedesTerminalCallback(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "cancel", "timeout", "cleanup-failure", "status-failure", "start-failure", "heartbeat-failure", "shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			events := []string{}
			var reported error
			a := lifecycleAPI{events: &events, status: "running", reported: &reported}
			b := &testBackend{events: &events}
			terminal := "complete"
			switch scenario {
			case "failure":
				b.errorWaiting = errors.New("exit 1")
				terminal = "fail"
			case "cancel":
				a.status = "cancelled"
				terminal = "ack"
			case "timeout":
				b.running = true
				terminal = "fail"
			case "cleanup-failure":
				b.cleanupError = errors.New("daemon unavailable")
				terminal = ""
			case "status-failure":
				a.failure = "status"
				terminal = ""
			case "heartbeat-failure":
				a.failure = "heartbeat"
				b.running = true
				terminal = ""
			case "shutdown":
				b.running = true
				terminal = ""
			case "start-failure":
				b.errorStarting = errors.New("create failed")
				terminal = ""
			}
			p := Probe{API: a, Backend: b, Duration: 10 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ticks := make(chan time.Time, 1)
			if scenario == "heartbeat-failure" {
				ticks <- time.Now()
			}
			if scenario == "shutdown" {
				cancel()
			}
			err := p.execute(ctx, multica.Task{ID: id}, ticks)
			if (err != nil) != (terminal == "") {
				t.Fatalf("error=%v events=%v", err, events)
			}
			if scenario == "failure" && reported != b.errorWaiting {
				t.Fatal("execution cause lost")
			}
			removed := false
			for _, event := range events {
				if event == "remove" {
					removed = true
				}
				if event == "complete" || event == "fail" || event == "ack" {
					if !removed || event != terminal {
						t.Fatalf("unsafe callback: %v", events)
					}
					terminal = ""
				}
			}
			if !removed && scenario != "start-failure" {
				t.Fatalf("execution not cleaned: %v", events)
			}
			if terminal != "" {
				t.Fatalf("missing callback: %v", events)
			}
		})
	}
}

func TestRejectedAgentDoesNotStopController(t *testing.T) {
	events := []string{}
	p := Probe{API: lifecycleAPI{events: &events, status: "running"}, Duration: time.Second, Interval: time.Millisecond, Launch: func(context.Context, multica.Task) (execution.Run, error) {
		return nil, &execution.RejectedError{Err: errors.New("conflicting authorization")}
	}}
	if err := p.Execute(context.Background(), multica.Task{ID: id}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lease", "start", "message", "status", "fail"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("callbacks=%v", events)
	}
}
