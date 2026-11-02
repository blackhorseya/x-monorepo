package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func main() {
	// `payment-rpc --version` prints the stamped build and exits, like the cobra binaries.
	// Everything else is configured through config.Load; `serve [--require-durable]` is the command (run).
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("payment-rpc version " + buildinfo.Get().String())
		return
	}
	if err := run(os.Args[1:]); err != nil {
		slog.Error("payment-rpc exited", "error", err)
		os.Exit(1)
	}
}

// requireDurableFlag makes the process refuse to start on settings that would serve payments from
// memory or without required auth. The Helm chart passes it as a container arg (extraEnv cannot
// change an arg); the default stays off so local development and the tests keep working.
const requireDurableFlag = "--require-durable"

// serveCommand is the one subcommand of this slice: the RPC server (docs/payment.md §1). consume
// and expire are the saga's (slice 3); until they exist they are unknown commands, not silent no-ops.
const serveCommand = "serve"

// run is the process after flag handling: args are the command line without the program name.
// The subcommand is serve, and its only flag is --require-durable; anything else is refused (a typo
// must not start the service without the guarantee the operator asked for). The messages name the
// position, never the value.
func run(args []string) error {
	if len(args) == 0 || args[0] != serveCommand {
		return fmt.Errorf("payment-rpc: a subcommand is required, the only one is %s", serveCommand)
	}
	requireDurable := false
	for n, a := range args[1:] {
		if a != requireDurableFlag {
			return fmt.Errorf("payment-rpc: unknown argument #%d (the only one is %s)", n+2, requireDurableFlag)
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

	provider, err := payment.ProvidePaymentProvider(cfg.Payment)
	if err != nil {
		return err
	}

	srv, cleanup, err := Initialize(cfg, provider)
	if err != nil {
		return err
	}
	defer cleanup()

	return srv.Run(c)
}
