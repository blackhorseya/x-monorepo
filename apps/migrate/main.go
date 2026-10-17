// apps/migrate applies every bounded context's Postgres migrations. Run it as a
// job (CI step, k8s Job, `task migrate -- up`) before rolling out services —
// services never migrate on start. Each BC owns a schema and a version table
// inside it, so BCs sharing one database stay independent.
//
//	migrate up        apply all pending migrations of every BC
//	migrate down      roll back one step in every BC (reverse registration order)
//	migrate version   print each BC's current version
//
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

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
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

func run(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: migrate up|down|version")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Postgres.URL == "" {
		return errors.New("APP_POSTGRES_URL is required")
	}

	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "up":
		for _, m := range migrations {
			if err := postgres.Up(c, cfg.Postgres, m); err != nil {
				return err
			}
			slog.Info("migrated", "schema", m.Schema, "direction", "up")
		}
	case "down":
		for _, m := range slices.Backward(migrations) {
			if err := postgres.Down(c, cfg.Postgres, m); err != nil {
				return err
			}
			slog.Info("migrated", "schema", m.Schema, "direction", "down")
		}
	case "version":
		for _, m := range migrations {
			v, dirty, err := postgres.Version(c, cfg.Postgres, m)
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%d\tdirty=%v\n", m.Schema, v, dirty)
		}
	default:
		return fmt.Errorf("unknown command %q (want up|down|version)", args[0])
	}

	return nil
}
