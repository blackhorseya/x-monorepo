//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf). The payment provider comes in as an argument: run builds
// it from payment.provider (payment.ProvidePaymentProvider) before anything else is opened.
func Initialize(cfg *config.AppConfig, provider port.PaymentProvider) (*server.Server, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "HTTP", "Log", "Postgres", "OTel"),
		postgres.NewDB,
		server.New,
		otel.ProviderSet,
		usecasex.NewRecorder,
		NewAuth,
		NewRoutes,
		NewDocs,
		NewReadiness,
		NoCredentialResolver,
		NoOIDC,
		ServerOptions,
		eventbus.ProvidePublisher,
		payment.ProviderSet,
	)

	return nil, nil, nil
}

// InitializeConsume builds `payment-rpc consume`: the router and payment's consumer use cases,
// publishing into the outbox (eventbus.ProvidePublisher) like every other BC. No listener, no auth.
func InitializeConsume(cfg *config.AppConfig, provider port.PaymentProvider) (*Consumer, func(), error) {
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

// InitializeExpire builds `payment-rpc expire`: the sweep over the database and the provider, publishing
// into the outbox. No listener, no router.
func InitializeExpire(cfg *config.AppConfig, provider port.PaymentProvider) (*payment.Expire, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "OTel", "Payment"),
		postgres.NewDB,
		otel.ProviderSet,
		usecasex.NewRecorder,
		eventbus.ProvidePublisher,
		payment.ExpireProviderSet,
	)

	return nil, nil, nil
}
