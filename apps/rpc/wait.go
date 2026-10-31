package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// cliOptions is what the command line asks for.
type cliOptions struct {
	version     bool
	waitCatalog bool
	waitBudget  time.Duration
}

func parseArgs(args []string) (cliOptions, error) {
	return cliOptions{}, nil
}

// waitForCatalog is the best-effort wait of an initContainer.
func waitForCatalog(c context.Context, cfg config.Catalog, budget, every time.Duration, logger *slog.Logger) {
}
