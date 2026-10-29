package usecase

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/reservation"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Expiry sweep limits: how many reservations one call handles.
const (
	DefaultExpireLimit = 100
	MaxExpireLimit     = 1000
)

// ExpireReservations is the reservation TTL: RESERVED reservations held at
// least OlderThan give their stock back (EXPIRED, StockReleased) and ask
// ordering to cancel the order (OrderCancellationRequested) — ordering moves
// the CONFIRMED order to CANCELLED, and the OrderCancelled it then publishes
// finds an EXPIRED row here and is dropped. Driven by `worker expire` (CLI /
// CronJob) and InventoryService.ExpireReservations (operator: dry run, small run).
//
// One call is one transaction (decorateTx): every stock Save in it compares
// the stock's version, and Repository.Update compares the reservation's status
// (Reservation itself has no version), which together are the lock between this
// sweep and a concurrent Commit / Release of the same reservation.
// A lost race (stock.ErrStale / reservation.ErrStale) fails the whole batch, which rolls back; the
// caller retries — the CLI on its next run, the rpc with ABORTED to the operator.
//
// Race with CompleteOrder: between CompleteOrder committing and inventory
// consuming its OrderCompleted the reservation is still RESERVED. A sweep
// landing in that window gives the stock back and asks for a cancellation
// ordering refuses (COMPLETED is terminal). The sweep does not try to avoid
// it; CommitStock repairs it when the OrderCompleted arrives: an EXPIRED
// reservation is sold from what is available then (COMMITTED), or ends
// COMMIT_SHORT when the stock was taken meanwhile (alert
// x-monorepo-commit-short). The window is one consumer hop against a TTL of hours.
type ExpireReservations struct {
	reservations reservation.Repository
	stocks       stock.Repository
	events       port.EventPublisher
	clock        port.Clock
}

func NewExpireReservations(reservations reservation.Repository, stocks stock.Repository, events port.EventPublisher, clock port.Clock) *ExpireReservations {
	return &ExpireReservations{reservations: reservations, stocks: stocks, events: events, clock: clock}
}

func (uc *ExpireReservations) Execute(c context.Context, in dto.ExpireReservationsInput) (*dto.ExpireReservationsOutput, error) {
	ctx := contextx.FromContext(c)

	if in.OlderThan <= 0 {
		return nil, reservation.ErrInvalidOlderThan
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = DefaultExpireLimit
	case limit > MaxExpireLimit:
		limit = MaxExpireLimit
	}

	now := uc.clock.Now()
	due, err := uc.reservations.ListReservedBefore(ctx, now.Add(-in.OlderThan), limit)
	if err != nil {
		return nil, fmt.Errorf("list reserved before: %w", err)
	}

	out := &dto.ExpireReservationsOutput{Expired: make([]dto.ExpiredReservation, 0, len(due)), DryRun: in.DryRun}
	for _, r := range due {
		out.Expired = append(out.Expired, dto.ExpiredReservation{OrderID: r.OrderID(), ReservedAt: r.UpdatedAt(), HeldFor: now.Sub(r.UpdatedAt())})
		if in.DryRun {
			continue
		}
		// A missing stock row is a precondition failure the domain reports (ErrStockMissing).
		stocks, err := loadStocks(ctx, uc.stocks, r.Lines())
		if err != nil {
			return nil, err
		}
		if err = r.Expire(stocks, now); err != nil {
			return nil, fmt.Errorf("expire reservation %s: %w", r.OrderID(), staleIfMoved(ctx, uc.reservations, r, err))
		}
		if err = saveStocks(ctx, uc.stocks, stocks); err != nil {
			return nil, err
		}
		if err = uc.reservations.Update(ctx, r); err != nil {
			return nil, fmt.Errorf("update reservation %s: %w", r.OrderID(), err)
		}
		if err = uc.events.Publish(ctx, r.PullEvents()...); err != nil {
			return nil, err
		}
	}
	if len(out.Expired) > 0 {
		ctx.Info("reservations expired", "count", len(out.Expired), "older_than", in.OlderThan.String(), "dry_run", in.DryRun)
	}

	return out, nil
}
