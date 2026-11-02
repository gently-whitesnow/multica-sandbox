package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	config := flag.String("config", "/etc/multica-sandbox/controller.json", "controller configuration")
	state := flag.String("state", "/var/lib/multica-sandbox", "persistent identity and lock directory")
	token := flag.String("token-file", "/run/secrets/multica_token", "trusted controller token file")
	flag.Parse()
	c, err := service.ReadConfig(*config)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c.Drain = drainSignals(ctx)
	return service.Run(ctx, c, *state, *token, os.Stdout)
}

// drainSignals maps SIGUSR1 to a deploy drain and SIGUSR2 to its release. The host
// deploy controller sends them with docker kill; a restart always starts undrained.
func drainSignals(ctx context.Context) <-chan bool {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGUSR1, syscall.SIGUSR2)
	drain := make(chan bool, 4)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-signals:
				drain <- s == syscall.SIGUSR1
			}
		}
	}()
	return drain
}
