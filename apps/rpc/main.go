package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		slog.Error("rpc", "error", err)
		os.Exit(2)
	}
	switch {
	case opts.version:
		// `rpc --version` prints the stamped build and exits, like the cobra binaries.
		fmt.Println("rpc version " + buildinfo.Get().String())
		return
	case opts.waitCatalog:
		// `rpc --wait-catalog[=60s]`: the initContainer of the Helm chart. It reads the same config as the
		// server (catalog.upstream), waits best-effort for the catalog and exits 0, so the server starts
		// either way. A config it cannot load is a failure: the server would refuse to start on it too.
		if err := waitCommand(opts.waitBudget); err != nil {
			slog.Error("rpc --wait-catalog", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("rpc exited", "error", err)
		os.Exit(1)
	}
}

// waitCommand is `rpc --wait-catalog`: load the config, wait for the catalog (best effort), return.
func waitCommand(budget time.Duration) error {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	return waitForCatalog(c, cfg.Catalog, budget, time.Second, slog.Default())
}

func run() error {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	srv, cleanup, err := Initialize(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return srv.Run(c)
}
