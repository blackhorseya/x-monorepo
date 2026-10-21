package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
)

// newPruneCmd is `worker prune`: delete what only grows — the outbox rows
// every consumer group has acknowledged and the inbox rows of consumed
// messages — once they are older than --older-than. Poison tables are not
// touched (poison drop is the explicit action), and in redis mode the
// streams are not trimmed here (XTRIM is a Redis-side policy).
func newPruneCmd(load func() (*config.AppConfig, error)) *cobra.Command {
	var (
		olderThan  time.Duration
		dryRun     bool
		unconsumed bool
		batch      int
	)
	cmd := &cobra.Command{
		Use:   "prune [<bc>] --older-than <duration>",
		Short: "delete acknowledged outbox rows and consumed inbox rows older than a duration",
		Long: `Delete, per bounded context, the rows nothing will read again:

  <bc>.events_<topic>  rows older than --older-than that EVERY consumer group with a
                       cursor on the table has acknowledged (in redis mode that is the
                       forwarder's cursor). A table no group has a cursor on is skipped
                       unless --unconsumed: with no cursor nothing proves it was read.
  <bc>.inbox           consumed message ids older than --older-than, except ids still
                       parked in any poison table (poison requeue republishes a message
                       under its original id; the inbox row is what keeps a BC that
                       already consumed it from doing so again).

Poison tables are never pruned here (see poison drop). Durations are Go syntax:
720h = 30 days. Start with --dry-run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if olderThan <= 0 {
				return fmt.Errorf("--older-than must be positive (e.g. 720h)")
			}
			bcs := schemas
			if len(args) == 1 {
				bc, err := schemaOf(args[0])
				if err != nil {
					return err
				}
				bcs = []string{bc}
			}
			opts := eventbus.PruneOptions{OlderThan: olderThan, Unconsumed: unconsumed, DryRun: dryRun, BatchSize: batch}
			return withDB(cmd, load, func(c context.Context, db *gorm.DB) error {
				return prune(c, db, bcs, opts, cmd.OutOrStdout())
			})
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "keep rows younger than this (required; Go duration, e.g. 720h)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count what would be deleted, delete nothing")
	cmd.Flags().BoolVar(&unconsumed, "unconsumed", false, "also prune topic tables no consumer group has a cursor on")
	cmd.Flags().IntVar(&batch, "batch", 10000, "rows per DELETE statement")
	_ = cmd.MarkFlagRequired("older-than")

	return cmd
}

func prune(c context.Context, db *gorm.DB, bcs []string, opts eventbus.PruneOptions, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	verb := "DELETED"
	if opts.DryRun {
		verb = "WOULD DELETE"
	}
	_, _ = fmt.Fprintf(w, "TABLE\t%s\tNOTE\n", verb)
	var total int64
	for _, bc := range bcs {
		tables, err := eventbus.TopicTables(c, db, bc)
		if err != nil {
			return err
		}
		for _, tbl := range tables {
			res, err := eventbus.PruneTopicTable(c, db, tbl, opts)
			if err != nil {
				return err
			}
			total += res.Rows
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", res.Table, res.Rows, res.Skipped)
		}
		res, err := eventbus.PruneInbox(c, db, bc, opts.OlderThan, schemas, opts.DryRun)
		if err != nil {
			return err
		}
		total += res.Rows
		_, _ = fmt.Fprintf(w, "%s\t%d\t\n", res.Table, res.Rows)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %d row(s) older than %s\n", map[bool]string{true: "would delete", false: "deleted"}[opts.DryRun], total, opts.OlderThan)

	return nil
}
