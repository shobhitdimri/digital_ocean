package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"feature-flag-service/internal/app"
	"feature-flag-service/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a, err := app.New(cfg)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
