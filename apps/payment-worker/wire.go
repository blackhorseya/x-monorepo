//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// InitializeConsume is the injector, and wire.go is the ONLY place AppConfig is mapped into component configs
// (via wire.FieldsOf). The payment provider comes in as a builder: run validates payment.provider
// (payment.BuildProvider) before anything else is opened, and the injector builds the provider once the database
// is open (the mock keeps its state there). The graph holds no server: there is no listener, no auth interceptor
// and no credential source. The router and payment's consumer use cases publish into the outbox
// (eventbus.ProvidePublisher) like every other BC.
func InitializeConsume(cfg *config.AppConfig, provider payment.ProviderBuilder) (*payment.Consumer, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "OTel", "Payment", "Worker"),
		postgres.NewDB,
		otel.ProviderSet,
		usecasex.NewRecorder,
		eventbus.ProvidePublisher,
		payment.ConsumerProviderSet,
		NewConsumer,
	)

	return nil, nil, nil
}
