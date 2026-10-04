package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/instance"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	server := flag.String("server", "", "Multica origin (isolated test server only)")
	workspace := flag.String("workspace", "", "test workspace UUID")
	daemon := flag.String("daemon", "", "stable, exclusively owned daemon UUID")
	lock := flag.String("lock", "", "absolute controller lock path; reuse on restart")
	duration := flag.Duration("duration", time.Second, "fake execution duration")
	interval := flag.Duration("interval", time.Second, "heartbeat/status polling interval (max 10s)")
	fail := flag.Bool("fail", false, "report a simulated failure")
	recoverOnly := flag.Bool("recover-only", false, "register and recover orphans without claiming")
	flag.Parse()
	if *interval <= 0 || *interval > 10*time.Second || *duration <= 0 {
		return fmt.Errorf("interval must be in (0,10s], duration must be positive")
	}
	if *lock == "" || (*lock)[0] != '/' {
		return fmt.Errorf("absolute -lock path required")
	}
	release, err := instance.Lock(*lock)
	if err != nil {
		return err
	}
	defer release()
	api, err := multica.New(*server, os.Getenv("MULTICA_PROBE_TOKEN"))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	probe := controller.Probe{API: api, Duration: *duration, Interval: *interval, Fail: *fail, Observe: func(event, id string) { fmt.Printf("%s task=%s\n", event, id) }}
	rt, recovered, err := probe.Connect(ctx, *workspace, *daemon)
	if err != nil {
		return err
	}
	fmt.Printf("registered runtime=%s orphaned=%d retried=%d upstream=%s\n", rt.ID, recovered.Orphaned, recovered.Retried, multica.UpstreamRevision)
	if *recoverOnly {
		return nil
	}
	return probe.Run(ctx, rt.ID)
}
