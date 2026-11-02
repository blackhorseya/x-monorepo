package usecase

import (
	"context"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

// RecordOrderEnded applies OrderCancelled or OrderCompleted to the order's payment. SPEC STAGE: it does nothing.
type RecordOrderEnded struct {
	payments payment.Repository
	outcomes payment.OutcomeRepository
	events   port.EventPublisher
	clock    port.Clock
	outcome  payment.Outcome
}

// NewRecordOrderCancelled is the use case of ordering.v1.OrderCancelled.
func NewRecordOrderCancelled(payments payment.Repository, outcomes payment.OutcomeRepository, events port.EventPublisher, clock port.Clock) *RecordOrderEnded {
	return &RecordOrderEnded{payments: payments, outcomes: outcomes, events: events, clock: clock, outcome: payment.OutcomeCancelled}
}

// NewRecordOrderCompleted is the use case of ordering.v1.OrderCompleted.
func NewRecordOrderCompleted(payments payment.Repository, outcomes payment.OutcomeRepository, events port.EventPublisher, clock port.Clock) *RecordOrderEnded {
	return &RecordOrderEnded{payments: payments, outcomes: outcomes, events: events, clock: clock, outcome: payment.OutcomeCompletedUnpaid}
}

func (uc *RecordOrderEnded) Execute(context.Context, dto.OrderEndedInput) (*dto.OrderEndedOutput, error) {
	return &dto.OrderEndedOutput{}, nil
}
