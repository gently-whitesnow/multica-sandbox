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
	return service.Run(ctx, c, *state, *token, os.Stdout)
}
