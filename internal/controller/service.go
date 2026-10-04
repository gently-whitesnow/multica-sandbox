package controller

import "context"

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
