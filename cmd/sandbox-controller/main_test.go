package main

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestDrainSignals pins the host deploy contract: SIGUSR1 drains, SIGUSR2 resumes.
func TestDrainSignals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drain := drainSignals(ctx)
	for _, want := range []bool{true, false} {
		sig := syscall.SIGUSR2
		if want {
			sig = syscall.SIGUSR1
		}
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-drain:
			if got != want {
				t.Fatalf("%v: drain=%v", sig, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v not delivered", sig)
		}
	}
}
