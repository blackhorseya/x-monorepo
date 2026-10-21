package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// schemas lists every BC the worker serves; their poison tables are what the
// poison commands operate on. Adding a BC: add its Schema here too.
var schemas = []string{ordering.Schema, inventory.Schema}

// newPoisonCmd is `worker poison`: list / show / requeue / drop the messages
// a BC's handlers gave up on after every retry (<bc>.events_poison). Rows are
// addressed by offset (the first column of list); a message poisoned twice
// is two rows. Arguments are validated before any database is opened; the
// commands are one-shot operator actions and share nothing with the router.
func newPoisonCmd(load func() (*config.AppConfig, error)) *cobra.Command {
	poison := &cobra.Command{
		Use:   "poison",
		Short: "operate a bounded context's poison queue",
		Long: `Operate a bounded context's poison table (<bc>.events_poison): the messages
its handlers gave up on after every retry. Needs postgres.url.

Rows are addressed by offset (the first column of list); a message poisoned
twice is two rows. <bc> is one of: ` + strings.Join(schemas, ", ") + `.`,
		Args: cobra.NoArgs, // "poison bogus" is an error, not help
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	var limit int
	list := &cobra.Command{
		Use:   "list [<bc>]",
		Short: "parked messages of one BC, or of every BC (oldest first)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bcs := schemas
			if len(args) == 1 {
				bc, err := schemaOf(args[0])
				if err != nil {
					return err
				}
				bcs = []string{bc}
			}
			return withDB(cmd, load, func(c context.Context, db *gorm.DB) error {
				return listPoison(c, db, bcs, limit, cmd.OutOrStdout())
			})
		},
	}
	list.Flags().IntVarP(&limit, "limit", "n", 50, "rows per BC, 0 = all")

	show := &cobra.Command{
		Use:   "show <bc> <offset>",
		Short: "one message in full: metadata and payload (proto bytes, base64)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bc, offsets, err := target(args)
			if err != nil {
				return err
			}
			return withDB(cmd, load, func(c context.Context, db *gorm.DB) error {
				return showPoison(c, db, bc, offsets[0], cmd.OutOrStdout())
			})
		},
	}

	// requeue and drop share the argument shape; op is the platform call.
	rowsCmd := func(use, short, long, label string, op func(context.Context, *gorm.DB, string, ...int64) (int, error)) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <bc> <offset>...|all",
			Short: short,
			Long:  long,
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				bc, offsets, err := target(args)
				if err != nil {
					return err
				}
				return withDB(cmd, load, func(c context.Context, db *gorm.DB) error {
					n, err := op(c, db, eventbus.PoisonTopic(bc), offsets...)
					if err != nil {
						return err
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %d message(s) of %s\n", label, n, eventbus.PoisonTopic(bc))
					return nil
				})
			},
		}
	}
	requeue := rowsCmd("requeue", "publish parked messages back to the topic they came from and unpark them",
		`Publish the rows back to the topic each was consumed from (its topic_poisoned
metadata), with the same message id and the poison metadata removed, and delete
them from the poison table in the same transaction. The message lands in the
topic's outbox table like any other publish, so every consumer group behind the
topic — including the Redis forwarder — handles it again; if it fails again it
is parked again. "all" = every row.`, "requeued", eventbus.Requeue)
	drop := rowsCmd("drop", "delete parked messages for good", `Delete the rows. "all" = every row.`, "dropped", eventbus.Drop)

	poison.AddCommand(list, show, requeue, drop)

	return poison
}

// withDB loads the config, opens the database the way the worker does and
// runs fn on it (poison and prune commands).
func withDB(cmd *cobra.Command, load func() (*config.AppConfig, error), fn func(context.Context, *gorm.DB) error) error {
	cfg, err := load()
	if err != nil {
		return err
	}
	if cfg.Postgres.URL == "" {
		return errors.New("postgres.url is required: the event tables live in Postgres")
	}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		return err
	}
	defer telCleanup()
	db, cleanup, err := postgres.NewDB(cfg.Postgres, tel)
	if err != nil {
		return err
	}
	defer cleanup()

	return fn(cmd.Context(), db)
}

// target parses "<bc> <offset>...|all": all = no offsets = every row.
func target(args []string) (string, []int64, error) {
	bc, err := schemaOf(args[0])
	if err != nil {
		return "", nil, err
	}
	if len(args) == 2 && args[1] == "all" {
		return bc, nil, nil
	}
	offsets := make([]int64, 0, len(args)-1)
	for _, a := range args[1:] {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("offset %q: want a number or all", a)
		}
		offsets = append(offsets, n)
	}

	return bc, offsets, nil
}

func schemaOf(bc string) (string, error) {
	if !slices.Contains(schemas, bc) {
		return "", fmt.Errorf("unknown bounded context %q (want one of %s)", bc, strings.Join(schemas, ", "))
	}

	return bc, nil
}

func listPoison(c context.Context, db *gorm.DB, bcs []string, limit int, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "OFFSET\tBC\tUUID\tPARKED AT\tTOPIC\tREASON")
	for _, bc := range bcs {
		rows, err := eventbus.ListPoison(c, db, eventbus.PoisonTopic(bc), limit)
		if err != nil {
			return err
		}
		for _, m := range rows {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
				m.Offset, bc, m.UUID, m.CreatedAt.Format("2006-01-02 15:04:05"), m.Topic, oneLine(m.Reason, 80))
		}
	}

	return w.Flush()
}

func showPoison(c context.Context, db *gorm.DB, bc string, offset int64, out io.Writer) error {
	rows, err := eventbus.ListPoison(c, db, eventbus.PoisonTopic(bc), 0)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(m eventbus.PoisonMessage) bool { return m.Offset == offset })
	if i < 0 {
		return fmt.Errorf("no row %d in %s", offset, eventbus.TableName(eventbus.PoisonTopic(bc)))
	}
	m := rows[i]
	_, _ = fmt.Fprintf(out, "offset:    %d\nuuid:      %s\nparked at: %s\ntopic:     %s\nhandler:   %s\nreason:    %s\nmetadata:\n",
		m.Offset, m.UUID, m.CreatedAt.Format("2006-01-02 15:04:05"), m.Topic, m.Handler, m.Reason)
	keys := make([]string, 0, len(m.Metadata))
	for k := range m.Metadata {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(out, "  %s: %s\n", k, m.Metadata[k])
	}
	// Payload is the topic's proto message; the worker has no generic decoder,
	// so it is printed raw for `buf convert` / protoc --decode.
	_, _ = fmt.Fprintf(out, "payload:   %d bytes (base64)\n  %s\n", len(m.Payload), base64.StdEncoding.EncodeToString(m.Payload))

	return nil
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}

	return s
}
