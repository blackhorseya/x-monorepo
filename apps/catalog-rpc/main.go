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
	// `catalog-rpc --version` prints the stamped build and exits, like the cobra binaries.
	// Everything else is configured through config.Load; --require-durable is the one flag (run).
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("catalog-rpc version " + buildinfo.Get().String())
		return
	}
	if err := run(os.Args[1:]); err != nil {
		slog.Error("catalog-rpc exited", "error", err)
		os.Exit(1)
	}
}

// requireDurableFlag makes the process refuse to start on settings that would serve prices from
// memory or without required auth. The Helm chart passes it as a container arg (extraEnv cannot
// change an arg); the default stays off so local development and the tests keep working.
const requireDurableFlag = "--require-durable"

// run is the process after flag handling: args are the command line without the program name.
// The only argument is --require-durable; anything else is refused (a typo must not start the
// service without the guarantee the operator asked for). The message names the position, never
// the value.
func run(args []string) error {
	requireDurable := false
	for n, a := range args {
		if a != requireDurableFlag {
			return fmt.Errorf("catalog-rpc: unknown argument #%d (the only one is %s)", n+1, requireDurableFlag)
		}
		requireDurable = true
	}

	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if requireDurable {
		if err := CheckDurable(cfg); err != nil {
			return err
		}
	}

	srv, cleanup, err := Initialize(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return srv.Run(c)
}
