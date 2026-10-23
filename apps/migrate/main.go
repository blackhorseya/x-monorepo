// apps/migrate applies every bounded context's Postgres migrations. Run it as a
// job (CI step, k8s Job, `task migrate -- up`) before rolling out services —
// services never migrate on start. Each BC owns a schema and a version table
// inside it, so BCs sharing one database stay independent.
//
//	migrate up        apply all pending migrations of every BC
//	migrate down      roll back one step in every BC (reverse registration order)
//	migrate version   print each BC's current version
//
// The binary is a cobra CLI like apps/worker; `--help` on any command.
// Adding a BC: append its infra/postgres Migration() to migrations below.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/spf13/cobra"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// migrations lists every BC, in dependency order: inventory publishes into
// ordering's event tables, so ordering goes first.
var migrations = []postgres.Migration{
	orderingpg.Migration(),
	inventorypg.Migration(),
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("migrate failed", "error", err)
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
		Use:           "migrate",
		Version:       buildinfo.Get().String(),
		Short:         "apply every bounded context's Postgres migrations",
		Long:          "migrate runs each bounded context's embedded migrations against postgres.url, one schema and version table per BC. Services never migrate on start: run `migrate up` before rolling them out.",
		SilenceUsage:  true, // a failing migration is not a usage error
		SilenceErrors: true, // main logs it
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = cmd.Help()
			return errors.New("a subcommand is required: up, down or version")
		},
	}

	// with loads the config and hands a command the Postgres settings.
	with := func(fn func(c context.Context, cfg config.Postgres) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			if cfg.Postgres.URL == "" {
				return errors.New("postgres.url is required (APP_POSTGRES_URL or configs/local.yaml)")
			}
			return fn(cmd.Context(), cfg.Postgres)
		}
	}

	root.AddCommand(
		&cobra.Command{
			Use:   "up",
			Short: "apply all pending migrations of every BC (no-op when current)",
			Args:  cobra.NoArgs,
			RunE: with(func(c context.Context, cfg config.Postgres) error {
				for _, m := range migrations {
					if err := postgres.Up(c, cfg, m); err != nil {
						return err
					}
					slog.Info("migrated", "schema", m.Schema, "direction", "up")
				}
				return nil
			}),
		},
		&cobra.Command{
			Use:   "down",
			Short: "roll back one step in every BC, in reverse registration order",
			Args:  cobra.NoArgs,
			RunE: with(func(c context.Context, cfg config.Postgres) error {
				for _, m := range slices.Backward(migrations) {
					if err := postgres.Down(c, cfg, m); err != nil {
						return err
					}
					slog.Info("migrated", "schema", m.Schema, "direction", "down")
				}
				return nil
			}),
		},
		&cobra.Command{
			Use:   "version",
			Short: "print each BC's current version and whether it is dirty",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return with(func(c context.Context, cfg config.Postgres) error {
					for _, m := range migrations {
						v, dirty, err := postgres.Version(c, cfg, m)
						if err != nil {
							return err
						}
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%d\tdirty=%v\n", m.Schema, v, dirty)
					}
					return nil
				})(cmd, args)
			},
		},
	)

	return root
}
