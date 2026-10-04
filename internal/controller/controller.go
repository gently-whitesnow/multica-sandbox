package controller

import (
	"context"
	"fmt"
	"time"

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
	Interval time.Duration
	Duration time.Duration
	Fail     bool
	Observe  func(string, string)
}

func (p *Probe) Connect(ctx context.Context, workspace, daemon string) (multica.Runtime, multica.Recovery, error) {
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

func (p *Probe) execute(ctx context.Context, t multica.Task, ticks <-chan time.Time) error {
	p.observe("claimed", t.ID)
	if err := p.API.RenewPreparation(ctx, t); err != nil {
		return err
	}
	if err := p.API.Start(ctx, t); err != nil {
		return err
	}
	p.observe("started", t.ID)
	if err := p.API.Message(ctx, t.ID); err != nil {
		return err
	}
	timer := time.NewTimer(p.Duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// No subprocess survives this fake executor. Restart delegates recovery to Multica.
			return ctx.Err()
		case <-ticks:
			if err := p.API.Heartbeat(ctx, t.RuntimeID); err != nil {
				return err
			}
			active, err := p.reconcile(ctx, t.ID)
			if err != nil || !active {
				return err
			}
		case <-timer.C:
			active, err := p.reconcile(ctx, t.ID)
			if err != nil || !active {
				return err
			}
			if p.Fail {
				err = p.API.Fail(ctx, t.ID)
			} else {
				err = p.API.Complete(ctx, t.ID)
			}
			if err == nil {
				p.observe("reported", t.ID)
			}
			return err
		}
	}
}

func (p *Probe) reconcile(ctx context.Context, id string) (bool, error) {
	status, err := p.API.Status(ctx, id)
	if err != nil {
		return false, err
	}
	switch status {
	case "running":
		return true, nil
	case "cancelled":
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
