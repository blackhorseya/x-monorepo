package main

import (
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// consumeEntry names the consumer the saga tests of this module build (InitializeConsume).
const consumeEntry = "payment-rpc saga test consumer"

// NewConsumer is pkgs/payment's Consumer under the test harness's name. `payment-rpc consume` no longer exists (#410):
// the process that runs this consumer is apps/payment-worker, which shares the implementation (pkgs/payment/consume.go).
// This stays only so the saga tests here (serve + expire + the consumer on one database) can build one.
func NewConsumer(cfg config.Worker, db *gorm.DB, logger *slog.Logger, tel *otel.Providers, publisher message.Publisher, subs payment.Subscriptions) (*payment.Consumer, error) {
	return payment.NewConsumer(consumeEntry, cfg, db, logger, tel, publisher, subs)
}

// CheckDurablePostgres is --require-durable for consume and expire.
func CheckDurablePostgres(cfg *config.AppConfig) error {
	return payment.CheckDurableConsume("payment-rpc", cfg)
}
