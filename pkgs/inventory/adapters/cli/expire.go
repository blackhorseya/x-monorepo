// Package cli adapts the inventory use cases an operator runs from the worker
// binary (`worker expire`). Like adapters/rpc it holds decorated
// usecasex.UseCase values and knows nothing of cobra: flags are parsed in
// apps/worker, which hands this adapter plain values and a writer.
package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type ExpireReservationsUC = usecasex.UseCase[dto.ExpireReservationsInput, *dto.ExpireReservationsOutput]

// Expire runs the reservation TTL sweep batch after batch until one comes
// back short, printing each reservation as it goes.
type Expire struct {
	expire ExpireReservationsUC
}

func NewExpire(expire ExpireReservationsUC) *Expire { return &Expire{expire: expire} }

// Run expires (or, dryRun, lists) every RESERVED reservation held at least
// olderThan, batch at a time — each batch its own transaction — and reports
// them on out as a table. A failed batch stops the run with its error; what
// earlier batches did stays done.
func (h *Expire) Run(c context.Context, out io.Writer, olderThan time.Duration, batch int, dryRun bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	verb := "EXPIRED"
	if dryRun {
		verb = "WOULD EXPIRE"
	}
	_, _ = fmt.Fprintf(w, "ORDER\tRESERVED AT\tHELD FOR\t%s\n", verb)
	var total int
	for {
		res, err := h.expire.Execute(c, dto.ExpireReservationsInput{OlderThan: olderThan, Limit: batch, DryRun: dryRun})
		if err != nil {
			_ = w.Flush()
			return err
		}
		for _, e := range res.Expired {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.OrderID, e.ReservedAt.UTC().Format(time.RFC3339), e.HeldFor.Truncate(time.Second), verb)
		}
		total += len(res.Expired)
		// A dry run never changes what the next batch would see: one page is the answer.
		if dryRun || len(res.Expired) < batch {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s %d reservation(s) held longer than %s\n", map[bool]string{true: "would expire", false: "expired"}[dryRun], total, olderThan)

	return nil
}
