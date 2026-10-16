package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("rpc exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	srv, err := Initialize(cfg)
	if err != nil {
		return err
	}

	return srv.Run(c)
}
