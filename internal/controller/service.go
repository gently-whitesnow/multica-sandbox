package controller

import (
	"context"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"time"
)

// Serve reuses only the controller, never an execution environment.
func (p *Probe) Serve(ctx context.Context, runtime string) error {
	for {
		if err := p.Run(ctx, runtime); err != nil {
			if err == ctx.Err() {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Execute runs one task of a fleet runtime; the fleet heartbeats its runtimes.
func (p *Probe) Execute(ctx context.Context, task multica.Task) error {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	return p.execute(ctx, task, ticker.C, false)
}
