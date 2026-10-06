package controller

import (
	"context"
	"errors"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"testing"
	"time"
)

type queueAPI struct {
	lifecycleAPI
	claims int
	failAt int
	cancel context.CancelFunc
}

func (a *queueAPI) Claim(context.Context, string) (*multica.Task, error) {
	a.claims++
	if a.claims == a.failAt {
		return nil, errors.New("claim unavailable")
	}
	return &multica.Task{ID: id}, nil
}
func (a *queueAPI) Complete(ctx context.Context, id string, result execution.Result) error {
	if a.claims == 2 && a.cancel != nil {
		a.cancel()
	}
	return a.lifecycleAPI.Complete(ctx, id, result)
}
func TestServeSequentialAttemptsAndStopsOnError(t *testing.T) {
	for _, failure := range []bool{false, true} {
		events := []string{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api := &queueAPI{lifecycleAPI: lifecycleAPI{events: &events, status: "running"}, cancel: cancel}
		if failure {
			api.failAt = 2
		}
		p := Probe{API: api, Duration: time.Millisecond, Interval: time.Second, Backend: &testBackend{events: &events}}
		err := p.Serve(ctx, id)
		if (err != nil) != failure || api.claims != 2 {
			t.Fatalf("claims=%d error=%v", api.claims, err)
		}
		launched, removed := 0, 0
		for _, event := range events {
			if event == "launch" {
				launched++
			}
			if event == "remove" {
				removed++
			}
		}
		want := 2
		if failure {
			want = 1
		}
		if launched != want || removed != want {
			t.Fatalf("launch=%d remove=%d", launched, removed)
		}
	}
}
