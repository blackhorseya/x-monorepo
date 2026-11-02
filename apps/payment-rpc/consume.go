package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Consumer is `payment-rpc consume`: the router that drives payment's handlers (ordering's
// OrderConfirmed / OrderCancelled / OrderCompleted, payment's own PaymentRefundRequested and
// WebhookReceived). It has no listener and no credentials of its own to check; it needs Postgres
// (the event tables are the outbox it reads) and, like the other processes, the provider.
type Consumer struct {
	router *message.Router
	logger *slog.Logger
}

// NewConsumer registers subs on one router with payment's poison topic. Only the Postgres broker
// exists for payment (worker.broker redis is refused rather than ignored); the consumer group is
// worker.consumer_group + ".payment", the naming apps/worker uses per bounded context.
func NewConsumer(cfg config.Worker, db *gorm.DB, logger *slog.Logger, tel *otel.Providers, publisher message.Publisher, subs payment.Subscriptions) (*Consumer, error) {
	if db == nil {
		return nil, errors.New("payment-rpc: consume needs postgres.url: the event tables are the outbox it reads")
	}
	if cfg.Broker != config.BrokerPostgres && cfg.Broker != "" {
		return nil, fmt.Errorf("payment-rpc: consume reads the Postgres outbox only; worker.broker %q is not supported", cfg.Broker)
	}
	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		return nil, err
	}
	start := eventbus.FromBeginning
	if cfg.StartFrom == config.StartFromLatest {
		start = eventbus.FromLatest
	}
	instruments, err := eventbus.NewInstruments(tel.Meter)
	if err != nil {
		return nil, err
	}
	sub, err := eventbus.NewPostgresSubscriber(db, cfg.ConsumerGroup+"."+paymentv1.Schema, start, logger, eventbus.WithPollInterval(cfg.PollInterval))
	if err != nil {
		return nil, err
	}
	err = eventbus.Register(router, sub, eventbus.ConsumerOptions{
		Poison:         publisher,
		PoisonTopic:    eventbus.PoisonTopic(paymentv1.Schema),
		Instruments:    instruments,
		HandlerTimeout: cfg.HandlerTimeout,
	}, subs...)
	if err != nil {
		return nil, err
	}

	return &Consumer{router: router, logger: logger}, nil
}

// Run blocks until c is done (SIGTERM), then lets the handlers in flight finish and ack.
func (x *Consumer) Run(c context.Context) error {
	x.logger.Info("payment-rpc consume started", buildinfo.Get().LogAttrs()...)
	defer x.logger.Info("payment-rpc consume stopped")

	return x.router.Run(c)
}

// CheckDurablePostgres is --require-durable for consume and expire: a process that would run without
// Postgres has nothing to read or sweep. (There is no auth to check: they listen on nothing.)
func CheckDurablePostgres(cfg *config.AppConfig) error {
	if strings.TrimSpace(cfg.Postgres.URL) == "" {
		return errors.New("payment-rpc: --require-durable: postgres.url is empty; there is nothing to read or sweep")
	}

	return checkProviderSecret(cfg)
}
