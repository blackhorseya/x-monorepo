package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Consumer is the router that drives payment's handlers (ordering's OrderConfirmed / OrderCancelled /
// OrderCompleted, payment's own PaymentRefundRequested and WebhookReceived). Two entries run it:
// apps/payment-worker, and `payment-rpc consume` until it is removed (#410 slice 2). It has no listener
// and no credentials of its own to check; it needs Postgres (the event tables are the outbox it reads)
// and, like the other processes, the provider.
//
// It lives here, not in a composition root, so the two entries share one implementation: apps/ may not
// import each other.
type Consumer struct {
	router *message.Router
	logger *slog.Logger
	entry  string
}

// ConsumerGroup is the offset cursor of payment's consumer: worker.consumer_group + ".payment", the
// naming apps/worker uses per bounded context. Every entry takes it from here. A different name would
// start a new group, which on worker.start_from=latest skips whatever was published in between.
func ConsumerGroup(cfg config.Worker) string {
	return cfg.ConsumerGroup + "." + paymentv1.Schema
}

// NewConsumer registers subs on one router with payment's poison topic. entry names the process the
// operator started ("payment-worker", "payment-rpc consume"): it opens the refusals and the log lines,
// and is the only thing that differs between the entries. Only the Postgres broker exists for payment
// (worker.broker redis is refused rather than ignored).
func NewConsumer(entry string, cfg config.Worker, db *gorm.DB, logger *slog.Logger, tel *otel.Providers, publisher message.Publisher, subs Subscriptions) (*Consumer, error) {
	if cfg.Broker != config.BrokerPostgres && cfg.Broker != "" {
		return nil, fmt.Errorf("%s reads the Postgres outbox only; worker.broker %q is not supported", entry, cfg.Broker)
	}
	if db == nil {
		return nil, fmt.Errorf("%s needs postgres.url: the event tables are the outbox it reads", entry)
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
	sub, err := eventbus.NewPostgresSubscriber(db, ConsumerGroup(cfg), start, logger, eventbus.WithPollInterval(cfg.PollInterval))
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

	return &Consumer{router: router, logger: logger, entry: entry}, nil
}

// Run blocks until c is done (SIGTERM), then lets the handlers in flight finish and ack.
func (x *Consumer) Run(c context.Context) error {
	x.logger.Info(x.entry+" started", buildinfo.Get().LogAttrs()...)
	defer x.logger.Info(x.entry + " stopped")

	return x.router.Run(c)
}

// CheckDurableConsume is --require-durable for the consumer and the sweep: a process that would run without
// Postgres has nothing to read or sweep. (There is no auth to check: they listen on nothing.) entry opens the
// refusals.
func CheckDurableConsume(entry string, cfg *config.AppConfig) error {
	if strings.TrimSpace(cfg.Postgres.URL) == "" {
		return fmt.Errorf("%s: --require-durable: postgres.url is empty; there is nothing to read or sweep", entry)
	}

	return CheckProviderSecret(entry, cfg)
}

// CheckProviderSecret is the part of --require-durable every workload shares: with the mock provider, payment.mock_webhook_secret
// has to be a real value. An external Secret can hold the key with an empty or blank value (optional: false only catches a
// MISSING key); the mock then signs with a random secret of its own in every process, and serve could not verify what another
// signed. The refusal names the setting, never a value.
func CheckProviderSecret(entry string, cfg *config.AppConfig) error {
	if cfg.Payment.Provider == MockProvider && strings.TrimSpace(cfg.Payment.MockWebhookSecret) == "" {
		return errors.New(entry + ": --require-durable: payment.mock_webhook_secret is empty or blank; every process would sign with a random secret of its own and serve could not verify what another signed")
	}

	return nil
}
