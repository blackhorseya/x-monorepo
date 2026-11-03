package main

import (
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// entry names this process in the refusals and the log lines of the shared consumer.
const entry = "payment-worker"

// NewConsumer is pkgs/payment's Consumer under this entry's name; the implementation (the router, the
// consumer group, the refusals) is shared with `payment-rpc consume`, not copied.
func NewConsumer(cfg config.Worker, db *gorm.DB, logger *slog.Logger, tel *otel.Providers, publisher message.Publisher, subs payment.Subscriptions) (*payment.Consumer, error) {
	return payment.NewConsumer(entry, cfg, db, logger, tel, publisher, subs)
}
