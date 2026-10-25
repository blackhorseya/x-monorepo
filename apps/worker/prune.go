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

A row stays as long as ONE group with a cursor has not read it: the NOTE column lists
every group with unread rows and the age of its oldest one — "held back by" when that
age is past --older-than (it really keeps rows from going), "behind" when younger (a
live group between two polls is normal there). A group that will never run again (an E2E run
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
			_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n", res.Table, res.Rows, note(res, opts))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %d row(s) older than %s\n", map[bool]string{true: "would delete", false: "deleted"}[opts.DryRun], total, opts.OlderThan)

	return nil
}

// note is the NOTE column of one topic table: why it was skipped, which
// groups were forgotten, which hold prunable rows back (oldest unread past
// --older-than) and which are merely behind.
func note(res eventbus.PruneResult, opts eventbus.PruneOptions) string {
	var parts []string
	if len(res.Forgotten) > 0 {
		verb := "forgot"
		if opts.DryRun {
			verb = "would forget"
		}
		parts = append(parts, verb+" "+strings.Join(res.Forgotten, ", "))
	}
	if res.Skipped != "" {
		parts = append(parts, res.Skipped)
	}
	var held, behind []string
	for _, b := range res.Blockers {
		s := fmt.Sprintf("%s (%d unread, oldest %s)", b.Group, b.Unread, age(b.OldestUnread))
		if b.OldestUnread >= opts.OlderThan {
			held = append(held, s)
		} else {
			behind = append(behind, s)
		}
	}
	if len(held) > 0 {
		parts = append(parts, "held back by "+strings.Join(capped(held), ", "))
	}
	if len(behind) > 0 {
		parts = append(parts, "behind: "+strings.Join(capped(behind), ", "))
	}

	return strings.Join(parts, "; ")
}

// capped keeps the first three entries and counts the rest.
func capped(names []string) []string {
	if len(names) <= 3 {
		return names
	}

	return append(names[:3:3], fmt.Sprintf("+%d more", len(names)-3))
}

// age renders a duration the way an operator reads it: 42s, 15m, 7h, 7d.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
