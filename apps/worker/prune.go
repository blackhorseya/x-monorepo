package main

import (
	"context"
	"fmt"
	"io"
	"strings"
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
		forget     []string
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

A row stays as long as ONE group with a cursor has not read it: the NOTE column names
the groups holding old rows back. A group that will never run again (an E2E run
against this database, a renamed worker.consumer_group, a BC group from before the
broker moved to redis) is removed with --forget-group <name>; if it ever comes back it
starts over as a new group (worker.start_from).

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
			opts := eventbus.PruneOptions{OlderThan: olderThan, Unconsumed: unconsumed, DryRun: dryRun, BatchSize: batch, ForgetGroups: forget}
			return withDB(cmd, load, func(c context.Context, db *gorm.DB) error {
				return prune(c, db, bcs, opts, cmd.OutOrStdout())
			})
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "keep rows younger than this (required; Go duration, e.g. 720h)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count what would be deleted, delete nothing")
	cmd.Flags().BoolVar(&unconsumed, "unconsumed", false, "also prune topic tables no consumer group has a cursor on")
	cmd.Flags().IntVar(&batch, "batch", 10000, "rows per DELETE statement")
	cmd.Flags().StringArrayVar(&forget, "forget-group", nil, "delete this consumer group's cursors first (repeatable); it no longer holds rows back")
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
		results, err := eventbus.PruneSchema(c, db, bc, opts, schemas)
		if err != nil {
			return err
		}
		for _, res := range results {
			total += res.Rows
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", res.Table, res.Rows, note(res, opts.DryRun))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %d row(s) older than %s\n", map[bool]string{true: "would delete", false: "deleted"}[opts.DryRun], total, opts.OlderThan)

	return nil
}

// note is the NOTE column of one topic table: why it was skipped, which
// groups were forgotten, and which still hold old rows back.
func note(res eventbus.PruneResult, dryRun bool) string {
	var parts []string
	if len(res.Forgotten) > 0 {
		verb := "forgot"
		if dryRun {
			verb = "would forget"
		}
		parts = append(parts, verb+" "+strings.Join(res.Forgotten, ", "))
	}
	if res.Skipped != "" {
		parts = append(parts, res.Skipped)
	}
	if len(res.Blockers) > 0 {
		names := make([]string, 0, len(res.Blockers))
		for i, b := range res.Blockers {
			if i == 3 {
				names = append(names, fmt.Sprintf("+%d more", len(res.Blockers)-3))
				break
			}
			names = append(names, fmt.Sprintf("%s (%d unread)", b.Group, b.Unread))
		}
		parts = append(parts, "held back by "+strings.Join(names, ", "))
	}

	return strings.Join(parts, "; ")
}
