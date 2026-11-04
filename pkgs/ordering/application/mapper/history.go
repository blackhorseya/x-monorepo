package mapper

import (
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// ToHistoryOutput maps one history entry. showSubject says whether the caller may see who acted.
func ToHistoryOutput(e order.HistoryEntry, showSubject bool) dto.HistoryEntryOutput {
	out := dto.HistoryEntryOutput{
		Seq:        int32(e.Seq()), //nolint:gosec // seq is 1..4
		To:         e.To().String(),
		OccurredAt: e.OccurredAt(),
		ActorKind:  e.ActorKind().String(),
		Cause:      e.Cause().String(),
	}
	if e.From() != order.StatusUnknown {
		out.From = e.From().String()
	}
	if showSubject {
		out.ActorSubject = e.ActorSubject()
	}

	return out
}
