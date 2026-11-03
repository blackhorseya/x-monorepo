package main

import (
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// consumeEntry names this process in the refusals and the log lines of the shared consumer.
const consumeEntry = "payment-rpc consume"

// NewConsumer is `payment-rpc consume`: pkgs/payment's Consumer under this entry's name. The implementation is
// shared with apps/payment-worker (#410); this command is removed in slice 2.
func NewConsumer(cfg config.Worker, db *gorm.DB, logger *slog.Logger, tel *otel.Providers, publisher message.Publisher, subs payment.Subscriptions) (*payment.Consumer, error) {
	return payment.NewConsumer(consumeEntry, cfg, db, logger, tel, publisher, subs)
}

// CheckDurablePostgres is --require-durable for consume and expire.
func CheckDurablePostgres(cfg *config.AppConfig) error {
	return payment.CheckDurableConsume("payment-rpc", cfg)
}
