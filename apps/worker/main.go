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
//
// The binary is a cobra CLI: no subcommand runs the consumers; `worker poison`
// (poison.go) is the operator side of the poison queue.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

// run executes the CLI with args (os.Args[1:]) until SIGINT / SIGTERM.
func run(args []string) error {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := newRootCmd(config.Load)
	cmd.SetArgs(args)

	return cmd.ExecuteContext(c)
}

// newRootCmd builds the CLI. load is how commands get their config
// (config.Load in main, a fixed config in tests).
func newRootCmd(load func() (*config.AppConfig, error)) *cobra.Command {
	root := &cobra.Command{
		Use:           "worker",
		Version:       buildinfo.Get().String(),
		Short:         "event consumers of every bounded context",
		Long:          "worker runs each bounded context's event handlers on one router (no subcommand) and operates their poison queues (poison) and retention (prune).",
		SilenceUsage:  true, // a failing worker is not a usage error
		SilenceErrors: true, // main logs it
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
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

			return w.Run(cmd.Context())
		},
	}
	root.AddCommand(newPoisonCmd(load), newPruneCmd(load))

	return root
}
