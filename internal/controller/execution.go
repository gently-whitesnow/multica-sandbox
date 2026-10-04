package controller

import (
	"context"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func (p *Probe) launch(ctx context.Context, t multica.Task) (<-chan error, func() error, error) {
	if p.Backend == nil {
		return nil, func() error { return nil }, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.Duration)
	run, err := p.Backend.Start(waitCtx, t.ID+":"+t.DispatchedAt)
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
