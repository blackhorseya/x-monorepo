package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
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

// The subcommands (docs/payment.md §1): serve is the RPC server and the webhook route, consume the event
// handlers, expire one run of the sweep (the CronJob).
const (
	serveCommand   = "serve"
	consumeCommand = "consume"
	expireCommand  = "expire"
)

// run is the process after flag handling: args are the command line without the program name.
// The subcommand is serve or consume, and its only flag is --require-durable; anything else is
// refused (a typo must not start the service without the guarantee the operator asked for). The
// messages name the position, never the value.
func run(args []string) error {
	if len(args) == 0 || (args[0] != serveCommand && args[0] != consumeCommand && args[0] != expireCommand) {
		return fmt.Errorf("payment-rpc: a subcommand is required: %s, %s or %s", serveCommand, consumeCommand, expireCommand)
	}
	cmd := args[0]
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

	if cmd != serveCommand && strings.TrimSpace(cfg.Postgres.URL) == "" {
		return fmt.Errorf("payment-rpc: %s needs postgres.url: there is nothing to read or sweep without it", cmd)
	}
	if requireDurable {
		check := CheckDurable
		if cmd != serveCommand {
			check = CheckDurablePostgres
		}
		if err := check(cfg); err != nil {
			return err
		}
	}

	provider, err := payment.ProvidePaymentProvider(cfg.Payment)
	if err != nil {
		return err
	}

	if cmd == expireCommand {
		expire, cleanup, err := InitializeExpire(cfg, provider)
		if err != nil {
			return err
		}
		defer cleanup()

		return expire.Run(c)
	}

	if cmd == consumeCommand {
		consumer, cleanup, err := InitializeConsume(cfg, provider)
		if err != nil {
			return err
		}
		defer cleanup()

		return consumer.Run(c)
	}

	srv, cleanup, err := Initialize(cfg, provider)
	if err != nil {
		return err
	}
	defer cleanup()

	return srv.Run(c)
}
