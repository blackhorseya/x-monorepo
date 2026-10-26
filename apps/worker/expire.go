package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// maxExpireBatch mirrors usecase.MaxExpireLimit (apps may not import application).
const maxExpireBatch = 1000

// newExpireCmd is `worker expire`: the reservation TTL. RESERVED reservations
// held longer than --older-than give their stock back (EXPIRED) and ordering is
// asked to cancel the order. The sweep itself is inventory's (adapters/cli,
// use case ExpireReservations); this file is flags and wiring.
func newExpireCmd(load func() (*config.AppConfig, error)) *cobra.Command {
	var (
		olderThan time.Duration
		dryRun    bool
		batch     int
	)
	cmd := &cobra.Command{
		Use:   "expire --older-than <duration>",
		Short: "expire reservations held longer than a duration: release the stock, ask ordering to cancel",
		Long: `Expire, oldest first, every RESERVED reservation whose hold started more than
--older-than ago: the stock goes back (inventory.v1.StockReleased, reservation EXPIRED)
and ordering is asked to cancel the order (ordering.v1.OrderCancellationRequested,
reason "reservation expired after <held>"); ordering moves the CONFIRMED order to
CANCELLED and the OrderCancelled it then publishes finds the EXPIRED row here and is
dropped. Only RESERVED reservations are touched: a reservation the customer completed
(COMMITTED) or cancelled (RELEASED) is never due.

One batch is one transaction; a lost race with a concurrent commit / release of the
same order (optimistic lock on the stock rows) fails the batch, which rolls back — rerun.
The same thing over RPC, for a dry run or a small run: InventoryService.ExpireReservations
(console Expire page). Durations are Go syntax: 24h. Start with --dry-run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if olderThan <= 0 {
				return fmt.Errorf("--older-than must be positive (e.g. 24h)")
			}
			if batch <= 0 || batch > maxExpireBatch {
				// The use case caps its limit at the same number; a bigger flag would make the
				// "batch came back short" test below stop after the first (capped) batch.
				return fmt.Errorf("--batch must be between 1 and %d", maxExpireBatch)
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			if cfg.Postgres.URL == "" {
				return errors.New("postgres.url is required: the reservations live in Postgres")
			}
			sweep, cleanup, err := InitializeExpire(cfg)
			if err != nil {
				return err
			}
			defer cleanup()

			return sweep.Run(cmd.Context(), cmd.OutOrStdout(), olderThan, batch, dryRun)
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "expire reservations held at least this long (required; Go duration, e.g. 24h)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would expire, change nothing")
	cmd.Flags().IntVar(&batch, "batch", 100, "reservations per transaction (at most 1000)")
	_ = cmd.MarkFlagRequired("older-than")

	return cmd
}
