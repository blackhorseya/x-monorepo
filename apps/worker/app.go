package main

import (
	"context"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Worker owns the router. Adding a BC: append its Subscriptions in NewWorker
// and its ProviderSet in wire.go.
type Worker struct {
	router *message.Router
	logger *slog.Logger
}

// NewWorker registers every BC's subscriptions on one router. All BCs read
// through one Postgres subscriber (one consumer group), and failed messages
// of a BC go to that BC's poison topic.
func NewWorker(
	cfg config.Worker,
	db *gorm.DB,
	logger *slog.Logger,
	tel *otel.Providers,
	publisher message.Publisher,
	orderingSubs []eventbus.Subscription,
) (*Worker, error) {
	sub, err := eventbus.NewPostgresSubscriber(db, cfg.ConsumerGroup, logger)
	if err != nil {
		return nil, err
	}

	router, err := eventbus.NewRouter(logger, tel)
	if err != nil {
		return nil, err
	}
	err = eventbus.Register(router, sub, eventbus.ConsumerOptions{
		Poison:      publisher,
		PoisonTopic: eventbus.PoisonTopic(ordering.Schema),
	}, orderingSubs...)
	if err != nil {
		return nil, err
	}

	return &Worker{router: router, logger: logger}, nil
}

// Run blocks until c is done, then drains in-flight handlers.
func (w *Worker) Run(c context.Context) error {
	w.logger.Info("worker started")
	defer w.logger.Info("worker stopped")

	return w.router.Run(c)
}
