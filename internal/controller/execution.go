package controller

import (
	"context"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func (p *Probe) launch(ctx context.Context, t multica.Task) (<-chan error, func() error, error) {
	if p.Backend == nil && p.Launch == nil {
		return nil, func() error { return nil }, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.Duration)
	var run execution.Run
	var err error
	if p.Launch != nil {
		run, err = p.Launch(waitCtx, t)
	} else {
		run, err = p.Backend.Start(waitCtx, t.AttemptKey())
	}
	if err != nil {
		cancel()
		return nil, nil, err
	}
	done := make(chan error, 1)
	waited := make(chan struct{})
	go func() { defer close(waited); done <- run.Wait(waitCtx) }()
	var once sync.Once
	var cleanupErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			<-waited
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanCancel()
			cleanupErr = run.Remove(cleanCtx)
		})
		return cleanupErr
	}
	return done, stop, nil
}

// CleanupError keeps a failed teardown distinguishable from a revoked API grant.
type CleanupError struct{ Err error }

func (e *CleanupError) Error() string { return "execution cleanup failed: " + e.Err.Error() }
func (e *CleanupError) Unwrap() error { return e.Err }
