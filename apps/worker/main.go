// apps/worker is the composition root for event consumers: it wires every
// BC's ProviderSet (the same use cases apps/rpc serves), subscribes each BC's
// Subscriptions on one Watermill router and runs it. Scale it by queue depth,
// independently of apps/rpc; several replicas with the same worker.consumer_group
// share the work.
//
// It needs postgres.url: the event tables are the outbox every use case
// publishes into. worker.broker picks what the handlers read from: those
// tables (postgres, default) or Redis Streams the worker forwards them into
// (redis, needs redis.url). See NewWorker.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited", "error", err)
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
	if cfg.Postgres.URL == "" {
		return errors.New("postgres.url is required: the event tables are the outbox the worker reads or forwards")
	}

	w, cleanup, err := Initialize(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return w.Run(c)
}
