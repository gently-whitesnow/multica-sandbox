package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type API interface {
	Register(context.Context, string, string) (multica.Runtime, error)
	Recover(context.Context, string) (multica.Recovery, error)
	Heartbeat(context.Context, string) error
	Claim(context.Context, string) (*multica.Task, error)
	RenewPreparation(context.Context, multica.Task) error
	Start(context.Context, multica.Task) error
	Status(context.Context, string) (string, error)
	Message(context.Context, string) error
	Complete(context.Context, string) error
	Fail(context.Context, string) error
	CancelAck(context.Context, string) error
}

type Probe struct {
	API      API
	Backend  execution.Backend
	Interval time.Duration
	Duration time.Duration
	Fail     bool
	Observe  func(string, string)
	Launch   func(context.Context, multica.Task) (execution.Run, error)
}

func (p *Probe) Connect(ctx context.Context, workspace, daemon string) (multica.Runtime, multica.Recovery, error) {
	if p.Backend != nil {
		if err := p.Backend.Reconcile(ctx); err != nil {
			return multica.Runtime{}, multica.Recovery{}, err
		}
	}
	rt, err := p.API.Register(ctx, workspace, daemon)
	if err != nil {
		return rt, multica.Recovery{}, err
	}
	recovered, err := p.API.Recover(ctx, rt.ID)
	return rt, recovered, err
}

func (p *Probe) Run(ctx context.Context, runtime string) error {
	if p.Interval <= 0 || p.Interval > 10*time.Second || p.Duration <= 0 {
		return fmt.Errorf("interval must be in (0,10s], duration must be positive")
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		if err := p.API.Heartbeat(ctx, runtime); err != nil {
			return err
		}
		task, err := p.API.Claim(ctx, runtime)
		if err != nil {
			return err
		}
		if task != nil {
			return p.execute(ctx, *task, ticker.C)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Probe) execute(ctx context.Context, t multica.Task, ticks <-chan time.Time) (result error) {
	p.observe("claimed", t.ID)
	if err := p.API.RenewPreparation(ctx, t); err != nil {
		return err
	}
	if err := p.API.Start(ctx, t); err != nil {
		return err
	}
	if err := p.API.Message(ctx, t.ID); err != nil {
		return err
	}
	done, stop, err := p.launch(ctx, t)
	if err != nil {
		var rejected *execution.RejectedError
		if errors.As(err, &rejected) {
			return p.finish(ctx, t.ID, err, func() error { return nil })
		}
		return err
	}
	defer func() {
		if cleanupErr := stop(); cleanupErr != nil {
			result = errors.Join(result, &CleanupError{Err: cleanupErr})
		}
	}()
	p.observe("started", t.ID)
	timer := time.NewTimer(p.Duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
			if err := p.API.Heartbeat(ctx, t.RuntimeID); err != nil {
				return err
			}
			active, err := p.reconcile(ctx, t.ID, stop)
			if err != nil || !active {
				return err
			}
		case err := <-done:
			return p.finish(ctx, t.ID, err, stop)
		case <-timer.C:
			var failure error
			if p.Backend != nil || p.Fail {
				failure = fmt.Errorf("execution failed or timed out")
			}
			return p.finish(ctx, t.ID, failure, stop)
		}
	}
}
func (p *Probe) finish(ctx context.Context, id string, failure error, stop func() error) error {
	if err := stop(); err != nil {
		return err
	}
	active, err := p.reconcile(ctx, id, stop)
	if err != nil || !active {
		return err
	}
	if failure != nil {
		err = p.API.Fail(ctx, id)
	} else {
		err = p.API.Complete(ctx, id)
	}
	if err == nil {
		p.observe("reported", id)
	}
	return err
}
func (p *Probe) reconcile(ctx context.Context, id string, stop func() error) (bool, error) {
	status, err := p.API.Status(ctx, id)
	if err != nil {
		return false, err
	}
	switch status {
	case "running":
		return true, nil
	case "cancelled":
		if err := stop(); err != nil {
			return false, err
		}
		err = p.API.CancelAck(ctx, id)
		if err == nil {
			p.observe("cancelled", id)
		}
		return false, err
	case "completed", "failed":
		p.observe("settled", id)
		return false, nil
	default:
		return false, fmt.Errorf("unexpected task state; execution stopped")
	}
}
func (p *Probe) observe(event, id string) {
	if p.Observe != nil {
		p.Observe(event, id)
	}
}
