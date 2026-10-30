package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	// The only flag: `catalog-rpc --version` prints the stamped build and exits, like
	// the cobra binaries. Everything else is configured through config.Load.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("catalog-rpc version " + buildinfo.Get().String())
		return
	}
	if err := run(); err != nil {
		slog.Error("catalog-rpc exited", "error", err)
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

	srv, cleanup, err := Initialize(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return srv.Run(c)
}
