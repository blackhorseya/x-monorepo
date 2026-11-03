// apps/payment-worker is the composition root of payment's event consumer: load config → wire → run the
// router that drives payment's handlers (ordering's OrderConfirmed / OrderCancelled / OrderCompleted,
// payment's own PaymentRefundRequested and WebhookReceived). No business logic, no mapping.
//
// It is `payment-rpc consume` as a binary of its own (#410): the same Consumer (pkgs/payment, shared, not
// copied), the same payment.* settings, the same consumer group (worker.consumer_group + ".payment"), the
// same refusals. It has no listener, no auth and no identity BC: the wire graph holds no server, so there
// is no credential source to refuse. It carries payment's provider credential (the mock's webhook secret),
// which apps/worker deliberately refuses. The schema is apps/migrate's; this binary never migrates.
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
	// `payment-worker --version` prints the stamped build and exits, like the other binaries.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("payment-worker version " + buildinfo.Get().String())
		return
	}
	if err := run(os.Args[1:]); err != nil {
		slog.Error("payment-worker exited", "error", err)
		os.Exit(1)
	}
}

// requireDurableFlag makes the process refuse to start on settings that would leave it nothing to read
// (an empty postgres.url) or with a provider secret it could not share. The Helm chart passes it as a
// container arg (extraEnv cannot change an arg); the default stays off so local development and the tests
// keep working.
const requireDurableFlag = "--require-durable"

// run is the process after flag handling: args are the command line without the program name. The only
// argument is --require-durable; anything else is refused (a typo must not start the service without the
// guarantee the operator asked for). The messages name the position, never the value.
func run(args []string) error {
	requireDurable := false
	for n, a := range args {
		if a != requireDurableFlag {
			return fmt.Errorf("payment-worker: unknown argument #%d (the only one is %s)", n+1, requireDurableFlag)
		}
		requireDurable = true
	}

	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.Postgres.URL) == "" {
		return fmt.Errorf("%s needs postgres.url: there is nothing to read without it", entry)
	}
	if requireDurable {
		if err := payment.CheckDurableConsume(entry, cfg); err != nil {
			return err
		}
	}

	provider, err := payment.BuildProvider(cfg.Payment)
	if err != nil {
		return err
	}

	consumer, cleanup, err := InitializeConsume(cfg, provider)
	if err != nil {
		return err
	}
	defer cleanup()

	return consumer.Run(c)
}
