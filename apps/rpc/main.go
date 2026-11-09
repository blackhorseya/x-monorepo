package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	if err := parseArgs(os.Args[1:]); err != nil {
		slog.Error("rpc", "error", err)
		os.Exit(2)
	}
	if len(os.Args) > 1 {
		// `rpc --version` prints the stamped build and exits, like the cobra binaries.
		fmt.Println("rpc version " + buildinfo.Get().String())
		return
	}
	if err := run(); err != nil {
		slog.Error("rpc exited", "error", err)
		os.Exit(1)
	}
}

// parseArgs reads the command line (without the program name). The only argument is --version / -v; anything
// else is an error, so a typo does not start the server quietly.
func parseArgs(args []string) error {
	switch {
	case len(args) == 0, len(args) == 1 && (args[0] == "--version" || args[0] == "-v"):
		return nil
	default:
		return errors.New("rpc: unknown argument (the only one is --version)")
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
